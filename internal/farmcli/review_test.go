package farmcli

// The K4 review's probes, kept as tests that must refuse: each was a way
// a compromised HUD, or a local process, got past the CLI.

import (
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"

	"github.com/Avalanche-io/c4-operator/hud/api"
	"github.com/Avalanche-io/c4-operator/internal/farmbook"
	"github.com/Avalanche-io/c4-operator/internal/keystore"
	"github.com/Avalanche-io/c4-operator/internal/provenance"
	"github.com/Avalanche-io/c4-operator/internal/provenance/people"
	"github.com/Avalanche-io/c4-operator/internal/provenance/provtest"
	"github.com/Avalanche-io/c4-operator/schema"
)

// rewriting fronts o's HUD, letting a test rewrite the people's index
// pages it serves.
func rewriting(t *testing.T, o *ownerIssuer, rewrite func(body []byte) (int, []byte)) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := httptest.NewRecorder()
		o.mux.ServeHTTP(rec, r)
		code, body := rec.Code, rec.Body.Bytes()
		if strings.HasPrefix(r.URL.Path, "/issuer/v1/people/index") {
			code, body = rewrite(body)
		}
		for k, v := range rec.Header() {
			w.Header()[k] = v
		}
		w.WriteHeader(code)
		w.Write(body)
	}))
	t.Cleanup(srv.Close)
	c, err := NewClient(Farm{HUD: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	c.Installer = testInstaller
	return c
}

// withNewDevice is a one-person farm where this machine read the index,
// and a browser of the owner's then enrolled.
func withNewDevice(t *testing.T) (*ownerIssuer, keystore.Store, string) {
	t.Helper()
	o := newOwnerIssuer(t)
	ctx := context.Background()
	st := keystore.Store{Dir: filepath.Join(t.TempDir(), "keys"), FileOnly: true}
	if _, err := EnrolOwner(ctx, o.client, st, "noor"); err != nil {
		t.Fatal(err)
	}
	cfg, _ := PeopleConfig(ctx, o.client)
	if ns, err := NewDevices(ctx, o.client, st, cfg); err != nil || len(ns) != 0 {
		t.Fatalf("the first run: %v %v", err, ns)
	}
	code, err := BookBrowserCode(ctx, o.client, st, o.scratch.ID, "noor")
	if err != nil {
		t.Fatal(err)
	}
	k, _ := keystore.Store{Dir: filepath.Join(t.TempDir(), "b"), FileOnly: true}.Key(keystore.Device)
	e, _ := enrolment(k, o.scratch.ID, "noor", "")
	e.Kind, e.Protection, e.Code = "browser", schema.ProtectionBrowser, code
	if err := o.client.Issuer(ctx, http.MethodPost, "/v1/people/browser", e, nil); err != nil {
		t.Fatal(err)
	}
	return o, st, deviceID(k)
}

// Probe P2: when the index can't be read, or its head doesn't verify,
// each run says so: silence is how a HUD hides a device.
func TestKeepKeysSaysWhenTheIndexFails(t *testing.T) {
	for _, mode := range []string{"503", "a bad signature"} {
		o, st, _ := withNewDevice(t)
		c := rewriting(t, o, func(body []byte) (int, []byte) {
			if mode == "503" {
				return http.StatusServiceUnavailable, []byte(`{"error":"down"}`)
			}
			var p people.IndexPage
			json.Unmarshal(body, &p)
			if len(p.Signed) > 0 {
				p.Signed[len(p.Signed)/2] ^= 1
			}
			b, _ := json.Marshal(p)
			return http.StatusOK, b
		})
		var said strings.Builder
		KeepKeys(context.Background(), c, KeepArgs{Store: st, Say: func(f string, v ...any) { fmt.Fprintf(&said, f, v...) }})
		if !strings.Contains(said.String(), "devices   ") {
			t.Errorf("%s: said nothing of devices: %q", mode, said.String())
		}
	}
}

// Probe P3: an index entry rewritten by the HUD (here, whose device it
// is) is not the entry its ID names, and is refused, not believed.
func TestRewrittenIndexEntryRefused(t *testing.T) {
	o, st, dev := withNewDevice(t)
	c := rewriting(t, o, func(body []byte) (int, []byte) {
		var p people.IndexPage
		json.Unmarshal(body, &p)
		for _, e := range p.Entries {
			if e.Entry != nil && e.Entry.Device == dev {
				e.Entry.Person = "someone-else"
			}
		}
		b, _ := json.Marshal(p)
		return http.StatusOK, b
	})
	cfg, _ := PeopleConfig(context.Background(), c)
	if ns, err := NewDevices(context.Background(), c, st, cfg); err == nil {
		t.Errorf("a rewritten entry: no error, notices %v", ns)
	}
	// the farm as it is tells of the device
	if ns, err := NewDevices(context.Background(), o.client, st, cfg); err != nil || len(ns) != 1 || ns[0].Device != dev {
		t.Errorf("the farm as it is: %v %+v", err, ns)
	}
}

// Probe P8: on a farm that keeps a farm book, the CLI reads the book as a
// verifier does (farmbook.CheckPage), so its runs raise no false alarm,
// and a new device sealed in the book is told.
func TestNoticesOnABookFarm(t *testing.T) {
	sf := provtest.NewFarm(t, time.Now())
	store, ptr, own := &farmbook.MemoryStore{}, &farmbook.MemoryPointer{}, &farmbook.MemorySealer{}
	queue, err := provenance.NewSigner(context.Background(), func(_ context.Context, pub ed25519.PublicKey) ([]*x509.Certificate, error) {
		c, err := sf.CA.Issue(pub, provenance.ComponentIdentity("queue"), 48*time.Hour, time.Now())
		return []*x509.Certificate{c, sf.CA.Cert}, err
	})
	if err != nil {
		t.Fatal(err)
	}
	reader := &farmbook.Reader{Farm: sf.ID, Roots: sf.Roots, Store: store, Pointer: ptr, Own: own}
	idx := &people.Index{Farm: sf.ID, Roots: sf.Roots, Store: store, Head: &people.MemoryHead{}, Book: reader}
	sealer := &farmbook.Sealer{Farm: sf.ID, Roots: sf.Roots, Store: store, Pointer: ptr, Own: own, Signer: queue, Log: logr.Discard(),
		Now: time.Now, Checkers: func() []farmbook.Checker {
			return []farmbook.Checker{people.NewBookChecker(sf.ID, sf.Roots, store), &farmbook.ControlChecker{Farm: sf.ID, Roots: sf.Roots, Store: store}}
		}}
	iss := &people.Issuer{CA: sf.CA, Index: idx, Owner: func(context.Context) string { return "noor" },
		InstallerToken: []byte(testInstaller), LocalOnly: true, Admin: func(_ context.Context, p string) bool { return p == "noor" }}
	mux := http.NewServeMux()
	mux.Handle("/issuer/v1/people/", http.StripPrefix("/issuer", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		iss.ServeHTTP(w, r.WithContext(people.WithCaller(r.Context(), "noor")))
	})))
	// the HUD's GET /book/farm, as it serves the book
	mux.HandleFunc("GET /api/v1/book/farm", func(w http.ResponseWriter, r *http.Request) {
		since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
		v, err := reader.View(r.Context(), time.Now())
		if err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		json.NewEncoder(w).Encode(farmbook.NewPage(v, since, 100))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c, _ := NewClient(Farm{HUD: srv.URL})
	c.Installer = testInstaller
	ctx := context.Background()
	st := keystore.Store{Dir: filepath.Join(t.TempDir(), "keys"), FileOnly: true}
	if _, err := EnrolOwner(ctx, c, st, "noor"); err != nil {
		t.Fatal(err)
	}
	if _, err := sealer.Seal(ctx); err != nil {
		t.Fatal(err)
	}
	var said strings.Builder
	say := func(f string, v ...any) { fmt.Fprintf(&said, f, v...) }
	for range 2 {
		KeepKeys(ctx, c, KeepArgs{Store: st, Say: say})
	}
	if strings.Contains(said.String(), "devices") {
		t.Errorf("a book farm with nothing new: %q", said.String())
	}
	code, err := BookBrowserCode(ctx, c, st, sf.ID, "noor")
	if err != nil {
		t.Fatal(err)
	}
	k, _ := keystore.Store{Dir: filepath.Join(t.TempDir(), "b"), FileOnly: true}.Key(keystore.Device)
	e, _ := enrolment(k, sf.ID, "noor", "")
	e.Kind, e.Protection, e.Code = "browser", schema.ProtectionBrowser, code
	if err := c.Issuer(ctx, http.MethodPost, "/v1/people/browser", e, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := sealer.Seal(ctx); err != nil {
		t.Fatal(err)
	}
	said.Reset()
	KeepKeys(ctx, c, KeepArgs{Store: st, Say: say})
	if !strings.Contains(said.String(), "a new device signed in as you") || !strings.Contains(said.String(), ShortDevice(deviceID(k))) {
		t.Errorf("the browser sealed in the book: %q", said.String())
	}
}

// Probe P4: the pin is checked against the farm, by its own index: up on
// a farm made again, whose owner some other key enrolled first, is
// refused, though this machine kept certificates that match the pin.
func TestPinCheckedAgainstTheFarm(t *testing.T) {
	ctx := context.Background()
	a := newOwnerIssuer(t)
	st := keystore.Store{Dir: filepath.Join(t.TempDir(), "keys"), FileOnly: true}
	outA, err := UpKeys(ctx, a.client, st, "noor", "", false)
	if err != nil {
		t.Fatal(err)
	}
	pinned := outA.Owner.Device
	if _, err := UpKeys(ctx, a.client, st, "noor", pinned, false); err != nil {
		t.Fatalf("the same farm again: %v", err)
	}
	b := newOwnerIssuer(t)
	other := keystore.Store{Dir: filepath.Join(t.TempDir(), "other"), FileOnly: true}
	if _, err := EnrolOwner(ctx, b.client, other, "noor"); err != nil {
		t.Fatal(err)
	}
	if _, err := UpKeys(ctx, b.client, st, "noor", pinned, false); !errors.Is(err, ErrOwnerKey) {
		t.Errorf("a farm whose owner is another key: %v", err)
	}
	cfg, _ := PeopleConfig(ctx, b.client)
	if err := CheckOwner(ctx, b.client, other, cfg, pinned); !errors.Is(err, ErrOwnerKey) {
		t.Errorf("CheckOwner on that farm: %v", err)
	}
}

// Probe P6, the farm: an act names the farm this machine's key was
// certified at, from its own chain, whatever the HUD's config says.
func TestActsNameTheFarmOfTheirKey(t *testing.T) {
	o := newOwnerIssuer(t)
	ctx := context.Background()
	st := keystore.Store{Dir: filepath.Join(t.TempDir(), "keys"), FileOnly: true}
	if _, err := EnrolOwner(ctx, o.client, st, "noor"); err != nil {
		t.Fatal(err)
	}
	if _, err := BookBrowserCode(ctx, o.client, st, "c4someotherfarm", "noor"); err == nil || !strings.Contains(err.Error(), "certified at") {
		t.Errorf("an act for a farm the HUD named: %v", err)
	}
}

// Probe P6, the subject: asked to revoke show-a/g1, the CLI signs nothing
// on a subject the HUD served for another grant, and sends no revoke.
func TestServedSubjectCheckedAgainstWhatWasNamed(t *testing.T) {
	o := newOwnerIssuer(t)
	ctx := context.Background()
	st := keystore.Store{Dir: filepath.Join(t.TempDir(), "keys"), FileOnly: true}
	if _, err := EnrolOwner(ctx, o.client, st, "noor"); err != nil {
		t.Fatal(err)
	}
	o.mux.HandleFunc("GET /api/v1/acts/needs", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(api.ActNeeds{Acts: map[string]api.ActNeed{"revoke": {Person: true}}})
	})
	o.mux.HandleFunc("GET /api/v1/grants", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(api.GrantList{Grants: []api.Grant{{Namespace: "show-a", Name: "g1",
			ActOn: &api.ActOn{Name: "submitgrant:show-b/OTHER@uid", Digest: map[string]string{"sha256": strings.Repeat("0", 64)}}}}})
	})
	sent := false
	o.mux.HandleFunc("DELETE /api/v1/grants/show-a/g1", func(w http.ResponseWriter, r *http.Request) {
		sent = true
		w.Write([]byte(`{"message":"ok"}`))
	})
	o.client.Acts = &Acts{Store: st, Present: true}
	if err := Revoke(ctx, o.client, Out{W: new(strings.Builder)}, "show-a/g1"); err == nil || !strings.Contains(err.Error(), "refused, nothing was signed") || sent {
		t.Errorf("another grant's subject: %v (sent %v)", err, sent)
	}
	if err := checkActOn(&api.ActOn{Name: "submitgrant:show-a/g1@9b0e0001", Digest: map[string]string{"sha256": strings.Repeat("ab", 32)}}, "submitgrant", "show-a", "g1"); err != nil {
		t.Errorf("the grant itself: %v", err)
	}
	if err := checkActOn(&api.ActOn{Name: "project:show-b@u1", Digest: map[string]string{"sha256": strings.Repeat("ab", 32)}}, "project", "", "show-a"); err == nil {
		t.Error("another project's subject")
	}
}

