package farmcli

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/Avalanche-io/c4-operator/internal/keystore"
	"github.com/Avalanche-io/c4-operator/internal/provenance"
	"github.com/Avalanche-io/c4-operator/internal/provenance/people"
	"github.com/Avalanche-io/c4-operator/schema"
)

// A person's keys on this machine, as the CLI enrols and uses them
// (PEOPLE-KEYS-2 3, K4): its keys are internal/keystore's; their
// certificates, which the people's issuer gives, are kept beside them.

// PeopleConfig is the people's issuer's config: the farm's ID, its sign-in
// provider, and a one-person farm's owner.
func PeopleConfig(ctx context.Context, c *Client) (*people.Config, error) {
	var cfg people.Config
	if err := c.Issuer(ctx, http.MethodGet, "/v1/people/config", nil, &cfg); err != nil {
		return nil, fmt.Errorf("the farm's people's issuer: %w", err)
	}
	return &cfg, nil
}

// deviceKind is this machine as a device record names it.
func deviceKind() string {
	switch runtime.GOOS {
	case "darwin":
		return "mac"
	case "windows":
		return "windows"
	}
	return "linux"
}

// deviceID is a key's device ID: the C4 ID of its public key.
func deviceID(k keystore.Key) string { return schema.DeviceID(k.SPKI()) }

// enrolment is k's request to be person's key at the farm, signed with k
// now: a device key, or a touch key for the device key touchFor names.
func enrolment(k keystore.Key, farm, person, touchFor string) (people.Enrolment, error) {
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return people.Enrolment{}, err
	}
	q := people.Request{Farm: farm, Person: person, Key: base64.RawURLEncoding.EncodeToString(k.SPKI()), TouchFor: touchFor,
		Nonce: base64.RawURLEncoding.EncodeToString(nonce), RequestedAt: time.Now().UTC().Format(time.RFC3339)}
	p := schema.DevicePredicate{Farm: q.Farm, Person: q.Person, Key: q.Key, TouchFor: q.TouchFor, Nonce: q.Nonce, RequestedAt: q.RequestedAt}
	sig, err := k.Sign(p.ProofMessage())
	if err != nil {
		return people.Enrolment{}, err
	}
	host, _ := os.Hostname()
	label := "c4 on " + host
	if touchFor != "" {
		label += " (touch)"
	}
	return people.Enrolment{Request: q, Proof: base64.RawURLEncoding.EncodeToString(sig), Protection: k.Protection(),
		Kind: deviceKind(), Label: label}, nil
}

// certPath is where a key's certificate is kept: beside it.
func certPath(st keystore.Store, kind keystore.Kind) string {
	return filepath.Join(st.Dir, string(kind)+".cert")
}

// saveCert keeps a key's certificate, readable only by the person.
func saveCert(st keystore.Store, kind keystore.Kind, c *people.Certified) error {
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return os.WriteFile(certPath(st, kind), b, 0o600)
}

// LoadCert is a key's certificate as the issuer gave it, or nil when it
// has none.
func LoadCert(st keystore.Store, kind keystore.Kind) (*people.Certified, error) {
	b, err := os.ReadFile(certPath(st, kind))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var c people.Certified
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("%s: %w", certPath(st, kind), err)
	}
	return &c, nil
}

// deviceCert is a certificate as provenance signs an act with it.
func deviceCert(c *people.Certified) provenance.DeviceCert {
	return provenance.DeviceCert{Identity: c.Identity, Device: c.Device, Chain: c.Chain}
}

// ErrNoIssuer means the farm serves no people's issuer: its operator is
// from before personal keys, or it names no people's store.
var ErrNoIssuer = errors.New("this farm serves no people's issuer")

// ErrOwnerKey means the farm's owner key is not the one this machine
// pinned when up enrolled it.
var ErrOwnerKey = errors.New("the farm's owner key is not the one this machine enrolled")

