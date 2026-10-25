package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// A background session a lead starts runs `claude --bg ...` through its
// Bash tool. Neither the transcript nor the CLI records which session that
// call started, so the link is a match, and its row says so
// (ParentClaimed). The rule:
//   - one launch, one session, and never a session the farm runs;
//   - the session runs in the folder the command ran in;
//   - it started between the call and its result (a launch the person had
//     to approve may wait long before it runs);
//   - a launch naming a session (-n, --name) takes only that name; unnamed
//     launches take the unclaimed sessions in the order they started.

// launchGrace is how long after a launch's result its session may still
// be seen starting, and how long before the call (clocks differ a little).
const launchGrace = 5 * time.Second

// launchOpen is how long a launch with no result yet may still claim a
// session.
const launchOpen = time.Minute

type bgLaunch struct {
	ID       string    // the Bash call
	At       time.Time // when it was called
	ResultAt time.Time // when its result came (zero while under way)
	Dir      string    // the folder its command cd'd into, as written ("" for the session's own)
	Name     string    // its -n or --name, when it gives one
}

// claudeLaunches are the claude --bg commands in an assistant message's
// Bash calls: only a command that is claude itself, not one that mentions
// it (echo, grep), with --bg as an argument of its own.
func claudeLaunches(content json.RawMessage, at time.Time) []bgLaunch {
	var parts []struct {
		Type  string `json:"type"`
		ID    string `json:"id"`
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
		dir := ""
		for _, cmd := range shellCommands(p.Input.Command) {
			words := cmd
			for len(words) > 0 && (strings.Contains(words[0], "=") || words[0] == "exec" || words[0] == "nohup" || words[0] == "env") {
				words = words[1:] // VAR=value, exec, nohup, env
			}
			if len(words) == 0 {
				continue
			}
			if words[0] == "cd" && len(words) > 1 {
				dir = words[1]
				continue
			}
			if filepath.Base(words[0]) != "claude" {
				continue
			}
			l := bgLaunch{ID: p.ID, At: at, Dir: dir}
			bg := false
			for i, w := range words[1:] {
				switch {
				case w == "--bg":
					bg = true
				case (w == "-n" || w == "--name") && i+2 < len(words):
					l.Name = words[i+2]
				case strings.HasPrefix(w, "--name="):
					l.Name = strings.TrimPrefix(w, "--name=")
				}
			}
			if bg {
				out = append(out, l)
			}
		}
	}
	return out
}

// shellCommands splits a shell line into its simple commands, each as its
// words, quotes removed: on ;, &&, ||, |, & and newlines outside quotes.
func shellCommands(line string) [][]string {
	var cmds [][]string
	var words []string
	var word strings.Builder
	inWord := false
	endWord := func() {
		if inWord {
			words = append(words, word.String())
			word.Reset()
			inWord = false
		}
	}
	endCmd := func() {
		endWord()
		if len(words) > 0 {
			cmds = append(cmds, words)
			words = nil
		}
	}
	rs := []rune(line)
	for i := 0; i < len(rs); i++ {
		c := rs[i]
		switch {
		case c == '\'':
			inWord = true
			for i++; i < len(rs) && rs[i] != '\''; i++ {
				word.WriteRune(rs[i])
			}
		case c == '"':
			inWord = true
			for i++; i < len(rs) && rs[i] != '"'; i++ {
				if rs[i] == '\\' && i+1 < len(rs) {
					i++
				}
				word.WriteRune(rs[i])
			}
		case c == '\\' && i+1 < len(rs):
			inWord = true
			i++
			word.WriteRune(rs[i])
		case c == ';' || c == '&' || c == '|' || c == '\n' || c == '(' || c == ')':
			endCmd()
		case c == ' ' || c == '\t':
			endWord()
		default:
			inWord = true
			word.WriteRune(c)
		}
	}
	endCmd()
	return cmds
}

// launchDir is the folder a launch ran its command in: its cd's, against
// the folder of the session that ran it.
func launchDir(l bgLaunch, cwd string) string {
	d := l.Dir
	switch {
	case d == "":
		return filepath.Clean(cwd)
	case d == "~" || strings.HasPrefix(d, "~/"):
		home, _ := os.UserHomeDir()
		return filepath.Join(home, strings.TrimPrefix(d, "~"))
	case filepath.IsAbs(d):
		return filepath.Clean(d)
	}
	return filepath.Join(cwd, d)
}

// claimParents links background sessions the CLI lists with no parent to
// the session whose launch started them, by the rule above.
func claimParents(rows []Observed, scans map[string]sessionScan, now time.Time) {
	cwd := map[string]string{}
	for _, r := range rows {
		cwd[r.Session] = r.Cwd
	}
	type launch struct {
		bgLaunch
		lead string
		dir  string
	}
	var named, unnamed []launch
	for session, sc := range scans {
		for _, l := range sc.bgLaunches {
			x := launch{bgLaunch: l, lead: session, dir: launchDir(l, cwd[session])}
			if l.Name != "" {
				named = append(named, x)
			} else {
				unnamed = append(unnamed, x)
			}
		}
	}
	byTime := func(ls []launch) {
		sort.Slice(ls, func(i, j int) bool { return ls[i].At.Before(ls[j].At) })
	}
	byTime(named)
	byTime(unnamed)
	// candidates in the order they started
	var cand []int
	for i, r := range rows {
		if r.Parent == "" && r.Kind == "background" && r.StartedAt != nil && !r.OnFarm {
			cand = append(cand, i)
		}
	}
	sort.SliceStable(cand, func(a, b int) bool { return rows[cand[a]].StartedAt.Before(*rows[cand[b]].StartedAt) })
	taken := map[int]bool{}
	claim := func(l launch, name string) {
		until := l.ResultAt
		if until.IsZero() {
			until = minTime(now, l.At.Add(launchOpen))
		}
		for _, i := range cand {
			r := &rows[i]
			if taken[i] || r.Session == l.lead || (name != "" && r.Name != name) || filepath.Clean(r.Cwd) != l.dir {
				continue
			}
			if r.StartedAt.Before(l.At.Add(-launchGrace)) || r.StartedAt.After(until.Add(launchGrace)) {
				continue
			}
			taken[i] = true
			r.Parent, r.ParentClaimed = l.lead, true
			return
		}
	}
	for _, l := range named {
		claim(l, l.Name)
	}
	for _, l := range unnamed {
		claim(l, "")
	}
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
