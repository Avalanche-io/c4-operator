package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
)

// Background is work a session started beside its turns, a background agent
// or a command run in the background, that has not reported back. Stopping
// the session kills it, so a yield waits for it (or records it, when it
// cannot wait), never stops over it unknowingly.
type Background struct {
	// ID is the tool use that started it.
	ID string `json:"id"`
	// Kind is agent or command.
	Kind string `json:"kind"`
	// Task is the CLI's own name for it: the agent's or the command's ID.
	Task string `json:"task,omitempty"`
	// Description is what it was doing, as the session described it.
	Description string `json:"description,omitempty"`
	// Transcript is a background agent's own transcript, so its work so far
	// can be read after it is stopped.
	Transcript string `json:"transcript,omitempty"`
	// Started is when it was started.
	Started time.Time `json:"started,omitzero"`
}

var notifyRE = regexp.MustCompile(`(?s)<task-notification>.*?</task-notification>`)
var notifyToolRE = regexp.MustCompile(`<tool-use-id>([^<]+)</tool-use-id>`)
var notifyTaskRE = regexp.MustCompile(`<task-id>([^<]+)</task-id>`)

// OpenBackground lists the background work a session's transcript started
// since its process started (since; work from an earlier process died with
// it) and has not heard back from: each launch (an agent the CLI reports as
// async_launched, or a command it gave a backgroundTaskId) with no
// task-notification for it, and not stopped with TaskStop. A background
// agent's own transcript is in the session's subagents directory.
func OpenBackground(transcript string, since time.Time) ([]Background, error) {
	f, err := os.Open(transcript)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	t := newBgTracker(transcript, since)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	for sc.Scan() {
		t.feed(sc.Bytes())
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", transcript, err)
	}
	return t.open(), nil
}

// bgTracker follows a transcript a line at a time for its background work:
// launched, and not yet reported or stopped.
type bgTracker struct {
	transcript string
	since      time.Time
	launched   map[string]*Background
	byTask     map[string]string
}

func newBgTracker(transcript string, since time.Time) *bgTracker {
	return &bgTracker{transcript: transcript, since: since, launched: map[string]*Background{}, byTask: map[string]string{}}
}

func (t *bgTracker) feed(line []byte) {
	var rec struct {
		Type          string          `json:"type"`
		Timestamp     time.Time       `json:"timestamp"`
		Message       json.RawMessage `json:"message"`
		ToolUseResult json.RawMessage `json:"toolUseResult"`
	}
	if json.Unmarshal(line, &rec) != nil {
		return
	}
	t.feedRecord(line, rec.Type, rec.Timestamp, rec.Message, rec.ToolUseResult)
}

// feedRecord takes a line already decoded this far.
func (t *bgTracker) feedRecord(line []byte, typ string, ts time.Time, msg, toolUseResult json.RawMessage) {
	// a notification that background work ended, wherever it appears
	if bytes.Contains(line, []byte("task-notification")) {
		for _, n := range notifyRE.FindAllString(unescapeJSON(string(line)), -1) {
			if m := notifyToolRE.FindStringSubmatch(n); m != nil {
				delete(t.launched, m[1])
			}
			if m := notifyTaskRE.FindStringSubmatch(n); m != nil {
				delete(t.launched, t.byTask[m[1]])
			}
		}
	}
	for _, id := range stopped(msg) {
		delete(t.launched, t.byTask[id])
	}
	if typ != "user" || len(toolUseResult) == 0 || toolUseResult[0] != '{' {
		return
	}
	// to the second: a fake or a coarse clock writes no fraction
	if !t.since.IsZero() && ts.Before(t.since.Truncate(time.Second)) {
		return
	}
	var res struct {
		IsAsync          bool   `json:"isAsync"`
		Status           string `json:"status"`
		AgentID          string `json:"agentId"`
		Description      string `json:"description"`
		BackgroundTaskID string `json:"backgroundTaskId"`
	}
	if json.Unmarshal(toolUseResult, &res) != nil {
		return
	}
	b := Background{Started: ts, Description: res.Description}
	switch {
	case res.IsAsync && res.Status == "async_launched" && res.AgentID != "":
		b.Kind, b.Task = "agent", res.AgentID
		b.Transcript = filepath.Join(strings.TrimSuffix(t.transcript, ".jsonl"), "subagents", "agent-"+res.AgentID+".jsonl")
	case res.BackgroundTaskID != "":
		b.Kind, b.Task = "command", res.BackgroundTaskID
	default:
		return
	}
	b.ID = toolUseID(msg)
	if b.ID == "" {
		b.ID = b.Task
	}
	t.launched[b.ID], t.byTask[b.Task] = &b, b.ID
}

func (t *bgTracker) open() []Background {
	out := make([]Background, 0, len(t.launched))
	for _, b := range t.launched {
		out = append(out, *b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Started.Before(out[j].Started) })
	return out
}

