package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Avalanche-io/c4-operator/work/tokens"
)

// A session's workers, read from disk as the CLI leaves them: each
// subagent has its own transcript beside the session's, in
// <session>/subagents/agent-<id>.jsonl, and a .meta.json naming its type,
// its description, the tool call that started it (toolUseId), whether it
// ran in the background, whether it finished, how deep it was started,
// and for a nested one the agent that started it (parentAgentId). A
// teammate's transcript says so in its records (teamName, agentName).
// Nothing is asked of the CLI; observing still only reads.

// Kinds of a worker row (Observed.Kind), beside a session's own.
const (
	KindSubagent = "subagent"
	KindTeammate = "teammate"
)

// WordWaiting is a worker's word while it waits on someone (WaitsOn): its
// person, at a permission prompt, or its lead, after a question.
const WordWaiting = "waiting"

// FinishedWorkersShownFor is how long a finished worker is listed without
// --all; older ones are counted on their session's row (WorkersDone).
const FinishedWorkersShownFor = time.Hour

// WorkerQuietAfter is how long a worker's transcript may stand still before
// a call at its end is taken for a prompt waiting on someone, and a
// teammate with nothing under way for idle.
const WorkerQuietAfter = 30 * time.Second

type workerMeta struct {
	AgentType             string `json:"agentType"`
	Description           string `json:"description"`
	Name                  string `json:"name"`
	Model                 string `json:"model"`
	ToolUseID             string `json:"toolUseId"`
	SpawnDepth            int    `json:"spawnDepth"`
	RequestShape          string `json:"requestShape"`
	RequestNonInteractive *bool  `json:"requestNonInteractive"`
	Finished              *bool  `json:"finished"`
	ParentAgentID         string `json:"parentAgentId"`
}

func (m workerMeta) finished() bool { return m.Finished != nil && *m.Finished }

// interactive reports whether the worker could put a prompt before its
// person: one in the foreground of a session that has one.
func (m workerMeta) interactive() bool {
	return m.RequestShape != "background" && (m.RequestNonInteractive == nil || !*m.RequestNonInteractive)
}

// metaCache keeps each .meta.json as last read, by its time, so a session
// with hundreds of finished workers is not read again each minute.
var metaCache = struct {
	sync.Mutex
	m map[string]cachedMeta
}{m: map[string]cachedMeta{}}

type cachedMeta struct {
	mod  time.Time
	meta workerMeta
	used time.Time
}

func readMeta(path string, now time.Time) (workerMeta, bool) {
	info, err := os.Stat(path)
	if err != nil {
		return workerMeta{}, false
	}
	metaCache.Lock()
	defer metaCache.Unlock()
	if c, ok := metaCache.m[path]; ok && c.mod.Equal(info.ModTime()) {
		c.used = now
		metaCache.m[path] = c
		return c.meta, true
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return workerMeta{}, false
	}
	var m workerMeta
	if json.Unmarshal(b, &m) != nil {
		return workerMeta{}, false
	}
	for k, c := range metaCache.m {
		if now.Sub(c.used) > ScanForgetAfter {
			delete(metaCache.m, k)
		}
	}
	metaCache.m[path] = cachedMeta{mod: info.ModTime(), meta: m, used: now}
	return m, true
}

type worker struct {
	id      string
	meta    workerMeta
	tr      string
	info    os.FileInfo
	scan    sessionScan
	scanned bool
}

func (w *worker) scanOf() sessionScan {
	if !w.scanned && w.info != nil {
		w.scan, w.scanned = scanSessionCached(w.tr, time.Time{}, w.info), true
	}
	return w.scan
}

