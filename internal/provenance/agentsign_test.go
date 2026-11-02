package provenance_test

import (
	"crypto/ecdsa"
	"encoding/json"
	"testing"
	"time"

	"github.com/Avalanche-io/c4-operator/internal/provenance"
	"github.com/Avalanche-io/c4-operator/internal/provenance/provtest"
	"github.com/Avalanche-io/c4-operator/schema"
)

// p256Key signs as a keystore key does.
type p256Key struct{ k *ecdsa.PrivateKey }

func (k p256Key) Sign(msg []byte) ([]byte, error) { return provenance.SignP256(k.k, msg) }

// An agent's act, signed with its own key under its grant, verifies as
// the farm takes it: by the agent, for the grant's person, under the
// grant its leaf names, graded signed, naming no device, with the params
// as given, compared as the farm compares them (canonical JSON). Signed
// under a grant its leaf doesn't name it is refused; with no certificate
// it isn't made.
func TestSignAgentActVerifies(t *testing.T) {
	now := time.Now()
	f := provtest.NewFarm(t, now)
	grant := provtest.ID("noor's grant to claude-code")
	k := f.Agent(t, "claude-code", grant)
	cert := provenance.AgentCert{Identity: k.Identity, Agent: "claude-code", Grant: grant, Person: "noor"}
	for _, c := range k.Chain {
		cert.Chain = append(cert.Chain, c.Raw)
	}
	on := provenance.ActSubject("workjob", "show-a", "sh040-light-v007", provtest.JobUID, provtest.JobSpecDigest)
	params := json.RawMessage(`{"unit": "ruling", "do": "approve", "waitsFor": "agent:claude-code"}`)
	b, err := provenance.SignAgentAct(provenance.AgentAct{Farm: f.ID, Subject: on, Act: "decide", Params: params, SignedAt: now}, p256Key{k.P256}, cert)
	if err != nil {
		t.Fatal(err)
	}
	v, a, err := provenance.VerifyAct(b, f.Roots, now, provenance.NewMemorySeen())
	if err != nil {
		t.Fatalf("the agent's act doesn't verify: %v", err)
	}
	p := a.Predicate
	want, _ := schema.ParamsID(params)
	got, _ := schema.ParamsID(p.Params)
	switch {
	case p.Chain.By.Agent != "claude-code" || p.Chain.By.Grade != schema.GradeSigned || p.Chain.For.Party != "noor":
		t.Errorf("by %+v for %+v", p.Chain.By, p.Chain.For)
	case len(p.Chain.Under) != 1 || p.Chain.Under[0].Grant != grant || v.Grant != grant:
		t.Errorf("under %+v, the leaf's grant %.16s", p.Chain.Under, v.Grant)
	case p.Device != "" || got != want || a.Subject[0].Name != on.Name:
		t.Errorf("device %q, params %s, subject %s", p.Device, p.Params, a.Subject[0].Name)
	}
	// Under a grant other than its leaf's: refused by the verifier.
	other := cert
	other.Grant = provtest.ID("another grant")
	b, err = provenance.SignAgentAct(provenance.AgentAct{Farm: f.ID, Subject: on, Act: "steer", SignedAt: now}, p256Key{k.P256}, other)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := provenance.VerifyAct(b, f.Roots, now, provenance.NewMemorySeen()); err == nil {
		t.Error("an act under another grant than its leaf's verifies")
	}
	if _, err := provenance.SignAgentAct(provenance.AgentAct{Farm: f.ID, Subject: on, Act: "steer"}, p256Key{k.P256}, provenance.AgentCert{}); err == nil {
		t.Error("an act signed with no certificate")
	}
}
