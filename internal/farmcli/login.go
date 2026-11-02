package farmcli

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Avalanche-io/c4-operator/internal/keystore"
	"github.com/Avalanche-io/c4-operator/internal/provenance"
	"github.com/Avalanche-io/c4-operator/internal/provenance/people"
)

// c4 farm login (PEOPLE-KEYS-2 3.2, K4): the person signs in at the
// studio's sign-in provider in their browser, as they would anyway, and
// this machine's key is certified on the ID token. The CLI makes its key
// first and sends the provider its device ID as the nonce, so the token
// names this key and no other (a loopback redirect, RFC 8252, with PKCE).
// There is no device flow: no provider takes a nonce in one, so on a
// machine with no browser the person forwards the loopback port over SSH.

// OIDCEndpoints are a sign-in provider's endpoints, as its discovery names
// them.
type OIDCEndpoints struct {
	Authorization string `json:"authorization_endpoint"`
	Token         string `json:"token_endpoint"`
}

// Discover reads a provider's endpoints from its discovery document.
func Discover(ctx context.Context, hc *http.Client, issuer string) (OIDCEndpoints, error) {
	var e OIDCEndpoints
	if err := secureURL("the sign-in provider", issuer); err != nil {
		return e, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(issuer, "/")+"/.well-known/openid-configuration", nil)
	if err != nil {
		return e, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return e, fmt.Errorf("the sign-in provider %s: %w", issuer, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return e, fmt.Errorf("the sign-in provider %s: %s", issuer, resp.Status)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&e); err != nil {
		return e, err
	}
	if e.Authorization == "" || e.Token == "" {
		return e, fmt.Errorf("the sign-in provider %s names no authorization or token endpoint", issuer)
	}
	for what, u := range map[string]string{"its authorization endpoint": e.Authorization, "its token endpoint": e.Token} {
		if err := secureURL("the sign-in provider's "+what, u); err != nil {
			return e, err
		}
	}
	return e, nil
}

// secureURL refuses a sign-in address that isn't https: a token or code
// over plain HTTP is anyone's on the way. Only this machine's own loopback
// address may be http, for a test.
func secureURL(what, raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("%s %q is not an address", what, Printable(raw))
	}
	switch h := u.Hostname(); {
	case u.Scheme == "https":
		return nil
	case u.Scheme == "http" && (h == "localhost" || h == "127.0.0.1" || h == "::1"):
		return nil
	}
	return fmt.Errorf("%s %q is not https: refused", what, Printable(raw))
}

// LoginArgs are c4 farm login's.
type LoginArgs struct {
	// Store is the farm's keys on this machine.
	Store keystore.Store
	// Farm is the people's issuer's config: the farm's ID and its provider.
	Farm *people.Config
	// Endpoints are the provider's (Discover); Provider reaches it.
	Endpoints OIDCEndpoints
	Provider  *http.Client
	// Open opens the person's browser at a URL.
	Open func(string) error
	// Port is the loopback port to listen on; 0 picks one. A machine with
	// no browser fixes it, and the person forwards it over SSH.
	Port int
	// Wait bounds the sign-in.
	Wait time.Duration
	// Say tells the person what to do.
	Say func(format string, a ...any)
	// Prompt is the provider's prompt: "none" for a renewal's silent round
	// trip, once a sign-in session is open in the browser.
	Prompt string
	// Interactive is a renewal the person asked for (c4 farm login), which
	// may ask them to sign in.
	Interactive bool
}

