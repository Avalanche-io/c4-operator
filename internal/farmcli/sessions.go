package farmcli

import (
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

// session is one Claude Code session on this machine, as `claude agents
// --json` lists it.
type session struct {
	Name      string `json:"name"`
	SessionID string `json:"sessionId"`
	Cwd       string `json:"cwd"`
	Kind      string `json:"kind"`
	Status    string `json:"status"`
	State     string `json:"state"`
	StartedAt int64  `json:"startedAt"` // ms
}

// word is the session's state in a word, and whether it waits on the
// person.
func (s session) word() (string, bool) {
	switch {
	case s.State == "blocked":
		return "needs you", true
	case s.State == "working", s.Status == "busy":
		return "working", false
	case s.State == "done":
		return "done", false
	case s.State == "stopped":
		return "stopped", false
	}
	return "idle", false
}

// SessionsArgs are `c4 farm agents`'s, with no subcommand.
type SessionsArgs struct {
	// All lists finished and stopped sessions too.
	All bool
	// Claude is the claude CLI (default claude on PATH).
	Claude string
}

// Sessions is `c4 farm agents` with nothing after it: the Claude Code
// sessions on this machine, read-only, one line each with its state in a
// word, and what needs the person. It only reads what `claude agents`
// reports: nothing is moved, stopped or signed in, and no farm is needed.
func Sessions(ctx context.Context, o Out, a SessionsArgs) error {
	bin := a.Claude
	if bin == "" {
		p, err := exec.LookPath("claude")
		if err != nil {
			return errors.New("Claude Code (claude) is not on PATH; c4 farm agents shows its sessions")
		}
		bin = p
	}
	args := []string{"agents", "--json"}
	if a.All {
		args = append(args, "--all")
	}
	out, err := exec.CommandContext(ctx, bin, args...).Output()
	if err != nil {
		return fmt.Errorf("claude agents: %w", err)
	}
	var ss []session
	if err := json.Unmarshal(out, &ss); err != nil {
		return fmt.Errorf("claude agents: %w", err)
	}
	if len(ss) == 0 {
		o.printf("no Claude Code sessions are running on this machine\n")
		return nil
	}
	// what needs the person first, then what is working, newest first
	rank := func(s session) int {
		w, needs := s.word()
		switch {
		case needs:
			return 0
		case w == "working":
			return 1
		case w == "idle":
			return 2
		}
		return 3
	}
	sort.SliceStable(ss, func(i, j int) bool {
		if ri, rj := rank(ss[i]), rank(ss[j]); ri != rj {
			return ri < rj
		}
		return ss[i].StartedAt > ss[j].StartedAt
	})
	home, _ := os.UserHomeDir()
	now := o.Clock.now()
	tw := tabwriter.NewWriter(o.W, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "AGENT\tSTATE\tFOLDER\tFOR")
	var waiting []string
	for _, s := range ss {
		w, needs := s.word()
		if needs {
			waiting = append(waiting, s.label())
		}
		dir := s.Cwd
		if home != "" && strings.HasPrefix(dir, home+string(filepath.Separator)) {
			dir = "~" + dir[len(home):]
		}
		age := "-"
		if s.StartedAt > 0 {
			age = Span(now.Sub(time.UnixMilli(s.StartedAt)).Truncate(time.Minute))
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", s.label(), w, dir, age)
	}
	tw.Flush()
	switch len(waiting) {
	case 0:
		o.printf("\nNothing needs you.\n")
	case 1:
		o.printf("\n%s needs you.\n", waiting[0])
	default:
		o.printf("\n%s need you: %s.\n", plural(len(waiting), "session", "sessions"), andList(waiting))
	}
	return nil
}

// label is the session's name, else its short ID.
func (s session) label() string {
	if s.Name != "" {
		return s.Name
	}
	if len(s.SessionID) > 8 {
		return s.SessionID[:8]
	}
	return s.SessionID
}

// andList is names as a sentence lists them: "a", "a and b", "a, b and c".
func andList(names []string) string {
	if len(names) < 2 {
		return strings.Join(names, "")
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}