// workersOf lists the workers of the session row o (its transcript
// scanned as sc), each as a row whose Parent is the session or the worker
// that started it. Without all, a finished worker is listed only for
// FinishedWorkersShownFor; the rest are counted in o.WorkersDone. The
// session's BackgroundOpen then counts only background work that is not a
// worker listed here.
func workersOf(o *Observed, sc sessionScan, now time.Time, all bool) []Observed {
	if o.Transcript == "" {
		return nil
	}
	base := strings.TrimSuffix(o.Transcript, ".jsonl")
	if isDir(filepath.Join(base, "workflows")) {
		o.WorkflowsNotShown = true
	}
	dir := filepath.Join(base, "subagents")
	metas, _ := filepath.Glob(filepath.Join(dir, "agent-*.meta.json"))
	if len(metas) == 0 {
		return nil
	}
	var order []*worker
	byID := map[string]*worker{}
	for _, m := range metas {
		meta, ok := readMeta(m, now)
		if !ok {
			continue
		}
		w := &worker{id: strings.TrimSuffix(strings.TrimPrefix(filepath.Base(m), "agent-"), ".meta.json"), meta: meta}
		w.tr = filepath.Join(dir, "agent-"+w.id+".jsonl")
		if info, err := os.Stat(w.tr); err == nil {
			w.info = info
		}
		order, byID[w.id] = append(order, w), w
	}
	var leadStart time.Time
	if o.StartedAt != nil {
		leadStart = *o.StartedAt
	}
	var out []Observed
	listedBackground := 0
	for _, w := range order {
		old := w.info == nil || now.Sub(w.info.ModTime()) > FinishedWorkersShownFor
		if !all && old && (w.meta.finished() || !o.Running) {
			o.WorkersDone++ // done and old: counted, not read
			continue
		}
		own := w.scanOf()
		// its starter: the session, or the worker that started it, read
		// even when that one is old and not listed itself
		parent, parentScan, parentRunning := o.Session, sc, o.Running
		if id := w.meta.ParentAgentID; id != "" {
			parent, parentScan = id, sessionScan{}
			if p := byID[id]; p != nil {
				parentScan = p.scanOf()
				parentRunning = o.Running && !p.meta.finished()
			}
		}
		row := Observed{Session: w.id, Name: w.meta.Description, Cwd: o.Cwd, Parent: parent, Description: w.meta.Description,
			Kind: KindSubagent, AgentType: w.meta.AgentType, Model: w.meta.Model, Depth: max(w.meta.SpawnDepth, 1)}
		row.Turn = parentScan.toolTurn[w.meta.ToolUseID]
		row.Turns, row.TurnsTotal = own.turns, own.turnsTotal
		teammate := own.teamName != "" || own.agentName != ""
		if teammate {
			row.Kind = KindTeammate
			if own.agentName != "" {
				row.Name = own.agentName
			}
		}
		if w.info != nil {
			t := w.info.ModTime()
			row.Transcript, row.LastActive, row.TurnAge = w.tr, &t, ageWords(now.Sub(t))
		}
		if len(own.usage) > 0 {
			row.Usage = &ObservedUse{Models: own.usage}
			for _, m := range own.usage {
				row.Weighted += tokens.Weighted(m.Model, tokens.Counts{Input: m.InputTokens, Output: m.OutputTokens, CacheRead: m.CacheReadTokens, CacheWrite: m.CacheWriteTokens})
			}
		}
		// one launched before its session's process started was the last
		// run's: what the session does now cannot end it
		beforeResume := parent == o.Session && !leadStart.IsZero() && !own.firstAt.IsZero() && own.firstAt.Before(leadStart)
		var ended bool
		switch {
		case w.meta.finished():
			ended = true
		case teammate:
			ended = !parentRunning
		case w.meta.RequestShape == "background":
			ended = !beforeResume && !parentScan.openBackground(w.meta.ToolUseID)
		default:
			ended = parentScan.results[w.meta.ToolUseID]
		}
		quiet := w.info == nil || now.Sub(w.info.ModTime()) > WorkerQuietAfter
		switch {
		case ended && own.lastError != "":
			row.Word = WordFailed
		case ended:
			row.Word = WordDone
		case !parentRunning || beforeResume:
			// its session, or the run that launched it, ended first
			row.Word = WordStopped
		case len(own.pendingTools) > 0 && quiet && w.meta.interactive():
			row.Word, row.Running, row.WaitsOn = WordWaiting, true, "person"
			row.NeedsYou = "it is waiting for your permission to run " + strings.Join(own.pendingTools, ", ")
		case teammate && quiet && len(own.pendingTools) == 0:
			row.Word, row.Running = WordIdle, true
			if asks(own.lastText) {
				row.Word, row.WaitsOn = WordWaiting, "lead"
			}
		default:
			row.Word, row.Running = WordWorking, true
			row.BackgroundOpen = len(own.background)
		}
		turnStates(row.Turns, row.Word == WordWorking, row.Word == WordWaiting)
		if !all && !row.Running && old {
			o.WorkersDone++
			continue
		}
		if row.Running && parent == o.Session && w.meta.RequestShape == "background" && parentScan.openBackground(w.meta.ToolUseID) {
			listedBackground++
		}
		out = append(out, row)
	}
	// background work the session started, less the workers listed here
	o.BackgroundOpen = max(0, o.BackgroundOpen-listedBackground)
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
