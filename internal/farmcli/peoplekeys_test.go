package farmcli

import (
	"context"
	"crypto/ecdsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Avalanche-io/c4-operator/internal/keystore"
	"github.com/Avalanche-io/c4-operator/internal/provenance"
	"github.com/Avalanche-io/c4-operator/internal/provenance/people"
	"github.com/Avalanche-io/c4-operator/internal/provenance/people/peopletest"
	"github.com/Avalanche-io/c4-operator/internal/provenance/provtest"
	"github.com/Avalanche-io/c4-operator/schema"
)

// ownerIssuer is a one-person farm's people's issuer, noor's, with no
// owner key enrolled yet: what c4 farm up finds.
type ownerIssuer struct {
	scratch *provtest.Farm
	index   *people.Index
	client  *Client
	mux     *http.ServeMux
}

const testInstaller = "6f1c3a9e0b7d4e2a8c5f1b3d7e9a0c2f4b6d8e0a2c4e6f8a0b2d4f6a8c0e2a4b"

func newOwnerIssuer(t *testing.T) *ownerIssuer {
	t.Helper()
	sf := provtest.NewFarm(t, time.Now())
	idx := &people.Index{Farm: sf.ID, Roots: sf.Roots, Store: &people.MemoryStore{}, Head: &people.MemoryHead{}}
	iss := &people.Issuer{CA: sf.CA, Index: idx, Owner: func(context.Context) string { return "noor" },
		InstallerToken: []byte(testInstaller), LocalOnly: true, Admin: func(_ context.Context, p string) bool { return p == "noor" }}
	mux := http.NewServeMux()
	mux.Handle("/issuer/v1/people/", http.StripPrefix("/issuer", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		iss.ServeHTTP(w, r.WithContext(people.WithCaller(r.Context(), "noor")))
	})))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c, err := NewClient(Farm{HUD: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	c.Installer = testInstaller
	return &ownerIssuer{scratch: sf, index: idx, client: c, mux: mux}
}

// up enrols the owner's key on the installer token, once, and keeps its
// certificate beside it, readable only by the person; asked again it
// answers from what it kept. An act signed with the key verifies, and the
// people's index finds its device in force.
func TestUpEnrolsTheOwnersKey(t *testing.T) {
	o := newOwnerIssuer(t)
	ctx := context.Background()
	st := keystore.Store{Dir: filepath.Join(t.TempDir(), "keys"), FileOnly: true}
	out, err := EnrolOwner(ctx, o.client, st, "noor")
	if err != nil {
		t.Fatal(err)
	}
	if out.Owner == nil || out.Owner.Identity != provenance.UserIdentity("noor") || out.Touch != nil {
		t.Fatalf("certified %+v", out)
	}
	fi, err := os.Stat(filepath.Join(st.Dir, "device.cert"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("the certificate: %v %v", fi, err)
	}
	again, err := EnrolOwner(ctx, o.client, st, "noor")
	if err != nil || again.Owner.Device != out.Owner.Device {
		t.Fatalf("up again: %v %+v", err, again)
	}

	key, err := st.Key(keystore.Device)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := LoadCert(st, keystore.Device)
	if err != nil || cert == nil {
		t.Fatal(err)
	}
	b, err := provenance.SignPersonAct(provenance.PersonAct{Farm: o.scratch.ID, Act: "approve", Person: "noor",
		Subject: provenance.ActSubject("workjob", "show-a", "sh040-light-v007", provtest.JobUID, provtest.JobSpecDigest),
		Params:  json.RawMessage(`{}`)}, key, deviceCert(cert), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	v, _, err := provenance.VerifyAct(b, o.scratch.Roots, time.Now(), provenance.NewMemorySeen())
	if err != nil {
		t.Fatal(err)
	}
	pos, err := o.index.Position(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := o.index.CheckAct(ctx, v, pos); err != nil {
		t.Errorf("the people's index: %v", err)
	}
}

// c4 farm open books a code with the owner's key; the browser it opens
// enrols its key with the code; the CLI reads back the key the code
// enrolled, the browser's, and countersigns it. A code no browser used
// reads as waiting.
func TestOpenEnrolsAndCountersignsTheBrowser(t *testing.T) {
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
	for _, c := range []struct{ inst, page, want string }{
		{"inst", "http://127.0.0.1:30080/", "http://127.0.0.1:30080/installer?code=inst&next=%2F#/?browser=" + code},
		{"inst", "http://127.0.0.1:30080/?as=artist#/needs", "http://127.0.0.1:30080/installer?code=inst&next=%2F%3Fas%3Dartist#/needs?browser=" + code},
		{"", "http://127.0.0.1:30080/#/jobs/x?tab=log", "http://127.0.0.1:30080/#/jobs/x?tab=log&browser=" + code},
	} {
		if u := BrowserURL("http://127.0.0.1:30080", c.inst, c.page, code); u != c.want {
			t.Errorf("the URL for %s: %s", c.page, u)
		}
	}
	if s, err := WatchCode(ctx, o.client, code, 0, time.Millisecond); err != nil || s.State != "waiting" {
		t.Fatalf("before the browser: %v %+v", err, s)
	}

	// the web app (K3), as it enrols with the code
	browser, err := keystore.Store{Dir: filepath.Join(t.TempDir(), "browser"), FileOnly: true}.Key(keystore.Device)
	if err != nil {
		t.Fatal(err)
	}
	e, err := enrolment(browser, o.scratch.ID, "noor", "")
	if err != nil {
		t.Fatal(err)
	}
	e.Kind, e.Protection, e.Code = "browser", schema.ProtectionBrowser, code
	if err := o.client.Issuer(ctx, http.MethodPost, "/v1/people/browser", e, nil); err != nil {
		t.Fatal(err)
	}

	s, err := WatchCode(ctx, o.client, code, time.Second, time.Millisecond)
	if err != nil || s.State != "enrolled" || s.Device != deviceID(browser) || s.Key != base64.RawURLEncoding.EncodeToString(browser.SPKI()) {
		t.Fatalf("after the browser: %v %+v", err, s)
	}
	if err := Countersign(ctx, o.client, st, o.scratch.ID, "noor", s); err != nil {
		t.Fatal(err)
	}
}

// p256 signs as a peopletest device does.
type p256 struct{ key *ecdsa.PrivateKey }

func (k p256) Sign(msg []byte) ([]byte, error) { return provenance.SignP256(k.key, msg) }

// The person approves a second machine's key from this one, confirmed with
// the touch key; the second machine enrols on the booked approval. The
// device ID is what the person compares: its short form, two groups of
// four.
func TestApproveAMachineWithATouch(t *testing.T) {
	o := peopletest.OnePerson(t)
	c, err := NewClient(Farm{HUD: o.Server.URL})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	other := peopletest.NewDevice(t)
	if got := ShortDevice(other.ID); len(got) != 9 || got[4] != ' ' || got[:4] != other.ID[2:6] {
		t.Errorf("short form %q of %s", got, other.ID)
	}
	booked, err := approveWith(ctx, c, o.ID(), other.ID, p256{o.Owner.P256}, o.Owner.Cert, p256{o.Touch.P256}, o.Touch.Cert)
	if err != nil {
		t.Fatal(err)
	}
	e := o.Enrolment(t, other, "noor", "", "linux")
	e.Approval = booked.Record
	var cert people.Certified
	if err := c.Issuer(ctx, http.MethodPost, "/v1/people/enroll", e, &cert); err != nil || cert.Device != other.ID {
		t.Errorf("enrolling on the approval: %v %+v", err, cert)
	}
	st := keystore.Store{Dir: filepath.Join(t.TempDir(), "keys"), FileOnly: true}
	if _, err := ApproveDevice(ctx, c, st, o.ID(), "laptop"); err == nil {
		t.Error("an approval of something that is no device ID")
	}
}

// up's keys step: the owner's key enrolled and answered, the same again
// with it pinned; a farm whose owner key is not the pinned one is refused,
// as is nothing from a farm with no people's issuer, which up goes on
// without.
func TestUpKeysPinsTheOwnersKey(t *testing.T) {
	o := newOwnerIssuer(t)
	ctx := context.Background()
	st := keystore.Store{Dir: filepath.Join(t.TempDir(), "keys"), FileOnly: true}
	out, err := UpKeys(ctx, o.client, st, "noor", "", false)
	if err != nil || out.Owner == nil || out.Touch != nil {
		t.Fatalf("up: %v %+v", err, out)
	}
	if again, err := UpKeys(ctx, o.client, st, "noor", out.Owner.Device, false); err != nil || again.Owner.Device != out.Owner.Device {
		t.Errorf("up again, pinned: %v %+v", err, again)
	}
	other := "c4" + out.Owner.Device[2:len(out.Owner.Device)-1] + "x"
	if _, err := UpKeys(ctx, o.client, st, "noor", other, false); !errors.Is(err, ErrOwnerKey) {
		t.Errorf("another owner key than the pinned one: %v", err)
	}

	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	c, err := NewClient(Farm{HUD: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := UpKeys(ctx, c, st, "noor", "", true); !errors.Is(err, ErrNoIssuer) {
		t.Errorf("a farm with no people's issuer: %v", err)
	}
}

// c4 farm open on a one-person farm: the browser's code rides in the
// address's fragment; the web app enrols its key with it; the CLI
// countersigns the key the code enrolled and prints it. A browser already
// enrolled ignores the code, and open doesn't wait long for it. A HUD whose
// web app enrols no browser opens as before.
func TestOpenBrowserCountersignsWhatEnrolled(t *testing.T) {
	o := newOwnerIssuer(t)
	o.mux.HandleFunc("/js/enrol.js", func(w http.ResponseWriter, r *http.Request) {})
	ctx := context.Background()
	st := keystore.Store{Dir: filepath.Join(t.TempDir(), "keys"), FileOnly: true}
	if _, err := EnrolOwner(ctx, o.client, st, "noor"); err != nil {
		t.Fatal(err)
	}
	browser, err := keystore.Store{Dir: filepath.Join(t.TempDir(), "browser"), FileOnly: true}.Key(keystore.Device)
	if err != nil {
		t.Fatal(err)
	}
	enrolled := false
	// the web app (K3): the code out of the fragment's query, its key
	// enrolled with it, once
	web := func(u string) error {
		_, frag, _ := strings.Cut(u, "#")
		_, q, _ := strings.Cut(frag, "?")
		v, _ := url.ParseQuery(q)
		if enrolled || v.Get("browser") == "" {
			return nil
		}
		e, err := enrolment(browser, o.scratch.ID, "noor", "")
		if err != nil {
			return err
		}
		e.Kind, e.Protection, e.Code = "browser", schema.ProtectionBrowser, v.Get("browser")
		enrolled = true
		return o.client.Issuer(ctx, http.MethodPost, "/v1/people/browser", e, nil)
	}
	var said strings.Builder
	a := OpenArgs{Store: st, Page: o.client.Base + "/#/needs", Open: web, Wait: 2 * time.Second, Poll: time.Millisecond,
		Say: func(f string, v ...any) { fmt.Fprintf(&said, f, v...) }}
	if err := OpenBrowser(ctx, o.client, a); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(said.String(), "its key is "+ShortDevice(deviceID(browser))+", countersigned") {
		t.Errorf("said: %s", said.String())
	}
	if !hasBrowser(ctx, o.client, "noor") {
		t.Error("no countersigned browser in the device list")
	}
	said.Reset()
	defer func(w time.Duration) { enrolledWait = w }(enrolledWait)
	enrolledWait = 50 * time.Millisecond
	start := time.Now()
	a.Wait = time.Minute
	if err := OpenBrowser(ctx, o.client, a); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 10*time.Second || said.Len() != 0 {
		t.Errorf("an enrolled browser: waited %v, said %q", time.Since(start), said.String())
	}

	// a HUD with no browser enrolment opens the page as it is
	plain := newOwnerIssuer(t)
	pst := keystore.Store{Dir: filepath.Join(t.TempDir(), "plain"), FileOnly: true}
	if _, err := EnrolOwner(ctx, plain.client, pst, "noor"); err != nil {
		t.Fatal(err)
	}
	var opened string
	plain.client.Installer = ""
	if err := OpenBrowser(ctx, plain.client, OpenArgs{Store: pst, Page: plain.client.Base + "/#/needs", Open: func(u string) error { opened = u; return nil }}); err != nil {
		t.Fatal(err)
	}
	if opened != plain.client.Base+"/#/needs" {
		t.Errorf("opened %s", opened)
	}
}
