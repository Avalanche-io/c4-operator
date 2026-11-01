package people_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Avalanche-io/c4-operator/internal/provenance"
	"github.com/Avalanche-io/c4-operator/internal/provenance/people"
	"github.com/Avalanche-io/c4-operator/internal/provenance/people/peopletest"
	"github.com/Avalanche-io/c4-operator/internal/provenance/provtest"
	"github.com/Avalanche-io/c4-operator/schema"
)

// The people's issuer's tests run against a fake sign-in provider at
// https://sso.studio.example, served over TLS by httptest with a
// certificate and signing keys made when the test runs. Nothing here is a
// real provider or a real credential.

const (
	ssoIssuer = peopletest.Issuer
	clientID  = peopletest.ClientID
	danaSub   = peopletest.DanaSub
	noorSub   = peopletest.NoorSub
)

// rig is a farm with a people's issuer: a scratch CA, an index in memory,
// the fake provider, and a clock the test moves.
type rig struct {
	t     *testing.T
	farm  *provtest.Farm
	iss   *people.Issuer
	index *people.Index
	store *people.MemoryStore
	head  *people.MemoryHead
	sso   *peopletest.SSO
	now   time.Time
	// addr is the address requests come from.
	addr string
	// caller is whom the HUD signed in for each request; "" for no one.
	caller string
}

// studio is a studio farm signing in at the fake provider: dana is an
// admin, noor isn't.
func studio(t *testing.T) *rig {
	r := newRig(t)
	r.iss.Provider = people.Provider{Issuer: ssoIssuer, ClientID: clientID}
	r.caller = "dana"
	return r
}

func newRig(t *testing.T) *rig {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Second)
	r := &rig{t: t, farm: provtest.NewFarm(t, now), store: &people.MemoryStore{}, head: &people.MemoryHead{}, now: now, addr: "192.0.2.10"}
	r.sso = peopletest.NewSSO(t, ssoIssuer)
	r.index = &people.Index{Farm: r.farm.ID, Roots: r.farm.Roots, Store: r.store, Head: r.head}
	r.iss = &people.Issuer{CA: r.farm.CA, Index: r.index, HTTPClient: r.sso.Client, Now: func() time.Time { return r.now },
		SignIns: people.StaticSignIns{"dana": {{Issuer: ssoIssuer, Subject: danaSub}}, "noor": {{Issuer: ssoIssuer, Subject: noorSub}}},
		Admin:   func(_ context.Context, p string) bool { return p == "dana" },
		ClientAddr: func(*http.Request) string {
			return r.addr
		}}
	return r
}

// onePerson is noor's one-person farm: no provider, the installer token.
func onePerson(t *testing.T) *rig {
	r := newRig(t)
	r.iss.Owner = func(context.Context) string { return "noor" }
	r.iss.InstallerToken = []byte(installerToken)
	r.iss.LocalOnly = true
	r.iss.Admin = func(_ context.Context, p string) bool { return p == "noor" }
	r.caller = "noor"
	return r
}

// installerToken is the scratch machine's installer token, made when the
// test runs.
var installerToken = codeOf(nil)

// dev is a device's key.
type dev struct {
	key  *ecdsa.PrivateKey
	spki []byte
	id   string
}

func newDev(t *testing.T) *dev {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	spki, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return &dev{key: key, spki: spki, id: schema.DeviceID(spki)}
}

// enrolment is d's request for person, signed with d's key.
func (r *rig) enrolment(d *dev, person, touchFor, kind string) people.Enrolment {
	nonce := make([]byte, 16)
	rand.Read(nonce)
	q := people.Request{Farm: r.farm.ID, Person: person, Key: base64.RawURLEncoding.EncodeToString(d.spki), TouchFor: touchFor,
		Nonce: base64.RawURLEncoding.EncodeToString(nonce), RequestedAt: r.now.Format(time.RFC3339)}
	p := schema.DevicePredicate{Farm: q.Farm, Person: q.Person, Key: q.Key, TouchFor: q.TouchFor, Nonce: q.Nonce, RequestedAt: q.RequestedAt}
	sig, err := provenance.SignP256(d.key, p.ProofMessage())
	if err != nil {
		r.t.Fatal(err)
	}
	protection := schema.ProtectionHardware
	if kind == "browser" {
		protection = schema.ProtectionBrowser
	}
	return people.Enrolment{Request: q, Proof: base64.RawURLEncoding.EncodeToString(sig), Protection: protection, Kind: kind, Label: person + "'s " + kind}
}

