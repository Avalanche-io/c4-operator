// Package people is the people's issuer (PEOPLE-KEYS-2 3, slice K2),
// beside the workload issuer (provenance.Issuer). It certifies a person's
// device key, a P-256 key that never leaves the device, as user/<name>
// with the device's ID, for at most 12 hours, and only on evidence a
// verifier can check later without trusting the HUD:
//   - on a studio farm, an ID token from the farm's one sign-in provider,
//     pinned to the provider, the farm's client ID, the nonce (the device
//     ID) and the subject (the person's sign-ins), never the email;
//   - on a one-person farm, the installer token, once, for
//     the owner's key and touch key; then the owner's key enrols the
//     browser with a one-time code;
//   - a touch key on a fresh step-up sign-in, or on a touch from another
//     of the person's touch keys.
//
// Each enrolment and renewal writes a device record (device/v1), signed by
// the issuer as component/issuer, with the device's own signature over its
// request and the evidence; the index books it (index.go). Removals, the
// sign-in provider setting and a one-person farm's codes are signed acts
// the index books too. Until slice K8 nothing refuses an act for want of
// any of this: the issuer is additive.
package people

import (
	"context"
	"crypto/ed25519"
	"crypto/subtle"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/Avalanche-io/c4-operator/internal/provenance"
	"github.com/Avalanche-io/c4-operator/schema"
)

// LeafTTL is how long a person's leaf lives (PEOPLE-KEYS-2 2.1): a
// device renews through the provider before it ends, and a leaver's last
// one ends within it.
const LeafTTL = 12 * time.Hour

// TwoPlaces is how close together two renewals of one device from
// different addresses are flagged (PEOPLE-KEYS-2 7). A device renews at
// about half its leaf's life, hours apart.
const TwoPlaces = time.Hour

// InstallerHeader carries a one-person farm's installer token, as the
// HUD's development viewers take it.
const InstallerHeader = "X-C4-Installer"

// Issuer is the people's issuer.
type Issuer struct {
	// CA is the farm's signing CA, which certifies device keys and the
	// issuer's own key (component/issuer).
	CA *provenance.CA
	// Index is the farm's index of device records.
	Index *Index
	// Provider is the install's sign-in provider (--sign-in-provider,
	// --sign-in-client-id), booked as the setting in force. Until K9 it
	// changes only by redeploying.
	Provider Provider
	// SignIns finds the person a provider subject is.
	SignIns SignIns
	// StepUp is what a touch key's sign-in must show.
	StepUp StepUp
	// WebAuthn is the relying party a browser's passkey touch key must be
	// made for: the HUD's host and origin (--hud-origin). nil refuses
	// passkeys; the CLI's touch key needs none.
	WebAuthn *WebAuthnRP
	// SoftwareTouchKeys lets a touch key state a software protection
	// (software-file, or a browser key that isn't a passkey): a test or
	// file-key farm, set at install (--software-touch-keys). Otherwise a
	// touch key states hardware, or is a passkey, so a key file planted on
	// the person's machine can't be enrolled as their touch (PEOPLE-KEYS-2
	// 4.3). It is stated, not attested.
	SoftwareTouchKeys bool
	// Owner is a one-person farm's person, from its farm binding or
	// --single-person-farm; nil or "" on a studio farm.
	Owner func(ctx context.Context) string
	// Admin reports whether a person may remove anyone's devices and set
	// the sign-in provider: the farm owner's admins. Before ORGANIZATION's
	// Parties, the farm binding's owner.
	Admin func(ctx context.Context, person string) bool
	// InstallerToken is a one-person farm's installer token
	// (--hud-installer-token-file); nil refuses the installer path.
	InstallerToken []byte
	// LocalOnly says the HUD listens only on a loopback address, so a
	// request's remote address is where it came from: the owner's routes
	// then need the machine's own.
	LocalOnly bool
	// Acts takes each act once, by its payload hash; nil keeps them in
	// this process's memory. K8 gives it a shared, lasting store.
	Acts provenance.SeenSet
	// HTTPClient reaches the sign-in provider; nil is a default client.
	HTTPClient *http.Client
	// ClientAddr is where a request came from, for the two-places flag;
	// nil is its remote address.
	ClientAddr func(*http.Request) string
	// Now is the issuer's clock; nil is time.Now.
	Now func() time.Time

	mu        sync.Mutex
	signer    *provenance.Signer
	providers map[Provider]*provider
	seen      *provenance.MemorySeen
}

func (i *Issuer) now() time.Time {
	if i.Now != nil {
		return i.Now()
	}
	return time.Now()
}