// stopped is the background work a message stops (TaskStop, KillShell).
func stopped(msg json.RawMessage) []string {
	var m struct {
		Content []struct {
			Type  string `json:"type"`
			Name  string `json:"name"`
			Input struct {
				TaskID  string `json:"task_id"`
				ShellID string `json:"shell_id"`
			} `json:"input"`
		} `json:"content"`
	}
	if json.Unmarshal(msg, &m) != nil {
		return nil
	}
	var out []string
	for _, c := range m.Content {
		if c.Type == "tool_use" && (c.Name == "TaskStop" || c.Name == "KillShell") {
			out = append(out, c.Input.TaskID+c.Input.ShellID)
		}
	}
	return out
}

// toolUseID is the tool use a tool result answers.
func toolUseID(msg json.RawMessage) string {
	var m struct {
		Content []struct {
			Type      string `json:"type"`
			ToolUseID string `json:"tool_use_id"`
		} `json:"content"`
	}
	if json.Unmarshal(msg, &m) != nil {
		return ""
	}
	for _, c := range m.Content {
		if c.Type == "tool_result" && c.ToolUseID != "" {
			return c.ToolUseID
		}
	}
	return ""
}

// unescapeJSON undoes JSON's escaping of a raw line enough to find the tags a
// notification carries inside a string.
func unescapeJSON(s string) string {
	return strings.NewReplacer(`\n`, "\n", `\"`, `"`, `<`, "<", `>`, ">").Replace(s)
}

// lostText tells a member that background work it started was stopped when
// it yielded, and where its work so far is, for the attempt that resumes it.
func lostText(bg []Background) string {
	if len(bg) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("When your session was stopped, background work you had started was still running, and it was stopped with it:\n")
	for _, w := range bg {
		fmt.Fprintf(&b, "- %s %s", w.Kind, orDash(w.Description))
		if w.Kind == "agent" && w.Transcript != "" {
			fmt.Fprintf(&b, " (its transcript so far: %s)", w.Transcript)
		}
		b.WriteString("\n")
	}
	b.WriteString("None of it reported back. Start again what you still need, using what its transcript already holds.")
	return b.String()
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(no description)"
	}
	return "\"" + s + "\""
}

// EnvYieldBackgroundWait bounds how long a member's stop waits for the
// background work it started to report (a Go duration, or seconds), as the
// queue resolves the yield.backgroundWait policy value onto the pod.
const EnvYieldBackgroundWait = "C4_YIELD_BACKGROUND_WAIT"

// defaultBackgroundWait is the wait with no value on the pod.
const defaultBackgroundWait = 30 * time.Minute

func backgroundWait(p *corev1.Pod) time.Duration {
	v := strings.TrimSpace(podEnv(p, EnvYieldBackgroundWait))
	if d, err := time.ParseDuration(v); err == nil && d >= 0 {
		return d
	}
	var secs int
	if _, err := fmt.Sscanf(v, "%d", &secs); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second
	}
	return defaultBackgroundWait
}

// harvestAt is when a stop that began at from stops waiting for background
// work: after the pod's background wait, and never past the farm's own
// deadline for the stop (a soft stop's yield-by, a deleting pod's grace),
// so the farm never has to kill the session with its work unrecorded.
func (r *Runner) harvestAt(p *corev1.Pod, from time.Time) time.Time {
	at := from.Add(backgroundWait(p))
	if s := p.Annotations[c4v1.AnnotationYieldBy]; s != "" {
		if t, err := time.Parse(time.RFC3339, s); err == nil && t.Add(-time.Minute).Before(at) {
			at = t.Add(-time.Minute)
		}
	}
	if p.DeletionTimestamp != nil && p.DeletionTimestamp.Add(-30*time.Second).Before(at) {
		at = p.DeletionTimestamp.Add(-30 * time.Second)
	}
	return at
}

// awaitBackground keeps a block's idle session running while background
// work it started has not reported (the session takes a new turn when it
// does), until harvestAt; then it records that work in a checkpoint, and the
// stop goes ahead. wait is true while it is waiting.
func (r *Runner) awaitBackground(ctx context.Context, p *corev1.Pod, m *Member, act Active) (wait bool, err error) {
	bg, err := OpenBackground(m.Transcript, act.Started())
	if err != nil || len(bg) == 0 {
		delete(r.bgSince, p.UID)
		return false, err
	}
	since, ok := r.bgSince[p.UID]
	if !ok {
		since = r.now()
		r.bgSince[p.UID] = since
	}
	if at := r.harvestAt(p, since); r.now().Before(at) {
		if !r.bgLogged[p.UID] {
			r.bgLogged[p.UID] = true
			r.logf("%s: its turn ended with %d background task(s) running; waits for them until %s", m.Name, len(bg), at.Local().Format("15:04:05"))
		}
		return true, nil
	}
	delete(r.bgSince, p.UID)
	if err := r.Harness.RecordStop(ctx, m.Name, bg); err != nil {
		return false, err
	}
	r.logf("%s: %d background task(s) did not report in time; recorded, and stopped with the session", m.Name, len(bg))
	return false, nil
}

// lostFile is where a unit records background work its session's stops cut
// short, beside its other control documents, so the record goes with the
// unit's output.
const lostFile = "background-lost.json"

// recordLost adds background work cut short to the unit's record.
func recordLost(out string, bg []Background) error {
	path := filepath.Join(out, ".c4", lostFile)
	var all []Background
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &all)
	}
	all = append(all, bg...)
	b, err := json.MarshalIndent(all, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}
