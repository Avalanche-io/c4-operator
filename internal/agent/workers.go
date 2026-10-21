package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/Avalanche-io/c4-operator/work/tokens"
)

// A session's workers, read from disk as the CLI leaves them: each
// subagent has its own transcript beside the session's, in
// <session>/subagents/agent-<id>.jsonl, and a .meta.json naming its type,
// its description, the tool call that started it (toolUseId), whether it
// ran in the background, whether it finished, and for a nested one the
// agent that started it (parentAgentId). Teammates have a name and no tool
// call. Nothing is asked of the CLI; observing still only reads.

// Kinds of a worker row (Observed.Kind), beside a session's own.
const (
	KindSubagent = "subagent"
	KindTeammate = "teammate"
)

// FinishedWorkersShownFor is how long a finished worker is listed without
// --all; older ones are counted on their session's row (WorkersDone).
const FinishedWorkersShownFor = time.Hour

type workerMeta struct {
	AgentType     string `json:"agentType"`
	Description   string `json:"description"`
	Name          string `json:"name"`
	ToolUseID     string `json:"toolUseId"`
	SpawnDepth    int    `json:"spawnDepth"`
	RequestShape  string `json:"requestShape"`
	Finished      *bool  `json:"finished"`
	ParentAgentID string `json:"parentAgentId"`
}

// workersOf lists the workers of the session row o (its transcript
// scanned as sc), each as a row whose Parent is the session or the worker
// that started it. Without all, a finished worker is listed only for
// FinishedWorkersShownFor; the rest are counted in o.WorkersDone.
func workersOf(o *Observed, sc sessionScan, now time.Time, all bool) []Observed {
	if o.Transcript == "" {
		return nil
	}
	dir := filepath.Join(strings.TrimSuffix(o.Transcript, ".jsonl"), "subagents")
	metas, _ := filepath.Glob(filepath.Join(dir, "agent-*.meta.json"))
	if len(metas) == 0 {
		return nil
	}
	type worker struct {
		id   string
		meta workerMeta
		tr   string
		info os.FileInfo
		scan sessionScan
	}
	ws := make([]*worker, 0, len(metas))
	byID := map[string]*worker{}
	for _, m := range metas {
		b, err := os.ReadFile(m)
		if err != nil {
			continue
		}
		w := &worker{id: strings.TrimSuffix(strings.TrimPrefix(filepath.Base(m), "agent-"), ".meta.json")}
		if json.Unmarshal(b, &w.meta) != nil {
			continue
		}
		w.tr = filepath.Join(dir, "agent-"+w.id+".jsonl")
		info, err := os.Stat(w.tr)
		if err == nil {
			w.info = info
		}
		finished := w.meta.Finished != nil && *w.meta.Finished
		if !all && (finished || !o.Running) && w.meta.ParentAgentID == "" && (w.info == nil || now.Sub(w.info.ModTime()) > FinishedWorkersShownFor) {
			// done and old: counted, not read
			o.WorkersDone++
			continue
		}
		if w.info != nil {
			w.scan = scanSessionCached(w.tr, time.Time{}, w.info)
		}
		ws, byID[w.id] = append(ws, w), w
	}
	var out []Observed
	for _, w := range ws {
		parentScan, parentRunning, parent := sc, o.Running, o.Session
		if p := byID[w.meta.ParentAgentID]; p != nil {
			parentScan, parent = p.scan, p.id
		}
		row := Observed{Session: w.id, Name: w.meta.Description, Cwd: o.Cwd, Parent: parent, Description: w.meta.Description, Kind: KindSubagent}
		if w.info != nil {
			row.Transcript = w.tr
		}
		if w.meta.Name != "" && w.meta.ToolUseID == "" {
			row.Kind, row.Name = KindTeammate, w.meta.Name
		}
		if w.info != nil {
			t := w.info.ModTime()
			row.LastActive, row.TurnAge = &t, ageWords(now.Sub(t))
		}
		if len(w.scan.usage) > 0 {
			row.Usage = &ObservedUse{Models: w.scan.usage}
			for _, m := range w.scan.usage {
				row.Weighted += tokens.Weighted(m.Model, tokens.Counts{Input: m.InputTokens, Output: m.OutputTokens, CacheRead: m.CacheReadTokens, CacheWrite: m.CacheWriteTokens})
			}
		}
		ended := w.meta.Finished != nil && *w.meta.Finished
		switch {
		case ended:
		case w.meta.ToolUseID == "":
			// a teammate: its own transcript is all there is
			ended = !parentRunning
		case w.meta.RequestShape == "background":
			ended = !parentScan.openBackground(w.meta.ToolUseID)
		default:
			ended = parentScan.results[w.meta.ToolUseID]
		}
		switch {
		case ended && w.scan.lastError != "":
			row.Word = WordFailed
		case ended:
			row.Word = WordDone
		case parentRunning:
			row.Word, row.Running = WordWorking, true
			row.BackgroundOpen = len(w.scan.background)
		default:
			// its session stopped with it still under way
			row.Word = WordStopped
		}
		if !all && !row.Running && (row.LastActive == nil || now.Sub(*row.LastActive) > FinishedWorkersShownFor) {
			o.WorkersDone++
			continue
		}
		out = append(out, row)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Running != out[j].Running {
			return out[i].Running
		}
		return out[i].Description < out[j].Description
	})
	return out
}