// Login signs the person in at the farm's provider and enrols this
// machine's device key on the ID token, keeping its certificate beside it.
func Login(ctx context.Context, c *Client, a LoginArgs) (*people.Certified, error) {
	if a.Farm == nil || a.Farm.Provider == nil {
		return nil, errors.New("this farm has no sign-in provider: a one-person farm's key is c4 farm up's")
	}
	key, err := a.Store.Key(keystore.Device)
	if err != nil {
		return nil, err
	}
	token, err := signIn(ctx, a, deviceID(key))
	if err != nil {
		return nil, err
	}
	// whose sign-in it is, as the farm names them: no one types a name, and
	// the request the key signs names that person
	var who people.WhoamiAnswer
	if err := c.Issuer(ctx, http.MethodPost, "/v1/people/whoami", people.Whoami{IDToken: token}, &who); err != nil {
		return nil, fmt.Errorf("whose sign-in this is: %w", err)
	}
	e, err := enrolment(key, a.Farm.Farm, who.Person, "")
	if err != nil {
		return nil, err
	}
	e.IDToken = token
	var cert people.Certified
	if err := c.Issuer(ctx, http.MethodPost, "/v1/people/enroll", e, &cert); err != nil {
		return nil, fmt.Errorf("enrolling this machine's key: %w", err)
	}
	if err := saveCert(a.Store, keystore.Device, &cert); err != nil {
		return nil, err
	}
	if a.Say != nil {
		a.Say("signed in as %s\n", who.Person)
	}
	return &cert, nil
}

// signIn runs the provider's authorization code flow on a loopback
// redirect, with PKCE (S256) and nonce, and returns the ID token.
func signIn(ctx context.Context, a LoginArgs, nonce string) (string, error) {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", a.Port))
	if err != nil {
		return "", fmt.Errorf("listening on 127.0.0.1:%d for the sign-in: %w", a.Port, err)
	}
	defer ln.Close()
	redirect := fmt.Sprintf("http://127.0.0.1:%d/callback", ln.Addr().(*net.TCPAddr).Port)
	state, verifier := randomURL(16), randomURL(32)
	sum := sha256.Sum256([]byte(verifier))
	q := url.Values{"response_type": {"code"}, "client_id": {a.Farm.Provider.ClientID}, "redirect_uri": {redirect},
		"scope": {"openid"}, "state": {state}, "nonce": {nonce},
		"code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])}, "code_challenge_method": {"S256"}}
	if a.Prompt != "" {
		q.Set("prompt", a.Prompt)
	}
	if err := secureURL("the sign-in provider's authorization endpoint", a.Endpoints.Authorization); err != nil {
		return "", err
	}
	if err := secureURL("the sign-in provider's token endpoint", a.Endpoints.Token); err != nil {
		return "", err
	}
	auth := a.Endpoints.Authorization + "?" + q.Encode()

	got := make(chan url.Values, 1)
	srv := &http.Server{ReadHeaderTimeout: 10 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/callback" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprintln(w, "Signed in. You can close this tab and go back to the terminal.")
		select {
		case got <- r.URL.Query():
		default:
		}
	})}
	go srv.Serve(ln)
	defer srv.Close()

	if a.Say != nil {
		a.Say("sign in   %s\n", auth)
		a.Say("          no browser here? from your own machine: ssh -L %d:127.0.0.1:%d <this machine>, then open that address there\n",
			ln.Addr().(*net.TCPAddr).Port, ln.Addr().(*net.TCPAddr).Port)
	}
	if a.Open != nil {
		// no browser opened: the person follows the address printed, or
		// forwards the port; a renewal doesn't wait for a browser that
		// isn't there
		if err := a.Open(auth); err != nil && a.Prompt == "none" {
			return "", fmt.Errorf("no browser opened for the sign-in: %w", err)
		}
	}
	wait := a.Wait
	if wait == 0 {
		wait = 5 * time.Minute
	}
	var back url.Values
	select {
	case back = <-got:
	case <-time.After(wait):
		return "", errors.New("no sign-in came back in time: run c4 farm login again")
	case <-ctx.Done():
		return "", ctx.Err()
	}
	if e := back.Get("error"); e != "" {
		return "", fmt.Errorf("the sign-in provider refused: %s %s", e, back.Get("error_description"))
	}
	if back.Get("state") != state {
		return "", errors.New("the sign-in came back for another request: run c4 farm login again")
	}
	return exchange(ctx, a, back.Get("code"), redirect, verifier)
}