// Farm is the farm's ID: the C4 ID of its CA's certificate.
func (i *Issuer) Farm() string { return provenance.FarmID(i.CA.Cert) }

// sign is the issuer's own signer, component/issuer, certified by the CA
// on the issuer's clock.
func (i *Issuer) sign(ctx context.Context) (*provenance.Signer, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.signer != nil {
		return i.signer, nil
	}
	s, err := provenance.NewSigner(ctx, func(_ context.Context, pub ed25519.PublicKey) ([]*x509.Certificate, error) {
		c, err := i.CA.Issue(pub, provenance.ComponentIdentity("issuer"), 24*time.Hour, i.now())
		if err != nil {
			return nil, err
		}
		return []*x509.Certificate{c, i.CA.Cert}, nil
	})
	if err != nil {
		return nil, err
	}
	i.signer = s
	return s, nil
}

func (i *Issuer) acts() provenance.SeenSet {
	if i.Acts != nil {
		return i.Acts
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.seen == nil {
		i.seen = &provenance.MemorySeen{Now: i.now}
	}
	return i.seen
}

func (i *Issuer) owner(ctx context.Context) string {
	if i.Owner == nil {
		return ""
	}
	return i.Owner(ctx)
}

func (i *Issuer) admin(ctx context.Context, person string) bool {
	return i.Admin != nil && person != "" && i.Admin(ctx, person)
}

type addrKey struct{}

// WithClientAddr says where a request came from, as the HUD knows it: the
// SSO proxy's X-Forwarded-For on a request with the proxy's secret, its
// remote address otherwise. It is for the two-places notice only.
func WithClientAddr(ctx context.Context, addr string) context.Context {
	return context.WithValue(ctx, addrKey{}, addr)
}

// ClientAddrFrom is the address WithClientAddr put in ctx, or "".
func ClientAddrFrom(ctx context.Context) string {
	a, _ := ctx.Value(addrKey{}).(string)
	return a
}

func (i *Issuer) addr(r *http.Request) string {
	if a := ClientAddrFrom(r.Context()); a != "" {
		return a
	}
	if i.ClientAddr != nil {
		return i.ClientAddr(r)
	}
	if h, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return h
	}
	return r.RemoteAddr
}

func (i *Issuer) installer(r *http.Request) bool {
	h := r.Header.Get(InstallerHeader)
	return len(i.InstallerToken) > 0 && h != "" && subtle.ConstantTimeCompare([]byte(h), i.InstallerToken) == 1
}

// ServeHTTP serves the people's issuer under /v1/people/ (internal/hud/
// API.md, "The people's issuer").
func (i *Issuer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	i.mux().ServeHTTP(w, r)
}

func (i *Issuer) mux() *http.ServeMux {
	m := http.NewServeMux()
	m.HandleFunc("GET /v1/people/config", i.handleConfig)
	m.HandleFunc("POST /v1/people/enroll", i.handleEnroll)
	m.HandleFunc("POST /v1/people/whoami", i.handleWhoami)
	m.HandleFunc("POST /v1/people/renew", i.handleRenew)
	m.HandleFunc("POST /v1/people/owner", i.handleOwner)
	m.HandleFunc("POST /v1/people/browser", i.handleBrowser)
	m.HandleFunc("GET /v1/people/codes/{code}", i.handleCode)
	m.HandleFunc("POST /v1/people/acts", i.handleAct)
	m.HandleFunc("GET /v1/people/index", i.handleIndex)
	m.HandleFunc("GET /v1/people/records/{id}", i.handleRecord)
	m.HandleFunc("GET /v1/people/devices", i.handleDevices)
	return m
}

// refusal is a request refused, with its status.
type refusal struct {
	code int
	msg  string
}

func (e *refusal) Error() string { return e.msg }

func refuse(code int, format string, a ...any) error {
	return &refusal{code: code, msg: fmt.Sprintf(format, a...)}
}

// fail writes err as {"error": ...}: a refusal with its status, a refused
// token as 403, anything else as 500.
func fail(w http.ResponseWriter, err error) {
	code := http.StatusInternalServerError
	var rf *refusal
	var tk *errSignIn
	switch {
	case errors.As(err, &rf):
		code = rf.code
	case errors.As(err, &tk):
		code = http.StatusForbidden
	case errors.Is(err, provenance.ErrUntrusted), errors.Is(err, provenance.ErrChain), errors.Is(err, provenance.ErrSignatureForm),
		errors.Is(err, provenance.ErrClock), errors.Is(err, provenance.ErrNeedsTouch), errors.Is(err, ErrNotInForce), errors.Is(err, ErrTouchKey):
		code = http.StatusForbidden
	case errors.Is(err, provenance.ErrSeen):
		code = http.StatusConflict
	}
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

// maxBody bounds a request: an enrolment carries an ID token, an act a
// bundle with two chains.
const maxBody = 64 << 10

func decode(r *http.Request, v any) error {
	d := json.NewDecoder(io.LimitReader(r.Body, maxBody))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return refuse(http.StatusBadRequest, "the request: %v", err)
	}
	return nil
}

