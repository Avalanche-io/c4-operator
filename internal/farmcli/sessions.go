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
			InputTokens  int64 `json:"inputTokens"`
			OutputTokens int64 `json:"outputTokens"`
		} `json:"models"`
	} `json:"usage,omitempty"`
	Raw json.RawMessage `json:"-"`
}

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

// tokens are what the session has used, input and output, every model.
func (r row) tokens() int64 {
	var n int64
	if r.Usage != nil {
		for _, m := range r.Usage.Models {
			n += m.InputTokens + m.OutputTokens
		}
	}
	return n
}

// SessionsArgs are `c4 farm agents`'s, with no subcommand.
type SessionsArgs struct {
	// All lists finished and stopped sessions too.
	All bool
	// Agent is c4-agent (default c4-agent on PATH); Claude is the claude
	// CLI, read directly only when c4-agent cannot observe.
	Agent, Claude string
}

// readSessions are this machine's sessions: from c4-agent observe, or,
// with a c4-agent too old to observe or none at all, from claude agents.
func readSessions(ctx context.Context, a SessionsArgs) ([]row, error) {
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
				return nil, fmt.Errorf("c4-agent observe: %w", err)
			}
			rows := make([]row, 0, len(doc.Sessions))
			for _, raw := range doc.Sessions {
				var r row
				if err := json.Unmarshal(raw, &r); err != nil {
					return nil, fmt.Errorf("c4-agent observe: %w", err)
				}
				r.Raw = raw
				rows = append(rows, r)
			}
			return rows, nil
		case !strings.Contains(stderr.String(), "unknown command"):
			return nil, fmt.Errorf("c4-agent observe: %s", strings.TrimSpace(stderr.String()))
		}
	}
	return claudeRows(ctx, a)
}

// claudeRows are the sessions `claude agents --json` lists, in observe's
// shape: no transcript is read, so they carry no question and no usage.
func claudeRows(ctx context.Context, a SessionsArgs) ([]row, error) {
	bin := a.Claude
	if bin == "" {
		p, err := exec.LookPath("claude")
		if err != nil {
			return nil, errors.New("Claude Code (claude) is not on PATH; c4 farm agents shows its sessions")
		}
		bin = p
	}
	args := []string{"agents", "--json"}
	if a.All {
		args = append(args, "--all")
	}
	out, err := exec.CommandContext(ctx, bin, args...).Output()
	if err != nil {
		return nil, fmt.Errorf("claude agents: %w", err)
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
		r := row{Session: s.SessionID, Name: s.Name, Kind: s.Kind, Cwd: s.Cwd, Running: s.State != "done" && s.State != "stopped"}
		switch {
		case s.State == "blocked":
			r.Word = "needs you"
		case s.State == "working", s.Status == "busy":
			r.Word = "working"
		case s.State == "done", s.State == "stopped":
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
	rows, err := readSessions(ctx, a)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		o.printf("no Claude Code sessions are running on this machine\n")
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
	now := o.Clock.now()
	tw := tabwriter.NewWriter(o.W, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "AGENT\tSTATE\tFOLDER\tUSED\tFOR")
	var waiting []row
	for _, r := range rows {
		if r.Word == "needs you" {
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
	if len(waiting) == 0 {
		o.printf("\nNothing needs you.\n")
		return nil
	}
	o.printf("\n")
	for _, r := range waiting {
		if r.NeedsYou != "" {
			o.printf("%s needs you: %s\n", r.label(), r.NeedsYou)
			continue
		}
		o.printf("%s needs you.\n", r.label())
	}
	return nil
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
