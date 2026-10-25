package agent

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Avalanche-io/c4-operator/work/tokens"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

// Observing reads the Claude Code sessions already on a machine, as they
// are, before anything is moved onto the farm: what each is doing in a
// word, what it has used, its open background work, a usage window it hit,
// and the one thing that needs its person, if any. It only reads (the CLI's
// own list, and the transcripts); it moves, stops and signs in nothing.

// Observed is one session as observing sees it.
type Observed struct {
	Session string `json:"session"`
	Name    string `json:"name,omitempty"`
	Kind    string `json:"kind"`
	Running bool   `json:"running"`
	// Word is its state in one word: needs you, at its limit, working,
	// failed, idle, done, or stopped.
	Word string `json:"word"`
	// NeedsYou is the one thing that needs its person, or "".
	NeedsYou   string     `json:"needsYou,omitempty"`
	Cwd        string     `json:"cwd,omitempty"`
	StartedAt  *time.Time `json:"startedAt,omitempty"`
	LastActive *time.Time `json:"lastActive,omitempty"`
	// TurnAge is how long since the session last wrote, in words
	// ("5 minutes").
	TurnAge        string        `json:"turnAge,omitempty"`
	BackgroundOpen int           `json:"backgroundOpen"`
	Usage          *ObservedUse  `json:"usage,omitempty"`
	Limit          *ObservedHalt `json:"limit,omitempty"`
	Transcript     string        `json:"transcript,omitempty"`
	// Parent, on a worker's row, is the session (or worker) that started
	// it; ParentClaimed says the link is a match, not a record. Description
	// is the one line its starter gave it. WaitsOn is "lead" or "person"
	// when it waits on one of them. WorkersDone, on a session's row, counts
	// its finished workers not listed (workers.go).
	// AgentType, Model and Depth, on a worker's row, are its agent type,
	// the model it was asked for and how deep it was started (1: by the
	// session). WorkflowsNotShown, on a session's row, says it ran workflow
	// agents, which observing does not read yet.
	AgentType         string `json:"agentType,omitempty"`
	Model             string `json:"model,omitempty"`
	Depth             int    `json:"depth,omitempty"`
	WorkflowsNotShown bool   `json:"workflowsNotShown,omitempty"`
	// Turns are the session's latest turns (TurnsShown of TurnsTotal), its
	// frames; Turn, on a worker's row, is the turn of its starter's that
	// started it.
	Turns         []ObservedTurn `json:"turns,omitempty"`
	TurnsTotal    int            `json:"turnsTotal,omitempty"`
	Turn          int            `json:"turn,omitempty"`
	Parent        string         `json:"parent,omitempty"`
	ParentClaimed bool           `json:"parentClaimed,omitempty"`
	Description   string         `json:"description,omitempty"`
	WaitsOn       string         `json:"waitsOn,omitempty"`
	WorkersDone   int            `json:"workersDone,omitempty"`
	// Account says which Claude Code sign-in the session runs under, as a
	// stable opaque label (accountOf): sessions that share it share one
	// usage window. It names no path, email or account.
	Account string `json:"account,omitempty"`
	// Weighted is what it has used in input-equivalent tokens, each model's
	// counts weighted as the farm weighs them (work/tokens), so every
	// reader shows one number.
	Weighted float64 `json:"weighted"`
	// OnFarm is true when the session is a member C4 runs (registered with
	// c4-agent, as c4 farm agents add registers one).
	OnFarm bool `json:"onFarm"`
}

// ObservedUse is what a session has used over its whole transcript.
type ObservedUse struct {
	Models []ModelUsage `json:"models"`
}

// Wrote is the tokens its models wrote, summed over its models.
func (u *ObservedUse) Wrote() int64 {
	if u == nil {
		return 0
	}
	var n int64
	for _, m := range u.Models {
		n += m.OutputTokens
	}
	return n
}

// Read is the tokens its models read, cache reads and writes included:
// nearly all of what a long session is billed for.
func (u *ObservedUse) Read() int64 {
	if u == nil {
		return 0
	}
	var n int64
	for _, m := range u.Models {
		n += m.InputTokens + m.CacheReadTokens + m.CacheWriteTokens
	}
	return n
}

// ObservedHalt is a usage window a session hit that has not reset.
type ObservedHalt struct {
	Kind   string     `json:"kind"`
	Text   string     `json:"text"`
	Resets *time.Time `json:"resets,omitempty"`
}

