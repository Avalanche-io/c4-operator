package people_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/Avalanche-io/c4-operator/internal/provenance/people"
	"github.com/Avalanche-io/c4-operator/internal/provenance/people/peopletest"
)

func whoami(t *testing.T, f *peopletest.Farm, token string) (int, people.WhoamiAnswer, string) {
	t.Helper()
	b, _ := json.Marshal(people.Whoami{IDToken: token})
	resp, err := http.Post(f.URL+"/v1/people/whoami", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out people.WhoamiAnswer
	var raw map[string]any
	dec := json.NewDecoder(resp.Body)
	dec.Decode(&raw)
	if p, ok := raw["person"].(string); ok {
		out.Person = p
	}
	msg, _ := raw["error"].(string)
	return resp.StatusCode, out, msg
}

// whoami says whose sign-in an ID token is, so the CLI signs its
// enrolment as them with no name typed: the token is checked as an
// enrolment's is, but not its nonce, since whoami certifies nothing. A
// subject no one holds is refused, and one two people hold names neither,
// here and at enrolment.
func TestWhoamiNamesTheTokensPerson(t *testing.T) {
	f := peopletest.Studio(t)
	now := time.Now()
	if code, got, msg := whoami(t, f, f.SSO.Token(now, peopletest.DanaSub, "any nonce", nil)); code != http.StatusOK || got.Person != "dana" {
		t.Errorf("dana's token: %d %+v %s", code, got, msg)
	}
	if code, _, _ := whoami(t, f, f.SSO.Token(now, peopletest.DanaSub, "", map[string]any{"aud": "another-client"})); code == http.StatusOK {
		t.Error("a token for another client was read")
	}
	if code, _, _ := whoami(t, f, f.SSO.Token(now.Add(-time.Hour), peopletest.DanaSub, "", nil)); code == http.StatusOK {
		t.Error("a token issued an hour ago was read")
	}
	if code, _, _ := whoami(t, f, f.SSO.Token(now, "00u-nobody", "", nil)); code != http.StatusForbidden {
		t.Errorf("no one's subject: %d", code)
	}

	// an admin's mistake: dana's subject in noor's sign-ins too
	ins := f.Issuer.SignIns.(people.StaticSignIns)
	ins["noor"] = append(ins["noor"], people.SignIn{Issuer: peopletest.Issuer, Subject: peopletest.DanaSub})
	if code, _, msg := whoami(t, f, f.SSO.Token(now, peopletest.DanaSub, "", nil)); code != http.StatusConflict {
		t.Errorf("a subject two people hold: %d %s", code, msg)
	}
	d := peopletest.NewDevice(t)
	e := f.Enrolment(t, d, "dana", "", "linux")
	e.IDToken = f.SSO.Token(now, peopletest.DanaSub, d.ID, nil)
	b, _ := json.Marshal(e)
	resp, err := http.Post(f.URL+"/v1/people/enroll", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("enrolling on a subject two people hold: %d", resp.StatusCode)
	}
}

// ProveSignIn checks a token as whoami does, for the HUD's proof that a
// provider subject is the caller's before it goes on their Party: a fresh
// token for the farm's client proves its subject, in its issuer's one
// spelling; one for another client, an old one, or a forged one proves
// nothing.
func TestProveSignIn(t *testing.T) {
	f := peopletest.Studio(t)
	ctx := context.Background()
	now := time.Now()
	got, err := f.Issuer.ProveSignIn(ctx, f.SSO.Token(now, "00u-anyone", "", nil))
	if err != nil || got != (people.SignIn{Issuer: people.CanonicalIssuer(peopletest.Issuer), Subject: "00u-anyone"}) {
		t.Errorf("a fresh token: %+v %v", got, err)
	}
	for what, tok := range map[string]string{
		"another client's": f.SSO.Token(now, peopletest.DanaSub, "", map[string]any{"aud": "another-client"}),
		"an hour old":      f.SSO.Token(now.Add(-time.Hour), peopletest.DanaSub, "", nil),
		"not a token":      "eyJhbGciOiJub25lIn0.e30.",
	} {
		if got, err := f.Issuer.ProveSignIn(ctx, tok); err == nil {
			t.Errorf("%s token proves %+v", what, got)
		}
	}
}
