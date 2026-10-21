package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"sort"
	"strings"
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
	// Word is its state in one word: working, needs you, idle, done,
	// stopped, or at its limit.
	Word string `json:"word"`
	// NeedsYou is the one thing that needs its person, or "".
	NeedsYou   string     `json:"needsYou,omitempty"`
	Cwd        string     `json:"cwd,omitempty"`
	StartedAt  *time.Time `json:"startedAt,omitempty"`
	LastActive *time.Time `json:"lastActive,omitempty"`
	// TurnAge is how long since the session last wrote, as a duration.
	TurnAge        string        `json:"turnAge,omitempty"`
	BackgroundOpen int           `json:"backgroundOpen"`
	Usage          *ObservedUse  `json:"usage,omitempty"`
	Limit          *ObservedHalt `json:"limit,omitempty"`
	Transcript     string        `json:"transcript,omitempty"`
}

// ObservedUse is what a session has used over its whole transcript.
type ObservedUse struct {
	Models []ModelUsage `json:"models"`
}

// ObservedHalt is a usage window a session hit that has not reset.
type ObservedHalt struct {
	Kind   string     `json:"kind"`
	Text   string     `json:"text"`
	Resets *time.Time `json:"resets,omitempty"`
}

// The words, so a reader can match on them.
const (
	WordWorking  = "working"
	WordNeedsYou = "needs you"
	WordIdle     = "idle"
	WordDone     = "done"
	WordStopped  = "stopped"
	WordAtLimit  = "at its limit"
)

// Observe lists the sessions in configDir (all of them, stopped background
// ones too, with all) as they are at now. A transcript it cannot read
// leaves its row with what the CLI said alone.
func (h *Harness) Observe(ctx context.Context, configDir string, all bool, now time.Time) ([]Observed, error) {
	as, err := h.Claude.Active(ctx, configDir, all)
	if err != nil {
		return nil, err
	}
	out := make([]Observed, 0, len(as))
	for _, a := range as {
		out = append(out, observe(configDir, a, now))
	}
	sort.SliceStable(out, func(i, j int) bool {
		// what needs its person first, then what is working
		ri, rj := wordRank(out[i].Word), wordRank(out[j].Word)
		if ri != rj {
			return ri < rj
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

func wordRank(w string) int {
	switch w {
	case WordNeedsYou:
		return 0
	case WordAtLimit:
		return 1
	case WordWorking:
		return 2
	case WordIdle:
		return 3
	case WordDone:
		return 4
	}
	return 5
}

func observe(configDir string, a Active, now time.Time) Observed {
	// the CLI gives a status (idle or busy) only to a session that runs
	o := Observed{Session: a.SessionID, Name: a.Name, Kind: a.Kind, Cwd: a.Cwd, Running: a.Status == "busy" || a.Status == "idle"}
	if t := a.Started(); !t.IsZero() {
		o.StartedAt = &t
	}
	switch {
	case a.State == "blocked":
		o.Word = WordNeedsYou
	case a.Status == "busy":
		o.Word = WordWorking
	case a.Kind == "background" && a.State == "done":
		o.Word = WordDone
	case !o.Running:
		o.Word = WordStopped
	default:
		o.Word = WordIdle
	}
	tr, err := FindTranscript(configDir, a.Cwd, a.SessionID)
	if err != nil {
		if o.Word == WordNeedsYou {
			o.NeedsYou = "it is waiting for you in Claude Code"
		}
		return o
	}
	o.Transcript = tr
	if info, err := os.Stat(tr); err == nil {
		t := info.ModTime()
		o.LastActive = &t
		o.TurnAge = max(now.Sub(t), 0).Truncate(time.Second).String()
	}
	if bg, err := OpenBackground(tr, a.Started()); err == nil && o.Running {
		o.BackgroundOpen = len(bg)
	}
	if info, err := os.Stat(tr); err == nil {
		if u, err := TranscriptUsage(tr, 0, info.Size()); err == nil && len(u.Models) > 0 {
			o.Usage = &ObservedUse{Models: u.Models}
		}
	}
	if l, _, err := scanUsageLimit(tr, 0); err == nil && l != nil && limitHolds(*l, now) {
		o.Limit = &ObservedHalt{Kind: l.Kind, Text: l.Text}
		if !l.Resets.IsZero() {
			r := l.Resets
			o.Limit.Resets = &r
		}
		if o.Word != WordNeedsYou && o.Word != WordWorking {
			o.Word = WordAtLimit
		}
		if l.Kind == "spend" {
			o.NeedsYou = "its spend limit, which only you can raise: " + l.Text
		}
	}
	if o.Word == WordNeedsYou {
		o.NeedsYou = "it is waiting for you in Claude Code"
		if ask := lastAssistantText(tr); ask != "" {
			o.NeedsYou = "it asks: " + ask
		}
	}
	return o
}

// limitHolds reports whether a usage limit hit has not reset at now: one
// with a reset time until then, one without for the probe interval.
func limitHolds(l UsageLimit, now time.Time) bool {
	if !l.Resets.IsZero() {
		return now.Before(l.Resets)
	}
	return now.Sub(l.At) < defaultLimitProbe
}

// lastAssistantText is the first line of the last thing the session's
// model said, from the transcript's tail, up to 120 characters.
func lastAssistantText(transcript string) string {
	f, err := os.Open(transcript)
	if err != nil {
		return ""
	}
	defer f.Close()
	const tail = 512 << 10
	if info, err := f.Stat(); err == nil && info.Size() > tail {
		if _, err := f.Seek(info.Size()-tail, io.SeekStart); err != nil {
			return ""
		}
	}
	last := ""
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 16<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if !strings.Contains(string(line), `"assistant"`) {
			continue
		}
		var rec struct {
			Type              string `json:"type"`
			IsAPIErrorMessage bool   `json:"isApiErrorMessage"`
			Message           struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(line, &rec) != nil || rec.Type != "assistant" || rec.IsAPIErrorMessage {
			continue
		}
		var parts []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(rec.Message.Content, &parts) != nil {
			continue
		}
		for _, p := range parts {
			if p.Type == "text" && strings.TrimSpace(p.Text) != "" {
				last = p.Text
			}
		}
	}
	first, _, _ := strings.Cut(strings.TrimSpace(last), "\n")
	if r := []rune(first); len(r) > 120 {
		first = string(r[:119]) + "…"
	}
	return first
}