// The words, so a reader can match on them.
const (
	WordNeedsYou = "needs you"
	WordAtLimit  = "at its limit"
	WordWorking  = "working"
	WordFailed   = "failed"
	WordIdle     = "idle"
	WordDone     = "done"
	WordStopped  = "stopped"
)

var wordOrder = []string{WordNeedsYou, WordAtLimit, WordWorking, WordFailed, WordIdle, WordDone, WordStopped}

// Observe lists the sessions in configDir as they are at now: the running
// ones and any that failed, or every one with all. What needs its person
// comes first.
func (h *Harness) Observe(ctx context.Context, configDir string, all bool, now time.Time) (out []Observed, err error) {
	as, err := h.Claude.Active(ctx, configDir, true)
	if errors.Is(err, exec.ErrNotFound) {
		return nil, errors.New("Claude Code isn't installed here, or isn't on the PATH (C4_AGENT_CLAUDE names it)")
	}
	if err != nil {
		return nil, err
	}
	var keep []Active
	for _, a := range as {
		if all || running(a) || a.State == "failed" {
			keep = append(keep, a)
		}
	}
	defer func() {
		if all {
			return
		}
		// a failure is news for a day; an older one is at rest
		out = slices.DeleteFunc(out, func(o Observed) bool {
			return o.Word == WordFailed && !o.Running && (o.LastActive == nil || now.Sub(*o.LastActive) > FailedNewsFor)
		})
	}()
	out = make([]Observed, len(keep))
	workers := make([][]Observed, len(keep))
	scans := make([]sessionScan, len(keep))
	var wg sync.WaitGroup
	work := make(chan int)
	for w := 0; w < min(4, len(keep)); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range work {
				out[i] = observe(configDir, keep[i], now)
				if tr := out[i].Transcript; tr != "" {
					if info, err := os.Stat(tr); err == nil {
						scans[i] = scanSessionCached(tr, keep[i].Started(), info)
						workers[i] = workersOf(&out[i], scans[i], now, all)
					}
				}
			}
		}()
	}
	for i := range keep {
		work <- i
	}
	close(work)
	wg.Wait()
	for i := range out {
		if m, err := h.Store.BySession(out[i].Session); err == nil && m != nil {
			out[i].OnFarm = true
		}
	}
	idx := make([]int, len(out))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool {
		i, j := idx[a], idx[b]
		ri, rj := wordRank(out[i].Word), wordRank(out[j].Word)
		if ri != rj {
			return ri < rj
		}
		return out[i].Name < out[j].Name
	})
	bySession := map[string]sessionScan{}
	for i := range out {
		bySession[out[i].Session] = scans[i]
	}
	claimParents(out, bySession, now)
	// each session's workers right after it, then the sessions it started
	rows := make([]Observed, 0, len(out))
	placed := map[int]bool{}
	var place func(i int)
	place = func(i int) {
		if placed[i] {
			return
		}
		placed[i] = true
		rows = append(append(rows, out[i]), workers[i]...)
		for _, j := range idx {
			if out[j].Parent == out[i].Session {
				place(j)
			}
		}
	}
	for _, i := range idx {
		if out[i].Parent == "" {
			place(i)
		}
	}
	for _, i := range idx {
		place(i) // a claimed parent that is not listed
	}
	acct := accountOf(configDir)
	for i := range rows {
		rows[i].Account = acct
	}
	return rows, nil
}

func wordRank(w string) int {
	for i, x := range wordOrder {
		if x == w {
			return i
		}
	}
	return len(wordOrder)
}

// running reports whether the CLI lists a session as running: only a
// running one has a status (idle or busy).
func running(a Active) bool { return a.Status == "busy" || a.Status == "idle" }