// UpKeys is c4 farm up's keys step on a local farm: the owner's device key
// and touch key enrolled (EnrolOwner), and the owner key checked against
// the one pinned, when one is. It answers ErrNoIssuer from a farm without
// a people's issuer, which up goes on without. touch is whether the person
// is at a terminal to give it: a touch key's proof asks for their touch,
// and an agent's or a script's up never puts that on their screen, so it
// enrols the device key alone and touch-list acts go unsigned.
func UpKeys(ctx context.Context, c *Client, st keystore.Store, owner, pinned string, touch bool) (*people.OwnerCertified, error) {
	if _, err := PeopleConfig(ctx, c); err != nil {
		var ae *APIError
		if errors.As(err, &ae) && ae.Status == http.StatusNotFound {
			return nil, ErrNoIssuer
		}
		return nil, err
	}
	out, err := enrolOwner(ctx, c, st, owner, touch)
	if err != nil {
		return nil, err
	}
	if pinned != "" && out.Owner.Device != pinned {
		return nil, fmt.Errorf("%w: it is %s, and this machine pinned %s when up enrolled it; if the farm was made again, remove its keys (%s) and run up again",
			ErrOwnerKey, ShortDevice(out.Owner.Device), ShortDevice(pinned), st.Dir)
	}
	return out, nil
}

// EnrolOwner is c4 farm up's step on a one-person farm (3.3): the owner's
// device key and touch key, made here at the terminal if they aren't, and
// enrolled on the installer token, once; their certificates are kept
// beside them. A machine that makes no touch key (no Secure Enclave)
// enrols the device key alone. A farm whose owner is enrolled already
// answers with the certificates this machine kept.
func EnrolOwner(ctx context.Context, c *Client, st keystore.Store, owner string) (*people.OwnerCertified, error) {
	return enrolOwner(ctx, c, st, owner, true)
}

func enrolOwner(ctx context.Context, c *Client, st keystore.Store, owner string, withTouch bool) (*people.OwnerCertified, error) {
	cfg, err := PeopleConfig(ctx, c)
	if err != nil {
		return nil, err
	}
	if cfg.OwnerEnrolled {
		dev, err := LoadCert(st, keystore.Device)
		if err != nil || dev == nil {
			return nil, fmt.Errorf("the farm's owner key is enrolled, and this machine has no certificate for it: %v", err)
		}
		touch, _ := LoadCert(st, keystore.Touch)
		return &people.OwnerCertified{Owner: dev, Touch: touch}, nil
	}
	dev, err := st.Key(keystore.Device)
	if err != nil {
		return nil, err
	}
	devEnrol, err := enrolment(dev, cfg.Farm, owner, "")
	if err != nil {
		return nil, err
	}
	body := people.OwnerEnrolment{Owner: devEnrol}
	// no touch key is made unless the person is there to give its proof
	var touch keystore.Key
	err = keystore.ErrNoTouch
	if withTouch {
		touch, err = st.Key(keystore.Touch)
	}
	switch {
	case errors.Is(err, keystore.ErrNoTouch):
		// touch-list acts go unsigned until a touch key is enrolled
	case err != nil:
		return nil, err
	default:
		t, err := enrolment(touch, cfg.Farm, owner, deviceID(dev))
		if err != nil {
			return nil, err
		}
		body.Touch = &t
	}
	var out people.OwnerCertified
	if err := c.Issuer(ctx, http.MethodPost, "/v1/people/owner", body, &out); err != nil {
		return nil, fmt.Errorf("enrolling the owner's key: %w", err)
	}
	if out.Owner == nil {
		return nil, errors.New("the farm's issuer certified no owner key")
	}
	if err := saveCert(st, keystore.Device, out.Owner); err != nil {
		return nil, err
	}
	if out.Touch != nil {
		if err := saveCert(st, keystore.Touch, out.Touch); err != nil {
			return nil, err
		}
	}
	return &out, nil
}

