package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
if [ -n "$FAKE_PROMPTS" ]; then printf '%s\n----\n' "$1" >> "$FAKE_PROMPTS"; fi
if [ -n "$FAKE_ASK" ] && [ "$(wc -l < "$FAKE_LOG")" -eq 1 ]; then
  printf '{"id":"0#1","to":"%s","at":"%s"}\n' "$FAKE_ASK_TO" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" > "$C4_OUTPUT/.c4/asking"; exit 0
fi
if [ -n "$FAKE_SLEEP" ]; then exec sleep "$FAKE_SLEEP"; fi
if [ -n "$FAKE_QUIET" ]; then exit 0; fi
if [ -n "$FAKE_WITHDRAW" ]; then echo "$FAKE_WITHDRAW" > "$C4_OUTPUT/.c4/withdraw"; exit 0; fi
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

// A block in a pod that asked another unit a question keeps its slot until
// the answer comes, then is woken with it; with no answer within its hold,
// a question to the job's answerer wakes it to decide within its brief.
func TestPodBlockHoldsForAnswer(t *testing.T) {
	run := func(t *testing.T, to, hold string, answer bool) (int, string, UnitDirs) {
		bin := filepath.Join(t.TempDir(), "claude")
		if err := os.WriteFile(bin, []byte(fakePrint), 0o755); err != nil {
			t.Fatal(err)
		}
		cwd, out, cfg, control := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
		prompts := filepath.Join(t.TempDir(), "prompts")
		t.Setenv("FAKE_SLUG", ProjectSlug(cwd))
		t.Setenv("FAKE_LOG", filepath.Join(t.TempDir(), "calls"))
		t.Setenv("FAKE_PROMPTS", prompts)
		t.Setenv("FAKE_ASK", "1")
		t.Setenv("FAKE_ASK_TO", to)
		t.Setenv("C4_ASK_HOLD", hold)
		t.Setenv("C4_JOB", "port")
		t.Setenv("C4_UNIT_NAME", "notes")
		t.Setenv("C4_OUTPUT", out)
		t.Setenv("C4_CONTROL", control)
		if answer {
			// the answer is there by the time the block holds for it
			os.WriteFile(filepath.Join(control, "steer"), []byte(`[{"seq":1,"at":"2026-10-20T06:00:00Z","text":"answer to 0#1 from plan: the lexer package","answer":{"id":"0#1","from":"plan"}}]`), 0o644)
		}
		b := &PodBlock{Out: out, ConfigDir: cfg, Cwd: cwd, Claude: bin, HookCommand: "true", Env: os.Getenv, Logf: t.Logf, AskPoll: 50 * time.Millisecond}
		code, err := b.Run(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		got, _ := os.ReadFile(prompts)
		return code, string(got), UnitDirs{Out: out, Control: control}
	}
	t.Run("answered", func(t *testing.T) {
		code, prompts, u := run(t, "unit:plan", "1m", true)
		if code != PodBlockDone || !strings.Contains(prompts, "was answered by plan:\nanswer to 0#1 from plan: the lexer package") {
			t.Fatalf("exit %d, prompts:\n%s", code, prompts)
		}
		if _, ok := ReadAsking(u.Out); ok {
			t.Fatal("the answered question is still open")
		}
		if b, _ := os.ReadFile(filepath.Join(u.Out, ".c4", "steer-seen")); strings.TrimSpace(string(b)) != "1" {
			t.Fatalf("the answer is not marked seen: %q", b)
		}
	})
	t.Run("unanswered", func(t *testing.T) {
		code, prompts, _ := run(t, "", "300ms", false)
		if code != PodBlockDone || !strings.Contains(prompts, "No answer to your question 0#1 came within 300ms") {
			t.Fatalf("exit %d, prompts:\n%s", code, prompts)
		}
	})
}

// A finished block woken to answer questions (C4_ANSWER_ONLY) is told to
// answer only, and its attempt completes when that turn ends, without a
// done of its own.
func TestPodBlockWokenToAnswer(t *testing.T) {
	ctx := context.Background()
	bin := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(bin, []byte(fakePrint), 0o755); err != nil {
		t.Fatal(err)
	}
	cwd := t.TempDir()
	prompts := filepath.Join(t.TempDir(), "prompts")
	t.Setenv("FAKE_SLUG", ProjectSlug(cwd))
	t.Setenv("FAKE_LOG", filepath.Join(t.TempDir(), "calls"))
	t.Setenv("C4_JOB", "port")
	t.Setenv("C4_UNIT_NAME", "notes")
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
	outA := t.TempDir()
	if code := run(outA, t.TempDir()); code != PodBlockDone {
		t.Fatalf("the block's own attempt exited %d", code)
	}
	st := &memStore{}
	id, err := CaptureView(ctx, st, "practice", outA)
	if err != nil {
		t.Fatal(err)
	}
	outB := filepath.Join(t.TempDir(), "out")
	if err := FetchView(ctx, st, "practice", id, outB); err != nil {
		t.Fatal(err)
	}
	t.Setenv("C4_ANSWER_ONLY", "2#1")
	t.Setenv("FAKE_QUIET", "1")
	t.Setenv("FAKE_PROMPTS", prompts)
	if code := run(outB, t.TempDir()); code != PodBlockDone {
		t.Fatalf("the wake to answer exited %d", code)
	}
	if b, _ := os.ReadFile(prompts); !strings.Contains(string(b), "Other units of this job asked you questions (2#1)") {
		t.Fatalf("prompt:\n%s", b)
	}
}

// An attempt resumed from a view does not inherit the open question of the
// attempt before it: that question's answer comes as this attempt's
// decision or as a message.
func TestPodBlockDropsAnInheritedQuestion(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(bin, []byte(fakePrint), 0o755); err != nil {
		t.Fatal(err)
	}
	cwd, out := t.TempDir(), t.TempDir()
	t.Setenv("FAKE_SLUG", ProjectSlug(cwd))
	calls := filepath.Join(t.TempDir(), "calls")
	t.Setenv("FAKE_LOG", calls)
	t.Setenv("C4_JOB", "port")
	t.Setenv("C4_UNIT_NAME", "notes")
	t.Setenv("C4_OUTPUT", out)
	t.Setenv("C4_ASK_HOLD", "10ms")
	os.MkdirAll(filepath.Join(out, ".c4"), 0o755)
	if err := WriteAsking(out, Asking{ID: "5#1", To: "unit:slow", At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	b := &PodBlock{Out: out, ConfigDir: t.TempDir(), Cwd: cwd, Claude: bin, HookCommand: "true", Env: os.Getenv, Logf: t.Logf, AskPoll: time.Millisecond}
	code, err := b.Run(context.Background())
	if err != nil || code != PodBlockDone {
		t.Fatalf("exit %d, %v", code, err)
	}
	// its own two parts, and no wake for a question it did not ask
	if b, _ := os.ReadFile(calls); strings.Count(string(b), "\n") != 2 {
		t.Fatalf("claude ran %q", b)
	}
}
