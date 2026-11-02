package people_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"errors"
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

// agents is noor's one-person farm with a farm book, her CLI key and
// touch key enrolled as c4 farm up enrols them, and an agent's key.
type agents struct {
	*booked
	noor  *provtest.Key
	agent *dev
}

const assistant = "claude-code"

func agentFarm(t *testing.T) *agents {
	t.Helper()
	b := inBook(onePerson(t), nil)
	b.index.Now = func() time.Time { return b.now }
	cli, touch := newDev(t), newDev(t)
	code, body, out := b.up(cli, touch)
	if code != http.StatusOK {
		t.Fatalf("up: %d %s", code, body)
	}
	k := b.key(cli, out.Owner)
	k.Touch = b.key(touch, out.Touch)
	b.seal()
	return &agents{booked: b, noor: k, agent: newDev(t)}
}

// later moves the farm's clock by d, with a queue certified then, as a
// farm's queue renews its own certificate.
func (a *agents) later(d time.Duration) {
	a.t.Helper()
	a.now = a.now.Add(d)
	q, err := provenance.NewSigner(context.Background(), func(_ context.Context, pub ed25519.PublicKey) ([]*x509.Certificate, error) {
		c, err := a.farm.CA.Issue(pub, provenance.ComponentIdentity("queue"), 48*time.Hour, a.now.Add(-time.Hour))
		return []*x509.Certificate{c, a.farm.CA.Cert}, err
	})
	if err != nil {
		a.t.Fatal(err)
	}
	a.sealer.Signer = q
}

// grant is noor's grant of the agent's key, confirmed unless signed.
func (a *agents) grant(agent string, d *dev, confirmed bool) []byte {
	return a.act(a.noor, "noor", people.ActGrant, deviceSubject(d.id), map[string]string{"agent": agent, "device": d.id}, confirmed)
}

// granted books noor's touched grant of the agent's key and returns the
// grant's ID.
func (a *agents) granted() string {
	a.t.Helper()
	code, body, b := a.book(a.grant(assistant, a.agent, true), "")
	if code != http.StatusOK || b.Entry.Kind != people.KindAgentGrant || b.Entry.Grant != b.Record {
		a.t.Fatalf("the grant: %d %s", code, body)
	}
	return b.Record
}

// certify asks for the agent's certificate with key d, as agent.
func (a *agents) certify(agent string, d, signer *dev) (int, string, *people.AgentCertified) {
	nonce := make([]byte, 16)
	rand.Read(nonce)
	q := people.AgentRequest{Farm: a.farm.ID, Agent: agent, Key: base64.RawURLEncoding.EncodeToString(d.spki),
		Nonce: base64.RawURLEncoding.EncodeToString(nonce), RequestedAt: a.now.Format(time.RFC3339)}
	sig, err := provenance.SignP256(signer.key, q.ProofMessage())
	if err != nil {
		a.t.Fatal(err)
	}
	var out people.AgentCertified
	code, body := a.call("POST", "/v1/people/agent", "", people.AgentEnrolment{Request: q, Proof: base64.RawURLEncoding.EncodeToString(sig)}, &out)
	return code, body, &out
}

// agentAct is an act the agent signs with its certificate.
func (a *agents) agentAct(c *people.AgentCertified) (*provenance.Verified, []byte) {
	a.t.Helper()
	// An act names a device only for a person's key; an agent's leaf
	// names its key, and its act the grant.
	k := &provtest.Key{Identity: c.Identity, Grant: c.Grant, P256: a.agent.key}
	for _, der := range c.Chain {
		cert, err := x509.ParseCertificate(der)
		if err != nil {
			a.t.Fatal(err)
		}
		k.Chain = append(k.Chain, cert)
	}
	g := schema.GradeSigned
	bundle := a.farm.SignAct(a.t, k, provtest.Act{Act: "steer", Params: map[string]any{"unit": "3", "text": "use the wide lens"}, SignedAt: a.now,
		Chain: &schema.ActChain{By: schema.Actor{Agent: assistant, Grade: g}, For: schema.For{Party: "noor", Grade: g},
			Under: []schema.Delegation{{Grant: c.Grant, Kind: "sign", From: "noor", To: "agent " + assistant}}}})
	v, err := provenance.VerifyBundle(bundle, a.farm.Roots)
	if err != nil {
		a.t.Fatal(err)
	}
	return v, bundle
}

