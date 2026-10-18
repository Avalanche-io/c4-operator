package agent

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeClaude stands in for the claude CLI. No test touches a real session.
type fakeClaude struct {
	mu      sync.Mutex
	active  map[string]Active // by session
	resumes []resumeCall
	stops   []string
	// copyOnResume makes Resume report a different session ID, as the CLI
	// does when it starts a copy.
	copyOnResume string
	// busyFor counts Active calls during which a session stays busy before
	// turning idle.
	busyFor map[string]int
}

type resumeCall struct {
	configDir, dir, session, prompt string
	flags                           []string
}

func newFake() *fakeClaude {
	return &fakeClaude{active: map[string]Active{}, busyFor: map[string]int{}}
}

func (f *fakeClaude) Active(ctx context.Context, configDir string) ([]Active, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []Active
	for s, a := range f.active {
		if n := f.busyFor[s]; n > 0 {
			f.busyFor[s] = n - 1
			a.Status = "busy"
		} else if a.Status == "busy" {
			a.Status = "idle"
			f.active[s] = a
		}
		out = append(out, a)
	}
	return out, nil
}

func (f *fakeClaude) Resume(ctx context.Context, configDir, dir, session string, flags []string, prompt string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resumes = append(f.resumes, resumeCall{configDir, dir, session, prompt, flags})
	if f.copyOnResume != "" {
		f.active[f.copyOnResume] = Active{SessionID: f.copyOnResume, Status: "busy"}
		return f.copyOnResume, nil
	}
	f.active[session] = Active{SessionID: session, Status: "busy", Pid: 4242}
	return session, nil
}

func (f *fakeClaude) Stop(ctx context.Context, configDir, session string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stops = append(f.stops, session)
	delete(f.active, session)
	return nil
}

const sess = "11111111-2222-3333-4444-555555555555"
const sess2 = "66666666-7777-8888-9999-aaaaaaaaaaaa"

// env sets up a config dir with a transcript for a session in cwd.
type env struct {
	t         *testing.T
	h         *Harness
	fake      *fakeClaude
	configDir string
	cwd       string
	clock     time.Time
}

func newEnv(t *testing.T) *env {
	t.Helper()
	root := t.TempDir()
	e := &env{t: t, fake: newFake(), configDir: filepath.Join(root, "claude"), cwd: filepath.Join(root, "work"), clock: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)}
	if err := os.MkdirAll(e.cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	e.h = New(&Store{Dir: filepath.Join(root, "agents")}, e.fake)
	e.h.Workspace = "none"
	e.h.Poll = time.Millisecond
	e.h.SettleQuiet, e.h.SettleMax = time.Millisecond, 10*time.Millisecond
	e.h.Now = func() time.Time { e.clock = e.clock.Add(time.Second); return e.clock }
	e.transcript(sess, "")
	return e
}

func (e *env) transcriptPath(session string) string {
	return filepath.Join(e.configDir, "projects", ProjectSlug(e.cwd), session+".jsonl")
}

// transcript appends text to a session's transcript, creating it.
func (e *env) transcript(session, text string) {
	e.t.Helper()
	p := e.transcriptPath(session)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		e.t.Fatal(err)
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := f.WriteString(text); err != nil {
		e.t.Fatal(err)
	}
	f.Close()
}

func (e *env) register(name, session string) *Member {
	e.t.Helper()
	m, err := e.h.Register(context.Background(), RegisterOptions{Name: name, Session: session, Cwd: e.cwd, ConfigDir: e.configDir, Flags: []string{"-n", name}})
	if err != nil {
		e.t.Fatal(err)
	}
	return m
}

func turn(i int) string { return fmt.Sprintf(`{"type":"assistant","turn":%d}`+"\n", i) }

