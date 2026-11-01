// Package people is the people's issuer (PEOPLE-KEYS-2 3, slice K2),
// beside the workload issuer (provenance.Issuer). It certifies a person's
// device key, a P-256 key that never leaves the device, as user/<name>
// with the device's ID, for at most 12 hours, and only on evidence a
// verifier can check later without trusting the HUD:
//   - on a studio farm, an ID token from the farm's one sign-in provider,
//     pinned to the provider, the farm's client ID, the nonce (the device
//     ID) and the subject (the person's sign-ins), never the email;
//   - on a one-person farm, the installer token over loopback, once, for
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
	// --sign-in-client-id), booked as the setting in force until an
	// admin's signed act sets another (set-sign-in-provider).
	Provider Provider
	// SignIns finds the person a provider subject is.
	SignIns SignIns
	// StepUp is what a touch key's sign-in must show.
	StepUp StepUp
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

func (i *Issuer) addr(r *http.Request) string {
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
		errors.Is(err, provenance.ErrClock), errors.Is(err, provenance.ErrNeedsTouch), errors.Is(err, ErrNotInForce):
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
	// Owner is a one-person farm's person, and OwnerEnrolled whether the
	// installer path has been used (it is then refused).
	Owner         string `json:"owner,omitempty"`
	OwnerEnrolled bool   `json:"ownerEnrolled"`
	LeafTTL       string `json:"leafTTL"`
	Lapse         string `json:"lapse"`
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
	pos, enrolled := int64(len(x.entries)), x.ownerRecord() != nil
	x.mu.Unlock()
	writeJSON(w, http.StatusOK, Config{Farm: i.Farm(), Order: OrderIssuer, Position: pos, Provider: set,
		Nonce:  "the device ID: the C4 ID of the key's DER SubjectPublicKeyInfo, as a string (c4...)",
		StepUp: StepUpConfig{MaxAge: age.String(), ACR: append(append([]string{}, DefaultStepUpACR...), i.StepUp.ACR...), AMR: "mfa, or factors of two kinds (RFC 8176): known (pwd, pin, kba), held (otp, sms, tel, hwk, swk, sc, pop), being (fpt, face, iris, retina, vbm)"},
		Owner:  i.owner(r.Context()), OwnerEnrolled: enrolled, LeafTTL: LeafTTL.String(), Lapse: Lapse.String()})
}

// IndexPage is the index from a position on.
type IndexPage struct {
	Farm     string       `json:"farm"`
	Order    string       `json:"order"`
	Position int64        `json:"position"`
	Entries  []IndexEntry `json:"entries"`
}

// IndexEntry is one entry and its C4 ID.
type IndexEntry struct {
	ID    string `json:"id"`
	Entry *Entry `json:"entry"`
}

func (i *Issuer) handleIndex(w http.ResponseWriter, r *http.Request) {
	from, _ := strconv.ParseInt(r.URL.Query().Get("from"), 10, 64)
	es, ids, err := i.Index.Entries(r.Context(), from)
	if err != nil {
		fail(w, err)
		return
	}
	page := IndexPage{Farm: i.Farm(), Order: OrderIssuer, Position: from + int64(len(es)), Entries: []IndexEntry{}}
	for k := range es {
		page.Entries = append(page.Entries, IndexEntry{ID: ids[k], Entry: es[k]})
	}
	writeJSON(w, http.StatusOK, page)
}

// handleRecord serves a record or entry the index names, by C4 ID, as
// stored: a device record, an act, a provider's key or an entry.
func (i *Issuer) handleRecord(w http.ResponseWriter, r *http.Request) {
	b, err := i.Index.Store.Get(r.Context(), r.PathValue("id"))
	if errors.Is(err, ErrNotFound) {
		fail(w, refuse(http.StatusNotFound, "no record %.16s", r.PathValue("id")))
		return
	}
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
	person := r.URL.Query().Get("person")
	x := i.Index
	x.mu.Lock()
	defer x.mu.Unlock()
	if err := x.sync(r.Context()); err != nil {
		fail(w, err)
		return
	}
	pos := int64(len(x.entries))
	out := []DeviceView{}
	for _, d := range x.inForce(pos, i.now()) {
		p := d.rec.Predicate
		if person != "" && p.Person != person {
			continue
		}
		dv := DeviceView{Device: d.id, Record: d.record, Person: p.Person, Kind: p.Kind, Label: p.Label, Protection: p.Protection,
			Evidence: p.Evidence, TouchFor: p.TouchFor, Enrolled: d.first, Renewed: d.last, RenewedAt: d.lastAt.UTC().Format(time.RFC3339),
			Countersigned: d.countersigns > 0}
		for _, e := range x.entries {
			if e.Kind == KindFlag && e.Device == d.id {
				dv.Flags = append(dv.Flags, e)
			}
		}
		out = append(out, dv)
	}
	slices.SortFunc(out, func(a, b DeviceView) int { return int(a.Enrolled - b.Enrolled) })
	writeJSON(w, http.StatusOK, map[string]any{"farm": i.Farm(), "position": pos, "devices": out})
}
