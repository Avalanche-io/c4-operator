package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
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
	// OnFarm is true when the session is a member C4 runs (registered with
	// c4-agent, as c4 farm agents add registers one).
	OnFarm bool `json:"onFarm"`
}

// ObservedUse is what a session has used over its whole transcript.
type ObservedUse struct {
	Models []ModelUsage `json:"models"`
}

// Tokens is its input and output, summed over its models.
func (u *ObservedUse) Tokens() int64 {
	if u == nil {
		return 0
	}
	var n int64
	for _, m := range u.Models {
		n += m.InputTokens + m.OutputTokens
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
func (h *Harness) Observe(ctx context.Context, configDir string, all bool, now time.Time) ([]Observed, error) {
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
	out := make([]Observed, len(keep))
	var wg sync.WaitGroup
	work := make(chan int)
	for w := 0; w < min(4, len(keep)); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range work {
				out[i] = observe(configDir, keep[i], now)
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
	sort.SliceStable(out, func(i, j int) bool {
		ri, rj := wordRank(out[i].Word), wordRank(out[j].Word)
		if ri != rj {
			return ri < rj
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
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
	if len(sc.usage) > 0 {
		o.Usage = &ObservedUse{Models: sc.usage}
	}
	switch {
	case a.Status == "busy":
		o.Word = WordWorking
	case a.State == "failed":
		o.Word, o.NeedsYou = WordFailed, "it stopped on an error"
		if sc.lastText != "" {
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
	return o
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
	lastText          string   // the last line of the model's last words
	pendingTools      []string // tools called at the end with no result yet
}

var scanCache = struct {
	sync.Mutex
	m map[string]cachedScan
}{m: map[string]cachedScan{}}

type cachedScan struct {
	size    int64
	mod     time.Time
	started time.Time
	scan    sessionScan
}

// scanSessionCached scans a transcript once per change to it.
func scanSessionCached(path string, started time.Time, info os.FileInfo) sessionScan {
	scanCache.Lock()
	c, ok := scanCache.m[path]
	scanCache.Unlock()
	if ok && c.size == info.Size() && c.mod.Equal(info.ModTime()) && c.started.Equal(started) {
		return c.scan
	}
	sc := scanSession(path, started)
	scanCache.Lock()
	scanCache.m[path] = cachedScan{size: info.Size(), mod: info.ModTime(), started: started, scan: sc}
	scanCache.Unlock()
	return sc
}

// scanSession reads a transcript once for everything observing shows.
func scanSession(path string, started time.Time) sessionScan {
	var s sessionScan
	f, err := os.Open(path)
	if err != nil {
		return s
	}
	defer f.Close()
	bg := newBgTracker(path, started)
	seen := map[string]bool{}
	by := map[string]*ModelUsage{}
	var order []string
	var pending map[string]string // tool use id -> tool name
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	for sc.Scan() {
		line := sc.Bytes()
		var rec struct {
			Type              string          `json:"type"`
			Timestamp         time.Time       `json:"timestamp"`
			IsAPIErrorMessage bool            `json:"isApiErrorMessage"`
			Error             string          `json:"error"`
			ToolUseResult     json.RawMessage `json:"toolUseResult"`
			Message           json.RawMessage `json:"message"`
		}
		if json.Unmarshal(line, &rec) != nil {
			continue
		}
		bg.feedRecord(line, rec.Type, rec.Timestamp, rec.Message, rec.ToolUseResult)
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
				continue
			}
			if s.limit != nil {
				s.successAfterLimit = true
			}
			if msg.Usage != nil && msg.Model != "" && msg.ID != "" && !seen[msg.ID] {
				seen[msg.ID] = true
				u := by[msg.Model]
				if u == nil {
					u = &ModelUsage{Model: msg.Model}
					by[msg.Model], order = u, append(order, msg.Model)
				}
				u.InputTokens += msg.Usage.Input
				u.OutputTokens += msg.Usage.Output
				u.CacheReadTokens += msg.Usage.CacheRead
				u.CacheWriteTokens += msg.Usage.CacheWrite
			}
			for _, p := range parts {
				switch p.Type {
				case "text":
					if t := strings.TrimSpace(p.Text); t != "" {
						s.lastText = lastLine(t)
						pending = nil
					}
				case "tool_use":
					if pending == nil {
						pending = map[string]string{}
					}
					pending[p.ID] = p.Name
				}
			}
		case "user":
			for _, p := range parts {
				if p.Type == "tool_result" {
					delete(pending, p.ToolUseID)
				}
			}
			if personSpoke(msg.Content, parts) {
				// what the model asked before is answered
				s.lastText = ""
			}
		}
	}
	for _, m := range order {
		s.usage = append(s.usage, *by[m])
	}
	s.background = bg.open()
	for _, name := range pending {
		if !strings.Contains(strings.Join(s.pendingTools, ","), name) {
			s.pendingTools = append(s.pendingTools, name)
		}
	}
	sort.Strings(s.pendingTools)
	return s
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
