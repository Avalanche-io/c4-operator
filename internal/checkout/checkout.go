// Package checkout gives one interface to every way C4 makes a writable
// workspace over a view: an overlay on Linux, an APFS clone on macOS, and
// later a c4d NFS export. Whatever the backend, a workspace's result is
// the same c4m changeset over its view, made by one capture engine; a
// backend decides only where the workspace's writes are kept and how
// they are read back (capture.Input.Walk).
//
// The contract is proposals/storage/CHECKOUT.md.
package checkout

import (
	"context"
	"strconv"
	"strings"

	"github.com/Avalanche-io/c4-operator/internal/capture"
	"github.com/Avalanche-io/c4-operator/internal/view"
)

// Backend makes, captures and removes workspaces.
type Backend interface {
	// Name is the backend's short name: "clone", "overlay", "nfs".
	Name() string
	// Mount makes a writable workspace of v at target.
	Mount(ctx context.Context, v *view.View, target string) (*Workspace, error)
	// Capture reads the workspace's writes into a changeset over its
	// view. in carries the store to write to (Project, Cache, Records,
	// Sink, Workers); the backend sets Parent, Upper and Walk.
	Capture(ctx context.Context, ws *Workspace, in capture.Input) (*capture.Result, error)
	// Unmount removes the workspace.
	Unmount(ctx context.Context, ws *Workspace) error
}

// Workspace is one writable checkout.
type Workspace struct {
	View   *view.View
	Target string // what the agent works in
	Upper  string // where its writes are, in its backend's form
	// Env is added to the agent's environment: git's settings for a
	// workspace (GitEnv). They live here, never in the workspace's files,
	// because everything written there is captured as a change.
	Env []string
}

// GitEnv returns env with git told, through GIT_CONFIG_COUNT, to compare
// only a file's size and whole-second mtime (core.checkStat=minimal,
// core.trustctime=false) and never to repack on its own (gc.auto=0). A
// workspace's files have new inode numbers and ctimes wherever it is
// made, so git's default check would re-hash every file of an index made
// elsewhere. Settings env already passes this way are kept.
func GitEnv(env []string) []string {
	n := 0
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "GIT_CONFIG_COUNT="); ok {
			n, _ = strconv.Atoi(v)
		}
	}
	out := make([]string, 0, len(env)+7)
	for _, kv := range env {
		if !strings.HasPrefix(kv, "GIT_CONFIG_COUNT=") {
			out = append(out, kv)
		}
	}
	for _, kv := range [][2]string{{"core.checkStat", "minimal"}, {"core.trustctime", "false"}, {"gc.auto", "0"}} {
		i := strconv.Itoa(n)
		out = append(out, "GIT_CONFIG_KEY_"+i+"="+kv[0], "GIT_CONFIG_VALUE_"+i+"="+kv[1])
		n++
	}
	return append(out, "GIT_CONFIG_COUNT="+strconv.Itoa(n))
}