func observe(configDir string, a Active, now time.Time) Observed {
	o := Observed{Session: a.SessionID, Name: a.Name, Kind: a.Kind, Cwd: a.Cwd, Running: running(a)}
	if t := a.Started(); !t.IsZero() {
		o.StartedAt = &t
	}
	tr, _ := FindTranscript(configDir, a.Cwd, a.SessionID)
	var sc sessionScan
	if tr != "" {
		o.Transcript = tr
		if info, err := os.Stat(tr); err == nil {
			t := info.ModTime()
			o.LastActive = &t
			o.TurnAge = ageWords(now.Sub(t))
			sc = scanSessionCached(tr, a.Started(), info)
		}
	}
	if o.Running {
		o.BackgroundOpen = len(sc.background)
	}
	o.Turns, o.TurnsTotal = sc.turns, sc.turnsTotal
	if len(sc.usage) > 0 {
		o.Usage = &ObservedUse{Models: sc.usage}
		for _, m := range sc.usage {
			o.Weighted += tokens.Weighted(m.Model, tokens.Counts{Input: m.InputTokens, Output: m.OutputTokens, CacheRead: m.CacheReadTokens, CacheWrite: m.CacheWriteTokens})
		}
	}
	switch {
	case a.Status == "busy":
		o.Word = WordWorking
	case a.State == "failed":
		o.Word, o.NeedsYou = WordFailed, "it stopped on an error"
		switch {
		case sc.lastError != "":
			o.NeedsYou += ": " + sc.lastError
		case sc.lastText != "":
			o.NeedsYou += ": " + sc.lastText
		}
	case o.Running && len(sc.pendingTools) > 0:
		// a tool call with no result at the transcript's end: a
		// permission prompt waiting on its person
		o.Word, o.NeedsYou = WordNeedsYou, "it is waiting for your permission to run "+strings.Join(sc.pendingTools, ", ")
	case o.Running && asks(sc.lastText):
		o.Word, o.NeedsYou = WordNeedsYou, "it asks: "+sc.lastText
	case a.Kind == "background" && a.State == "done":
		o.Word = WordDone
	case !o.Running:
		o.Word = WordStopped
	default:
		// idle, a blocked background session that asked nothing included:
		// the CLI reads a finished task as blocked too
		o.Word = WordIdle
	}
	if l := sc.limit; l != nil && !sc.successAfterLimit && limitHolds(*l, now) {
		o.Limit = &ObservedHalt{Kind: l.Kind, Text: l.Text}
		if !l.Resets.IsZero() {
			r := l.Resets
			o.Limit.Resets = &r
		}
		if o.Word != WordNeedsYou && o.Word != WordWorking {
			o.Word = WordAtLimit
		}
		if l.Kind == "spend" && o.NeedsYou == "" {
			o.NeedsYou = "its spend limit, which only you can raise: " + l.Text
		}
	}
	turnStates(o.Turns, o.Word == WordWorking, len(sc.pendingTools) > 0 && o.Running)
	return o
}

// turnStates sets each turn's state: every turn before the latest is done;
// the latest is running while its session works, waiting when it ended on
// a call with no result, else done.
func turnStates(turns []ObservedTurn, working, waiting bool) {
	for i := range turns {
		turns[i].State = "done"
	}
	if n := len(turns); n > 0 {
		switch {
		case working:
			turns[n-1].State = "running"
		case waiting:
			turns[n-1].State = "waiting"
		}
	}
}

// asks reports whether a model's last words end on a question.
func asks(text string) bool {
	return strings.HasSuffix(strings.TrimSpace(text), "?")
}

// ageWords says a duration as a person would: "just now", "5 minutes",
// "3 hours", "2 days".
func ageWords(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return plural(int(d/time.Minute), "minute")
	case d < 48*time.Hour:
		return plural(int(d/time.Hour), "hour")
	}
	return plural(int(d/(24*time.Hour)), "day")
}

func plural(n int, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	return fmt.Sprintf("%d %ss", n, unit)
}

// limitHolds reports whether a usage limit hit has not reset at now: one
// with a reset time until then, one without for the probe interval.
func limitHolds(l UsageLimit, now time.Time) bool {
	if !l.Resets.IsZero() {
		return now.Before(l.Resets)
	}
	return now.Sub(l.At) < defaultLimitProbe
}

// sessionScan is what one pass over a transcript finds.
type sessionScan struct {
	usage             []ModelUsage
	background        []Background
	limit             *UsageLimit
	successAfterLimit bool
	lastText          string          // the last line of the model's last words
	lastError         string          // the first line of an API error since its last words
	pendingTools      []string        // tools called at the end with no result yet
	results           map[string]bool // tool calls that have their result
	bgLaunches        []bgLaunch      // claude --bg commands it ran
	firstAt           time.Time       // its first record's time
	turns             []ObservedTurn  // its turns, oldest first
	toolTurn          map[string]int  // the turn each tool call was made in
	turnsTotal        int             // how many turns it had in all
	teamName          string          // a teammate's team, from its records
	agentName         string          // a teammate's name, from its records
}

var scanCache = struct {
	sync.Mutex
	m     map[string]*scanner
	swept time.Time
}{m: map[string]*scanner{}}