// openBackground reports whether the background work a tool call started
// is still under way in this transcript.
func (s sessionScan) openBackground(toolUseID string) bool {
	for _, b := range s.background {
		if b.ID == toolUseID {
			return true
		}
	}
	return false
}

// A background session a lead starts runs `claude --bg ...` through its
// Bash tool. Neither the transcript nor the CLI records which session that
// call started, so the link is a match: a launch within launchMatch before
// the session started, the one naming it (-n or --name) when several do.
// Its row says so (ParentClaimed).

// launchMatch is how long before a background session started a lead's
// launch may be, to be taken for its start.
const launchMatch = 30 * time.Second

type bgLaunch struct {
	At   time.Time
	Name string // its -n or --name, when it gives one
}

var claudeBg = regexp.MustCompile(`(?:^|[\s;&|(])claude\s[^;&|]*--bg\b`)
var launchName = regexp.MustCompile(`(?:-n|--name)(?:=|\s+)(?:"([^"]+)"|'([^']+)'|(\S+))`)

// claudeLaunches are the claude --bg commands in an assistant message's
// Bash calls.
func claudeLaunches(content json.RawMessage, at time.Time) []bgLaunch {
	var parts []struct {
		Type  string `json:"type"`
		Name  string `json:"name"`
		Input struct {
			Command string `json:"command"`
		} `json:"input"`
	}
	if json.Unmarshal(content, &parts) != nil {
		return nil
	}
	var out []bgLaunch
	for _, p := range parts {
		if p.Type != "tool_use" || p.Name != "Bash" {
			continue
		}
		for _, loc := range claudeBg.FindAllStringIndex(p.Input.Command, -1) {
			l := bgLaunch{At: at}
			if m := launchName.FindStringSubmatch(p.Input.Command[loc[0]:]); m != nil {
				l.Name = m[1] + m[2] + m[3]
			}
			out = append(out, l)
		}
	}
	return out
}

// claimParents links each background session the CLI lists with no parent
// to the session whose launch most likely started it.
func claimParents(rows []Observed, scans map[string]sessionScan) {
	for i := range rows {
		b := &rows[i]
		if b.Parent != "" || b.Kind != "background" || b.StartedAt == nil {
			continue
		}
		var named, near []string
		for session, sc := range scans {
			if session == b.Session {
				continue
			}
			for _, l := range sc.bgLaunches {
				d := b.StartedAt.Sub(l.At)
				if d < -5*time.Second || d > launchMatch {
					continue
				}
				near = append(near, session)
				if l.Name != "" && l.Name == b.Name {
					named = append(named, session)
				}
			}
		}
		switch {
		case len(named) == 1:
			b.Parent, b.ParentClaimed = named[0], true
		case len(named) == 0 && len(near) == 1:
			b.Parent, b.ParentClaimed = near[0], true
		}
	}
}
