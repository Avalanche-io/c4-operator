// Package peopletest serves a real people's issuer for clients' tests: the
// HUD's web app enrolling a browser (in Node, against an httptest server),
// and the CLI. It makes a scratch farm, a fake sign-in provider and every
// key when the test runs; nothing is a real provider or a real credential.
//
//	f := peopletest.Studio(t)          // dana and noor sign in at the fake provider
//	token := f.SSO.Token(time.Now(), peopletest.DanaSub, deviceID, nil)
//	// POST f.URL + "/v1/people/enroll" with that token
//
//	o := peopletest.OnePerson(t)       // noor's farm, her owner key enrolled
//	code := o.NewCode(t)               // a one-time code, booked, for a minute
//	// open the web app with code; it POSTs f.URL + "/v1/people/browser"
//
// The server serves the people routes at /issuer/v1/people/, as the HUD's
// origin does, with Farm.Caller as the person signed in; a studio farm's
// also serves GET /sso/token, a fake provider's token for any subject and
// nonce, for a client that can't make the redirect in its test.
package peopletest

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc/oidctest"

	"github.com/Avalanche-io/c4-operator/internal/provenance"
	"github.com/Avalanche-io/c4-operator/internal/provenance/people"
	"github.com/Avalanche-io/c4-operator/internal/provenance/provtest"
	"github.com/Avalanche-io/c4-operator/schema"
)

// The fake provider and the people who sign in at it.
const (
	Issuer   = "https://sso.studio.example"
	ClientID = "c4-farm-studio"
	DanaSub  = "00u-dana-7f3a"
	NoorSub  = "00u-noor-2c9e"
)

// SSO is a fake sign-in provider: oidctest's discovery and keys over TLS,
// with a certificate and an ES256 key made when the test runs.
type SSO struct {
	Issuer string
	// Client reaches the provider, whatever its host: give it to the
	// issuer as its HTTPClient.
	Client *http.Client

	key *ecdsa.PrivateKey
	kid string
}

// NewSSO serves a fake provider whose issuer URL is issuer (https://...).
func NewSSO(t testing.TB, issuer string) *SSO {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	s := &oidctest.Server{PublicKeys: []oidctest.PublicKey{{PublicKey: key.Public(), KeyID: "sso-key-1", Algorithm: "ES256"}}, Algorithms: []string{"ES256"}}
	s.SetIssuer(issuer)
	cert, leaf := tlsCert(t, strings.TrimPrefix(issuer, "https://"))
	ts := httptest.NewUnstartedServer(s)
	ts.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	ts.StartTLS()
	t.Cleanup(ts.Close)
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	addr := ts.Listener.Addr().String()
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool},
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		}}}
	return &SSO{Issuer: issuer, Client: client, key: key, kid: "sso-key-1"}
}

// tlsCert is a self-signed serving certificate for host.
func tlsCert(t testing.TB, host string) (tls.Certificate, *x509.Certificate) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: host}, DNSNames: []string{host},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(der)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, leaf
}

// Token signs an ID token: the provider's issuer, the farm's client ID,
// subject sub, issued at now and good for an hour, nonce nonce (the
// device ID it certifies), with claims laid over (nil deletes one).
func (s *SSO) Token(now time.Time, sub, nonce string, over map[string]any) string {
	c := map[string]any{"iss": s.Issuer, "aud": ClientID, "sub": sub, "nonce": nonce,
		"iat": now.Unix(), "exp": now.Add(time.Hour).Unix(), "email": "dana@studio.example"}
	for k, v := range over {
		if v == nil {
			delete(c, k)
			continue
		}
		c[k] = v
	}
	b, _ := json.Marshal(c)
	return oidctest.SignIDToken(s.key, s.kid, "ES256", string(b))
}

// StepUp is the claims of a step-up sign-in at now, for a touch key's
// token: a fresh auth_time, a password and a one-time code.
func StepUp(now time.Time) map[string]any {
	return map[string]any{"auth_time": now.Unix(), "amr": []string{"pwd", "otp"}}
}

