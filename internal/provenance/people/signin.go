package people

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	jose "github.com/go-jose/go-jose/v4"

	"github.com/Avalanche-io/c4-operator/schema"
)

// Checking an ID token (PEOPLE-KEYS-2 3.1). The issuer certifies a
// person's device key only on an ID token from the farm's one sign-in
// provider, pinned four ways: the provider (iss), the audience (the
// farm's own client ID), the nonce (the device ID, so the provider's "this
// is dana" binds this key and no other) and the subject, matched against
// the person's sign-ins, never the email. go-oidc checks the signature
// against the provider's keys (JWKS), the issuer, the audience and the
// expiry; the issuer checks the rest.

// Provider is a farm's sign-in provider: its issuer URL and the farm's
// own client ID there.
type Provider struct {
	Issuer   string `json:"issuer"`
	ClientID string `json:"clientID"`
}

// SignIn is one sign-in subject: a provider's subject ID.
type SignIn struct {
	Issuer  string `json:"issuer"`
	Subject string `json:"subject"`
}

// SignIns are ORGANIZATION's Party signIns: the provider subject IDs that
// are each person. Nothing else names a person, the email least of all,
// which a provider may let people change. The issuer books their content
// in the index whenever it changes (KindSignIns), so a change is visible,
// and a verifier reads the sign-ins in force at a record's position.
type SignIns interface {
	// Load reads every person's sign-ins as they are now.
	Load(ctx context.Context) (StaticSignIns, error)
}

// StaticSignIns is each person's sign-ins, by person.
type StaticSignIns map[string][]SignIn

// Load is the sign-ins themselves.
func (s StaticSignIns) Load(context.Context) (StaticSignIns, error) { return s, nil }

// Person is the person whose sign-ins hold this subject at this provider,
// or "".
func (s StaticSignIns) Person(issuer, subject string) string {
	for person, ins := range s {
		if slices.Contains(ins, SignIn{Issuer: issuer, Subject: subject}) {
			return person
		}
	}
	return ""
}

// canonical is the sign-ins as the index books them: canonical JSON, each
// person's sign-ins sorted, no person without one.
func (s StaticSignIns) canonical() []byte {
	m := map[string][]SignIn{}
	for person, ins := range s {
		if len(ins) == 0 {
			continue
		}
		sorted := slices.Clone(ins)
		slices.SortFunc(sorted, func(a, b SignIn) int {
			if c := strings.Compare(a.Issuer, b.Issuer); c != 0 {
				return c
			}
			return strings.Compare(a.Subject, b.Subject)
		})
		m[person] = slices.Compact(sorted)
	}
	raw, _ := json.Marshal(m)
	b, _ := schema.Canonical(raw) // strings only, so this cannot fail
	return b
}

// parseSignIns reads booked sign-ins, which must be in their canonical
// form.
func parseSignIns(b []byte) (StaticSignIns, error) {
	var s StaticSignIns
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("booked sign-ins: %w", err)
	}
	if !bytes.Equal(s.canonical(), b) {
		return nil, errors.New("booked sign-ins are not in their canonical form")
	}
	return s, nil
}

// TokenAge is how long ago an ID token may have been issued: enrolment
// and renewal each go back to the provider for a fresh one.
const TokenAge = 10 * time.Minute

// tokenSkew is how far ahead of the issuer's clock a token's iat or
// auth_time may be.
const tokenSkew = time.Minute

// StepUp is what a touch key's ID token must show (PEOPLE-KEYS-2 4.3): a
// recent sign-in, with more than one factor.
type StepUp struct {
	// MaxAge is how long before the token the person signed in
	// (auth_time); 0 is DefaultStepUpAge.
	MaxAge time.Duration
	// ACR are acr values that mean multi-factor at this provider, beside
	// DefaultStepUpACR. amr is read as RFC 8176 says (multiFactor).
	ACR []string
}

// DefaultStepUpAge is how recent a step-up sign-in must be.
const DefaultStepUpAge = 5 * time.Minute

// DefaultStepUpACR are the acr values every provider's step-up may use:
// OpenID PAPE's multi-factor policies.
var DefaultStepUpACR = []string{
	"http://schemas.openid.net/pape/policies/2007/06/multi-factor",
	"http://schemas.openid.net/pape/policies/2007/06/multi-factor-physical",
}

// factorClass is each RFC 8176 amr value that is a factor, by class:
// something known, something held, something one is. Others (user
// presence, risk, geolocation, mca) are not factors.
var factorClass = map[string]string{
	"pwd": "known", "pin": "known", "kba": "known",
	"otp": "held", "sms": "held", "tel": "held", "hwk": "held", "swk": "held", "sc": "held", "pop": "held",
	"fpt": "is", "face": "is", "iris": "is", "retina": "is", "vbm": "is",
}

