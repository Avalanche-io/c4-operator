package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakePrint is claude in print mode as a block pod runs it: it appends a
// turn to the session's transcript (refusing --resume of a session it has
// no transcript for), logs how it was called, and leaves a next part on
// its first call and the task done after that.
const fakePrint = `#!/bin/sh
sid=""; mode=""
while [ $# -gt 0 ]; do
  case "$1" in
    --session-id) sid=$2; mode=new; shift ;;
    --resume) sid=$2; mode=resume; shift ;;
    --) shift; break ;;
  esac
  shift
done
tr="$CLAUDE_CONFIG_DIR/projects/$FAKE_SLUG/$sid.jsonl"
mkdir -p "$(dirname "$tr")"
if [ "$mode" = resume ] && [ ! -f "$tr" ]; then echo "No conversation found with session ID: $sid" >&2; exit 1; fi
echo '{"type":"assistant","message":{"id":"m","model":"opus","content":[{"type":"text","text":"a turn"}]}}' >> "$tr"
echo "$mode $sid" >> "$FAKE_LOG"
if [ -n "$FAKE_NEEDS" ]; then echo "$FAKE_NEEDS" > "$C4_OUTPUT/.c4/needs.json"; exit 0; fi
if [ "$(wc -l < "$FAKE_LOG")" -eq 1 ]; then echo "part 2" > "$C4_OUTPUT/.c4/more"; else echo "done" > "$C4_OUTPUT/.c4/done"; fi
`

// A block in a pod starts its session fresh, wakes it for its next part,
// and leaves it in its output view; a later attempt elsewhere (another
// output root, another holder store) resumes the same session by its ID
// from that view, with its checkpoint chain intact.
func TestPodBlockRunsAndResumesFromItsView(t *testing.T) {
	ctx := context.Background()
	bin := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(bin, []byte(fakePrint), 0o755); err != nil {
		t.Fatal(err)
	}
	cwd := t.TempDir()
	log := filepath.Join(t.TempDir(), "calls")
	t.Setenv("FAKE_SLUG", ProjectSlug(cwd))
	t.Setenv("FAKE_LOG", log)
	t.Setenv("C4_JOB", "port")
	t.Setenv("C4_UNIT_NAME", "notes")
	t.Setenv(EnvBlock, "1")
	t.Setenv(EnvBlockPrompt, "Write notes.")
	run := func(out, cfg string) int {
		t.Helper()
		t.Setenv("C4_OUTPUT", out)
		b := &PodBlock{Out: out, ConfigDir: cfg, Cwd: cwd, Claude: bin, HookCommand: "true", Env: os.Getenv, Logf: t.Logf}
		code, err := b.Run(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return code
	}

	// the first attempt: fresh, two parts, done
	outA, cfgA := t.TempDir(), t.TempDir()
	if code := run(outA, cfgA); code != PodBlockDone {
		t.Fatalf("first attempt exited %d", code)
	}
	si, err := ReadSessionInfo(outA)
	if err != nil {
		t.Fatal(err)
	}
	calls, _ := os.ReadFile(log)
	if want := "new " + si.Session + "\nresume " + si.Session + "\n"; string(calls) != want {
		t.Fatalf("calls %q, want %q", calls, want)
	}
	if b, _ := os.ReadFile(filepath.Join(outA, SessionDir, "transcript.jsonl")); strings.Count(string(b), "\n") != 2 {
		t.Fatalf("the view's transcript has %q", b)
	}

	// a later attempt from that view, elsewhere: the same session, resumed
	st := &memStore{}
	id, err := CaptureView(ctx, st, "practice", outA)
	if err != nil {
		t.Fatal(err)
	}
	outB, cfgB := filepath.Join(t.TempDir(), "out"), t.TempDir()
	if err := FetchView(ctx, st, "practice", id, outB); err != nil {
		t.Fatal(err)
	}
	if code := run(outB, cfgB); code != PodBlockDone {
		t.Fatalf("resumed attempt exited %d", code)
	}
	calls, _ = os.ReadFile(log)
	if lines := strings.Split(strings.TrimSpace(string(calls)), "\n"); lines[len(lines)-1] != "resume "+si.Session {
		t.Fatalf("the resumed attempt ran %q", lines[len(lines)-1])
	}
	tr := filepath.Join(cfgB, "projects", ProjectSlug(cwd), si.Session+".jsonl")
	if b, _ := os.ReadFile(tr); strings.Count(string(b), "\n") != 3 {
		t.Fatalf("B's transcript has %q", b)
	}
	sb := &Store{Dir: filepath.Join(outB, SessionDir, "agents")}
	m, err := sb.Get(si.Member)
	if err != nil {
		t.Fatal(err)
	}
	if m.Transcript != tr || m.Epoch != 1 {
		t.Fatalf("member on B: %+v", m)
	}
	if r, err := sb.Verify(m.Name); err != nil || r.Broken != 0 {
		t.Fatalf("the chain on B: %+v %v", r, err)
	}
}

// With a credential lease mounted, the pod's store reads its credentials
// file through a link to the lease's, so a renewal reaches it; a pod whose
// lease is missing does not start its block.
func TestPodBlockLinksItsLease(t *testing.T) {
	leaseDir, cfg := t.TempDir(), filepath.Join(t.TempDir(), "claude")
	b := &PodBlock{ConfigDir: cfg, LeaseDir: leaseDir}
	if err := b.linkLease(); err == nil {
		t.Fatal("linked a lease that is not there")
	}
	if err := os.WriteFile(filepath.Join(leaseDir, ".credentials.json"), []byte("first"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := b.linkLease(); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(leaseDir, ".credentials.json"), []byte("renewed"), 0o600)
	if got, _ := os.ReadFile(filepath.Join(cfg, ".credentials.json")); string(got) != "renewed" {
		t.Fatalf("the store reads %q", got)
	}
}

// A block in a pod that reports missing information ends its attempt with
// status 12 and the text as the container's termination message; its
// session is in its view for the attempt that resumes it.
func TestPodBlockNeedsInformation(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(bin, []byte(fakePrint), 0o755); err != nil {
		t.Fatal(err)
	}
	cwd, out, cfg := t.TempDir(), t.TempDir(), t.TempDir()
	term := filepath.Join(t.TempDir(), "termination-log")
	t.Setenv("FAKE_SLUG", ProjectSlug(cwd))
	t.Setenv("FAKE_LOG", filepath.Join(t.TempDir(), "calls"))
	t.Setenv("FAKE_NEEDS", `{"needs": "the delivery frame rate", "why": "the brief gives 24 and 25"}`)
	t.Setenv("C4_JOB", "port")
	t.Setenv("C4_UNIT_NAME", "notes")
	t.Setenv("C4_OUTPUT", out)
	t.Setenv("C4_TERMINATION_LOG", term)
	b := &PodBlock{Out: out, ConfigDir: cfg, Cwd: cwd, Claude: bin, HookCommand: "true", Env: os.Getenv, Logf: t.Logf}
	code, err := b.Run(context.Background())
	if err != nil || code != PodBlockNeedsInformation {
		t.Fatalf("exit %d, %v", code, err)
	}
	if got, _ := os.ReadFile(term); string(got) != "the delivery frame rate\nwhy: the brief gives 24 and 25" {
		t.Fatalf("termination message %q", got)
	}
	if _, err := ReadSessionInfo(out); err != nil {
		t.Fatalf("the session is not in its view: %v", err)
	}
}