// Farm is a scratch farm with a people's issuer in memory, served at URL.
type Farm struct {
	// Scratch is the farm's CA, roots and ID, and signs scratch acts.
	Scratch *provtest.Farm
	Issuer  *people.Issuer
	Index   *people.Index
	Store   *people.MemoryStore
	Head    *people.MemoryHead
	Server  *httptest.Server
	// URL is where the people routes are: Server.URL + "/issuer", so a
	// client calls URL + "/v1/people/...", as at the HUD's origin.
	URL string
	// SSO is a studio farm's provider; nil on a one-person farm.
	SSO *SSO

	mu     sync.Mutex
	caller string
}

// SetCaller says whom the HUD has signed in for the requests that follow:
// "" for no one.
func (f *Farm) SetCaller(person string) {
	f.mu.Lock()
	f.caller = person
	f.mu.Unlock()
}

// ID is the farm's ID, as an act names it.
func (f *Farm) ID() string { return f.Scratch.ID }

func newFarm(t testing.TB) *Farm {
	t.Helper()
	sf := provtest.NewFarm(t, time.Now())
	f := &Farm{Scratch: sf, Store: &people.MemoryStore{}, Head: &people.MemoryHead{}}
	f.Index = &people.Index{Farm: sf.ID, Roots: sf.Roots, Store: f.Store, Head: f.Head}
	f.Issuer = &people.Issuer{CA: sf.CA, Index: f.Index}
	mux := http.NewServeMux()
	mux.Handle("/issuer/v1/people/", http.StripPrefix("/issuer", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		who := f.caller
		f.mu.Unlock()
		if who != "" {
			r = r.WithContext(people.WithCaller(r.Context(), who))
		}
		f.Issuer.ServeHTTP(w, r)
	})))
	mux.HandleFunc("GET /sso/token", func(w http.ResponseWriter, r *http.Request) {
		if f.SSO == nil {
			http.Error(w, "a one-person farm has no sign-in provider", http.StatusNotFound)
			return
		}
		q := r.URL.Query()
		var over map[string]any
		if q.Get("stepUp") != "" {
			over = StepUp(time.Now())
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"idToken": f.SSO.Token(time.Now(), q.Get("sub"), q.Get("nonce"), over)})
	})
	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Server.Close)
	f.URL = f.Server.URL + "/issuer"
	return f
}

// Studio is a studio farm signing in at the fake provider, its install's
// sign-in provider: dana (DanaSub), an admin, and noor (NoorSub). Reads
// are signed in as dana.
func Studio(t testing.TB) *Farm {
	t.Helper()
	f := newFarm(t)
	f.SSO = NewSSO(t, Issuer)
	f.Issuer.Provider = people.Provider{Issuer: Issuer, ClientID: ClientID}
	f.Issuer.HTTPClient = f.SSO.Client
	f.Issuer.SignIns = people.StaticSignIns{"dana": {{Issuer: Issuer, Subject: DanaSub}}, "noor": {{Issuer: Issuer, Subject: NoorSub}}}
	f.Issuer.Admin = func(_ context.Context, p string) bool { return p == "dana" }
	f.SetCaller("dana")
	return f
}

// OnePersonFarm is noor's one-person farm, her owner key and touch key
// enrolled as c4 farm up enrols them.
type OnePersonFarm struct {
	*Farm
	// InstallerToken is the machine's installer token (X-C4-Installer).
	InstallerToken string
	// Owner is noor's CLI key, and Touch its touch key.
	Owner, Touch *Device
}

// OnePerson is noor's farm with her owner key enrolled. Reads are signed
// in as noor.
func OnePerson(t testing.TB) *OnePersonFarm {
	t.Helper()
	o := &OnePersonFarm{Farm: newFarm(t), InstallerToken: randomHex(32), Owner: NewDevice(t), Touch: NewDevice(t)}
	o.Issuer.Owner = func(context.Context) string { return "noor" }
	o.Issuer.InstallerToken = []byte(o.InstallerToken)
	o.Issuer.LocalOnly = true // the server listens on 127.0.0.1, as a local farm's HUD does
	o.Issuer.Admin = func(_ context.Context, p string) bool { return p == "noor" }
	o.SetCaller("noor")
	touch := o.Enrolment(t, o.Touch, "noor", o.Owner.ID, "mac")
	var out people.OwnerCertified
	o.call(t, "/v1/people/owner", people.OwnerEnrolment{Owner: o.Enrolment(t, o.Owner, "noor", "", "mac"), Touch: &touch}, &out, people.InstallerHeader, o.InstallerToken)
	o.Owner.Cert, o.Touch.Cert = out.Owner, out.Touch
	return o
}