func TestCheckpointChainAndVerify(t *testing.T) {
	e := newEnv(t)
	e.transcript(sess, turn(1))
	e.register("lead", sess)
	ctx := context.Background()
	for i := 2; i <= 5; i++ {
		e.transcript(sess, turn(i))
		cp, changed, err := e.h.Checkpoint(ctx, "lead", "manual")
		if err != nil || !changed {
			t.Fatalf("turn %d: changed=%v err=%v", i, changed, err)
		}
		if cp.Epoch != 0 || cp.Session != sess {
			t.Fatalf("checkpoint %+v", cp)
		}
	}
	// No new bytes: a Stop checkpoint is skipped.
	if _, changed, _ := e.h.checkpointLocked(ctx, mustGet(t, e, "lead"), "stop"); changed {
		t.Fatal("a stop with no new turn wrote a checkpoint")
	}
	r, err := e.h.Store.Verify("lead")
	if err != nil || r.Broken != 0 || r.Checked != 5 {
		t.Fatalf("verify: %+v %v", r, err)
	}
	// Tamper with an early byte: verify finds it.
	p := e.transcriptPath(sess)
	data, _ := os.ReadFile(p)
	data[3] = 'X'
	os.WriteFile(p, data, 0o644)
	if r, _ := e.h.Store.Verify("lead"); r.Broken == 0 {
		t.Fatal("verify missed a changed transcript")
	}
}

func mustGet(t *testing.T, e *env, name string) *Member {
	t.Helper()
	m, err := e.h.Store.Get(name)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestPartialLineIsNotCheckpointed(t *testing.T) {
	e := newEnv(t)
	e.transcript(sess, turn(1))
	e.register("lead", sess)
	e.transcript(sess, `{"type":"assistant","half`)
	cp, _, err := e.h.Checkpoint(context.Background(), "lead", "manual")
	if err != nil {
		t.Fatal(err)
	}
	if want := int64(len(turn(1))); cp.Offset != want {
		t.Fatalf("offset %d, want %d (the last complete line)", cp.Offset, want)
	}
}

func TestRewrittenTranscriptRestartsChain(t *testing.T) {
	e := newEnv(t)
	e.transcript(sess, turn(1)+turn(2))
	e.register("lead", sess)
	os.WriteFile(e.transcriptPath(sess), []byte(turn(9)), 0o644)
	cp, _, err := e.h.Checkpoint(context.Background(), "lead", "manual")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cp.Note, "restarted") {
		t.Fatalf("note %q", cp.Note)
	}
	if r, err := e.h.Store.Verify("lead"); err != nil || r.Broken != 0 {
		t.Fatalf("verify after restart: %+v %v", r, err)
	}
}

func TestResumeRefusedWhileRunning(t *testing.T) {
	e := newEnv(t)
	e.transcript(sess, turn(1))
	e.register("lead", sess)
	e.fake.active[sess] = Active{SessionID: sess, Status: "idle", Pid: 7}
	_, err := e.h.Resume(context.Background(), "lead", "go on")
	if !errors.Is(err, ErrLive) {
		t.Fatalf("err %v, want ErrLive", err)
	}
	if len(e.fake.resumes) != 0 {
		t.Fatal("resumed a running session")
	}
	if m := mustGet(t, e, "lead"); m.Epoch != 0 {
		t.Fatalf("epoch %d changed on a refused resume", m.Epoch)
	}
}

func TestResumeStoppedRaisesEpochAndPassesFlags(t *testing.T) {
	e := newEnv(t)
	e.transcript(sess, turn(1))
	e.register("lead", sess)
	res, err := e.h.Resume(context.Background(), "lead", "pick up where you left off")
	if err != nil {
		t.Fatal(err)
	}
	if res.Member.Epoch != 1 {
		t.Fatalf("epoch %d", res.Member.Epoch)
	}
	c := e.fake.resumes[0]
	if c.session != sess || c.dir != e.cwd || c.configDir != e.configDir || c.prompt != "pick up where you left off" || strings.Join(c.flags, " ") != "-n lead" {
		t.Fatalf("call %+v", c)
	}
	// Now it runs: a second resume is refused, which is the bug that forked
	// a live agent on 2026-10-18.
	if _, err := e.h.Resume(context.Background(), "lead", ""); !errors.Is(err, ErrLive) {
		t.Fatalf("second resume: %v", err)
	}
	if len(e.fake.resumes) != 1 {
		t.Fatalf("%d resume calls", len(e.fake.resumes))
	}
}

func TestResumeDetectsCopy(t *testing.T) {
	e := newEnv(t)
	e.transcript(sess, turn(1))
	e.register("lead", sess)
	e.fake.copyOnResume = sess2
	res, err := e.h.Resume(context.Background(), "lead", "")
	if !errors.Is(err, ErrForked) || res.Copy != sess2 {
		t.Fatalf("res %+v err %v", res, err)
	}
}