// Probe P1: a local process reads the code from open's argv and uses it
// first. The person compares the short ID the CLI prints with the one the
// browser shows, so the other key is not countersigned: it is removed,
// and nothing countersigned. With no one at the terminal nothing is.
func TestOpenCountersignsOnlyWhatThePersonSees(t *testing.T) {
	o := newOwnerIssuer(t)
	o.mux.HandleFunc("/js/enrol.js", func(w http.ResponseWriter, r *http.Request) {})
	ctx := context.Background()
	st := keystore.Store{Dir: filepath.Join(t.TempDir(), "keys"), FileOnly: true}
	if _, err := EnrolOwner(ctx, o.client, st, "noor"); err != nil {
		t.Fatal(err)
	}
	enrol := func(code string) keystore.Key {
		k, _ := keystore.Store{Dir: filepath.Join(t.TempDir(), "b"), FileOnly: true}.Key(keystore.Device)
		e, _ := enrolment(k, o.scratch.ID, "noor", "")
		e.Kind, e.Protection, e.Code = "browser", schema.ProtectionBrowser, code
		if err := o.client.Issuer(ctx, http.MethodPost, "/v1/people/browser", e, nil); err != nil {
			return nil
		}
		return k
	}
	var attacker, real keystore.Key
	open := func(u string) error {
		_, frag, _ := strings.Cut(u, "#")
		_, q, _ := strings.Cut(frag, "?")
		code := strings.TrimPrefix(q, "browser=")
		attacker = enrol(code) // first, from ps
		real, _ = keystore.Store{Dir: filepath.Join(t.TempDir(), "real"), FileOnly: true}.Key(keystore.Device)
		return nil
	}
	var said strings.Builder
	err := OpenBrowser(ctx, o.client, OpenArgs{Store: st, Page: o.client.Base + "/#/needs", Open: open, Wait: 2 * time.Second, Poll: time.Millisecond,
		Say:     func(f string, v ...any) { fmt.Fprintf(&said, f, v...) },
		Confirm: func(q string) bool { return strings.Contains(q, ShortDevice(deviceID(real))) }})
	if err != nil || attacker == nil {
		t.Fatalf("open: %v (the other key enrolled: %v)", err, attacker != nil)
	}
	if strings.Contains(said.String(), "countersigned;") || !strings.Contains(said.String(), "is removed") {
		t.Errorf("said %q", said.String())
	}
	if hasBrowser(ctx, o.client, "noor") {
		t.Error("a countersigned browser after the person said no")
	}
	var out struct{ Devices []people.DeviceView }
	o.client.Issuer(ctx, http.MethodGet, "/v1/people/devices?person=noor", nil, &out)
	for _, d := range out.Devices {
		if d.Device == deviceID(attacker) {
			t.Error("the other key is still in force")
		}
	}
}