// NewCode books a fresh one-time code with the owner's key, as c4 farm
// open does, and returns it: it enrols one browser, within a minute.
func (o *OnePersonFarm) NewCode(t testing.TB) string {
	t.Helper()
	code := randomHex(32)
	k := o.Owner.Key(t)
	act := o.Scratch.SignAct(t, k, provtest.Act{Subject: provenance.Subject{Name: "farm:" + o.ID(), Digest: map[string]string{"c4": o.ID()}},
		Act: "browser-code", Params: map[string]string{"code": people.CodeHash(code)}, SignedAt: time.Now(),
		Chain: &schema.ActChain{By: schema.Actor{Person: "noor", Grade: schema.GradeSigned}, For: schema.For{Party: "noor", Grade: schema.GradeSigned}}})
	o.call(t, "/v1/people/acts", act, nil)
	return code
}

// call posts to the issuer in process, from the farm's own machine.
func (f *Farm) call(t testing.TB, path string, body, out any, hdr ...string) {
	t.Helper()
	b, ok := body.([]byte)
	if !ok {
		b, _ = json.Marshal(body)
	}
	req := httptest.NewRequest("POST", path, strings.NewReader(string(b)))
	req.RemoteAddr = "127.0.0.1:52144"
	for k := 0; k+1 < len(hdr); k += 2 {
		req.Header.Set(hdr[k], hdr[k+1])
	}
	w := httptest.NewRecorder()
	f.Issuer.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("POST %s: %d %s", path, w.Code, w.Body)
	}
	if out != nil {
		if err := json.Unmarshal(w.Body.Bytes(), out); err != nil {
			t.Fatal(err)
		}
	}
}

// Device is a scratch device key, and its certificate once enrolled.
type Device struct {
	P256 *ecdsa.PrivateKey
	SPKI []byte
	// ID is its device ID: the C4 ID of SPKI, an ID token's nonce.
	ID   string
	Cert *people.Certified
}

// NewDevice makes a device key.
func NewDevice(t testing.TB) *Device {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	spki, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return &Device{P256: key, SPKI: spki, ID: schema.DeviceID(spki)}
}

// Key is the device as a provtest signer, with the chain it was
// certified with.
func (d *Device) Key(t testing.TB) *provtest.Key {
	t.Helper()
	if d.Cert == nil {
		t.Fatal("the device is not enrolled")
	}
	k := &provtest.Key{Identity: d.Cert.Identity, Device: d.Cert.Device, P256: d.P256}
	for _, der := range d.Cert.Chain {
		c, err := x509.ParseCertificate(der)
		if err != nil {
			t.Fatal(err)
		}
		k.Chain = append(k.Chain, c)
	}
	return k
}

// Enrolment is d's request for person, at this farm, signed with d's key
// now; kind is browser, mac, windows or linux.
func (f *Farm) Enrolment(t testing.TB, d *Device, person, touchFor, kind string) people.Enrolment {
	t.Helper()
	nonce := make([]byte, 16)
	rand.Read(nonce)
	q := people.Request{Farm: f.ID(), Person: person, Key: base64.RawURLEncoding.EncodeToString(d.SPKI), TouchFor: touchFor,
		Nonce: base64.RawURLEncoding.EncodeToString(nonce), RequestedAt: time.Now().UTC().Format(time.RFC3339)}
	p := schema.DevicePredicate{Farm: q.Farm, Person: q.Person, Key: q.Key, TouchFor: q.TouchFor, Nonce: q.Nonce, RequestedAt: q.RequestedAt}
	sig, err := provenance.SignP256(d.P256, p.ProofMessage())
	if err != nil {
		t.Fatal(err)
	}
	protection := schema.ProtectionHardware
	if kind == "browser" {
		protection = schema.ProtectionBrowser
	}
	return people.Enrolment{Request: q, Proof: base64.RawURLEncoding.EncodeToString(sig), Protection: protection, Kind: kind, Label: person + "'s " + kind}
}

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}
