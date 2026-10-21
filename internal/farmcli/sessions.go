package farmcli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/Avalanche-io/c4-operator/work/tokens"
)

// row is one Claude Code session on this machine as `c4-agent observe
// --json` gives it: its state in a word, what it asks of its person, and
// what it has used. Raw is the row as read, which the HUD takes as it is.
type row struct {
	Session    string     `json:"session"`
	Name       string     `json:"name,omitempty"`
	Kind       string     `json:"kind"`
	Running    bool       `json:"running"`
	Word       string     `json:"word"`
	NeedsYou   string     `json:"needsYou,omitempty"`
	Cwd        string     `json:"cwd,omitempty"`
	StartedAt  *time.Time `json:"startedAt,omitempty"`
	LastActive *time.Time `json:"lastActive,omitempty"`
	Usage      *struct {
		Models []struct {
			Model            string `json:"model"`
			InputTokens      int64  `json:"inputTokens"`
			OutputTokens     int64  `json:"outputTokens"`
			CacheReadTokens  int64  `json:"cacheReadTokens"`
			CacheWriteTokens int64  `json:"cacheWriteTokens"`
		} `json:"models"`
	} `json:"usage,omitempty"`
	// Weighted is observe's own weighted count, when it carries one: the
	// CLI and the HUD read it ahead of their own sum, so they cannot drift.
	Weighted *float64        `json:"weighted,omitempty"`
	Raw      json.RawMessage `json:"-"`
}

// ended is a session that is over: failed, done or stopped, or no longer
// running.
func (r row) ended() bool {
	return !r.Running || r.Word == "failed" || r.Word == "done" || r.Word == "stopped"
}

// last is when the session was last active, else when it started.
func (r row) last() time.Time {
	switch {
	case r.LastActive != nil:
		return *r.LastActive
	case r.StartedAt != nil:
		return *r.StartedAt
	}
	return time.Time{}
}

// oldEnded is how long ago an ended session must have last been active
// to be history rather than state: c4 farm agents folds it into one line.
const oldEnded = 24 * time.Hour

// label is the session's name, else its short ID.
func (r row) label() string {
	if r.Name != "" {
		return r.Name
	}
	if len(r.Session) > 8 {
		return r.Session[:8]
	}
	return r.Session
}

// tokens are what the session has used, every model, as the farm counts
// spend: each token class weighed by its price against base input
// (work/tokens), so cache reads, a tenth of input or less, do not swell
// it tenfold.
func (r row) tokens() int64 {
	if r.Weighted != nil {
		return int64(*r.Weighted)
	}
	var n float64
	if r.Usage != nil {
		for _, m := range r.Usage.Models {
			n += tokens.Weighted(m.Model, tokens.Counts{Input: m.InputTokens, Output: m.OutputTokens,
				CacheRead: m.CacheReadTokens, CacheWrite: m.CacheWriteTokens})
		}
	}
	return int64(n)
}

// SessionsArgs are `c4 farm agents`'s, with no subcommand.
type SessionsArgs struct {
	// All lists finished and stopped sessions too.
	All bool
	// Agent is c4-agent (default c4-agent on PATH); Claude is the claude
	// CLI, read directly only when c4-agent cannot observe.
	Agent, Claude string
}

// installClaude is what a machine without Claude Code is told.
const installClaude = "Claude Code (claude) is not installed on this machine; install it (https://claude.com/claude-code), then run c4 farm agents again"

// claudeMissing says an error is Claude Code not being there.
func claudeMissing(msg string) bool {
	return strings.Contains(msg, "claude") && (strings.Contains(msg, "executable file not found") || strings.Contains(msg, "not found"))
}