// multiFactor reports whether amr says more than one factor: mfa, or
// factors of two classes (a password and a one-time code; a security key
// and its PIN).
func multiFactor(amr []string) bool {
	classes := map[string]bool{}
	for _, a := range amr {
		if a == "mfa" {
			return true
		}
		if c := factorClass[a]; c != "" {
			classes[c] = true
		}
	}
	return len(classes) >= 2
}

// signIn is an ID token that passed, and the provider key that signed it.
type signIn struct {
	raw     string
	token   *oidc.IDToken
	issuer  string
	subject string
	// keyID is the C4 ID of the provider's key (its JWK, canonical), jwk
	// that JWK and kid its key ID at the provider.
	keyID, kid string
	jwk        []byte
	// signIns are the sign-ins the subject was matched against, canonical.
	signIns []byte
}

// claims are the claims beyond what go-oidc reads.
type claims struct {
	AZP      string   `json:"azp"`
	AuthTime int64    `json:"auth_time"`
	ACR      string   `json:"acr"`
	AMR      []string `json:"amr"`
}

// provider is one provider's discovery and verifier, made once for each
// setting.
type provider struct {
	setting  Provider
	verifier *oidc.IDTokenVerifier
	jwksURL  string
	algs     []jose.SignatureAlgorithm

	mu   sync.Mutex
	keys []jose.JSONWebKey
}

// errSignIn is a token refused; its message says why.
type errSignIn struct{ msg string }

func (e *errSignIn) Error() string { return e.msg }

func refusedToken(format string, a ...any) error { return &errSignIn{fmt.Sprintf(format, a...)} }

// provider returns the setting's provider, discovering it the first time.
func (i *Issuer) provider(ctx context.Context, set Provider) (*provider, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if p := i.providers[set]; p != nil {
		return p, nil
	}
	// Discovery and keys are fetched with the issuer's client, and outlive
	// the request that first needs them.
	octx := oidc.ClientContext(context.WithoutCancel(ctx), i.client())
	op, err := oidc.NewProvider(octx, set.Issuer)
	if err != nil {
		return nil, fmt.Errorf("the sign-in provider %s: %w", set.Issuer, err)
	}
	var disc struct {
		JWKS string   `json:"jwks_uri"`
		Algs []string `json:"id_token_signing_alg_values_supported"`
	}
	if err := op.Claims(&disc); err != nil {
		return nil, err
	}
	p := &provider{setting: set, jwksURL: disc.JWKS,
		verifier: op.VerifierContext(octx, &oidc.Config{ClientID: set.ClientID, Now: i.now})}
	for _, a := range disc.Algs {
		p.algs = append(p.algs, jose.SignatureAlgorithm(a))
	}
	if len(p.algs) == 0 {
		p.algs = []jose.SignatureAlgorithm{jose.RS256}
	}
	if i.providers == nil {
		i.providers = map[Provider]*provider{}
	}
	i.providers[set] = p
	return p, nil
}

func (i *Issuer) client() *http.Client {
	if i.HTTPClient != nil {
		return i.HTTPClient
	}
	return &http.Client{Timeout: 15 * time.Second}
}

// checkIDToken checks an ID token against the provider set: its
// signature, issuer, audience (the client ID, and azp when the token
// names more than one audience or any azp), expiry, issued within
// TokenAge, and the nonce, which must be device, the device ID of the key
// it certifies. stepUp asks a touch key's more: auth_time within
// StepUp.MaxAge and more than one factor in acr or amr. The subject is
// the caller's to match.
func (i *Issuer) checkIDToken(ctx context.Context, set Provider, raw, device string, stepUp bool) (*signIn, error) {
	if set.Issuer == "" || set.ClientID == "" {
		return nil, refusedToken("this farm has no sign-in provider: an admin sets one (set-sign-in-provider)")
	}
	p, err := i.provider(ctx, set)
	if err != nil {
		return nil, err
	}
	tok, err := p.verifier.Verify(ctx, raw)
	if err != nil {
		var exp *oidc.TokenExpiredError
		if errors.As(err, &exp) {
			return nil, refusedToken("the ID token expired at %s: sign in again", exp.Expiry.UTC().Format(time.RFC3339))
		}
		return nil, refusedToken("the ID token is not from %s for client %s: %v", set.Issuer, set.ClientID, err)
	}
	var c claims
	if err := tok.Claims(&c); err != nil {
		return nil, refusedToken("the ID token's claims: %v", err)
	}
	now := i.now()
	switch {
	case (len(tok.Audience) > 1 || c.AZP != "") && c.AZP != set.ClientID:
		return nil, refusedToken("the ID token was issued to %q (azp), not this farm's client %s", c.AZP, set.ClientID)
	case tok.IssuedAt.IsZero() || tok.IssuedAt.After(now.Add(tokenSkew)):
		return nil, refusedToken("the ID token says it was issued at %s, which is not before now", tok.IssuedAt.UTC().Format(time.RFC3339))
	case now.Sub(tok.IssuedAt) > TokenAge:
		return nil, refusedToken("the ID token was issued %s ago: enrolment and renewal take a fresh one, from the last %s", now.Sub(tok.IssuedAt).Round(time.Second), TokenAge)
	case tok.Nonce != device:
		return nil, refusedToken("the ID token's nonce is %q, not the key's device ID %s: a token is certified only for the key it names", tok.Nonce, device)
	case tok.Subject == "":
		return nil, refusedToken("the ID token names no subject")
	}
	if stepUp {
		if err := i.checkStepUp(c, now); err != nil {
			return nil, err
		}
	}
	keyID, kid, jwk, err := p.signingKey(ctx, i.client(), raw)
	if err != nil {
		return nil, err
	}
	return &signIn{raw: raw, token: tok, issuer: tok.Issuer, subject: tok.Subject, keyID: keyID, kid: kid, jwk: jwk}, nil
}

