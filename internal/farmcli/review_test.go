package farmcli

// The K4 review's probes, kept as tests that must refuse: each was a way
// a compromised HUD, or a local process, got past the CLI.

import (
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"

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