// The issuer certifies an agent's own key as agent/<name>, naming the
// key and the grant, only while a person's touched grant of that key is
// in force: not before it, not for another key or agent, not on a grant
// without the touch, not on another key's proof, and not once the person
// removed the integration. A touched grant made again renews it, under a
// new ID. The leaf lives at most 12 hours.
func TestAgentKeyCertifiedOnlyUnderAGrantInForce(t *testing.T) {
	a := agentFarm(t)
	if code, body, _ := a.certify(assistant, a.agent, a.agent); code != http.StatusForbidden || !strings.Contains(body, "granted by no one") {
		t.Fatalf("before any grant: %d %s", code, body)
	}
	if code, body, _ := a.book(a.grant(assistant, a.agent, false), ""); code != http.StatusForbidden || !strings.Contains(body, "touch") {
		t.Errorf("a grant without the touch: %d %s", code, body)
	}
	grant := a.granted()
	code, body, c := a.certify(assistant, a.agent, a.agent)
	if code != http.StatusOK {
		t.Fatalf("under the grant: %d %s", code, body)
	}
	leaf, err := x509.ParseCertificate(c.Chain[0])
	if err != nil {
		t.Fatal(err)
	}
	switch {
	case provenance.CertIdentity(leaf) != provenance.AgentIdentity(assistant) || c.Identity != provenance.AgentIdentity(assistant):
		t.Errorf("certified as %s", provenance.CertIdentity(leaf))
	case provenance.CertGrant(leaf) != grant || c.Grant != grant || c.Person != "noor":
		t.Errorf("the leaf names grant %.16s for %s, want %.16s for noor", provenance.CertGrant(leaf), c.Person, grant)
	case provenance.CertDevice(leaf) != a.agent.id || provenance.CertDeviceID(leaf) != a.agent.id:
		t.Errorf("the leaf names device %.16s", provenance.CertDevice(leaf))
	case leaf.NotAfter.After(a.now.Add(people.LeafTTL)):
		t.Errorf("the leaf lives to %v", leaf.NotAfter)
	}
	other := newDev(t)
	for name, req := range map[string][3]any{
		"another key":        {assistant, other, other},
		"another agent":      {"other-agent", a.agent, a.agent},
		"another key's word": {assistant, a.agent, other},
	} {
		if code, body, _ := a.certify(req[0].(string), req[1].(*dev), req[2].(*dev)); code != http.StatusForbidden {
			t.Errorf("%s: %d %s", name, code, body)
		}
	}
	// The HUD's check of an act under the grant: the grant's agent and key
	// only, whatever a leaf says.
	v, _ := a.agentAct(c)
	if g, err := a.iss.AgentGrantFor(context.Background(), v); err != nil || g.Person != "noor" || g.Grant != grant {
		t.Errorf("the HUD's check of the agent's act: %+v %v", g, err)
	}
	for name, identity := range map[string]string{"another agent": provenance.AgentIdentity("other-agent"), "another key": ""} {
		key := a.agent
		if identity == "" {
			identity, key = provenance.AgentIdentity(assistant), other
		}
		spki, _ := x509.MarshalPKIXPublicKey(&key.key.PublicKey)
		leaf, err := a.farm.CA.IssueLeaf(&key.key.PublicKey, identity, schema.DeviceID(spki), grant, time.Hour, a.now)
		if err != nil {
			t.Fatal(err)
		}
		forged := &provenance.Verified{Identity: identity, Grant: provenance.CertGrant(leaf), Device: provenance.CertDevice(leaf)}
		if _, err := a.iss.AgentGrantFor(context.Background(), forged); !errors.Is(err, people.ErrNoGrant) {
			t.Errorf("%s's leaf naming the grant: %v", name, err)
		}
	}
	// The integration removed: the person's revoke of the grant.
	revoke := a.act(a.noor, "noor", people.ActRevoke, provenance.Subject{Name: "grant:" + grant, Digest: map[string]string{"c4": grant}}, map[string]string{}, false)
	if code, body, b := a.book(revoke, ""); code != http.StatusOK || b.Entry.Kind != people.KindGrantEnd || b.Entry.End != people.EndRemoved {
		t.Fatalf("the revoke: %d %s", code, body)
	}
	if code, body, _ := a.certify(assistant, a.agent, a.agent); code != http.StatusForbidden {
		t.Errorf("after the integration was removed: %d %s", code, body)
	}
	if res := refusals(a.seal()); len(res) > 0 {
		t.Errorf("the queue refused %v", res)
	}
	// A touched c4 farm integrate grants it again, as a new grant.
	again := a.granted()
	code, body, c = a.certify(assistant, a.agent, a.agent)
	if code != http.StatusOK || c.Grant != again || again == grant {
		t.Errorf("granted again: %d %s, grant %.16s", code, body, c.Grant)
	}
}

