package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
)

func recheckPod(extra ...corev1.EnvVar) *corev1.Pod {
	p := hostPod()
	c := &p.Spec.Containers[0]
	c.Env = append(c.Env, corev1.EnvVar{Name: EnvRecheck, Value: "1"},
		corev1.EnvVar{Name: "C4_NEEDED", Value: "the delivery frame rate\nwhy: the brief gives 24 and 25"},
		corev1.EnvVar{Name: EnvRecheckWhy, Value: "unit schema succeeded"})
	c.Env = append(c.Env, extra...)
	return p
}

func writeControl(t *testing.T, u UnitDirs, name, doc string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(u.Out, ".c4", name), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A re-check's prompt gives the question and what landed, and asks for a
// decision only; its turn ends withdrawn (with the why), still needed with
// sharper text, or still needed as it was when the block decided nothing
// or wrote a withdrawal it could not mean. A withdrawal wins over a stale
// needs.json.
func TestRecheckPromptAndEnd(t *testing.T) {
	p := recheckPod()
	u := UnitDirsAt(t.TempDir())
	os.MkdirAll(filepath.Join(u.Out, ".c4"), 0o755)
	got := reworkPrompt(p, u)
	for _, want := range []string{"the delivery frame rate", "Since then: unit schema succeeded.", "decide one thing: does your question still apply?",
		"write withdraw in " + u.Out + "/.c4/", "change nothing, and do not resume your task"} {
		if !strings.Contains(got, want) {
			t.Errorf("the re-check prompt lacks %q:\n%s", want, got)
		}
	}
	old := "the delivery frame rate\nwhy: the brief gives 24 and 25"
	for _, c := range []struct {
		name, withdraw, needs string
		withdrawn             bool
		text                  string
	}{
		{"withdrawn", `{"why": "the schema unit fixed 24 fps"}`, `{"needs": "stale"}`, true, "the schema unit fixed 24 fps"},
		{"sharper", "", `{"needs": "24 or 25 fps for the delivery", "about": ["unit:conform", "plates/"]}`, false,
			"24 or 25 fps for the delivery\nabout: unit:conform, path:plates/"},
		{"silent", "", "", false, old},
		{"no why", `{"why": ""}`, "", false, old},
	} {
		if c.withdraw != "" {
			writeControl(t, u, "withdraw", c.withdraw)
		}
		if c.needs != "" {
			writeControl(t, u, "needs.json", c.needs)
		}
		w, text, note := recheckEnd(p, u)
		if w != c.withdrawn || text != c.text {
			t.Errorf("%s: withdrawn %v %q (%s), want %v %q", c.name, w, text, note, c.withdrawn, c.text)
		}
		for _, f := range []string{"withdraw", "needs.json"} {
			if _, err := os.Stat(filepath.Join(u.Out, ".c4", f)); err == nil {
				t.Errorf("%s: %s left behind", c.name, f)
			}
		}
	}
	if d := recheckLimit(p); d != 300*time.Second {
		t.Errorf("default limit %v", d)
	}
	if d := recheckLimit(recheckPod(corev1.EnvVar{Name: EnvRecheckSeconds, Value: "45"})); d != 45*time.Second {
		t.Errorf("limit %v", d)
	}
}

// On a host, a re-check that withdrew its question ends Succeeded with the
// why; one that did not ends NeedsInformation with the text to keep.
func TestHostRecheckDone(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		withdraw    string
		phase       corev1.PodPhase
		reason, msg string
	}{
		{`{"why": "the lead's schema fixes 24 fps"}`, corev1.PodSucceeded, "Withdrawn", "the lead's schema fixes 24 fps"},
		{"", corev1.PodFailed, "NeedsInformation", "the delivery frame rate\nwhy: the brief gives 24 and 25"},
	} {
		e := newEnv(t)
		m := e.register("lead", sess)
		r, cl := hostRunner(t, e, recheckPod())
		p := getPod(t, cl)
		u, err := r.prepareUnit(p, m)
		if err != nil {
			t.Fatal(err)
		}
		if c.withdraw != "" {
			writeControl(t, u, "withdraw", c.withdraw)
		}
		if err := r.recheckDone(ctx, p, m, u); err != nil {
			t.Fatal(err)
		}
		got := getPod(t, cl)
		if got.Status.Phase != c.phase || got.Status.Reason != c.reason || got.Status.Message != c.msg {
			t.Errorf("pod %s %s %q, want %s %s %q", got.Status.Phase, got.Status.Reason, got.Status.Message, c.phase, c.reason, c.msg)
		}
	}
}