// checkStepUp checks a touch key's token is a fresh sign-in with more
// than one factor.
func (i *Issuer) checkStepUp(c claims, now time.Time) error {
	age := i.StepUp.MaxAge
	if age == 0 {
		age = DefaultStepUpAge
	}
	auth := time.Unix(c.AuthTime, 0)
	switch {
	case c.AuthTime == 0:
		return refusedToken("a touch key needs a step-up sign-in, and the ID token has no auth_time")
	case auth.After(now.Add(tokenSkew)) || now.Sub(auth) > age:
		return refusedToken("a touch key needs a step-up sign-in from the last %s; this one was at %s", age, auth.UTC().Format(time.RFC3339))
	case slices.Contains(DefaultStepUpACR, c.ACR) || c.ACR != "" && slices.Contains(i.StepUp.ACR, c.ACR):
		return nil
	case multiFactor(c.AMR):
		return nil
	}
	return refusedToken("a touch key needs a step-up sign-in with more than one factor; the ID token says acr %q, amr %v", c.ACR, c.AMR)
}

// signingKey finds the provider key that signed raw, in the provider's
// keys as fetched now or before, and verifies the token with it: the key
// the device record names (providerKey), and the index books the first
// time its ID is seen.
func (p *provider) signingKey(ctx context.Context, hc *http.Client, raw string) (keyID, kid string, jwk []byte, err error) {
	jws, err := jose.ParseSigned(raw, p.algs)
	if err != nil || len(jws.Signatures) != 1 {
		return "", "", nil, refusedToken("the ID token is not a JWS with one signature")
	}
	kid = jws.Signatures[0].Header.KeyID
	for fetch := 0; fetch < 2; fetch++ {
		p.mu.Lock()
		keys := p.keys
		p.mu.Unlock()
		for _, k := range keys {
			if kid != "" && k.KeyID != kid {
				continue
			}
			if _, err := jws.Verify(&k); err != nil {
				continue
			}
			b, err := canonicalJWK(k)
			if err != nil {
				return "", "", nil, err
			}
			return c4id(b), k.KeyID, b, nil
		}
		if err := p.fetchKeys(ctx, hc); err != nil {
			return "", "", nil, err
		}
	}
	return "", "", nil, refusedToken("no key of %s's verifies the ID token (kid %q)", p.setting.Issuer, kid)
}

func (p *provider) fetchKeys(ctx context.Context, hc *http.Client) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.jwksURL, nil)
	if err != nil {
		return err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("the provider's keys: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("the provider's keys: %s", resp.Status)
	}
	var set jose.JSONWebKeySet
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&set); err != nil {
		return fmt.Errorf("the provider's keys: %w", err)
	}
	p.mu.Lock()
	p.keys = set.Keys
	p.mu.Unlock()
	return nil
}

// canonicalJWK is a provider key as the index keeps it: its public JWK,
// canonical (RFC 8785). Its C4 ID is the providerKey a device record
// names.
func canonicalJWK(k jose.JSONWebKey) ([]byte, error) {
	b, err := k.Public().MarshalJSON()
	if err != nil {
		return nil, err
	}
	return schema.Canonical(b)
}