// An agent's grant ends after 90 days in which the agent made no act, and
// the lapse is booked once, and sealed. Asking for certificates is not an
// act: it renews nothing.
func TestAgentGrantLapsesUnused(t *testing.T) {
	a := agentFarm(t)
	a.granted()
	a.seal()
	day := 24 * time.Hour
	a.later(60 * day)
	if code, body, _ := a.certify(assistant, a.agent, a.agent); code != http.StatusOK {
		t.Fatalf("on day 60: %d %s", code, body)
	}
	a.later(31 * day)
	if code, body, _ := a.certify(assistant, a.agent, a.agent); code != http.StatusForbidden {
		t.Fatalf("on day 91, unused: %d %s", code, body)
	}
	// The refusal books nothing; the leader's sweep books the lapse.
	if n := len(a.pending(people.KindGrantEnd)); n != 0 {
		t.Errorf("a refusal booked %d lapses", n)
	}
	if es, err := a.iss.BookLapses(context.Background()); err != nil || len(es) != 1 {
		t.Fatalf("the sweep booked %d lapses: %v", len(es), err)
	}
	if res := a.seal(); len(refusals(res)) > 0 {
		t.Fatalf("the queue refused the lapse: %v", refusals(res))
	}
	ends := a.entries(people.KindGrantEnd)
	if len(ends) != 1 || ends[0].End != people.EndLapsed || ends[0].Person != "noor" {
		t.Fatalf("the lapse is booked as %+v", ends)
	}
	// Signed by its writer, the issuer.
	rec, err := a.store.Get(context.Background(), ends[0].Record)
	if err != nil {
		t.Fatal(err)
	}
	if v, err := provenance.VerifyBundle(rec, a.farm.Roots); err != nil || v.PredicateType != people.PredicateGrantLapse || v.Identity != provenance.ComponentIdentity("issuer") {
		t.Errorf("the lapse's record: %v, %+v", err, v)
	}
	if es, _ := a.iss.BookLapses(context.Background()); len(es) != 0 {
		t.Errorf("the lapse booked again: %d", len(es))
	}
}