// scanSessionCached scans a transcript as it grows: a transcript is only
// ever appended to, so each call reads the lines written since the last
// one, never the whole file again. One that shrank, or a session whose
// process started anew, is read from its start.
func scanSessionCached(path string, started time.Time, info os.FileInfo) sessionScan {
	now := time.Now()
	scanCache.Lock()
	if now.Sub(scanCache.swept) > scanSweepEvery {
		// a transcript no one asked about for a day is forgotten
		for k, c := range scanCache.m {
			if now.Sub(c.used) > ScanForgetAfter {
				delete(scanCache.m, k)
			}
		}
		scanCache.swept = now
	}
	sc := scanCache.m[path]
	if sc == nil || !sc.started.Equal(started) || info.Size() < sc.offset || (sc.info != nil && !os.SameFile(sc.info, info)) {
		// new, or a session started anew, or a file shorter or replaced
		sc = newScanner(path, started)
		scanCache.m[path] = sc
	}
	sc.used = now
	scanCache.Unlock()
	sc.mu.Lock()
	defer sc.mu.Unlock()
	if info.Size() != sc.size || !info.ModTime().Equal(sc.mod) {
		sc.readMore()
		sc.size, sc.mod, sc.info = info.Size(), info.ModTime(), info
	}
	return sc.result()
}

// ScanForgetAfter is how long a transcript's scan is kept unasked for;
// scanSweepEvery how often old ones are looked for.
const (
	ScanForgetAfter = 24 * time.Hour
	scanSweepEvery  = time.Hour
)

// scanSession reads a transcript once for everything observing shows.
func scanSession(path string, started time.Time) sessionScan {
	sc := newScanner(path, started)
	sc.readMore()
	return sc.result()
}

// scanner reads a transcript a line at a time, from where it last stopped.
type scanner struct {
	mu      sync.Mutex
	path    string
	started time.Time
	offset  int64 // past the last complete line read
	size    int64
	mod     time.Time
	s       sessionScan
	bg      *bgTracker
	seen    map[string]bool
	by      map[string]*ModelUsage
	order   []string
	pending map[string]string // tool use id -> tool name
	info    os.FileInfo       // the file last read, to know it again
	used    time.Time         // when it was last asked for
	// launchIdx finds a claude --bg launch by its call; shared is the
	// results map last handed out, copied again only once it grows.
	launchIdx map[string]int
	shared    map[string]bool
	toolTurn  map[string]int // the turn each tool call was made in
}

func newScanner(path string, started time.Time) *scanner {
	return &scanner{path: path, started: started, bg: newBgTracker(path, started), seen: map[string]bool{}, by: map[string]*ModelUsage{}}
}

// readMore reads the complete lines written since the last read; a line
// still being written is left for the next.
func (sc *scanner) readMore() {
	f, err := os.Open(sc.path)
	if err != nil {
		return
	}
	defer f.Close()
	if _, err := f.Seek(sc.offset, io.SeekStart); err != nil {
		return
	}
	br := bufio.NewReaderSize(f, 1<<20)
	for {
		line, err := br.ReadBytes('\n')
		if err != nil {
			return // EOF: a partial line waits
		}
		sc.offset += int64(len(line))
		sc.line(line[:len(line)-1])
	}
}

