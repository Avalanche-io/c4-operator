package farmbook_test

import (
	"context"
	"testing"
	"time"

	"github.com/Avalanche-io/c4-operator/internal/farmbook"
	"github.com/Avalanche-io/c4-operator/internal/provenance"
	"github.com/Avalanche-io/c4-operator/internal/provenance/provtest"
)

// agentChecker stands in for the people's issuer's checker: it admits an
// agent grant's entries as written.
type agentChecker struct{}

func (agentChecker) Kinds() []string {
	return []string{farmbook.KindAgentGrant, farmbook.KindAgentUse, farmbook.KindGrantEnd, "device"}
}
func (agentChecker) Admit(ctx context.Context, raw []byte, p farmbook.Placement) ([]byte, time.Time, error) {
	return deviceChecker{}.Admit(ctx, raw, p)
}

// An agent's grant is in force from its entry until its end is booked, or
// until AgentLapse has passed since the agent last used it, judged at
// each position by when it was sealed: a use keeps it, nothing else does,
// and nothing after its end or lapse brings it back. A use or an end
// booked before the grant counts for nothing.
func TestAgentGrantInForceUntilItsEndOrLapse(t *testing.T) {
	r := newRig(t)
	r.sealer.Checkers = func() []farmbook.Checker { return []farmbook.Checker{agentChecker{}} }
	// later moves the clock, with a queue certified then, as a farm's
	// queue renews its own.
	later := func(d time.Duration) {
		r.now = r.now.Add(d)
		r.farm.Now = r.now.Add(-time.Hour)
		r.sealer.Signer = signer(t, r.farm, provenance.ComponentIdentity("queue"))
	}
	write := func(kind, grant string) int64 {
		t.Helper()
		e := &farmbook.Entry{Kind: kind, Grant: grant, Order: farmbook.OrderPending, At: r.now.Format(time.RFC3339Nano)}
		if kind == "device" {
			e = &farmbook.Entry{Kind: kind, Device: provtest.ID(grant), Person: "noor", Order: farmbook.OrderPending, At: r.now.Format(time.RFC3339Nano)}
		}
		if _, err := r.writer.Append(r.ctx, farmbook.Submission{Entry: e}); err != nil {
			t.Fatal(err)
		}
		r.seal()
		return r.read().Head().Position
	}
	g, ended, early := provtest.ID("noor's grant to claude-code"), provtest.ID("a grant removed"), provtest.ID("a grant used before it was made")
	day := 24 * time.Hour
	p1 := write(farmbook.KindAgentGrant, g) // the grant
	later(89 * day)
	p2 := write("device", "a tick") // 89 days on, unused: in force
	later(2 * day)
	p3 := write("device", "a tick")     // 91 days on, unused: lapsed
	write(farmbook.KindGrantEnd, early) // an end before the grant
	write(farmbook.KindAgentUse, early) // a use before it
	pe := write(farmbook.KindAgentGrant, early)
	pg := write(farmbook.KindAgentGrant, ended)
	pu := write(farmbook.KindAgentUse, ended)
	pend := write(farmbook.KindGrantEnd, ended)
	later(day)
	pafter := write(farmbook.KindAgentUse, ended) // a use after its end
	b := r.read()
	for _, c := range []struct {
		what  string
		grant string
		pos   int64
		in    bool
	}{
		{"at its own position", g, p1, true}, {"89 days unused", g, p2, true}, {"91 days unused", g, p3, false},
		{"after it lapsed, whatever comes", g, pafter, false},
		{"a grant whose end and use came first", early, pe, true},
		{"before its end", ended, pu, true}, {"at its end", ended, pend, false}, {"used after its end", ended, pafter, false},
		{"before it was made", ended, pg - 1, false},
	} {
		on, err := b.InForce(farmbook.ForceAgentGrant, c.grant, c.pos)
		if err != nil || (on != nil) != c.in {
			t.Errorf("%s (position %d): in force %v (%v), want %v", c.what, c.pos, on != nil, err, c.in)
		}
	}
	at, _ := b.SealedAt(p1)
	if l, err := b.GrantLapses(g, p2); err != nil || !l.Equal(at.Add(farmbook.AgentLapse)) {
		t.Errorf("the grant lapses at %v (%v), want 90 days after it was sealed, %v", l, err, at.Add(farmbook.AgentLapse))
	}
}

