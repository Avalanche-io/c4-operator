package farmcli

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Avalanche-io/c4-operator/internal/keystore"
	"github.com/Avalanche-io/c4-operator/internal/provenance"
	"github.com/Avalanche-io/c4-operator/internal/provenance/people"
	"github.com/Avalanche-io/c4-operator/internal/provenance/people/peopletest"
)

// fakeAuth is a provider's authorize and token endpoints in front of
// peopletest's fake provider: authorize sends the browser back to the
// redirect with a code; the token endpoint trades it, with the PKCE
// verifier, for an ID token whose nonce is the one the request carried.
type fakeAuth struct {
	mu    sync.Mutex
	sso   *peopletest.SSO
	codes map[string]url.Values
	sub   string
}

func (f *fakeAuth) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.URL.Path {
	case "/authorize":
		q := r.URL.Query()
		if q.Get("client_id") != peopletest.ClientID || q.Get("code_challenge_method") != "S256" || q.Get("nonce") == "" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		code := fmt.Sprintf("code-%d", len(f.codes))
		f.codes[code] = q
		http.Redirect(w, r, q.Get("redirect_uri")+"?code="+code+"&state="+url.QueryEscape(q.Get("state")), http.StatusFound)
	case "/token":
		r.ParseForm()
		q, ok := f.codes[r.PostForm.Get("code")]
		sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
		if !ok || base64.RawURLEncoding.EncodeToString(sum[:]) != q.Get("code_challenge") || r.PostForm.Get("redirect_uri") != q.Get("redirect_uri") {
			http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"id_token": f.sso.Token(time.Now(), f.sub, q.Get("nonce"), nil)})
	default:
		http.NotFound(w, r)
	}
}

// c4 farm login: the person signs in in their browser; the provider sends
// it back to the CLI's loopback port with a code; the CLI trades it with
// its PKCE verifier for an ID token whose nonce is its key's device ID,
// and the farm certifies the key on it. The token names this key alone: a
// key the token's nonce doesn't name is refused.
func TestLoginCertifiesThisMachinesKey(t *testing.T) {
	f := peopletest.Studio(t)
	auth := &fakeAuth{sso: f.SSO, codes: map[string]url.Values{}, sub: peopletest.DanaSub}
	provider := httptest.NewServer(auth)
	defer provider.Close()
	c, err := NewClient(Farm{HUD: f.Server.URL})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	cfg, err := PeopleConfig(ctx, c)
	if err != nil || cfg.Provider == nil {
		t.Fatalf("config: %v %+v", err, cfg)
	}
	st := keystore.Store{Dir: filepath.Join(t.TempDir(), "keys"), FileOnly: true}
	var said strings.Builder
	a := LoginArgs{Store: st, Farm: cfg, Provider: provider.Client(),
		Endpoints: OIDCEndpoints{Authorization: provider.URL + "/authorize", Token: provider.URL + "/token"},
		// the browser: follow the provider's redirect back to the CLI
		Open: func(u string) error {
			resp, err := provider.Client().Get(u)
			if err == nil {
				resp.Body.Close()
			}
			return err
		},
		Wait: 10 * time.Second, Say: func(format string, v ...any) { fmt.Fprintf(&said, format, v...) }}
	cert, err := Login(ctx, c, a)
	if err != nil {
		t.Fatal(err)
	}
	key, _ := st.Key(keystore.Device)
	if cert.Identity != provenance.UserIdentity("dana") || cert.Device != deviceID(key) {
		t.Errorf("certified %+v", cert)
	}
	if kept, err := LoadCert(st, keystore.Device); err != nil || kept == nil || kept.Record != cert.Record {
		t.Errorf("the kept certificate: %v %+v", err, kept)
	}
	if !strings.Contains(said.String(), "signed in as dana\n") || !strings.Contains(said.String(), "ssh -L ") {
		t.Errorf("no SSH recipe for a machine with no browser:\n%s", said.String())
	}

	// a token whose nonce names another key certifies nothing
	other := keystore.Store{Dir: filepath.Join(t.TempDir(), "other"), FileOnly: true}
	okey, _ := other.Key(keystore.Device)
	e, err := enrolment(okey, cfg.Farm, "dana", "")
	if err != nil {
		t.Fatal(err)
	}
	e.IDToken = f.SSO.Token(time.Now(), peopletest.DanaSub, deviceID(key), nil)
	if err := c.Issuer(ctx, http.MethodPost, "/v1/people/enroll", e, nil); err == nil {
		t.Error("a token for another key certified this one")
	}
}

// loginAs signs dana in on a studio farm, through the loopback flow, and
// returns the client, the farm's config and her keys on this machine.
func loginAs(t *testing.T, f *peopletest.Farm) (*Client, *people.Config, keystore.Store) {
	t.Helper()
	auth := &fakeAuth{sso: f.SSO, codes: map[string]url.Values{}, sub: peopletest.DanaSub}
	provider := httptest.NewServer(auth)
	t.Cleanup(provider.Close)
	c, err := NewClient(Farm{HUD: f.Server.URL})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := PeopleConfig(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	st := keystore.Store{Dir: filepath.Join(t.TempDir(), "keys"), FileOnly: true}
	_, err = Login(context.Background(), c, LoginArgs{Store: st, Farm: cfg, Provider: provider.Client(),
		Endpoints: OIDCEndpoints{Authorization: provider.URL + "/authorize", Token: provider.URL + "/token"},
		Open: func(u string) error {
			resp, err := provider.Client().Get(u)
			if err == nil {
				resp.Body.Close()
			}
			return err
		}, Wait: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return c, cfg, st
}

// The CLI reads the people's index itself and tells its person of a
// device enrolled as them since, its ID token checked against the
// provider key the index booked; it says each once. A head that goes back
// from one it saw signed is refused.
func TestNewDevicesAreToldFromTheIndex(t *testing.T) {
	f := peopletest.Studio(t)
	c, cfg, st := loginAs(t, f)
	ctx := context.Background()
	if ns, err := NewDevices(ctx, c, st, cfg); err != nil || len(ns) != 0 {
		t.Fatalf("before another device: %v %+v", err, ns)
	}
	// another device signs in as dana
	d := peopletest.NewDevice(t)
	e := f.Enrolment(t, d, "dana", "", "browser")
	e.IDToken = f.SSO.Token(time.Now(), peopletest.DanaSub, d.ID, nil)
	if err := c.Issuer(ctx, http.MethodPost, "/v1/people/enroll", e, nil); err != nil {
		t.Fatal(err)
	}
	ns, err := NewDevices(ctx, c, st, cfg)
	if err != nil || len(ns) != 1 || ns[0].Device != d.ID || ns[0].Problem != "" || ns[0].Kind != "browser" {
		t.Fatalf("the new device: %v %+v", err, ns)
	}
	if !strings.Contains(ns[0].String(), "a new device signed in as you: dana's browser (browser)") {
		t.Errorf("said: %s", ns[0])
	}
	if again, err := NewDevices(ctx, c, st, cfg); err != nil || len(again) != 0 {
		t.Errorf("told twice: %v %+v", err, again)
	}
	// this machine saw a head further on than the farm now serves
	w, _ := loadWatch(st)
	w.HeadPosition += 5
	w.Position = w.HeadPosition
	saveWatch(st, w)
	if _, err := NewDevices(ctx, c, st, cfg); !errors.Is(err, ErrIndexRollback) {
		t.Errorf("a head that goes back: %v", err)
	}
}
