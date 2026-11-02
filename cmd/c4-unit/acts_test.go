package main

import (
	"bytes"
	"crypto/x509"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Avalanche-io/c4-operator/internal/needs"
	"github.com/Avalanche-io/c4-operator/internal/provenance"
	"github.com/Avalanche-io/c4-operator/schema"
)

const actsJobUID = "9c1d2e7f-0000-4000-8000-000000000001"

// signingUnit is a unit with a key: its env as the queue gives it, and an
// issuer that certifies it as its attempt.
func signingUnit(t *testing.T) (env, map[string]string, *x509.CertPool, string) {
	t.Helper()
	ca, err := provenance.NewCA(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca.Cert)
	out := t.TempDir()
	m := map[string]string{"C4_OUTPUT": out, "C4_NAMESPACE": "show-a", "C4_JOB": "sh040-comp", "C4_UNIT": "3", "C4_ATTEMPT": "2",
		"C4_JOB_UID": actsJobUID, "C4_POD": "sh040-comp-u3", "C4_POD_UID": "5e1f0c2a-0000-4000-8000-0000000000a3", "C4_SPEC": strings.Repeat("ab", 32), "C4_OWNER": "dana",
		"C4_TERMINATION_LOG": filepath.Join(t.TempDir(), "termination-log"), "C4_CONTROL": t.TempDir()}
	e := vars(m)
	e.issue = provenance.LocalIssuer(ca, provenance.UnitIdentity("show-a", "sh040-comp", "3", actsJobUID, 2), time.Hour)
	e.sleep = func(time.Duration) {}
	return e, m, roots, out
}