// readSessions are this machine's sessions: from c4-agent observe, or,
// with a c4-agent too old to observe or none at all, from claude agents,
// which cannot say what needs the person (observed is false).
func readSessions(ctx context.Context, a SessionsArgs) (rows []row, observed bool, err error) {
	bin := a.Agent
	if bin == "" {
		bin, _ = exec.LookPath("c4-agent")
	}
	if bin != "" {
		args := []string{"observe", "--json"}
		if a.All {
			args = append(args, "--all")
		}
		var stderr bytes.Buffer
		cmd := exec.CommandContext(ctx, bin, args...)
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		switch {
		case err == nil:
			var doc struct {
				Sessions []json.RawMessage `json:"sessions"`
			}
			if err := json.Unmarshal(out, &doc); err != nil {
				return nil, false, fmt.Errorf("c4-agent observe: %w", err)
			}
			rows := make([]row, 0, len(doc.Sessions))
			for _, raw := range doc.Sessions {
				var r row
				if err := json.Unmarshal(raw, &r); err != nil {
					return nil, false, fmt.Errorf("c4-agent observe: %w", err)
				}
				r.Raw = raw
				rows = append(rows, r)
			}
			return rows, true, nil
		case !strings.Contains(stderr.String(), "unknown command"):
			msg := strings.TrimPrefix(strings.TrimSpace(stderr.String()), "c4-agent: ")
			if claudeMissing(msg) {
				return nil, false, errors.New(installClaude)
			}
			return nil, false, fmt.Errorf("c4-agent observe: %s", msg)
		}
	}
	rows, err = claudeRows(ctx, a)
	return rows, false, err
}

// claudeRows are the sessions `claude agents --json` lists, in observe's
// shape: no transcript is read, so they carry no question and no usage.
func claudeRows(ctx context.Context, a SessionsArgs) ([]row, error) {
	bin := a.Claude
	if bin == "" {
		p, err := exec.LookPath("claude")
		if err != nil {
			return nil, errors.New(installClaude)
		}
		bin = p
	}
	args := []string{"agents", "--json"}
	if a.All {
		args = append(args, "--all")
	}
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		// an old claude has no agents command; say so without its path
		msg, _, _ := strings.Cut(strings.TrimSpace(stderr.String()), "\n")
		if msg == "" || strings.Contains(msg, "unknown") || strings.Contains(msg, "agents") {
			return nil, errors.New("this Claude Code is too old to list its sessions; run claude update, then c4 farm agents again")
		}
		return nil, fmt.Errorf("claude agents: %s", msg)
	}
	var ss []struct {
		Name, SessionID, Cwd, Kind, Status, State string
		StartedAt                                 int64 // ms
	}
	if err := json.Unmarshal(out, &ss); err != nil {
		return nil, fmt.Errorf("claude agents: %w", err)
	}
	rows := make([]row, 0, len(ss))
	for _, s := range ss {
		r := row{Session: s.SessionID, Name: s.Name, Kind: s.Kind, Cwd: s.Cwd, Running: s.State != "done" && s.State != "stopped" && s.State != "failed"}
		switch {
		case s.State == "blocked":
			r.Word = "needs you"
		case s.State == "working", s.Status == "busy":
			r.Word = "working"
		case s.State == "done", s.State == "stopped", s.State == "failed":
			r.Word = s.State
		default:
			r.Word = "idle"
		}
		if s.StartedAt > 0 {
			t := time.UnixMilli(s.StartedAt).UTC()
			r.StartedAt = &t
		}
		r.Raw, _ = json.Marshal(r)
		rows = append(rows, r)
	}
	return rows, nil
}