// A browser enrolled from the CLI (3.3): c4 farm open books a one-time
// code with the owner's key, the web app it opens enrols its key with the
// code, and the CLI reads back the key the code enrolled and countersigns
// it in the farm book. A code someone else used first shows as a key the
// CLI didn't see the browser make.

// ownerAct signs a silent act of the owner's with their device key, as the
// issuer books it.
func ownerAct(st keystore.Store, farm, owner string, on provenance.Subject, act string, params any) (json.RawMessage, error) {
	key, err := st.Key(keystore.Device)
	if err != nil {
		return nil, err
	}
	cert, err := LoadCert(st, keystore.Device)
	if err != nil {
		return nil, err
	}
	if cert == nil {
		return nil, errors.New("this machine's key is not enrolled at the farm: c4 farm up enrols it")
	}
	p, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	b, err := provenance.SignPersonAct(provenance.PersonAct{Farm: farm, Subject: on, Act: act, Params: p, Person: owner},
		key, deviceCert(cert), nil, nil)
	return json.RawMessage(b), err
}

// BookBrowserCode books a one-time code for the browser c4 farm open opens:
// the act browser-code on the farm, naming the code by its SHA-256, signed
// with the owner's key. It returns the code, which only this machine and
// the browser it opens see.
func BookBrowserCode(ctx context.Context, c *Client, st keystore.Store, farm, owner string) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	code := hex.EncodeToString(raw)
	act, err := ownerAct(st, farm, owner, provenance.Subject{Name: "farm:" + farm, Digest: map[string]string{"c4": farm}},
		"browser-code", map[string]string{"code": people.CodeHash(code)})
	if err != nil {
		return "", err
	}
	if err := c.Issuer(ctx, http.MethodPost, "/v1/people/acts", act, nil); err != nil {
		return "", fmt.Errorf("booking the browser's code: %w", err)
	}
	return code, nil
}

// BrowserURL is page, an address of the HUD at base, with the browser's
// code where only the web app reads it: in the fragment's query
// (#/<route>?browser=<code>), which no request or log carries. installer
// is the installer code that signs the browser in, if any: /installer
// sends the browser on to the page's path, and a redirect with no fragment
// keeps the one the browser has, so the route and the code go in the
// fragment of the /installer address itself.
func BrowserURL(base, installer, page, code string) string {
	base = strings.TrimSuffix(base, "/")
	path, frag, _ := strings.Cut(page, "#")
	if frag == "" {
		frag = "/"
	}
	sep := "?"
	if strings.Contains(frag, "?") {
		sep = "&"
	}
	frag += sep + "browser=" + url.QueryEscape(code)
	if installer == "" {
		return path + "#" + frag
	}
	next := strings.TrimPrefix(path, base)
	if next == "" {
		next = "/"
	}
	return base + "/installer?" + url.Values{"code": {installer}, "next": {next}}.Encode() + "#" + frag
}

// WatchCode reads the code's state until the browser has enrolled with it,
// the code has lapsed, or wait has passed.
func WatchCode(ctx context.Context, c *Client, code string, wait, poll time.Duration) (*people.CodeState, error) {
	end := time.Now().Add(wait)
	for {
		var st people.CodeState
		if err := c.Issuer(ctx, http.MethodGet, "/v1/people/codes/"+people.CodeHash(code), nil, &st); err != nil {
			return nil, err
		}
		if st.State != "waiting" || time.Now().After(end) {
			return &st, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(poll):
		}
	}
}

// Countersign books the owner's countersignature of the browser's device:
// the act countersign-device on it, naming its record.
func Countersign(ctx context.Context, c *Client, st keystore.Store, farm, owner string, enrolled *people.CodeState) error {
	act, err := ownerAct(st, farm, owner, provenance.Subject{Name: "device:" + enrolled.Device, Digest: map[string]string{"c4": enrolled.Device}},
		"countersign-device", map[string]string{"record": enrolled.Record})
	if err != nil {
		return err
	}
	if err := c.Issuer(ctx, http.MethodPost, "/v1/people/acts", act, nil); err != nil {
		return fmt.Errorf("countersigning the browser: %w", err)
	}
	return nil
}

