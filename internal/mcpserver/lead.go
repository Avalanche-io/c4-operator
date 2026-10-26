package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// A lead is a person's own Claude Code session that puts workers on the
// farm (c4_agent). The farm MCP server runs as a child of that session, so
// it can say which session it serves and where that session works, and
// read the lead's HEAD there: the worker's base travels in the request as
// a commit, and the farm never reads the lead's tree.

// lead is the session this server serves.
type lead struct {
	// Session is the lead's session ID: a claim (X-C4-Agent), graded
	// claimed, never an identity; the person's own token is that.
	Session string
	// Cwd is the folder the lead works in.
	Cwd string
}

// readLead finds the session this server serves. Claude Code starts an
// MCP server as its own child with CLAUDE_PID, CLAUDE_CODE_SESSION_ID,
// CLAUDE_CONFIG_DIR and CLAUDE_PROJECT_DIR in its environment, and keeps a
// file per running session, <config>/sessions/<pid>.json, with its
// sessionId and cwd (both checked with Claude Code 2.1.310). The file is
// read at each call, so it is preferred to the environment the server
// started with; whether it follows a /clear is the CLI's to say, not
// something checked here. It is the CLI's own file, so its absence or a
// new shape means only that it says nothing, and the environment is used
// instead.
func readLead(getenv func(string) string, ppid int) lead {
	var l lead
	dir := getenv("CLAUDE_CONFIG_DIR")
	if dir == "" {
		if home, err := os.UserHomeDir(); err == nil {
			dir = filepath.Join(home, ".claude")
		}
	}
	pids := []string{getenv("CLAUDE_PID")}
	if ppid > 1 {
		pids = append(pids, strconv.Itoa(ppid))
	}
	for _, pid := range pids {
		if pid == "" || dir == "" {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, "sessions", filepath.Base(pid)+".json"))
		if err != nil {
			continue
		}
		var f struct {
			SessionID string `json:"sessionId"`
			Cwd       string `json:"cwd"`
		}
		if json.Unmarshal(b, &f) == nil && f.SessionID != "" {
			l = lead{Session: f.SessionID, Cwd: f.Cwd}
			break
		}
	}
	if l.Session == "" {
		l.Session = getenv("CLAUDE_CODE_SESSION_ID")
	}
	if l.Cwd == "" {
		l.Cwd = getenv("CLAUDE_PROJECT_DIR")
	}
	if l.Cwd == "" {
		l.Cwd, _ = os.Getwd()
	}
	return l
}

// inBlock reports whether this server runs inside a farm block, whose
// session may not start workers (LEAD decision 3). On a host the marker is
// kept by the agent; the HUD refuses a unit's own token regardless.
func inBlock(getenv func(string) string) bool { return getenv("C4_AGENT_BLOCK") == "1" }

// errInBlock is c4_agent's answer inside a block.
var errInBlock = errors.New("a worker may not start workers: only a lead calls c4_agent. Do the work yourself, or say in your summary what should be split off")

// leadBase is the commit a worker starts from: want (a commit or a ref;
// HEAD by default) resolved in the lead's folder to its full ID, so the
// worker starts where the lead was even if a branch moves. dirty lists up
// to five paths with changes the worker will not see (untracked files
// too: a design the lead has not committed is the usual one).
func leadBase(ctx context.Context, cwd, want string) (sha string, dirty []string, err error) {
	want = strings.TrimSpace(want)
	if want == "" {
		want = "HEAD"
	}
	if strings.HasPrefix(want, "-") || strings.ContainsAny(want, " \t\n") {
		return "", nil, fmt.Errorf("base %q is not a commit", want)
	}
	out, err := exec.CommandContext(ctx, "git", "-C", cwd, "rev-parse", "--verify", "--quiet", "--end-of-options", want+"^{commit}").Output()
	sha = strings.TrimSpace(string(out))
	if err != nil || sha == "" {
		if want == "HEAD" {
			return "", nil, fmt.Errorf("%s has no commit to start a worker from: commit first", cwd)
		}
		return "", nil, fmt.Errorf("base %s is not a commit in %s", want, cwd)
	}
	st, err := exec.CommandContext(ctx, "git", "-C", cwd, "status", "--porcelain").Output()
	if err != nil {
		return sha, nil, nil
	}
	for _, line := range strings.Split(strings.TrimRight(string(st), "\n"), "\n") {
		if len(line) > 3 && len(dirty) < 5 {
			dirty = append(dirty, strings.TrimSpace(line[3:]))
		}
	}
	return sha, dirty, nil
}