func TestRegisterRefusesReplacementWithoutNewPhase(t *testing.T) {
	e := newEnv(t)
	e.transcript(sess, turn(1))
	e.register("lead", sess)
	e.transcript(sess2, turn(1))
	ctx := context.Background()
	_, err := e.h.Register(ctx, RegisterOptions{Name: "lead", Session: sess2, Cwd: e.cwd, ConfigDir: e.configDir})
	if !errors.Is(err, ErrHasSession) {
		t.Fatalf("err %v, want ErrHasSession", err)
	}
	// A new phase is refused while the old session runs.
	e.fake.active[sess] = Active{SessionID: sess, Status: "busy"}
	if _, err := e.h.Register(ctx, RegisterOptions{Name: "lead", Session: sess2, Cwd: e.cwd, ConfigDir: e.configDir, NewPhase: true}); !errors.Is(err, ErrLive) {
		t.Fatalf("new phase over a live session: %v", err)
	}
	delete(e.fake.active, sess)
	m, err := e.h.Register(ctx, RegisterOptions{Name: "lead", Session: sess2, Cwd: e.cwd, ConfigDir: e.configDir, NewPhase: true})
	if err != nil {
		t.Fatal(err)
	}
	if m.Phase != 1 || len(m.Previous) != 1 || m.Previous[0] != sess || strings.Join(m.Flags, " ") != "-n lead" {
		t.Fatalf("member %+v", m)
	}
	// The same session again is idempotent.
	if _, err := e.h.Register(ctx, RegisterOptions{Name: "lead", Session: sess2, Cwd: e.cwd, ConfigDir: e.configDir}); err != nil {
		t.Fatal(err)
	}
}

func TestForbiddenFlags(t *testing.T) {
	e := newEnv(t)
	for _, f := range []string{"--fork-session", "--resume=x", "--bg", "--continue"} {
		_, err := e.h.Register(context.Background(), RegisterOptions{Name: "lead", Session: sess, Cwd: e.cwd, ConfigDir: e.configDir, Flags: []string{f}})
		if err == nil {
			t.Fatalf("flag %s accepted", f)
		}
	}
}

func TestHook(t *testing.T) {
	e := newEnv(t)
	e.transcript(sess, turn(1))
	e.register("lead", sess)
	ctx := context.Background()
	// A session that is not a member is ignored.
	cp, err := e.h.Hook(ctx, strings.NewReader(`{"session_id":"`+sess2+`","hook_event_name":"Stop"}`))
	if err != nil || cp != nil {
		t.Fatalf("non-member: %v %v", cp, err)
	}
	e.transcript(sess, turn(2))
	cp, err = e.h.Hook(ctx, strings.NewReader(`{"session_id":"`+sess+`","transcript_path":"`+e.transcriptPath(sess)+`","hook_event_name":"Stop"}`))
	if err != nil || cp == nil || cp.Event != "stop" || cp.Offset != int64(len(turn(1)+turn(2))) {
		t.Fatalf("hook checkpoint %+v %v", cp, err)
	}
	if _, err := e.h.Hook(ctx, strings.NewReader("not json")); err == nil {
		t.Fatal("bad hook input accepted")
	}
}

func TestYield(t *testing.T) {
	e := newEnv(t)
	e.transcript(sess, turn(1))
	e.register("lead", sess)
	ctx := context.Background()
	// Not running: parked, nothing stopped.
	if r, err := e.h.Yield(ctx, "lead", time.Second); err != nil || r.State != "parked" || len(e.fake.stops) != 0 {
		t.Fatalf("parked: %+v %v", r, err)
	}
	// Busy for a few polls, then idle: stopped after a yield checkpoint.
	e.fake.active[sess] = Active{SessionID: sess, Status: "busy"}
	e.fake.busyFor[sess] = 3
	e.transcript(sess, turn(2))
	r, err := e.h.Yield(ctx, "lead", time.Hour)
	if err != nil || r.State != "stopped" || r.Checkpoint == nil || r.Checkpoint.Event != "yield" {
		t.Fatalf("yield: %+v %v", r, err)
	}
	if len(e.fake.stops) != 1 || e.fake.stops[0] != sess {
		t.Fatalf("stops %v", e.fake.stops)
	}
	// Busy past the timeout: left running.
	e.fake.active[sess] = Active{SessionID: sess, Status: "busy"}
	e.fake.busyFor[sess] = 1 << 30
	if _, err := e.h.Yield(ctx, "lead", 0); !errors.Is(err, ErrBusy) {
		t.Fatalf("busy: %v", err)
	}
	if len(e.fake.stops) != 1 {
		t.Fatal("stopped a busy session")
	}
}

