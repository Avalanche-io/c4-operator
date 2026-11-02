package people_test

import (
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Avalanche-io/c4-operator/internal/farmbook"
	"github.com/Avalanche-io/c4-operator/internal/provenance"
	"github.com/Avalanche-io/c4-operator/internal/provenance/people"
	"github.com/Avalanche-io/c4-operator/internal/provenance/provtest"
	"github.com/Avalanche-io/c4-operator/schema"
)

// The K5 review's probes (c4-accountability, 2026-11-01 19:06), each kept
// as a test that must refuse.

// studioBook is a studio farm with a farm book: dana (an admin) and noor
// (not), each with a laptop key and a touch key. The queue knows dana is
// an admin, as the manager's does from the farm binding.
func studioBook(t *testing.T) (*booked, *provtest.Key, *provtest.Key) {
	t.Helper()
	b := inBook(studio(t), nil)
	b.index.Now = func() time.Time { return b.now }
	b.index.Admin = b.iss.Admin
	checkers := b.sealer.Checkers
	b.sealer.Checkers = func() []farmbook.Checker {
		cs := checkers()
		for _, c := range cs {
			if pc, ok := c.(*people.BookChecker); ok {
				pc.WithAdmin(b.iss.Admin)
			}
		}
		return cs
	}
	enrol := func(person, sub string) *provtest.Key {
		laptop, touch := newDev(t), newDev(t)
		k := b.key(laptop, b.signIn(laptop, person, sub))
		e := b.enrolment(touch, person, laptop.id, "mac")
		e.IDToken = b.sso.Token(b.now, sub, touch.id, map[string]any{"auth_time": b.now.Unix(), "amr": []string{"hwk", "pin"}})
		var tc people.Certified
		if code, body := b.call("POST", "/v1/people/enroll", "", e, &tc); code != http.StatusOK {
			t.Fatalf("touch key for %s: %d %s", person, code, body)
		}
		k.Touch = b.key(touch, &tc)
		return k
	}
	dana := enrol("dana", danaSub)
	noor := enrol("noor", noorSub)
	b.seal()
	return b, dana, noor
}

func (b *booked) certifyAgent(agent string, d *dev) (int, string, *people.AgentCertified) {
	a := &agents{booked: b, agent: d}
	return a.certify(agent, d, d)
}

// submit writes e as the issuer writes, signed by it, so the queue's
// refusal is for what the entry says.
func (b *booked) submit(e people.Entry) {
	b.t.Helper()
	e.Farm, e.Order, e.At = b.farm.ID, people.OrderPending, b.now.Format(time.RFC3339Nano)
	b.asIssuerAt(&e)
	st, err := b.ptr.Read(context.Background())
	if err != nil {
		b.t.Fatal(err)
	}
	if _, err := farmbook.Submit(context.Background(), b.store, b.ptr, st, []farmbook.Submission{{Entry: e}}); err != nil {
		b.t.Fatal(err)
	}
}

// store keeps a record in the book's store and returns its C4 ID.
func (b *booked) keep(rec []byte) string {
	b.t.Helper()
	id := provtest.ID(string(rec))
	if err := b.store.Put(context.Background(), id, rec); err != nil {
		b.t.Fatal(err)
	}
	return id
}

// Probe: dana grants noor's agent's key, which is public in the book.
// Refused at the issuer and at the seal: the key stays noor's agent's.
func TestGrantOfAnotherPersonsAgentKeyRefused(t *testing.T) {
	b, dana, noor := studioBook(t)
	agent := newDev(t)
	params := map[string]string{"agent": assistant, "device": agent.id}
	if code, body, _ := b.book(b.act(noor, "noor", people.ActGrant, deviceSubject(agent.id), params, true), ""); code != http.StatusOK {
		t.Fatalf("noor's grant: %d %s", code, body)
	}
	danas := b.act(dana, "dana", people.ActGrant, deviceSubject(agent.id), params, true)
	if code, body, _ := b.book(danas, ""); code != http.StatusForbidden || !strings.Contains(body, "another person's agent key") {
		t.Errorf("dana's grant of noor's agent's key: %d %s", code, body)
	}
	if _, _, c := b.certifyAgent(assistant, agent); c.Person != "noor" {
		t.Errorf("the agent's key is certified for %q", c.Person)
	}
	b.seal()
	id := b.keep(danas)
	b.submit(people.Entry{Kind: people.KindAgentGrant, Record: id, Grant: id, Agent: assistant, Device: agent.id, Person: "dana", By: "dana"})
	if res := refusals(b.seal()); len(res) != 1 || res[0].Reason != farmbook.ReasonRejected {
		t.Errorf("the seal of dana's grant of noor's agent's key: %+v", res)
	}
}