// The agent's own acts keep its grant: booked as its use at most once a
// day, each renewing its 90 days; past them it lapses. A certificate never
// outlives the lapse, and an act under a lapsed grant is neither taken nor
// booked as a use.
func TestAgentUseKeepsItsGrant(t *testing.T) {
	a := agentFarm(t)
	a.granted()
	a.seal()
	lapses := a.now.Add(farmbook.AgentLapse)
	day := 24 * time.Hour
	a.later(89*day + 18*time.Hour) // six hours before the lapse
	_, _, c := a.certify(assistant, a.agent, a.agent)
	if c.Grant == "" || c.Expires.After(lapses) || !c.Expires.Before(a.now.Add(people.LeafTTL)) {
		t.Fatalf("six hours before the lapse the certificate lives to %v, past the lapse at %v", c.Expires, lapses)
	}
	v, act := a.agentAct(c)
	if err := a.iss.UseGrant(context.Background(), v, act); err != nil {
		t.Fatal(err)
	}
	_, again := a.agentAct(c)
	if err := a.iss.UseGrant(context.Background(), v, again); err != nil {
		t.Fatal(err)
	}
	if n := len(a.pending(people.KindAgentUse)); n != 1 {
		t.Errorf("two acts in a day booked %d uses, want one", n)
	}
	if res := a.seal(); len(refusals(res)) > 0 {
		t.Fatalf("the queue refused the use: %v", refusals(res))
	}
	a.later(89 * day) // day 178: 89 days since the agent's act
	if code, body, _ := a.certify(assistant, a.agent, a.agent); code != http.StatusOK {
		t.Fatalf("89 days after the agent's act: %d %s", code, body)
	}
	a.later(2 * day)
	if code, body, _ := a.certify(assistant, a.agent, a.agent); code != http.StatusForbidden {
		t.Fatalf("91 days after the agent's act: %d %s", code, body)
	}
	if err := a.iss.UseGrant(context.Background(), v, act); !errors.Is(err, people.ErrNoGrant) {
		t.Errorf("an act under the lapsed grant is booked as its use: %v", err)
	}
	if _, err := a.iss.AgentGrantFor(context.Background(), v); !errors.Is(err, people.ErrNoGrant) {
		t.Errorf("the HUD's check of the lapsed grant: %v", err)
	}
	if _, err := a.iss.BookLapses(context.Background()); err != nil {
		t.Fatal(err)
	}
	if res := a.seal(); len(refusals(res)) > 0 {
		t.Fatalf("the queue refused the lapse: %v", refusals(res))
	}
	if ends := a.entries(people.KindGrantEnd); len(ends) != 1 || ends[0].End != people.EndLapsed {
		t.Errorf("the lapse is booked as %+v", ends)
	}
}