// A busy issuer answers 429 with Retry-After: WatchCode waits as long as
// it says and reads again, rather than failing c4 farm open; a 429 that
// lasts past the deadline is a code still waiting.
func TestWatchCodeRidesOutA429(t *testing.T) {
	o := newOwnerIssuer(t)
	ctx := context.Background()
	st := keystore.Store{Dir: filepath.Join(t.TempDir(), "keys"), FileOnly: true}
	if _, err := EnrolOwner(ctx, o.client, st, "noor"); err != nil {
		t.Fatal(err)
	}
	code, err := BookBrowserCode(ctx, o.client, st, o.scratch.ID, "noor")
	if err != nil {
		t.Fatal(err)
	}
	busy := 2
	limited := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/issuer/v1/people/codes/") && busy > 0 {
			busy--
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"error":"slow down"}`))
			return
		}
		o.mux.ServeHTTP(w, r)
	}))
	defer limited.Close()
	c, _ := NewClient(Farm{HUD: limited.URL})
	c.Installer = testInstaller
	start := time.Now()
	s, err := WatchCode(ctx, c, code, 3*time.Second, time.Millisecond)
	if err != nil || s.State != "waiting" || time.Since(start) < 2*time.Second {
		t.Errorf("after two 429s: %v %+v, waited %v", err, s, time.Since(start))
	}
	busy = 1000
	if s, err := WatchCode(ctx, c, code, 50*time.Millisecond, time.Millisecond); err != nil || s.State != "waiting" {
		t.Errorf("a 429 past the deadline: %v %+v", err, s)
	}
	if d := retryAfter("2"); d != 2*time.Second {
		t.Errorf("Retry-After 2: %v", d)
	}
}

// An issuer read answered 429 is asked again after its Retry-After; a
// write is never repeated.
func TestIssuerReadsRideOutA429WritesDont(t *testing.T) {
	calls := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls[r.Method]++
		if calls[r.Method] == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	c, _ := NewClient(Farm{HUD: srv.URL})
	if err := c.Issuer(context.Background(), http.MethodGet, "/v1/people/config", nil, &map[string]any{}); err != nil || calls["GET"] != 2 {
		t.Errorf("a read: %v after %d calls", err, calls["GET"])
	}
	if err := c.Issuer(context.Background(), http.MethodPost, "/v1/people/acts", map[string]any{}, nil); err == nil || calls["POST"] != 1 {
		t.Errorf("a write: %v after %d calls", err, calls["POST"])
	}
}
