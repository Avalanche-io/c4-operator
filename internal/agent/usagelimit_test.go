package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
)

// limitHit is the record Claude Code writes when a turn hits a usage limit.
func limitHit(text string, at time.Time) string {
	return fmt.Sprintf(`{"type":"assistant","timestamp":%q,"isApiErrorMessage":true,"error":"rate_limit","apiErrorStatus":429,"message":{"role":"assistant","model":"<synthetic>","content":[{"type":"text","text":%q}]}}`+"\n",
		at.UTC().Format(time.RFC3339Nano), text)
}

// The CLI's words for each limit, and when each resets.
func TestUsageLimitReset(t *testing.T) {
	la, _ := time.LoadLocation("America/Los_Angeles")
	at := time.Date(2026, 9, 20, 23, 30, 0, 0, la)
	for _, c := range []struct {
		text, kind string
		resets     time.Time
	}{
		{"You've hit your session limit · resets 1:10am (America/Los_Angeles)", "session", time.Date(2026, 9, 21, 1, 10, 0, 0, la)},
		{"You've hit your session limit · resets 11:50pm (America/Los_Angeles)", "session", time.Date(2026, 9, 20, 23, 50, 0, 0, la)},
		{"You've hit your weekly limit · resets Sep 27 at 12pm (America/Los_Angeles)", "weekly", time.Date(2026, 9, 27, 12, 0, 0, 0, la)},
		{"You've hit your weekly limit · resets 12pm (America/Los_Angeles)", "weekly", time.Date(2026, 9, 21, 12, 0, 0, 0, la)},
		{"You've hit your monthly spend limit · raise it at claude.ai/settings/usage", "spend", time.Time{}},
		{"You've reached your Fable limit. Run /usage-credits to continue or switch models with /model.", "model", time.Time{}},
	} {
		got, _ := resetTime(c.text, at)
		if k := limitKind(c.text); k != c.kind || !got.Equal(c.resets) {
			t.Errorf("%q: %s %v, want %s %v", c.text, k, got, c.kind, c.resets)
		}
	}
}

// A member's turn that ends at a usage limit holds the machine's slots at 0
// until a minute past the reset, with the limit as the node's capacity
// reason; then they come back. The transcript is read from where the last
// look stopped, so a limit is noted once.
func TestUsageLimitHoldsTheMachine(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	m := e.register("lead", sess)
	now := time.Date(2026, 10, 20, 20, 0, 0, 0, time.UTC)
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "host", Labels: map[string]string{c4v1.LabelSlotLimit: "4"}}}
	r, c := hostRunner(t, e, hostPod())
	if err := c.Create(ctx, node); err != nil {
		t.Fatal(err)
	}
	r.Slots, r.Now, r.limitRead = 4, func() time.Time { return now }, map[string]int64{}
	var logs []string
	r.Logf = func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }

	e.transcript(sess, limitHit("You've hit your session limit · resets 1:10pm (America/Los_Angeles)", now.Add(-time.Minute)))
	r.noteUsageLimits(m)
	slots := func() (string, string) {
		t.Helper()
		var n corev1.Node
		if err := c.Get(ctx, types.NamespacedName{Name: "host"}, &n); err != nil {
			t.Fatal(err)
		}
		if err := r.capacity(ctx, &n); err != nil {
			t.Fatal(err)
		}
		c.Get(ctx, types.NamespacedName{Name: "host"}, &n)
		return n.Labels[c4v1.LabelSlotLimit], n.Annotations[c4v1.AnnotationCapacityReason]
	}
	if l, why := slots(); l != "0" || !strings.Contains(why, "usage limit: You've hit your session limit") || !strings.Contains(why, "until 2026-10-20T20:10:00Z") {
		t.Fatalf("slots %s %q", l, why)
	}
	r.noteUsageLimits(m)
	if n := strings.Count(strings.Join(logs, "\n"), "hit a session usage limit"); n != 1 {
		t.Errorf("noted %d times", n)
	}
	now = now.Add(11 * time.Minute) // past the reset and its minute
	r.RaiseAfter = time.Nanosecond
	slots()
	now = now.Add(time.Second)
	if l, why := slots(); l != "4" || why != "" {
		t.Fatalf("after the reset: slots %s %q", l, why)
	}
	// a limit with no reset time holds for the probe interval
	e.transcript(sess, limitHit("You've hit your monthly spend limit · raise it at claude.ai/settings/usage", now))
	r.noteUsageLimits(m)
	if held, _ := r.limitHold(); !held || !r.limitUntil.Equal(now.Add(defaultLimitProbe)) {
		t.Fatalf("spend limit: %v until %v", held, r.limitUntil)
	}
}

// In a pod, a turn that hits a usage limit ends the attempt with exit 75
// and the limit as its message: not the block giving up.
func TestPodBlockStopsAtAUsageLimit(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(bin, []byte(fakePrint), 0o755); err != nil {
		t.Fatal(err)
	}
	cwd, out := t.TempDir(), t.TempDir()
	term := filepath.Join(t.TempDir(), "termination-log")
	t.Setenv("FAKE_SLUG", ProjectSlug(cwd))
	t.Setenv("FAKE_LOG", filepath.Join(t.TempDir(), "calls"))
	t.Setenv("FAKE_LIMIT", "You've hit your session limit · resets 1:10am (America/Los_Angeles)")
	t.Setenv("C4_JOB", "port")
	t.Setenv("C4_UNIT_NAME", "notes")
	t.Setenv(EnvBlock, "1")
	t.Setenv(EnvBlockPrompt, "Write notes.")
	t.Setenv("C4_OUTPUT", out)
	t.Setenv("C4_TERMINATION_LOG", term)
	b := &PodBlock{Out: out, ConfigDir: t.TempDir(), Cwd: cwd, Claude: bin, HookCommand: "true", Env: os.Getenv, Logf: t.Logf}
	code, err := b.Run(context.Background())
	if err != nil || code != PodBlockUsageLimit {
		t.Fatalf("exit %d, %v", code, err)
	}
	if got, _ := os.ReadFile(term); !strings.HasPrefix(string(got), "usage limit: You've hit your session limit") || !strings.Contains(string(got), "(until ") {
		t.Fatalf("termination message %q", got)
	}
}