// Sessions is `c4 farm agents` with nothing after it: the Claude Code
// sessions on this machine, read-only, one line each with its state in a
// word, what it has used, and then what needs the person. Nothing is
// moved, stopped or signed in, and no farm is needed.
func Sessions(ctx context.Context, o Out, a SessionsArgs) error {
	rows, observed, err := readSessions(ctx, a)
	if err != nil {
		return err
	}
	now := o.Clock.now()
	// a session that ended a day ago or more is history, not state: one
	// line says it is there, and --all lists it
	var old []row
	if !a.All {
		kept := rows[:0]
		for _, r := range rows {
			if r.ended() && now.Sub(r.last()) >= oldEnded {
				old = append(old, r)
				continue
			}
			kept = append(kept, r)
		}
		rows = kept
	}
	if len(rows) == 0 {
		o.printf("no Claude Code sessions are running on this machine\n")
		olderEnded(o, old, now)
		return nil
	}
	// what needs the person first, then what is working, newest first
	rank := func(r row) int {
		switch r.Word {
		case "needs you":
			return 0
		case "at its limit":
			return 1
		case "working":
			return 2
		case "idle":
			return 3
		}
		return 4
	}
	started := func(r row) time.Time {
		if r.StartedAt == nil {
			return time.Time{}
		}
		return *r.StartedAt
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if ri, rj := rank(rows[i]), rank(rows[j]); ri != rj {
			return ri < rj
		}
		return started(rows[i]).After(started(rows[j]))
	})
	home, _ := os.UserHomeDir()
	tw := tabwriter.NewWriter(o.W, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "AGENT\tSTATE\tFOLDER\tUSED\tFOR")
	var waiting []row
	for _, r := range rows {
		if r.Word == "needs you" || r.Word == "at its limit" {
			waiting = append(waiting, r)
		}
		dir := r.Cwd
		if home != "" && strings.HasPrefix(dir, home+string(filepath.Separator)) {
			dir = "~" + dir[len(home):]
		}
		used := "-"
		if n := r.tokens(); n > 0 {
			used = tokenCount(n)
		}
		age := "-"
		if r.StartedAt != nil {
			age = Span(now.Sub(*r.StartedAt).Truncate(time.Minute))
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", r.label(), r.Word, dir, used, age)
	}
	tw.Flush()
	olderEnded(o, old, now)
	switch {
	case len(waiting) == 0 && !observed:
		// claude agents says which sessions are blocked, but not what on,
		// nor what a limit holds up
		o.printf("\nWithout c4-agent, c4 farm agents can't tell what needs you; brew install avalanche-io/tap/c4 brings it\n")
		return nil
	case len(waiting) == 0:
		o.printf("\nNothing needs you.\n")
		return nil
	}
	o.printf("\n")
	for _, r := range waiting {
		if r.Word == "at its limit" {
			if r.NeedsYou != "" {
				o.printf("%s is at its limit: %s\n", r.label(), r.NeedsYou)
				continue
			}
			o.printf("%s is at its limit and waits for it to reset, or for you\n", r.label())
			continue
		}
		if r.NeedsYou != "" {
			o.printf("%s needs you: %s\n", r.label(), r.NeedsYou)
			continue
		}
		o.printf("%s needs you.\n", r.label())
	}
	return nil
}

// olderEnded is the line for the ended sessions folded out of the list:
// how many, and how long ago the latest ended.
func olderEnded(o Out, old []row, now time.Time) {
	if len(old) == 0 {
		return
	}
	latest := old[0].last()
	for _, r := range old[1:] {
		if t := r.last(); t.After(latest) {
			latest = t
		}
	}
	what := "1 older session ended"
	if len(old) > 1 {
		what = fmt.Sprintf("%d older sessions ended, the latest", len(old))
	}
	ago := ""
	if !latest.IsZero() {
		ago = " " + daysAgo(now.Sub(latest))
	}
	o.printf("%s%s; c4 farm agents --all lists them\n", what, ago)
}

// daysAgo is a day or more as people say it: 1 day ago, 74 days ago.
func daysAgo(d time.Duration) string {
	n := int(d / (24 * time.Hour))
	if n == 1 {
		return "1 day ago"
	}
	return fmt.Sprintf("%d days ago", n)
}

// tokenCount is a token count as people read it: 950, 41k, 2.3M.
func tokenCount(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM tokens", float64(n)/1e6)
	case n >= 1000:
		return fmt.Sprintf("%dk tokens", n/1000)
	}
	return fmt.Sprintf("%d tokens", n)
}

// andList is names as a sentence lists them: "a", "a and b", "a, b and c".
func andList(names []string) string {
	if len(names) < 2 {
		return strings.Join(names, "")
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}
