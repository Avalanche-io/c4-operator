package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"

	"github.com/Avalanche-io/c4-operator/internal/node"
)

// An idle member with messages it has not read is woken: its idle session
// is stopped at its turn boundary and resumed by its ID with the messages
// (a steer, a question to it with how to answer, an answer to its own), and
// they are marked read, so it is woken once. A member mid-turn, or with
// background work open, is not woken; it reads them when it is idle.
func TestHostWakesAnIdleMember(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	start := e.clock
	m := e.register("lead", sess)
	r, c := hostRunner(t, e, hostPod())
	r.since = map[types.UID]time.Time{}
	p := getPod(t, c)
	u, err := r.prepareUnit(p, m)
	if err != nil {
		t.Fatal(err)
	}
	msgs := []SteerMessage{
		{Seq: 1, Actor: "joshua", Text: "start round 7: the bundle endpoint"},
		{Seq: 2, Actor: "unit:c4-sched", Text: "which pool serves agents on this machine?", Ask: &AskRef{ID: "4#2", From: "unit:c4-sched"}},
		{Seq: 3, Text: "use the team pool", Answer: &AskRef{ID: "0#1", From: "unit:income"}},
	}
	b, _ := json.Marshal(msgs)
	if err := os.WriteFile(filepath.Join(u.Control, "steer"), b, 0o644); err != nil {
		t.Fatal(err)
	}

	e.fake.active[sess] = Active{SessionID: sess, Status: "busy", Kind: "background", StartedAt: start.UnixMilli()}
	e.fake.busyFor[sess] = 5
	if woke, err := r.wakeOnMessages(ctx, p, m, u); woke || err != nil || len(e.fake.stops) != 0 {
		t.Fatalf("woke a member mid-turn: %v %v", woke, err)
	}
	delete(e.fake.busyFor, sess)
	e.fake.active[sess] = Active{SessionID: sess, Status: "idle", Kind: "background", StartedAt: start.UnixMilli()}
	e.transcript(sess, launchCommand("toolu_c", "bsuite", start.Add(time.Minute)))
	if woke, err := r.wakeOnMessages(ctx, p, m, u); woke || err != nil || len(e.fake.stops) != 0 {
		t.Fatalf("woke a member over its background work: %v %v", woke, err)
	}
	e.transcript(sess, notified("toolu_c", "bsuite"))

	woke, err := r.wakeOnMessages(ctx, p, m, u)
	if !woke || err != nil || len(e.fake.stops) != 1 {
		t.Fatalf("not woken: %v %v, %d stops", woke, err, len(e.fake.stops))
	}
	prompt := e.fake.resumes[len(e.fake.resumes)-1].prompt
	for _, want := range []string{"from joshua: start round 7", "a question from unit:c4-sched (4#2): which pool",
		"the answer to your question 0#1, from unit:income: use the team pool", "c4-unit answer --dir " + u.Out + " --ask <its id>"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the prompt lacks %q:\n%s", want, prompt)
		}
	}
	if n, _ := node.ReadSteerSeen(u.Out); n != 3 {
		t.Errorf("steer-seen %d", n)
	}
	e.fake.active[sess] = Active{SessionID: sess, Status: "idle", Kind: "background", StartedAt: e.clock.UnixMilli()}
	if woke, _ := r.wakeOnMessages(ctx, p, m, u); woke {
		t.Error("woken twice for the same messages")
	}
}