// The queue checks each entry of an agent's grant against what it books:
// a lapse of a grant the agent used lately, a use whose act is under
// another grant, and a grant of a person's own device key are refused at
// the seal, never sealed.
func TestAgentEntriesCheckedAtTheSeal(t *testing.T) {
	a := agentFarm(t)
	grant := a.granted()
	_, _, c := a.certify(assistant, a.agent, a.agent)
	a.seal()
	write := func(e people.Entry) {
		t.Helper()
		// Written as the issuer writes, signed by it (book-entry/v1), so
		// the queue's refusal is for what the entry says.
		e.Farm, e.Order, e.At = a.farm.ID, people.OrderPending, a.now.Format(time.RFC3339Nano)
		a.asIssuer(&e)
		st, err := a.ptr.Read(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := farmbook.Submit(context.Background(), a.store, a.ptr, st, []farmbook.Submission{{Entry: e}}); err != nil {
			t.Fatal(err)
		}
	}
	for who, want := range map[string]string{"issuer": farmbook.ReasonDisagrees, "hud": farmbook.ReasonWrongSigner} {
		rec := a.signedAs(provenance.ComponentIdentity(who), people.PredicateGrantLapse, provenance.Subject{Name: "grant:" + grant, Digest: map[string]string{"c4": grant}},
			map[string]string{"farm": a.farm.ID, "grant": grant, "agent": assistant, "device": a.agent.id, "person": "noor", "lastUse": a.now.Format(time.RFC3339Nano)})
		write(people.Entry{Kind: people.KindGrantEnd, End: people.EndLapsed, Record: rec, Grant: grant, Agent: assistant, Device: a.agent.id, Person: "noor"})
		if res := refusals(a.seal()); len(res) != 1 || res[0].Reason != want {
			t.Errorf("a lapse signed by the %s of a grant made today: %+v", who, res)
		}
	}
	_, act := a.agentAct(c)
	id := provtest.ID(string(act))
	if err := a.store.Put(context.Background(), id, act); err != nil {
		t.Fatal(err)
	}
	other := provtest.ID("another grant")
	write(people.Entry{Kind: people.KindAgentUse, Record: id, Grant: other, Agent: assistant, Device: a.agent.id, Person: "noor"})
	if res := refusals(a.seal()); len(res) != 1 || res[0].Reason != farmbook.ReasonNotInForce {
		t.Errorf("a use under a grant never booked: %+v", res)
	}
	// The agent's act under the first grant, booked as its use of a second.
	second := a.granted()
	write(people.Entry{Kind: people.KindAgentUse, Record: id, Grant: second, Agent: assistant, Device: a.agent.id, Person: "noor"})
	if res := refusals(a.seal()); len(res) != 1 || res[0].Reason != farmbook.ReasonDisagrees {
		t.Errorf("an act under one grant booked as another's use: %+v", res)
	}
	// noor's grant of the key to a reviewer, booked as a grant to the
	// assistant.
	gact := a.grant("reviewer", a.agent, true)
	gid := provtest.ID(string(gact))
	if err := a.store.Put(context.Background(), gid, gact); err != nil {
		t.Fatal(err)
	}
	write(people.Entry{Kind: people.KindAgentGrant, Record: gid, Grant: gid, Agent: assistant, Device: a.agent.id, Person: "noor", By: "noor"})
	if res := refusals(a.seal()); len(res) != 1 || res[0].Reason != farmbook.ReasonDisagrees {
		t.Errorf("a grant booked for another agent than its act names: %+v", res)
	}
	// Each entry the issuer wrote is signed by it (book-entry/v1); one
	// written without its signature is refused as unsigned.
	for _, e := range a.entries(people.KindAgentGrant) {
		if len(e.Sig) == 0 {
			t.Errorf("grant %.16s was sealed without the issuer's signature", e.Grant)
		}
	}
	st, err := a.ptr.Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	unsigned := people.Entry{Kind: people.KindAgentUse, Record: id, Grant: grant, Agent: assistant, Device: a.agent.id, Person: "noor",
		Farm: a.farm.ID, Order: people.OrderPending, At: a.now.Format(time.RFC3339Nano)}
	if _, err := farmbook.Submit(context.Background(), a.store, a.ptr, st, []farmbook.Submission{{Entry: unsigned}}); err != nil {
		t.Fatal(err)
	}
	if res := refusals(a.seal()); len(res) != 1 || res[0].Reason != farmbook.ReasonUnsigned {
		t.Errorf("an agent's use written without the issuer's signature: %+v", res)
	}
	// A grant of noor's own CLI key, which is a person's device: refused at
	// the issuer, and at the seal.
	own := a.act(a.noor, "noor", people.ActGrant, deviceSubject(a.noor.Device), map[string]string{"agent": assistant, "device": a.noor.Device}, true)
	if code, body, _ := a.book(own, ""); code != http.StatusForbidden || !strings.Contains(body, "person's device") {
		t.Errorf("a grant of a person's own key: %d %s", code, body)
	}
	oid := provtest.ID(string(own))
	if err := a.store.Put(context.Background(), oid, own); err != nil {
		t.Fatal(err)
	}
	write(people.Entry{Kind: people.KindAgentGrant, Record: oid, Grant: oid, Agent: assistant, Device: a.noor.Device, Person: "noor", By: "noor"})
	if res := refusals(a.seal()); len(res) != 1 || res[0].Reason != farmbook.ReasonRejected {
		t.Errorf("the seal of a grant of a person's own key: %+v", res)
	}
}

// A farm with no farm book takes no agent grant, and certifies no agent.
func TestNoAgentGrantWithoutTheBook(t *testing.T) {
	r := onePerson(t)
	cli, touch := newDev(t), newDev(t)
	_, _, out := r.up(cli, touch)
	k := r.key(cli, out.Owner)
	k.Touch = r.key(touch, out.Touch)
	agent := newDev(t)
	act := r.act(k, "noor", people.ActGrant, deviceSubject(agent.id), map[string]string{"agent": assistant, "device": agent.id}, true)
	if code, body, _ := r.book(act, ""); code != http.StatusNotImplemented || !strings.Contains(body, "farm book") {
		t.Errorf("a grant without the book: %d %s", code, body)
	}
}

// pending is the entries of one kind waiting for the seal, as the issuer
// reads them.
func (a *agents) pending(kind string) []farmbook.Sealed {
	a.t.Helper()
	v, err := (&farmbook.Reader{Farm: a.farm.ID, Roots: a.farm.Roots, Store: a.store, Pointer: a.ptr}).View(context.Background(), a.now)
	if err != nil {
		a.t.Fatal(err)
	}
	var out []farmbook.Sealed
	for _, s := range v.Pending() {
		if s.Entry.Kind == kind {
			out = append(out, *s)
		}
	}
	return out
}

// refusals are the entries a seal refused; none when nothing was pending.
func refusals(res *farmbook.SealResult) []schema.BookRefusal {
	if res == nil {
		return nil
	}
	return res.Refused
}

// signedAs signs a statement as identity, with a leaf the farm's CA
// issues now, and stores it in the book's store; it returns its C4 ID.
func (a *agents) signedAs(identity, predicateType string, subject provenance.Subject, predicate any) string {
	a.t.Helper()
	sg, err := provenance.NewSigner(context.Background(), func(_ context.Context, pub ed25519.PublicKey) ([]*x509.Certificate, error) {
		c, err := a.farm.CA.Issue(pub, identity, time.Hour, a.now.Add(-time.Minute))
		return []*x509.Certificate{c, a.farm.CA.Cert}, err
	})
	if err != nil {
		a.t.Fatal(err)
	}
	rec, err := provenance.SignStatement(context.Background(), sg, predicateType, []provenance.Subject{subject}, predicate, a.now)
	if err != nil {
		a.t.Fatal(err)
	}
	id := provtest.ID(string(rec))
	if err := a.store.Put(context.Background(), id, rec); err != nil {
		a.t.Fatal(err)
	}
	return id
}

// A grant past 90 days of the agent's disuse is refused live, at the
// issuer's clock, on a quiet book where nothing has been sealed since and
// its lapse isn't booked yet: judged at the last seal it would still be
// in force. The refusal books nothing; the sweep books the lapse.
func TestAgentGrantRefusedLiveBeforeItsLapseIsBooked(t *testing.T) {
	a := agentFarm(t)
	grant := a.granted()
	a.seal()
	a.later(91 * 24 * time.Hour)
	// A leaf for the agent's key under the grant, valid now: what an act
	// made today would carry.
	leaf, err := a.farm.CA.IssueLeaf(&a.agent.key.PublicKey, provenance.AgentIdentity(assistant), a.agent.id, grant, time.Hour, a.now)
	if err != nil {
		t.Fatal(err)
	}
	v := &provenance.Verified{Identity: provenance.CertIdentity(leaf), Grant: provenance.CertGrant(leaf), Device: provenance.CertDevice(leaf)}
	if n := len(a.entries(people.KindGrantEnd)) + len(a.pending(people.KindGrantEnd)); n != 0 {
		t.Fatalf("%d lapses booked before the check", n)
	}
	if b := a.fbook(); b.Head().Position == 0 {
		t.Fatal("nothing sealed")
	} else if on, err := b.InForce(farmbook.ForceAgentGrant, grant, b.Head().Position); err != nil || on == nil {
		t.Fatalf("judged at the last seal the grant is in force, as the test needs: %v %v", on, err)
	}
	if _, err := a.iss.AgentGrantFor(context.Background(), v); !errors.Is(err, people.ErrNoGrant) {
		t.Fatalf("91 days unused, before its lapse is booked: %v", err)
	}
	if n := len(a.pending(people.KindGrantEnd)); n != 0 {
		t.Errorf("the refusal booked %d lapses, want none", n)
	}
	if es, err := a.iss.BookLapses(context.Background()); err != nil || len(es) != 1 {
		t.Errorf("the sweep booked %d lapses: %v", len(es), err)
	}
}