// WebAuthnRP is the relying party passkeys are made for: its ID (the
// HUD's host) and the HUD's origin.
type WebAuthnRP struct {
	ID     string `json:"id"`
	Origin string `json:"origin"`
}

// Config is what a client reads before it enrols: the farm, the sign-in
// provider to send the person to and how, and whether a one-person farm's
// owner is enrolled.
type Config struct {
	Farm     string `json:"farm"`
	Order    string `json:"order"`
	Position int64  `json:"position"`
	// Provider is the setting in force, with its grade; nil when the farm
	// has none.
	Provider *Setting `json:"provider"`
	// Nonce says what the ID token's nonce must be.
	Nonce string `json:"nonce"`
	// StepUp says what a touch key's sign-in must show.
	StepUp StepUpConfig `json:"stepUp"`
	// WebAuthn is the relying party a passkey touch key is made for (its
	// rp.id) and the origin it signs at; nil when the farm takes none.
	WebAuthn *WebAuthnRP `json:"webauthn,omitempty"`
	// Owner is a one-person farm's person, and OwnerEnrolled whether the
	// installer path has been used (it is then refused).
	Owner         string `json:"owner,omitempty"`
	OwnerEnrolled bool   `json:"ownerEnrolled"`
	LeafTTL       string `json:"leafTTL"`
	Lapse         string `json:"lapse"`
	// SoftwareTouchKeys says this farm takes a touch key that states a
	// software protection (--software-touch-keys); otherwise a touch key
	// states hardware or is a passkey.
	SoftwareTouchKeys bool `json:"softwareTouchKeys,omitempty"`
}

// StepUpConfig is StepUp as a client reads it.
type StepUpConfig struct {
	MaxAge string   `json:"maxAge"`
	ACR    []string `json:"acr"`
	AMR    string   `json:"amr"`
}

// Setting is the sign-in provider in force, as the index books it.
type Setting struct {
	Provider
	// Grade: install, signed or confirmed. Until the touch (K9) an
	// admin's setting is signed, not confirmed, and K8 can't be switched
	// on while it is.
	Grade    string `json:"grade"`
	Position int64  `json:"position"`
	Record   string `json:"record,omitempty"`
}

