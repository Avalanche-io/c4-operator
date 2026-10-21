package queue

import (
	"os"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/Avalanche-io/c4-operator/work/sched"
)

// A new leader starts the day at what the projects spent on burst, not at
// nothing, so a restart cannot refill a daily budget; the next day starts
// at nothing.
func TestBurstSpendSurvivesARestart(t *testing.T) {
	ns, q, now := farm(t, "cpu", 1, 4)
	q.Namespace = ns
	tick(t, q)
	q.spentMu.Lock()
	q.spent["show"] = 12.5
	q.spentMu.Unlock()
	*now = now.Add(2 * time.Minute)
	tick(t, q)

	leader := func(at time.Time) *Queue {
		return &Queue{Client: k8s, Policy: sched.Decide, Cfg: sched.DefaultConfig(), Log: zap.New(zap.WriteTo(os.Stderr)),
			Now: func() time.Time { return at }, CaptureWait: time.Minute, Namespace: ns}
	}
	next := leader(now.Add(time.Minute))
	tick(t, next)
	if got := next.Spent("show"); got != 12.5 {
		t.Fatalf("after a restart show has spent %v today, want 12.5", got)
	}
	tomorrow := leader(now.Add(24 * time.Hour))
	tick(t, tomorrow)
	if got := tomorrow.Spent("show"); got != 0 {
		t.Fatalf("the next day show has spent %v, want 0", got)
	}
}