// Probe: noor revokes dana's grant, and is neither its person nor an
// admin. The issuer refuses it, and so does the seal; an admin's revoke
// of another person's grant is sealed.
func TestRevokeByNeitherThePersonNorAnAdminRefused(t *testing.T) {
	b, dana, noor := studioBook(t)
	revokeOf := func(k *provtest.Key, person, grant string) []byte {
		return b.act(k, person, people.ActRevoke, provenance.Subject{Name: "grant:" + grant, Digest: map[string]string{"c4": grant}}, map[string]string{}, false)
	}
	agent := newDev(t)
	_, _, gb := b.book(b.act(dana, "dana", people.ActGrant, deviceSubject(agent.id), map[string]string{"agent": assistant, "device": agent.id}, true), "")
	grant := gb.Record
	b.seal()
	rv := revokeOf(noor, "noor", grant)
	if code, body, _ := b.book(rv, ""); code != http.StatusForbidden {
		t.Errorf("noor's revoke of dana's grant at the issuer: %d %s", code, body)
	}
	b.submit(people.Entry{Kind: people.KindGrantEnd, End: people.EndRemoved, Record: b.keep(rv), Grant: grant, Agent: assistant, Device: agent.id, Person: "dana", By: "noor"})
	if res := refusals(b.seal()); len(res) != 1 || res[0].Reason != farmbook.ReasonWrongSigner {
		t.Errorf("the seal of noor's revoke of dana's grant: %+v", res)
	}
	// dana, an admin, revokes noor's agent's grant: sealed.
	other := newDev(t)
	_, _, nb := b.book(b.act(noor, "noor", people.ActGrant, deviceSubject(other.id), map[string]string{"agent": assistant, "device": other.id}, true), "")
	if code, body, _ := b.book(revokeOf(dana, "dana", nb.Record), ""); code != http.StatusOK {
		t.Fatalf("an admin's revoke: %d %s", code, body)
	}
	if res := refusals(b.seal()); len(res) != 0 {
		t.Errorf("the seal of an admin's revoke: %+v", res)
	}
}