// The attempt after a withdrawal hears its question and why it was
// withdrawn; a needs.json names what it is about, checked.
func TestWithdrawnPromptAndAbout(t *testing.T) {
	p := hostPod()
	c := &p.Spec.Containers[0]
	c.Env = append(c.Env, corev1.EnvVar{Name: "C4_NEEDED", Value: "the delivery frame rate"},
		corev1.EnvVar{Name: EnvWithdrawn, Value: "the lead's schema fixes 24 fps"})
	got := reworkPrompt(p, UnitDirsAt(t.TempDir()))
	for _, want := range []string{"your brief lacked:\nthe delivery frame rate", "That question was withdrawn: the lead's schema fixes 24 fps"} {
		if !strings.Contains(got, want) {
			t.Errorf("the prompt lacks %q:\n%s", want, got)
		}
	}
	u := UnitDirsAt(t.TempDir())
	os.MkdirAll(filepath.Join(u.Out, ".c4"), 0o755)
	writeControl(t, u, "needs.json", `{"needs": "x", "about": ["job:show"]}`)
	if _, ok, err := takeNeeds(u); !ok || err == nil || !strings.Contains(err.Error(), "namespace/name") {
		t.Errorf("a job named without its namespace: %v %v", ok, err)
	}
}

// A block in a pod re-checks in one turn of its resumed session: it
// withdraws (exit 0, the why as its message), keeps its question (exit 12,
// the new text), decides nothing (exit 12, the old text), or runs past its
// time (exit 12, the old text).
func TestPodBlockRechecks(t *testing.T) {
	ctx := context.Background()
	bin := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(bin, []byte(fakePrint), 0o755); err != nil {
		t.Fatal(err)
	}
	cwd := t.TempDir()
	term := filepath.Join(t.TempDir(), "termination-log")
	prompts := filepath.Join(t.TempDir(), "prompts")
	t.Setenv("FAKE_SLUG", ProjectSlug(cwd))
	t.Setenv("FAKE_LOG", filepath.Join(t.TempDir(), "calls"))
	t.Setenv("C4_JOB", "port")
	t.Setenv("C4_UNIT_NAME", "notes")
	t.Setenv("C4_TERMINATION_LOG", term)
	run := func(out string) int {
		t.Helper()
		t.Setenv("C4_OUTPUT", out)
		os.Remove(term)
		b := &PodBlock{Out: out, ConfigDir: t.TempDir(), Cwd: cwd, Claude: bin, HookCommand: "true", Env: os.Getenv, Logf: t.Logf}
		code, err := b.Run(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return code
	}
	// the attempt that stopped for information, and its view
	t.Setenv("FAKE_NEEDS", `{"needs": "the delivery frame rate", "about": ["unit:schema"]}`)
	outA := t.TempDir()
	if code := run(outA); code != PodBlockNeedsInformation {
		t.Fatalf("the first attempt exited %d", code)
	}
	if b, _ := os.ReadFile(term); string(b) != "the delivery frame rate\nabout: unit:schema" {
		t.Fatalf("termination message %q", b)
	}
	st := &memStore{}
	id, err := CaptureView(ctx, st, "practice", outA)
	if err != nil {
		t.Fatal(err)
	}
	fromView := func() string {
		t.Helper()
		out := filepath.Join(t.TempDir(), "out")
		if err := FetchView(ctx, st, "practice", id, out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	t.Setenv(EnvRecheck, "1")
	t.Setenv("C4_NEEDED", "the delivery frame rate\nabout: unit:schema")
	t.Setenv(EnvRecheckWhy, "unit schema succeeded")
	t.Setenv("FAKE_PROMPTS", prompts)
	for _, c := range []struct {
		name string
		env  map[string]string
		code int
		msg  string
	}{
		{"withdraws", map[string]string{"FAKE_NEEDS": "", "FAKE_WITHDRAW": `{"why": "schema fixes 24 fps"}`}, PodBlockDone, "schema fixes 24 fps"},
		{"keeps it", map[string]string{"FAKE_NEEDS": `{"needs": "24 or 25 for the delivery"}`}, PodBlockNeedsInformation, "24 or 25 for the delivery"},
		{"decides nothing", map[string]string{"FAKE_NEEDS": "", "FAKE_QUIET": "1"}, PodBlockNeedsInformation, "the delivery frame rate\nabout: unit:schema"},
		{"past its time", map[string]string{"FAKE_NEEDS": "", "FAKE_SLEEP": "20", EnvRecheckSeconds: "1"}, PodBlockNeedsInformation, "the delivery frame rate\nabout: unit:schema"},
	} {
		for _, k := range []string{"FAKE_WITHDRAW", "FAKE_QUIET", "FAKE_SLEEP", EnvRecheckSeconds} {
			t.Setenv(k, "")
		}
		for k, v := range c.env {
			t.Setenv(k, v)
		}
		began := time.Now()
		if code := run(fromView()); code != c.code {
			t.Errorf("%s: exit %d, want %d", c.name, code, c.code)
		}
		if b, _ := os.ReadFile(term); string(b) != c.msg {
			t.Errorf("%s: termination message %q, want %q", c.name, b, c.msg)
		}
		if d := time.Since(began); d > 10*time.Second {
			t.Errorf("%s: took %v", c.name, d)
		}
	}
	if b, _ := os.ReadFile(prompts); !strings.Contains(string(b), "Since then: unit schema succeeded.") {
		t.Errorf("prompts:\n%s", b)
	}
	if n := strings.Count(string(mustRead(t, prompts)), "----"); n != 4 {
		t.Errorf("%d turns in four re-checks", n)
	}
}

func mustRead(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