// line reads one record.
func (sc *scanner) line(line []byte) {
	s := &sc.s
	var rec struct {
		Type              string          `json:"type"`
		Timestamp         time.Time       `json:"timestamp"`
		IsAPIErrorMessage bool            `json:"isApiErrorMessage"`
		Error             string          `json:"error"`
		ToolUseResult     json.RawMessage `json:"toolUseResult"`
		Message           json.RawMessage `json:"message"`
		TeamName          string          `json:"teamName"`
		AgentName         string          `json:"agentName"`
	}
	if json.Unmarshal(line, &rec) != nil {
		return
	}
	if s.firstAt.IsZero() && !rec.Timestamp.IsZero() {
		s.firstAt = rec.Timestamp
	}
	if s.teamName == "" && rec.TeamName != "" {
		s.teamName = rec.TeamName
	}
	if s.agentName == "" && rec.AgentName != "" {
		s.agentName = rec.AgentName
	}
	if n := len(s.turns); n > 0 && !rec.Timestamp.IsZero() && rec.Timestamp.After(s.turns[n-1].End) {
		s.turns[n-1].End = rec.Timestamp
	}
	sc.bg.feedRecord(line, rec.Type, rec.Timestamp, rec.Message, rec.ToolUseResult)
	var msg struct {
		ID      string          `json:"id"`
		Model   string          `json:"model"`
		Content json.RawMessage `json:"content"`
		Usage   *struct {
			Input      int64 `json:"input_tokens"`
			Output     int64 `json:"output_tokens"`
			CacheRead  int64 `json:"cache_read_input_tokens"`
			CacheWrite int64 `json:"cache_creation_input_tokens"`
		} `json:"usage"`
	}
	_ = json.Unmarshal(rec.Message, &msg)
	var parts []struct {
		Type      string `json:"type"`
		Text      string `json:"text"`
		ID        string `json:"id"`
		Name      string `json:"name"`
		ToolUseID string `json:"tool_use_id"`
	}
	_ = json.Unmarshal(msg.Content, &parts)
	switch rec.Type {
	case "assistant":
		if rec.IsAPIErrorMessage {
			if rec.Error != "rate_limit" {
				for _, p := range parts {
					if t := strings.TrimSpace(p.Text); p.Type == "text" && t != "" {
						s.lastError = firstLine(t)
					}
				}
			}
			if rec.Error == "rate_limit" {
				var text []string
				for _, p := range parts {
					if p.Type == "text" {
						text = append(text, p.Text)
					}
				}
				l := &UsageLimit{Text: strings.TrimSpace(strings.Join(text, " ")), At: rec.Timestamp}
				l.Kind = limitKind(l.Text)
				if t, ok := resetTime(l.Text, rec.Timestamp); ok {
					l.Resets = t
				}
				s.limit, s.successAfterLimit = l, false
			}
			return
		}
		if msg.Model == syntheticModel {
			return // the CLI's own filler ("No response requested."), not the model's
		}
		if s.limit != nil {
			s.successAfterLimit = true
		}
		s.lastError = ""
		if msg.Usage != nil && msg.Model != "" && msg.ID != "" && !sc.seen[msg.ID] {
			sc.seen[msg.ID] = true
			u := sc.by[msg.Model]
			if u == nil {
				u = &ModelUsage{Model: msg.Model}
				sc.by[msg.Model], sc.order = u, append(sc.order, msg.Model)
			}
			u.InputTokens += msg.Usage.Input
			u.OutputTokens += msg.Usage.Output
			u.CacheReadTokens += msg.Usage.CacheRead
			u.CacheWriteTokens += msg.Usage.CacheWrite
			if n := len(s.turns); n > 0 {
				t := &s.turns[n-1]
				t.Tokens += msg.Usage.Input + msg.Usage.Output + msg.Usage.CacheRead + msg.Usage.CacheWrite
				t.Weighted += tokens.Weighted(msg.Model, tokens.Counts{Input: msg.Usage.Input, Output: msg.Usage.Output, CacheRead: msg.Usage.CacheRead, CacheWrite: msg.Usage.CacheWrite})
			}
		}
		for _, p := range parts {
			switch p.Type {
			case "text":
				if t := strings.TrimSpace(p.Text); t != "" {
					s.lastText = lastLine(t)
					sc.pending = nil
				}
			case "tool_use":
				if sc.pending == nil {
					sc.pending = map[string]string{}
				}
				sc.pending[p.ID] = p.Name
				if sc.toolTurn == nil {
					sc.toolTurn = map[string]int{}
				}
				sc.toolTurn[p.ID] = len(s.turns)
			}
		}
		if bytes.Contains(msg.Content, []byte("--bg")) {
			for _, l := range claudeLaunches(msg.Content, rec.Timestamp) {
				if sc.launchIdx == nil {
					sc.launchIdx = map[string]int{}
				}
				sc.launchIdx[l.ID] = len(s.bgLaunches)
				s.bgLaunches = append(s.bgLaunches, l)
			}
		}
	case "user":
		if startsTurn(msg.Content, parts) {
			s.turns = append(s.turns, ObservedTurn{Index: len(s.turns) + 1, Start: rec.Timestamp, End: rec.Timestamp})
		}
		for _, p := range parts {
			if p.Type == "tool_result" {
				delete(sc.pending, p.ToolUseID)
				if s.results == nil {
					s.results = map[string]bool{}
				}
				s.results[p.ToolUseID] = true
				if i, ok := sc.launchIdx[p.ToolUseID]; ok {
					s.bgLaunches[i].ResultAt = rec.Timestamp
				}
			}
		}
		if personSpoke(msg.Content, parts) {
			// what the model asked before is answered
			s.lastText = ""
		}
	}
}