// unitActs reads and verifies a unit's .c4/acts.
func unitActs(t *testing.T, out string, roots *x509.CertPool) []*schema.Act {
	t.Helper()
	var raw []json.RawMessage
	b, err := os.ReadFile(filepath.Join(out, ".c4", "acts"))
	if err != nil {
		return nil
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	var acts []*schema.Act
	for _, r := range raw {
		v, a, err := provenance.VerifyAct(r, roots, time.Now(), provenance.NewMemorySeen())
		if err != nil {
			t.Fatalf("an act does not verify: %v", err)
		}
		if v.Identity != provenance.UnitIdentity("show-a", "sh040-comp", "3", actsJobUID, 2) {
			t.Fatalf("signed by %s", v.Identity)
		}
		acts = append(acts, a)
	}
	return acts
}

// A unit with a key signs each control as itself, before the note the
// relay carries after it: by the unit, for the job's owner, on its own
// pod and the spec it was made under, its params the note's.
func TestUnitSignsItsControls(t *testing.T) {
	e, m, roots, out := signingUnit(t)
	var buf bytes.Buffer
	if c, err := run([]string{"ask", "--to", "unit:plan", "--ask", "Which module owns the tokenizer?"}, &buf, e); c != 0 || err != nil {
		t.Fatalf("ask: %d %v", c, err)
	}
	if c, err := run([]string{"answer", "--ask", "5#1", "--answer", "tokenize/"}, &buf, e); c != 0 || err != nil {
		t.Fatalf("answer: %d %v", c, err)
	}
	if c, _ := run([]string{"wait", "--server", "person:dana", "--ask", "Approve the v3 plates?", "--wait", "0s"}, &buf, e); c != 1 {
		t.Fatalf("wait: %d", c)
	}
	acts := unitActs(t, out, roots)
	if len(acts) != 3 {
		t.Fatalf("%d acts", len(acts))
	}
	want := []struct {
		act    string
		params string
	}{
		{"ask", `{"ask":"Which module owns the tokenizer?","seq":1,"to":"unit:plan"}`},
		{"answer", `{"answer":"tokenize/","ask":"5#1","seq":1}`},
		{"wait", `{"ask":"Approve the v3 plates?","seq":1,"server":"person:dana"}`},
	}
	subject := provenance.ActSubject("pod", "show-a", "sh040-comp-u3", "5e1f0c2a-0000-4000-8000-0000000000a3", m["C4_SPEC"])
	for i, a := range acts {
		p := a.Predicate
		if p.Act != want[i].act || string(p.Params) != want[i].params || p.Chain.For.Party != "dana" ||
			p.Chain.By.Unit != schema.UnitRef("show-a", "sh040-comp", actsJobUID, "3") ||
			len(a.Subject) != 1 || a.Subject[0].Name != subject.Name || a.Subject[0].Digest["sha256"] != m["C4_SPEC"] {
			t.Errorf("act %d: %s %s for %s on %+v", i, p.Act, p.Params, p.Chain.For.Party, a.Subject)
		}
	}
	if strings.Contains(buf.String(), "could not be signed") {
		t.Fatalf("said it could not sign: %q", buf.String())
	}
}

// A report the unit ends on waits for the farm to have its act; one the
// relay never confirms still goes, and says so.
func TestNeedsWaitsForItsAct(t *testing.T) {
	e, m, roots, out := signingUnit(t)
	var errs bytes.Buffer
	e.stderr = &errs
	os.WriteFile(filepath.Join(m["C4_CONTROL"], "acts.seen"), []byte("1"), 0o644)
	if c, err := run([]string{"needs", "--what", "the delivery frame rate", "--why", "the brief gives 24 and 25", "--about", "unit:schema"}, &bytes.Buffer{}, e); c != needsCode || err != nil {
		t.Fatalf("needs: %d %v", c, err)
	}
	acts := unitActs(t, out, roots)
	if len(acts) != 1 || acts[0].Predicate.Act != "needs" ||
		string(acts[0].Predicate.Params) != `{"about":["unit:schema"],"what":"the delivery frame rate","why":"the brief gives 24 and 25"}` || errs.Len() != 0 {
		t.Fatalf("needs act %+v, said %q", acts, errs.String())
	}

	// the relay has not confirmed the withdrawal's act: the unit says so,
	// after the wait, and withdraws
	clock := time.Date(2026, 11, 1, 20, 0, 0, 0, time.UTC)
	e.now = func() time.Time { return clock }
	e.sleep = func(d time.Duration) { clock = clock.Add(d) }
	m["C4_RECHECK"] = "1"
	if c, err := run([]string{"withdraw", "--why", "schema fixes 24 fps"}, &bytes.Buffer{}, e); c != 0 || err != nil {
		t.Fatalf("withdraw: %d %v", c, err)
	}
	if !strings.Contains(errs.String(), "did not confirm having it within 30s") {
		t.Fatalf("said %q", errs.String())
	}
	if b, _ := os.ReadFile(m["C4_TERMINATION_LOG"]); string(b) != "withdrawn: schema fixes 24 fps" {
		t.Fatalf("termination message %q", b)
	}
}

// A unit with no key signs nothing and its controls go as before; one
// that has signed its allowance says so, and its control still goes.
func TestUnsignedAndTheAllowance(t *testing.T) {
	out := t.TempDir()
	var errs bytes.Buffer
	e := vars(map[string]string{"C4_OUTPUT": out, "C4_UNIT": "3"})
	e.stderr = &errs
	if c, err := run([]string{"ask", "--ask", "Which module?"}, &bytes.Buffer{}, e); c != 0 || err != nil {
		t.Fatalf("ask: %d %v", c, err)
	}
	if _, err := os.Stat(filepath.Join(out, ".c4", "acts")); err == nil || errs.Len() != 0 {
		t.Fatalf("a unit with no key signed, or said %q", errs.String())
	}

	e2, _, roots, out2 := signingUnit(t)
	e2.stderr = &errs
	for range unitActsMax {
		if _, err := signAct(e2, out2, "ask", map[string]any{"seq": 1}); err != nil {
			t.Fatal(err)
		}
	}
	if c, err := run([]string{"ask", "--ask", "one more"}, &bytes.Buffer{}, e2); c != 0 || err != nil ||
		!strings.Contains(errs.String(), "signed 48 acts, the most one may") {
		t.Fatalf("past the allowance: %d %v, said %q", c, err, errs.String())
	}
	if n := len(unitActs(t, out2, roots)); n != unitActsMax {
		t.Fatalf("%d acts", n)
	}
}

// A needs too long for a termination message is cut to fit before it is
// signed, so the act and the message say exactly the same.
func TestLongNeedsSignedAsWritten(t *testing.T) {
	e, m, roots, out := signingUnit(t)
	os.WriteFile(filepath.Join(m["C4_CONTROL"], "acts.seen"), []byte("1"), 0o644)
	why := strings.Repeat("the brief gives 24 here and 25 there. ", 200)
	if c, err := run([]string{"needs", "--what", "the delivery frame rate", "--why", why}, &bytes.Buffer{}, e); c != needsCode || err != nil {
		t.Fatalf("needs: %d %v", c, err)
	}
	msg, _ := os.ReadFile(m["C4_TERMINATION_LOG"])
	acts := unitActs(t, out, roots)
	var p struct{ What, Why string }
	if len(acts) != 1 || json.Unmarshal(acts[0].Predicate.Params, &p) != nil {
		t.Fatalf("acts %v", acts)
	}
	if len(msg) > needs.MessageMax || string(msg) != needs.Message(p.What, p.Why, nil) || !strings.HasSuffix(p.Why, needs.CutMark) {
		t.Fatalf("message %d bytes, signed why ends %q", len(msg), p.Why[max(0, len(p.Why)-12):])
	}
}