// Probe: a use dated 100 days after the grant, with none between, is
// refused at the seal, and the grant isn't in force after it.
func TestUseAfterTheGrantLapsedRefusedAtTheSeal(t *testing.T) {
	a := agentFarm(t)
	grant := a.granted()
	a.seal()
	a.later(100 * 24 * time.Hour)
	leaf, err := a.farm.CA.IssueLeaf(&a.agent.key.PublicKey, provenance.AgentIdentity(assistant), a.agent.id, grant, time.Hour, a.now.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	c := &people.AgentCertified{AgentGrant: people.AgentGrant{Grant: grant}, Identity: provenance.CertIdentity(leaf), Chain: [][]byte{leaf.Raw, a.farm.CA.Cert.Raw}}
	_, act := a.agentAct(c)
	a.submit(people.Entry{Kind: people.KindAgentUse, Record: a.keep(act), Grant: grant, Agent: assistant, Device: a.agent.id, Person: "noor"})
	if res := refusals(a.seal()); len(res) != 1 || res[0].Reason != farmbook.ReasonNotInForce {
		t.Errorf("a use 100 days after the grant: %+v", res)
	}
	bk := a.fbook()
	if on, _ := bk.InForceAt(farmbook.ForceAgentGrant, grant, bk.Head().Position, a.now); on != nil {
		t.Error("the grant is in force again")
	}
}

// Probe: after a renewal (a second touched grant of the same key to the
// same agent), revoking the grant in use leaves none: the renewal ended
// the first, booked, so nothing falls back to it. A grant booked while an
// older one of the same person, key and agent is live is refused at the
// seal.
func TestRevokingTheGrantInUseFallsBackToNone(t *testing.T) {
	a := agentFarm(t)
	g1 := a.granted()
	a.later(time.Hour)
	g2 := a.granted()
	ends := a.pending(people.KindGrantEnd)
	if len(ends) != 1 || ends[0].Entry.Grant != g1 {
		t.Fatalf("the renewal booked %d ends", len(ends))
	}
	if code, body, c := a.certify(assistant, a.agent, a.agent); code != http.StatusOK || c.Grant != g2 {
		t.Fatalf("certified under %.12s: %d %s", c.Grant, code, body)
	}
	rv := a.act(a.noor, "noor", people.ActRevoke, provenance.Subject{Name: "grant:" + g2, Digest: map[string]string{"c4": g2}}, map[string]string{}, false)
	if code, body, _ := a.book(rv, ""); code != http.StatusOK {
		t.Fatalf("revoke g2: %d %s", code, body)
	}
	if code, body, _ := a.certify(assistant, a.agent, a.agent); code != http.StatusForbidden {
		t.Errorf("after revoking the grant in use: %d %s", code, body)
	}
	if res := refusals(a.seal()); len(res) != 0 {
		t.Fatalf("the seal: %+v", res)
	}
	for _, e := range a.entries(people.KindGrantEnd) {
		if e.Grant == g1 && e.End != people.EndReplaced || e.Grant == g2 && e.End != people.EndRemoved {
			t.Errorf("grant %.12s ended %q", e.Grant, e.End)
		}
	}
	// A third grant, booked without ending the live one first: refused.
	g3 := a.granted()
	a.seal()
	act := a.grant(assistant, a.agent, true)
	id := a.keep(act)
	a.submit(people.Entry{Kind: people.KindAgentGrant, Record: id, Grant: id, Agent: assistant, Device: a.agent.id, Person: "noor", By: "noor"})
	if res := refusals(a.seal()); len(res) != 1 || res[0].Reason != farmbook.ReasonDisagrees {
		t.Errorf("a grant while %.12s is live: %+v", g3, res)
	}
}

// A key granted to an agent is never enrolled as a person's device, at
// the issuer or at the seal.
func TestAgentKeyNeverEnrolledAsADevice(t *testing.T) {
	b, _, noor := studioBook(t)
	agent := newDev(t)
	if code, body, _ := b.book(b.act(noor, "noor", people.ActGrant, deviceSubject(agent.id), map[string]string{"agent": assistant, "device": agent.id}, true), ""); code != http.StatusOK {
		t.Fatalf("noor's grant: %d %s", code, body)
	}
	e := b.enrolment(agent, "noor", "", "mac")
	e.IDToken = b.sso.Token(b.now, noorSub, agent.id, nil)
	if code, body := b.call("POST", "/v1/people/enroll", "", e, nil); code != http.StatusForbidden || !strings.Contains(body, "granted to an agent") {
		t.Errorf("enrolling the agent's key as noor's device: %d %s", code, body)
	}
}

// asIssuerAt is asIssuer with the issuer's leaf valid at b.now, however
// far the test moved the clock.
func (b *booked) asIssuerAt(e *people.Entry) {
	b.t.Helper()
	s, err := provenance.NewSigner(context.Background(), func(_ context.Context, pub ed25519.PublicKey) ([]*x509.Certificate, error) {
		c, err := b.farm.CA.Issue(pub, provenance.ComponentIdentity("issuer"), 48*time.Hour, b.now.Add(-time.Hour))
		return []*x509.Certificate{c, b.farm.CA.Cert}, err
	})
	if err != nil {
		b.t.Fatal(err)
	}
	c := *e
	c.Sig, c.Position, c.Prev, c.Order, c.Farm = nil, 0, "", people.OrderPending, b.farm.ID
	raw, _ := json.Marshal(&c)
	canon, _ := schema.Canonical(raw)
	sig, err := provenance.SignStatement(context.Background(), s, provenance.PredicateBookEntry,
		[]provenance.Subject{{Name: "book-entry:" + e.Kind, Digest: map[string]string{"c4": provtest.ID(string(canon))}}}, map[string]string{"farm": b.farm.ID, "kind": e.Kind}, b.now)
	if err != nil {
		b.t.Fatal(err)
	}
	e.Sig, _ = schema.Canonical(sig)
}

// Refused agent certificate requests from one address are answered 429,
// without reading the book, past AgentRefusalsPerMinute in a minute; a
// minute later they are answered again.
func TestRefusedAgentCertificatesRateLimited(t *testing.T) {
	a := agentFarm(t)
	for n := 0; n < people.AgentRefusalsPerMinute; n++ {
		if code, body, _ := a.certify(assistant, a.agent, a.agent); code != http.StatusForbidden {
			t.Fatalf("refusal %d: %d %s", n+1, code, body)
		}
	}
	if code, _, _ := a.certify(assistant, a.agent, a.agent); code != http.StatusTooManyRequests {
		t.Errorf("past %d refusals in a minute: %d, want 429", people.AgentRefusalsPerMinute, code)
	}
	a.now = a.now.Add(time.Minute)
	a.granted()
	if code, body, _ := a.certify(assistant, a.agent, a.agent); code != http.StatusOK {
		t.Errorf("a minute later, granted: %d %s", code, body)
	}
}

// A replacement ends only a grant of the same person, key and agent as
// the new grant it books: one booking noor's grant of her key to another
// agent, written to end her grant to the assistant, is refused at the
// seal.
func TestReplacementOfAnotherAgentsGrantRefused(t *testing.T) {
	a := agentFarm(t)
	grant := a.granted()
	a.seal()
	act := a.grant("reviewer", a.agent, true)
	a.submit(people.Entry{Kind: people.KindGrantEnd, End: people.EndReplaced, Record: a.keep(act), Grant: grant, Agent: "reviewer", Device: a.agent.id, Person: "noor", By: "noor"})
	if res := refusals(a.seal()); len(res) != 1 || res[0].Reason != farmbook.ReasonDisagrees {
		t.Errorf("a replacement by a grant to another agent: %+v", res)
	}
}