// Approving a machine with a touch (7, K4 item 5): a one-person farm
// reached from a second machine has no sign-in provider to vouch for it,
// so the person approves its key from this machine, with their touch. The
// device ID binds the approval: the other screen shows it, the person
// compares its short form here, and the new machine enrols on the booked
// approval within 10 minutes. What it is (its kind, its label) arrives in
// the new-device notice once it enrols, never from the new machine's word
// beforehand.

// ShortDevice is a device ID as a person compares it on two screens: the
// first eight characters after "c4", in two groups of four.
func ShortDevice(id string) string {
	s := strings.TrimPrefix(id, "c4")
	if len(s) < 8 {
		return s
	}
	return s[:4] + " " + s[4:8]
}

// ApproveDevice books the person's approval of a machine's key to enrol as
// them: approve-machine on device:<ID>, confirmed, signed with this
// machine's touch key, which asks for their touch.
func ApproveDevice(ctx context.Context, c *Client, st keystore.Store, farm, device string) (*people.Booked, error) {
	if !strings.HasPrefix(device, "c4") {
		return nil, fmt.Errorf("%q is not a device ID: copy the one the other machine shows (c4…)", device)
	}
	dev, err := st.Key(keystore.Device)
	if err != nil {
		return nil, err
	}
	devCert, err := LoadCert(st, keystore.Device)
	if err != nil || devCert == nil {
		return nil, fmt.Errorf("this machine's key is not enrolled: %v", err)
	}
	if !st.Has(keystore.Touch) {
		return nil, errors.New("this machine has no touch key: approve it from a machine that has one (c4 farm up makes one on a Mac)")
	}
	touch, err := st.Key(keystore.Touch)
	if err != nil {
		return nil, err
	}
	touchCert, err := LoadCert(st, keystore.Touch)
	if err != nil || touchCert == nil {
		return nil, fmt.Errorf("this machine's touch key is not enrolled: %v", err)
	}
	return approveWith(ctx, c, farm, device, dev, devCert, touch, touchCert)
}

// approveWith is ApproveDevice with its keys given.
func approveWith(ctx context.Context, c *Client, farm, device string, dev provenance.P256Signer, devCert *people.Certified,
	touch provenance.P256Signer, touchCert *people.Certified) (*people.Booked, error) {
	person := strings.TrimPrefix(devCert.Identity, provenance.IdentityBase+"user/")
	tc := deviceCert(touchCert)
	b, err := provenance.SignPersonAct(provenance.PersonAct{Farm: farm, Act: "approve-machine", Person: person, Confirmed: true,
		Subject: provenance.Subject{Name: "device:" + device, Digest: map[string]string{"c4": device}}, Params: json.RawMessage("{}")},
		dev, deviceCert(devCert), touch, &tc)
	if err != nil {
		return nil, err
	}
	var booked people.Booked
	if err := c.Issuer(ctx, http.MethodPost, "/v1/people/acts", json.RawMessage(b), &booked); err != nil {
		return nil, fmt.Errorf("approving the machine: %w", err)
	}
	return &booked, nil
}

// KeysFor is a farm's keys on this machine: ~/.c4/<farm>/keys, which an
// integrated agent's sandbox denies (K5). C4_KEYS=file keeps key files and
// makes no touch key, for a test or a scratch walk, which must never ask
// for a touch.
func KeysFor(farm string) keystore.Store {
	home, _ := os.UserHomeDir()
	return keystore.Store{Dir: filepath.Join(home, ".c4", farm, "keys"), FileOnly: os.Getenv("C4_KEYS") == "file"}
}

