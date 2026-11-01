package farmcli

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Avalanche-io/c4-operator/internal/keystore"
	"github.com/Avalanche-io/c4-operator/internal/provenance"
	"github.com/Avalanche-io/c4-operator/internal/provenance/people"
	"github.com/Avalanche-io/c4-operator/internal/provenance/provtest"
	"github.com/Avalanche-io/c4-operator/schema"
)

// ownerIssuer is a one-person farm's people's issuer, noor's, with no
// owner key enrolled yet: what c4 farm up finds.
type ownerIssuer struct {
	scratch *provtest.Farm
	index   *people.Index
	client  *Client
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
	return &ownerIssuer{scratch: sf, index: idx, client: c}
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
	if u := BrowserURL("http://127.0.0.1:30080", "inst", code); u != "http://127.0.0.1:30080/installer?code=inst&next=%2F#/?browser="+code {
		t.Errorf("the URL: %s", u)
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