func (i *Issuer) handleConfig(w http.ResponseWriter, r *http.Request) {
	set, err := i.setting(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	age := i.StepUp.MaxAge
	if age == 0 {
		age = DefaultStepUpAge
	}
	x := i.Index
	x.mu.Lock()
	pos, enrolled := x.pos, x.ownerRecord() != nil
	x.mu.Unlock()
	writeJSON(w, http.StatusOK, Config{Farm: i.Farm(), Order: x.Order(), Position: pos, Provider: set, WebAuthn: i.WebAuthn,
		Nonce:  "the device ID: the C4 ID of the key's DER SubjectPublicKeyInfo, as a string (c4...)",
		StepUp: StepUpConfig{MaxAge: age.String(), ACR: append(append([]string{}, DefaultStepUpACR...), i.StepUp.ACR...), AMR: "mfa, or factors of two kinds (RFC 8176): known (pwd, pin, kba), held (otp, sms, tel, hwk, swk, sc, pop), being (fpt, face, iris, retina, vbm)"},
		Owner:  i.owner(r.Context()), OwnerEnrolled: enrolled, LeafTTL: LeafTTL.String(), Lapse: Lapse.String(), SoftwareTouchKeys: i.SoftwareTouchKeys})
}

// Reads need a caller (PEOPLE-KEYS-2 7): the person the HUD signed in,
// which it puts in the request's context (WithCaller). A device record's
// ID token and every address the issuer saw go only to that device's
// person and the farm's admins; anyone else gets the record without them,
// labelled as redacted. The config alone is for anyone, since a device
// reads it before it has a key.

type callerKey struct{}

// WithCaller says whom a read is for: the person the HUD signed in.
func WithCaller(ctx context.Context, person string) context.Context {
	return context.WithValue(ctx, callerKey{}, person)
}

// CallerFrom is the person WithCaller put in ctx, or "".
func CallerFrom(ctx context.Context) string {
	p, _ := ctx.Value(callerKey{}).(string)
	return p
}

// caller is a read's caller, or a refusal.
func caller(r *http.Request) (string, error) {
	if p := CallerFrom(r.Context()); p != "" {
		return p, nil
	}
	return "", refuse(http.StatusUnauthorized, "the people's index is read signed in: through the HUD, as yourself")
}

// sees reports whether caller may read person's ID tokens and addresses:
// the person themselves, or an admin.
func (i *Issuer) sees(ctx context.Context, caller, person string) bool {
	return caller == person || i.admin(ctx, caller)
}

// Sees reports whether caller may read person's ID tokens and addresses:
// the person themselves, or an admin. The farm book's reads at the HUD
// keep the same rule (Private).
func (i *Issuer) Sees(ctx context.Context, caller, person string) bool {
	return caller != "" && i.sees(ctx, caller, person)
}

// Private is what a caller may read of a booked entry's private part (its
// addresses), by the rule above: the part, as stored, when sees says the
// caller may read its person's; otherwise Redacted names what is left out.
// An entry with no private part has nothing to leave out. An entry from
// before private parts, with its addresses in it, is withheld whole from
// anyone else ("entry"): its ID would give them away.
func Private(ctx context.Context, store Store, raw []byte, sees func(person string) bool) (json.RawMessage, Redacted, error) {
	var e Entry
	if err := json.Unmarshal(raw, &e); err != nil {
		return nil, nil, err
	}
	switch {
	case e.Addr != "" || len(e.Seen) > 0:
		if sees(e.Person) {
			return nil, nil, nil
		}
		return nil, Redacted{"entry"}, nil
	case e.Private == "":
		return nil, nil, nil
	case !sees(e.Person):
		return nil, Redacted{"private"}, nil
	}
	b, err := store.Get(ctx, e.Private)
	if err != nil {
		return nil, nil, err
	}
	return b, nil, nil
}

// Redacted is what a read leaves out, by field name.
type Redacted []string

// redact is e as caller may read it.
func (i *Issuer) redact(ctx context.Context, caller string, e *Entry) (*Entry, Redacted) {
	if (e.Addr == "" && len(e.Seen) == 0) || i.sees(ctx, caller, e.Person) {
		return e, nil
	}
	c := *e
	var out Redacted
	if c.Addr != "" {
		c.Addr, out = "", append(out, "addr")
	}
	if len(c.Seen) > 0 {
		c.Seen, out = nil, append(out, "seen")
	}
	return &c, out
}

// IndexPage is the index from a position on, and its head as the issuer
// signed it, which a client keeps so it can refuse a head that goes back.
type IndexPage struct {
	Farm     string       `json:"farm"`
	Order    string       `json:"order"`
	Position int64        `json:"position"`
	Head     string       `json:"head,omitempty"`
	Signed   []byte       `json:"signed,omitempty"`
	Entries  []IndexEntry `json:"entries"`
}

// IndexEntry is one entry and its C4 ID, as booked. Private is its
// private part, for a caller who may read it; Redacted names what the
// caller may not. An entry with a private part checks against its ID
// either way.
type IndexEntry struct {
	ID       string          `json:"id"`
	Entry    *Entry          `json:"entry"`
	Private  json.RawMessage `json:"private,omitempty"`
	Redacted Redacted        `json:"redacted,omitempty"`
}

func (i *Issuer) handleIndex(w http.ResponseWriter, r *http.Request) {
	who, err := caller(r)
	if err != nil {
		fail(w, err)
		return
	}
	from, _ := strconv.ParseInt(r.URL.Query().Get("from"), 10, 64)
	x := i.Index
	p, err := x.page(r.Context(), from)
	if err != nil {
		fail(w, err)
		return
	}
	es, ids := p.entries, p.ids
	// The position the entries reach, from clamped to the index: a page
	// from past the end, or before the start, answers the true end.
	page := IndexPage{Farm: i.Farm(), Order: x.Order(), Position: p.end, Entries: []IndexEntry{}}
	if x.Book != nil {
		// The book's head segment, signed by the queue: a client keeps it
		// (farmbook.LastHead) and refuses one that goes back.
		if p.head != nil {
			page.Head, page.Signed = p.head.ID, p.head.Signed
		}
	} else {
		h, err := x.Head.Read(r.Context())
		if err != nil {
			fail(w, err)
			return
		}
		if h.Position == page.Position {
			page.Head, page.Signed = h.ID, h.Signed
		}
	}
	for k := range es {
		e, red := i.redact(r.Context(), who, es[k])
		ie := IndexEntry{ID: ids[k], Entry: e, Redacted: red}
		if p := es[k].Private; p != "" {
			if i.sees(r.Context(), who, es[k].Person) {
				ie.Private, _ = x.Store.Get(r.Context(), p)
			} else {
				ie.Redacted = append(ie.Redacted, "private")
			}
		}
		page.Entries = append(page.Entries, ie)
	}
	writeJSON(w, http.StatusOK, page)
}

// RedactedRecord is a record the caller may not read whole: a device
// record without its ID token, or an entry without its addresses.
type RedactedRecord struct {
	Record   string         `json:"record"`
	Redacted Redacted       `json:"redacted"`
	Device   *schema.Device `json:"device,omitempty"`
	Entry    *Entry         `json:"entry,omitempty"`
}

// handleRecord serves a record or entry the index names, by C4 ID: as
// stored, to a caller who may read it whole; redacted otherwise.
func (i *Issuer) handleRecord(w http.ResponseWriter, r *http.Request) {
	who, err := caller(r)
	if err != nil {
		fail(w, err)
		return
	}
	id := r.PathValue("id")
	x := i.Index
	x.mu.Lock()
	err = x.sync(r.Context())
	var dev *schema.Device
	var entry *Entry
	booked := false
	if err == nil {
		dev = x.devices[id]
		entry, booked = x.entryByID(id)
		booked = booked || x.books(id)
	}
	x.mu.Unlock()
	switch {
	case err != nil:
		fail(w, err)
		return
	case !booked:
		fail(w, refuse(http.StatusNotFound, "the index books no record %.16s", id))
		return
	case dev != nil && dev.Predicate.IDToken != "" && !i.sees(r.Context(), who, dev.Predicate.Person):
		c := *dev
		c.Predicate.IDToken = ""
		writeJSON(w, http.StatusOK, RedactedRecord{Record: id, Redacted: Redacted{"idToken"}, Device: &c})
		return
	case entry != nil:
		if e, red := i.redact(r.Context(), who, entry); red != nil {
			writeJSON(w, http.StatusOK, RedactedRecord{Record: id, Redacted: red, Entry: e})
			return
		}
	}
	b, err := x.Store.Get(r.Context(), id)
	if err != nil {
		fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(b)
}

// DeviceView is one device in force, as a person's device list shows it.
type DeviceView struct {
	Device     string `json:"device"`
	Record     string `json:"record"`
	Person     string `json:"person"`
	Kind       string `json:"kind"`
	Label      string `json:"label,omitempty"`
	Protection string `json:"protection"`
	Evidence   string `json:"evidence"`
	TouchFor   string `json:"touchFor,omitempty"`
	Enrolled   int64  `json:"enrolled"`
	Renewed    int64  `json:"renewed"`
	RenewedAt  string `json:"renewedAt"`
	// Countersigned says the owner's CLI countersigned a browser its code
	// enrolled; Flags are what the issuer flagged of the device.
	Countersigned bool     `json:"countersigned,omitempty"`
	Flags         []*Entry `json:"flags,omitempty"`
}

func (i *Issuer) handleDevices(w http.ResponseWriter, r *http.Request) {
	who, err := caller(r)
	if err != nil {
		fail(w, err)
		return
	}
	person := r.URL.Query().Get("person")
	x := i.Index
	x.mu.Lock()
	defer x.mu.Unlock()
	if err := x.sync(r.Context()); err != nil {
		fail(w, err)
		return
	}
	pos := x.pos
	out := []DeviceView{}
	for _, d := range x.inForce(pos, i.now()) {
		p := d.rec.Predicate
		if person != "" && p.Person != person {
			continue
		}
		dv := DeviceView{Device: d.id, Record: d.record, Person: p.Person, Kind: p.Kind, Label: p.Label, Protection: p.Protection,
			Evidence: p.Evidence, TouchFor: p.TouchFor, Enrolled: d.first, Renewed: d.last, RenewedAt: d.lastAt.UTC().Format(time.RFC3339),
			Countersigned: d.countersigns > 0}
		for _, e := range x.all() {
			if e.Kind == KindFlag && e.Device == d.id {
				f, _ := i.redact(r.Context(), who, e)
				if f == e && e.Private != "" && i.sees(r.Context(), who, e.Person) {
					c := *e
					c.Seen = x.seenOf(e)
					f = &c
				}
				dv.Flags = append(dv.Flags, f)
			}
		}
		out = append(out, dv)
	}
	slices.SortFunc(out, func(a, b DeviceView) int { return int(a.Enrolled - b.Enrolled) })
	writeJSON(w, http.StatusOK, map[string]any{"farm": i.Farm(), "position": pos, "devices": out})
}