// tokenAlgs are the JWS algorithms a booked token may be read with; the
// booked key itself decides which verifies.
var tokenAlgs = []jose.SignatureAlgorithm{jose.RS256, jose.RS384, jose.RS512, jose.PS256, jose.PS384, jose.PS512,
	jose.ES256, jose.ES384, jose.ES512, jose.EdDSA}

// parseJWK reads a booked provider key.
func parseJWK(b []byte) (*jose.JSONWebKey, error) {
	var k jose.JSONWebKey
	if err := k.UnmarshalJSON(b); err != nil {
		return nil, fmt.Errorf("a provider key: %w", err)
	}
	if !k.IsPublic() {
		return nil, errors.New("a provider key must be public")
	}
	return &k, nil
}

// CheckBookedToken checks a device record's ID token as a verifier reading
// the index later does (PEOPLE-KEYS-2 3.1, 7): its signature against the
// provider key the index booked for it, since the provider may have
// rotated the key away and the booked copy is what the verifier trusts,
// the farm's word; its issuer and audience against the provider setting
// in force at the record's position (azp too, when it names more than one
// audience or any azp); its nonce, the record's device ID; and its
// subject, a sign-in of the record's person in the sign-ins in force
// there. Expiry is not checked: the token was fresh when it was taken.
func CheckBookedToken(d *schema.DevicePredicate, jwk []byte, set Provider, ins StaticSignIns) error {
	k, err := parseJWK(jwk)
	if err != nil {
		return err
	}
	if c4id(jwk) != d.ProviderKey {
		return fmt.Errorf("the booked key is %.16s, the record names %.16s", c4id(jwk), d.ProviderKey)
	}
	jws, err := jose.ParseSigned(d.IDToken, tokenAlgs)
	if err != nil {
		return err
	}
	payload, err := jws.Verify(k)
	if err != nil {
		return err
	}
	var c struct {
		Issuer  string          `json:"iss"`
		Subject string          `json:"sub"`
		Aud     json.RawMessage `json:"aud"`
		AZP     string          `json:"azp"`
		Nonce   string          `json:"nonce"`
	}
	if err := json.Unmarshal(payload, &c); err != nil {
		return err
	}
	var aud []string
	if json.Unmarshal(c.Aud, &aud) != nil {
		var one string
		json.Unmarshal(c.Aud, &one)
		aud = []string{one}
	}
	spki, _ := d.PublicKey()
	switch {
	case set.Issuer == "" || c.Issuer != set.Issuer:
		return fmt.Errorf("the token is from %q, and the provider in force at its record was %q", c.Issuer, set.Issuer)
	case !slices.Contains(aud, set.ClientID):
		return fmt.Errorf("the token is for %v, not the farm's client %s", aud, set.ClientID)
	case (len(aud) > 1 || c.AZP != "") && c.AZP != set.ClientID:
		return fmt.Errorf("the token was issued to %q (azp), not the farm's client %s", c.AZP, set.ClientID)
	case c.Nonce != schema.DeviceID(spki):
		return fmt.Errorf("the token's nonce %q is not the record's device", c.Nonce)
	case ins.Person(c.Issuer, c.Subject) != d.Person:
		return fmt.Errorf("subject %q at %s was not %s's sign-in at the record's position", c.Subject, c.Issuer, d.Person)
	}
	return nil
}

// VerifyBookedToken checks the ID token of a device record the index
// books (CheckBookedToken), against the provider key it names and the
// setting and sign-ins in force at its position.
func (x *Index) VerifyBookedToken(ctx context.Context, record string) error {
	x.mu.Lock()
	defer x.mu.Unlock()
	if err := x.sync(ctx); err != nil {
		return err
	}
	e, d := x.find(KindDevice, record), x.devices[record]
	if e == nil || d == nil {
		return fmt.Errorf("the index books no device record %.16s", record)
	}
	if d.Predicate.Evidence != schema.EvidenceIDToken {
		return fmt.Errorf("device record %.16s was certified on %s, not an ID token", record, d.Predicate.Evidence)
	}
	jwk, err := x.Store.Get(ctx, d.Predicate.ProviderKey)
	if err != nil || !x.booked(KindProviderKey, d.Predicate.ProviderKey) {
		return fmt.Errorf("the provider key %.16s is not booked: %v", d.Predicate.ProviderKey, err)
	}
	set := x.settingAt(e.Position)
	if set == nil {
		return fmt.Errorf("no sign-in provider is in force at position %d", e.Position)
	}
	ins := StaticSignIns{}
	if id := x.signInsAt(e.Position); id != "" {
		b, err := x.Store.Get(ctx, id)
		if err != nil {
			return err
		}
		if ins, err = parseSignIns(b); err != nil {
			return err
		}
	}
	return CheckBookedToken(&d.Predicate, jwk, set.Provider, ins)
}