// call sends a request to the issuer, from host: local is the farm's own
// machine, "" anywhere else, as httptest makes it. It decodes the answer
// into out when it is 200.
func (r *rig) call(method, path, host string, body any, out any, hdr ...string) (int, string) {
	r.t.Helper()
	var b []byte
	switch v := body.(type) {
	case nil:
	case []byte:
		b = v
	default:
		b, _ = json.Marshal(v)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(b))
	if r.caller != "" {
		req = req.WithContext(people.WithCaller(req.Context(), r.caller))
	}
	if host == local {
		req.RemoteAddr = local
	}
	for k := 0; k+1 < len(hdr); k += 2 {
		req.Header.Set(hdr[k], hdr[k+1])
	}
	w := httptest.NewRecorder()
	r.iss.ServeHTTP(w, req)
	if w.Code == http.StatusOK && out != nil {
		if err := json.Unmarshal(w.Body.Bytes(), out); err != nil {
			r.t.Fatalf("%s %s: %v: %s", method, path, err, w.Body)
		}
	}
	return w.Code, w.Body.String()
}

// local is a request from the farm's own machine.
const local = "127.0.0.1:52144"

// signIn enrols d for person on a token the provider issued for sub, and
// fails the test unless it is certified.
func (r *rig) signIn(d *dev, person, sub string) *people.Certified {
	r.t.Helper()
	e := r.enrolment(d, person, "", "browser")
	e.IDToken = r.sso.Token(r.now, sub, d.id, nil)
	var c people.Certified
	if code, body := r.call("POST", "/v1/people/enroll", "", e, &c); code != http.StatusOK {
		r.t.Fatalf("enrol %s: %d %s", person, code, body)
	}
	return &c
}

// key is d as a signer, with the chain the issuer certified.
func (r *rig) key(d *dev, c *people.Certified) *provtest.Key {
	r.t.Helper()
	k := &provtest.Key{Identity: c.Identity, Device: c.Device, P256: d.key}
	for _, der := range c.Chain {
		cert, err := x509.ParseCertificate(der)
		if err != nil {
			r.t.Fatal(err)
		}
		k.Chain = append(k.Chain, cert)
	}
	return k
}

// act signs an act as person with k, on subject, confirmed with k's
// touch key when it has one and confirmed is set.
func (r *rig) act(k *provtest.Key, person, verb string, subject provenance.Subject, params any, confirmed bool) []byte {
	grade := schema.GradeSigned
	if confirmed {
		grade = schema.GradeConfirmed
	}
	return r.farm.SignAct(r.t, k, provtest.Act{Subject: subject, Act: verb, Params: params, SignedAt: r.now, Confirmed: confirmed,
		Chain: &schema.ActChain{By: schema.Actor{Person: person, Grade: grade}, For: schema.For{Party: person, Grade: grade}}})
}

func deviceSubject(id string) provenance.Subject {
	return provenance.Subject{Name: "device:" + id, Digest: map[string]string{"c4": id}}
}

func (r *rig) farmSubject() provenance.Subject {
	return provenance.Subject{Name: "farm:" + r.farm.ID, Digest: map[string]string{"c4": r.farm.ID}}
}

// entries is the index's entries of one kind.
func (r *rig) entries(kind string) []*people.Entry {
	r.t.Helper()
	es, _, err := r.index.Entries(context.Background(), 0)
	if err != nil {
		r.t.Fatal(err)
	}
	var out []*people.Entry
	for _, e := range es {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

// codeOf makes a one-time code, or a token, as the CLI does: 32 random
// bytes, hex.
func codeOf(testing.TB) string {
	b := make([]byte, 32)
	rand.Read(b)
	return hex.EncodeToString(b)
}