// exchange trades the authorization code for the ID token, as a public
// client: the PKCE verifier proves it is the client that asked.
func exchange(ctx context.Context, a LoginArgs, code, redirect, verifier string) (string, error) {
	form := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {redirect},
		"client_id": {a.Farm.Provider.ClientID}, "code_verifier": {verifier}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.Endpoints.Token, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	hc := a.Provider
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("the sign-in provider's token endpoint: %w", err)
	}
	defer resp.Body.Close()
	var out struct {
		IDToken string `json:"id_token"`
		Error   string `json:"error"`
	}
	json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out)
	if resp.StatusCode != http.StatusOK || out.IDToken == "" {
		return "", fmt.Errorf("the sign-in provider gave no ID token: %s %s", resp.Status, out.Error)
	}
	return out.IDToken, nil
}

func randomURL(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// RenewWithin is how long before its certificate ends the CLI renews a
// key: a certificate lives at most 12 hours.
const RenewWithin = 2 * time.Hour

// NeedsRenewal reports whether a key's certificate ends within RenewWithin.
func NeedsRenewal(cert *people.Certified, now time.Time) bool {
	return cert != nil && cert.Expires.Sub(now) < RenewWithin
}

// Renew renews this machine's keys before their certificates end, keeping
// the new ones beside them. On a farm with no sign-in provider a key
// renews on its own proof, the touch key with it. On a farm with one,
// every device renews on a fresh ID token for its person, from the
// provider's silent round trip, so a leaver's last certificate ends: the
// device key here; a touch key there takes a step-up sign-in (K9).
func Renew(ctx context.Context, c *Client, a LoginArgs) (*people.Certified, error) {
	mine, err := LoadCert(a.Store, keystore.Device)
	if err != nil {
		return nil, err
	}
	if mine == nil {
		return nil, errors.New("this machine's key is not enrolled: c4 farm login enrols it")
	}
	person := strings.TrimPrefix(mine.Identity, provenance.IdentityBase+"user/")
	key, err := a.Store.Key(keystore.Device)
	if err != nil {
		return nil, err
	}
	e, err := enrolment(key, a.Farm.Farm, person, "")
	if err != nil {
		return nil, err
	}
	if a.Farm.Provider != nil {
		// silent unless the person is at c4 farm login, renewing
		if !a.Interactive {
			a.Prompt = "none"
		}
		if e.IDToken, err = signIn(ctx, a, deviceID(key)); err != nil {
			return nil, fmt.Errorf("renewing on a fresh sign-in: %w", err)
		}
	}
	var cert people.Certified
	if err := c.Issuer(ctx, http.MethodPost, "/v1/people/renew", e, &cert); err != nil {
		return nil, fmt.Errorf("renewing this machine's key: %w", err)
	}
	if err := saveCert(a.Store, keystore.Device, &cert); err != nil {
		return nil, err
	}
	if a.Farm.Provider == nil && a.Store.Has(keystore.Touch) {
		touch, err := a.Store.Key(keystore.Touch)
		if err != nil {
			return nil, err
		}
		// never an unannounced touch during an ordinary command
		if a.Say != nil {
			a.Say("key       renewing your touch key for this farm: touch to continue\n")
		}
		touch = keystore.WithReason(touch, "c4: renew your touch key for this farm")
		te, err := enrolment(touch, a.Farm.Farm, person, deviceID(key))
		if err != nil {
			return nil, err
		}
		var tc people.Certified
		if err := c.Issuer(ctx, http.MethodPost, "/v1/people/renew", te, &tc); err != nil {
			return nil, fmt.Errorf("renewing this machine's touch key: %w", err)
		}
		if err := saveCert(a.Store, keystore.Touch, &tc); err != nil {
			return nil, err
		}
	}
	return &cert, nil
}