// TestForcedRestarts simulates the restart of 2026-10-18 many times: a
// member's transcript grows turn by turn, the process dies at a random
// point (sometimes in the middle of a line), the harness resumes it, and
// the chain must stay verifiable, every checkpoint must land on a line
// boundary, the epoch must rise once per resume, and a second resume of
// the live session must always be refused.
func TestForcedRestarts(t *testing.T) {
	e := newEnv(t)
	e.transcript(sess, turn(0))
	e.register("lead", sess)
	ctx := context.Background()
	rng := rand.New(rand.NewSource(1018))
	n := 0
	for restart := 1; restart <= 200; restart++ {
		// Some turns, each ending in a Stop hook.
		for k := rng.Intn(4); k > 0; k-- {
			n++
			e.transcript(sess, turn(n))
			if _, err := e.h.Hook(ctx, strings.NewReader(`{"session_id":"`+sess+`"}`)); err != nil {
				t.Fatal(err)
			}
		}
		// The machine dies, perhaps in the middle of writing a line.
		if rng.Intn(2) == 0 {
			e.transcript(sess, `{"type":"assistant","cut`)
			delete(e.fake.active, sess)
			cp, _, err := e.h.Checkpoint(ctx, "lead", "manual")
			if err != nil {
				t.Fatal(err)
			}
			data, _ := os.ReadFile(e.transcriptPath(sess))
			if cp.Offset == 0 || data[cp.Offset-1] != '\n' {
				t.Fatalf("restart %d: checkpoint at %d is not a line boundary", restart, cp.Offset)
			}
			e.transcript(sess, "\"}\n") // the CLI finishes the line on resume
		}
		delete(e.fake.active, sess)
		res, err := e.h.Resume(ctx, "lead", "")
		if err != nil {
			t.Fatalf("restart %d: %v", restart, err)
		}
		if res.Member.Epoch != restart {
			t.Fatalf("restart %d: epoch %d", restart, res.Member.Epoch)
		}
		if _, err := e.h.Resume(ctx, "lead", ""); !errors.Is(err, ErrLive) {
			t.Fatalf("restart %d: second resume not refused: %v", restart, err)
		}
	}
	if r, err := e.h.Store.Verify("lead"); err != nil || r.Broken != 0 {
		t.Fatalf("verify after 200 restarts: %+v %v", r, err)
	}
	if got := len(e.fake.resumes); got != 200 {
		t.Fatalf("%d resume calls, want 200", got)
	}
}

func TestProjectSlug(t *testing.T) {
	cases := map[string]string{
		"/Users/joshua/ws/Seattle":                                    "-Users-joshua-ws-Seattle",
		"/Users/joshua/ws/active/software/openra/.worktrees/F3-spine": "-Users-joshua-ws-active-software-openra--worktrees-F3-spine",
	}
	for in, want := range cases {
		if got := ProjectSlug(in); got != want {
			t.Errorf("ProjectSlug(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSealWorkspaceGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	dir := t.TempDir()
	gitRun := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.email=t@example.com", "-c", "user.name=t"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	gitRun("init", "-q")
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a\n"), 0o644)
	gitRun("add", "a.txt")
	gitRun("commit", "-q", "-m", "a")
	ctx := context.Background()
	clean := SealWorkspace(ctx, dir, "")
	if clean.Mode != "git" || clean.Head == "" || clean.Dirty != "" || clean.Err != "" {
		t.Fatalf("clean %+v", clean)
	}
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("b\n"), 0o644)
	edited := SealWorkspace(ctx, dir, "")
	os.WriteFile(filepath.Join(dir, "new.txt"), []byte("n\n"), 0o644)
	untracked := SealWorkspace(ctx, dir, "")
	if edited.Dirty == "" || untracked.Dirty == "" || edited.Dirty == untracked.Dirty || untracked.Untracked != 1 {
		t.Fatalf("edited %+v untracked %+v", edited, untracked)
	}
	if again := SealWorkspace(ctx, dir, ""); again.Dirty != untracked.Dirty {
		t.Fatal("the same tree sealed to a different ID")
	}
	if v := SealWorkspace(ctx, dir, "view"); v.View == "" || v.Err != "" {
		t.Fatalf("view mode %+v", v)
	}
	if n := SealWorkspace(ctx, t.TempDir(), ""); n.Mode != "none" {
		t.Fatalf("outside a repo: %+v", n)
	}
}