// The agent's use renews its 90 days: a grant used on day 89 is in force
// on day 170, and lapses 90 days after that use. A pending grant counts
// in a view before the seal.
func TestAgentUseKeepsTheGrant(t *testing.T) {
	r := newRig(t)
	r.sealer.Checkers = func() []farmbook.Checker { return []farmbook.Checker{agentChecker{}} }
	later := func(d time.Duration) {
		r.now = r.now.Add(d)
		r.farm.Now = r.now.Add(-time.Hour)
		r.sealer.Signer = signer(t, r.farm, provenance.ComponentIdentity("queue"))
	}
	write := func(kind, grant string, seal bool) int64 {
		t.Helper()
		e := &farmbook.Entry{Kind: kind, Grant: grant, Order: farmbook.OrderPending, At: r.now.Format(time.RFC3339Nano)}
		if _, err := r.writer.Append(r.ctx, farmbook.Submission{Entry: e}); err != nil {
			t.Fatal(err)
		}
		if seal {
			r.seal()
		}
		return r.read().Head().Position
	}
	g := provtest.ID("dana's grant to an assistant")
	day := 24 * time.Hour
	write(farmbook.KindAgentGrant, g, true)
	later(89 * day)
	pu := write(farmbook.KindAgentUse, g, true)
	later(81 * day)
	p170 := write(farmbook.KindAgentUse, "another grant", true)
	b := r.read()
	if on, _ := b.InForce(farmbook.ForceAgentGrant, g, p170); on == nil {
		t.Error("a grant used on day 89, on day 170")
	}
	used, _ := b.SealedAt(pu)
	if l, _ := b.GrantLapses(g, p170); !l.Equal(used.Add(farmbook.AgentLapse)) {
		t.Errorf("it lapses at %v, want 90 days after its use, %v", l, used.Add(farmbook.AgentLapse))
	}
	later(10 * day)
	p180 := write(farmbook.KindAgentUse, "another grant", true)
	if on, _ := r.read().InForce(farmbook.ForceAgentGrant, g, p180); on != nil {
		t.Error("91 days after its last use, the grant is in force")
	}
	// Pending, in a view.
	fresh := provtest.ID("a grant not sealed yet")
	write(farmbook.KindAgentGrant, fresh, false)
	v, err := r.reader().View(r.ctx, r.now)
	if err != nil {
		t.Fatal(err)
	}
	if on, err := v.InForce(farmbook.ForceAgentGrant, fresh, v.Position()); err != nil || on == nil || !on.Pending {
		t.Errorf("a pending grant in a view: %v %v", on, err)
	}
	if l, err := v.GrantLapses(fresh, v.Position()); err != nil || !l.Equal(r.now.Add(farmbook.AgentLapse)) {
		t.Errorf("a pending grant lapses at %v (%v), want %v", l, err, r.now.Add(farmbook.AgentLapse))
	}
}

// On a quiet book, a grant judged at its last seal is in force, and judged
// at the clock 91 days on it has lapsed: a verifier passes the seal time,
// a live decision the clock, through the one rule.
func TestAgentGrantJudgedAtTheClock(t *testing.T) {
	r := newRig(t)
	r.sealer.Checkers = func() []farmbook.Checker { return []farmbook.Checker{agentChecker{}} }
	g := provtest.ID("a grant on a quiet book")
	e := &farmbook.Entry{Kind: farmbook.KindAgentGrant, Grant: g, Order: farmbook.OrderPending, At: r.now.Format(time.RFC3339Nano)}
	if _, err := r.writer.Append(r.ctx, farmbook.Submission{Entry: e}); err != nil {
		t.Fatal(err)
	}
	r.seal()
	b := r.read()
	head := b.Head().Position
	later := r.now.Add(91 * 24 * time.Hour)
	if on, err := b.InForce(farmbook.ForceAgentGrant, g, head); err != nil || on == nil {
		t.Errorf("judged at its seal: %v %v", on, err)
	}
	if on, err := b.InForceAt(farmbook.ForceAgentGrant, g, head, later); err != nil || on != nil {
		t.Errorf("judged at the clock 91 days on: %v %v", on, err)
	}
	if on, _ := b.InForceAt(farmbook.ForceAgentGrant, g, head, r.now.Add(89*24*time.Hour)); on == nil {
		t.Error("judged at the clock 89 days on")
	}
}
