package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A runner that observes reports the machine's sessions to the farm as its
// node, with its own token, once a minute: each row as observe writes it,
// a registered member's marked on the farm. Without it, nothing is posted.
func TestRunnerPostsObservedSessions(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.register("lead", sess)
	const loose = "bbbbbbbb-0000-4000-8000-000000000001"
	e.transcript(loose, turn(1))
	e.fake.active[sess] = Active{SessionID: sess, Name: "lead", Kind: "background", Status: "idle", State: "done", Cwd: e.cwd}
	e.fake.active[loose] = Active{SessionID: loose, Name: "research", Kind: "interactive", Status: "idle", Cwd: e.cwd}
	var posts []ObservedReport
	var auth, path string
	hud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth, path = r.Header.Get("Authorization"), r.URL.Path
		var rep ObservedReport
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &rep)
		posts = append(posts, rep)
		w.Write([]byte(`{"shownTo":["noor"]}`))
	}))
	defer hud.Close()
	r, _ := hostRunner(t, e, hostPod())
	now := time.Date(2026, 10, 21, 3, 0, 0, 0, time.UTC)
	r.Now, r.Node, r.Owners = func() time.Time { return now }, "laptop-1", []string{"noor"}
	// reports are made beside the heartbeat: wait for each
	tick := func() {
		r.observeTick(ctx)
		for deadline := time.Now().Add(5 * time.Second); r.observing.Load() && time.Now().Before(deadline); {
			time.Sleep(time.Millisecond)
		}
	}
	tick()
	if len(posts) != 0 {
		t.Fatal("posted without --observe")
	}
	r.Observe, r.HUD, r.ObserveConfigDir, r.HUDToken = true, hud.URL, e.configDir, func() string { return "tok" }
	tick()
	tick() // within the minute: nothing more
	if len(posts) != 1 || path != "/api/v1/nodes/laptop-1/observed" || auth != "Bearer tok" {
		t.Fatalf("%d posts to %s with %q", len(posts), path, auth)
	}
	onFarm := map[string]bool{}
	for _, o := range posts[0].Sessions {
		onFarm[o.Name] = o.OnFarm
		if o.Transcript != "" || filepath.IsAbs(o.Cwd) {
			t.Errorf("%s: a path on the machine left it: %q %q", o.Name, o.Cwd, o.Transcript)
		}
	}
	if !onFarm["lead"] || onFarm["research"] || len(posts[0].Sessions) != 2 || posts[0].Machine == "" || strings.Join(posts[0].Owners, ",") != strings.Join(r.Owners, ",") || len(r.Owners) == 0 {
		t.Fatalf("report %+v", posts[0])
	}
	now = now.Add(time.Minute)
	tick()
	if len(posts) != 2 {
		t.Fatalf("%d posts after a minute", len(posts))
	}
}

// Without Claude Code on the PATH, observing says so plainly.
func TestObserveWithoutClaudeCode(t *testing.T) {
	h := New(&Store{Dir: t.TempDir()}, &CLI{Path: "claude-not-installed-here"})
	_, err := h.Observe(context.Background(), t.TempDir(), false, time.Now())
	if err == nil || !strings.Contains(err.Error(), "Claude Code isn't installed here, or isn't on the PATH") {
		t.Fatalf("%v", err)
	}
}