// result is what the lines read so far say.
func (sc *scanner) result() sessionScan {
	s := sc.s
	s.usage = nil
	for _, m := range sc.order {
		s.usage = append(s.usage, *sc.by[m])
	}
	s.background = sc.bg.open()
	s.pendingTools = nil
	for _, name := range sc.pending {
		if !strings.Contains(strings.Join(s.pendingTools, ","), name) {
			s.pendingTools = append(s.pendingTools, name)
		}
	}
	sort.Strings(s.pendingTools)
	if len(sc.s.results) != len(sc.shared) {
		sc.shared = make(map[string]bool, len(sc.s.results))
		for k, v := range sc.s.results {
			sc.shared[k] = v
		}
	}
	s.results = sc.shared
	s.bgLaunches = append([]bgLaunch(nil), sc.s.bgLaunches...)
	s.turns = append([]ObservedTurn(nil), sc.s.turns[max(0, len(sc.s.turns)-TurnsShown):]...)
	s.turnsTotal = len(sc.s.turns)
	s.toolTurn = sc.toolTurn
	return s
}

// accountOf is the opaque label of a config folder's sign-in: the first
// 12 hex digits of a SHA-256 of its absolute path ("" for the CLI's
// default, which is CLAUDE_CONFIG_DIR or ~/.claude).
func accountOf(configDir string) string {
	if configDir == "" {
		configDir = os.Getenv("CLAUDE_CONFIG_DIR")
	}
	if configDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		configDir = filepath.Join(home, ".claude")
	}
	abs, err := filepath.Abs(configDir)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256([]byte(filepath.Clean(abs)))
	return hex.EncodeToString(sum[:])[:12]
}

// TurnsShown is how many of a session's latest turns a row lists.
const TurnsShown = 200

// ObservedTurn is one turn of a session, a frame of its work: from the
// record that started it (a prompt, a notification, a steer) to its last,
// with what it used. State is the latest turn's: running while the
// session works, waiting when it ended on a call with no result, done.
type ObservedTurn struct {
	Index    int       `json:"index"`
	Start    time.Time `json:"start"`
	End      time.Time `json:"end"`
	Tokens   int64     `json:"tokens"`
	Weighted float64   `json:"weighted"`
	State    string    `json:"state"`
}

// startsTurn reports whether a user record starts a turn: anything but a
// record that only carries tool results.
func startsTurn(content json.RawMessage, parts []struct {
	Type      string `json:"type"`
	Text      string `json:"text"`
	ID        string `json:"id"`
	Name      string `json:"name"`
	ToolUseID string `json:"tool_use_id"`
}) bool {
	if len(parts) == 0 {
		return len(bytes.TrimSpace(content)) > 0
	}
	for _, p := range parts {
		if p.Type != "tool_result" {
			return true
		}
	}
	return false
}

// syntheticModel marks a message the CLI wrote itself, not a model.
const syntheticModel = "<synthetic>"

// FailedNewsFor is how long a failed session that is no longer running is
// listed without --all: after that, it is at rest.
const FailedNewsFor = 24 * time.Hour

// firstLine is a text's first line, up to 120 characters.
func firstLine(text string) string {
	l, _, _ := strings.Cut(strings.TrimSpace(text), "\n")
	if r := []rune(strings.TrimSpace(l)); len(r) > 120 {
		return string(r[:119]) + "…"
	}
	return strings.TrimSpace(l)
}

// lastLine is a text's last line, up to 120 characters: where a question
// usually is.
func lastLine(text string) string {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	l := strings.TrimSpace(lines[len(lines)-1])
	if r := []rune(l); len(r) > 120 {
		l = "…" + string(r[len(r)-119:])
	}
	return l
}

// personSpoke reports whether a user message is the person writing, not a
// tool result, a task notification or a reminder the CLI injected.
func personSpoke(content json.RawMessage, parts []struct {
	Type      string `json:"type"`
	Text      string `json:"text"`
	ID        string `json:"id"`
	Name      string `json:"name"`
	ToolUseID string `json:"tool_use_id"`
}) bool {
	injected := func(t string) bool {
		t = strings.TrimSpace(t)
		return strings.HasPrefix(t, "<task-notification>") || strings.HasPrefix(t, "<system-reminder>") || t == ""
	}
	var text string
	if json.Unmarshal(content, &text) == nil {
		return !injected(text)
	}
	for _, p := range parts {
		if p.Type == "text" && !injected(p.Text) {
			return true
		}
	}
	return false
}
