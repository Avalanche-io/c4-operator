package provenance

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Avalanche-io/c4-operator/schema"
)

// A unit signs an act as itself with a key certified as its attempt: the
// act verifies, by the unit, for the job's owner, under the job's
// ownership, graded signed, from its farm. A key certified as anything
// else signs no unit act, and a unit act needs its job's owner.
func TestSignUnitAct(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	ca, err := NewCA(now)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca.Cert)
	const jobUID = "9c1d2e7f-0000-4000-8000-000000000001"
	unit, err := NewSigner(ctx, LocalIssuer(ca, UnitIdentity("show-a", "sh040-comp", "3", jobUID, 2), time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	subject := ActSubject("workjob", "show-a", "sh040-comp", jobUID, strings.Repeat("ab", 32))
	b, err := SignUnitAct(ctx, unit, UnitAct{Subject: subject, Act: "steer", Params: json.RawMessage(`{"unit":"4","text":"use the v3 plates"}`), Owner: "dana"}, now)
	if err != nil {
		t.Fatal(err)
	}
	v, act, err := VerifyAct(b, roots, now, NewMemorySeen())
	if err != nil {
		t.Fatalf("the unit's act: %v", err)
	}
	p := act.Predicate
	if v.Identity != UnitIdentity("show-a", "sh040-comp", "3", jobUID, 2) || p.Farm != FarmID(ca.Cert) || p.Act != "steer" ||
		p.Chain.By.Unit != schema.UnitRef("show-a", "sh040-comp", jobUID, "3") || p.Chain.By.Grade != schema.GradeSigned ||
		p.Chain.For.Party != "dana" || len(p.Chain.Under) != 1 || p.Chain.Under[0].Job != "show-a/sh040-comp" || p.Chain.Under[0].Kind != "ownership" {
		t.Fatalf("the act: %+v by %s", p, v.Identity)
	}

	node, err := NewSigner(ctx, LocalIssuer(ca, NodeIdentity("w1"), time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := SignUnitAct(ctx, node, UnitAct{Subject: subject, Act: "steer", Owner: "dana"}, now); err == nil {
		t.Fatal("a node's key signed a unit's act")
	}
	if _, err := SignUnitAct(ctx, unit, UnitAct{Subject: subject, Act: "steer"}, now); err == nil {
		t.Fatal("a unit act for no one")
	}
}

// A unit's identity reads back whole, and only a well-formed one does.
func TestParseUnitIdentity(t *testing.T) {
	u, ok := ParseUnitIdentity(UnitIdentity("show-a", "sh040-comp", "3", "9c1d2e7f", 2))
	if !ok || u != (UnitOf{Namespace: "show-a", Job: "sh040-comp", Unit: "3", JobUID: "9c1d2e7f", Attempt: 2}) {
		t.Fatalf("%+v %v", u, ok)
	}
	for _, bad := range []string{
		NodeIdentity("w1"),
		IdentityBase + "unit/show-a/sh040-comp/3@9c1d2e7f/0",
		IdentityBase + "unit/show-a/sh040-comp/3@9c1d2e7f/x",
		IdentityBase + "unit/show-a/sh040-comp/3/2",
		IdentityBase + "unit/show-a/3@9c1d2e7f/2",
	} {
		if _, ok := ParseUnitIdentity(bad); ok {
			t.Errorf("%s read as a unit", bad)
		}
		if unitSigns(bad, schema.UnitRef("show-a", "sh040-comp", "9c1d2e7f", "3")) {
			t.Errorf("%s signs as the unit", bad)
		}
	}
}

// A unit's report on itself verifies when the farm reads it, however
// long after, and as often as it reads it; a person's act, or a unit's
// act that is no report, does not.
func TestVerifyUnitReport(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	ca, err := NewCA(now)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca.Cert)
	const jobUID = "9c1d2e7f-0000-4000-8000-000000000001"
	unit, err := NewSigner(ctx, LocalIssuer(ca, UnitIdentity("show-a", "sh040-comp", "3", jobUID, 2), time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	on := ActSubject("pod", "show-a", "sh040-comp-u3", "5e1f0c2a-0000-4000-8000-0000000000a3", strings.Repeat("ab", 32))
	b, err := SignUnitAct(ctx, unit, UnitAct{Subject: on, Act: "needs", Params: json.RawMessage(`{"what":"the frame rate"}`), Owner: "dana"}, now)
	if err != nil {
		t.Fatal(err)
	}
	for _, at := range []time.Time{now, now.Add(3 * time.Hour), now.Add(3 * time.Hour)} {
		if v, a, err := VerifyUnitReport(b, roots, at); err != nil || a.Predicate.Act != "needs" || v.Identity != UnitIdentity("show-a", "sh040-comp", "3", jobUID, 2) {
			t.Fatalf("read at %s: %v", at, err)
		}
	}
	if _, _, err := VerifyUnitReport(b, roots, now.Add(-time.Hour)); err == nil {
		t.Fatal("a report signed after it was read")
	}
	steer, _ := SignUnitAct(ctx, unit, UnitAct{Subject: on, Act: "steer", Owner: "dana"}, now)
	if _, _, err := VerifyUnitReport(steer, roots, now); err == nil {
		t.Fatal("a unit's steer read as its report")
	}
	other := x509.NewCertPool()
	ca2, _ := NewCA(now)
	other.AddCert(ca2.Cert)
	if _, _, err := VerifyUnitReport(b, other, now); err == nil {
		t.Fatal("another farm's roots took it")
	}
}