// OpenArgs are c4 farm open's (and up's) when it opens the HUD.
type OpenArgs struct {
	// Store is the farm's keys on this machine.
	Store keystore.Store
	// Page is the HUD page to open.
	Page string
	// Open opens the person's browser at an address.
	Open func(string) error
	// Wait bounds the wait for the browser to enrol: a code lives a
	// minute. Poll is how often the code is read.
	Wait, Poll time.Duration
	// Say tells the person what happened.
	Say func(format string, a ...any)
}

// OpenBrowser opens a page of the HUD in the person's browser, signed in.
// On a one-person farm whose owner key is on this machine, and whose HUD
// enrols browsers, the address carries a one-time code the owner's key
// booked: the web app enrols its key with it, and the CLI reads back the
// key the code enrolled, countersigns it, and prints it, so the person can
// see it is the one the HUD shows (PEOPLE-KEYS-2 3.3). A browser whose key
// is certified already ignores the code; the wait is short when the owner
// has a browser countersigned. Anywhere else the page opens as before.
func OpenBrowser(ctx context.Context, c *Client, a OpenArgs) error {
	say := a.Say
	if say == nil {
		say = func(string, ...any) {}
	}
	cfg := browserEnrols(ctx, c, a.Store)
	if cfg == nil {
		u, err := SignedIn(ctx, c, a.Page)
		if err != nil {
			return err
		}
		return a.Open(u)
	}
	code, err := BookBrowserCode(ctx, c, a.Store, cfg.Farm, cfg.Owner)
	if err != nil {
		return err
	}
	inst, err := InstallerCode(ctx, c)
	if err != nil {
		return err
	}
	if err := a.Open(BrowserURL(c.Base, inst, a.Page, code)); err != nil {
		return err
	}
	wait, poll := cmp.Or(a.Wait, time.Minute), cmp.Or(a.Poll, 500*time.Millisecond)
	had := hasBrowser(ctx, c, cfg.Owner)
	if had {
		wait = min(wait, enrolledWait)
	}
	s, err := WatchCode(ctx, c, code, wait, poll)
	if err != nil {
		return fmt.Errorf("the HUD is open, but whether the browser enrolled its key: %w", err)
	}
	if s.State != "enrolled" {
		if !had {
			say("browser   the HUD is open, but the browser did not enrol its key, so it reads and does not act yet: c4 farm open again enrols it\n")
		}
		return nil
	}
	if err := Countersign(ctx, c, a.Store, cfg.Farm, cfg.Owner, s); err != nil {
		return err
	}
	say("browser   its key is %s, countersigned; the HUD's device page shows the same\n", ShortDevice(s.Device))
	return nil
}

// enrolledWait is how long open waits on a code when the person has a
// browser enrolled already, which most likely ignores it: a fresh one
// enrols in a second or two.
var enrolledWait = 8 * time.Second

// browserEnrols is the farm's people's config when open enrols the
// browser: a one-person farm, this machine's owner key enrolled there, and
// a HUD whose web app enrols browsers.
func browserEnrols(ctx context.Context, c *Client, st keystore.Store) *people.Config {
	if cert, err := LoadCert(st, keystore.Device); err != nil || cert == nil {
		return nil
	}
	cfg, err := PeopleConfig(ctx, c)
	if err != nil || cfg.Owner == "" || !cfg.OwnerEnrolled {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, c.Base+"/js/enrol.js", nil)
	if err != nil {
		return nil
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	return cfg
}

// hasBrowser reports whether the person has a browser in force that the
// owner's CLI countersigned. It only sets how long open waits, so the
// farm's word is enough.
func hasBrowser(ctx context.Context, c *Client, person string) bool {
	var out struct {
		Devices []people.DeviceView `json:"devices"`
	}
	if err := c.Issuer(ctx, http.MethodGet, "/v1/people/devices?person="+url.QueryEscape(person), nil, &out); err != nil {
		return false
	}
	for _, d := range out.Devices {
		if d.Kind == "browser" && d.Countersigned {
			return true
		}
	}
	return false
}
