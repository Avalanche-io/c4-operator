package farmcli

import (
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Avalanche-io/c4-operator/internal/keystore"
	"github.com/Avalanche-io/c4-operator/internal/provenance"
	"github.com/Avalanche-io/c4-operator/internal/provenance/people"
	"github.com/Avalanche-io/c4-operator/schema"
)

// New-device notices from outside the HUD (PEOPLE-KEYS-2 7, K4 item 6): a
// compromised HUD could enrol a device and hide it from its own pages, so
// on each run the CLI reads the people's index itself, from where it last
// stopped, checks each new device record of its person's, its ID token
// against the provider key the index booked, and says so. It keeps the
// last signed head and refuses an index whose head goes back, since the
// manager holds the issuer's key (10.4 item 19).

// indexWatch is what the CLI keeps of the people's index, beside its keys.
type indexWatch struct {
	// Position is how far it has read; Head and HeadPosition the last head
	// the issuer signed that it saw.
	Position     int64  `json:"position"`
	Head         string `json:"head,omitempty"`
	HeadPosition int64  `json:"headPosition"`
	// ProviderKeys are the provider keys the index booked, by the C4 ID of
	// each one's record (a device record's providerKey), with the
	// provider's own key ID.
	ProviderKeys map[string]string `json:"providerKeys,omitempty"`
}

func watchPath(st keystore.Store) string { return filepath.Join(st.Dir, "index.json") }

func loadWatch(st keystore.Store) (*indexWatch, error) {
	b, err := os.ReadFile(watchPath(st))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var w indexWatch
	return &w, json.Unmarshal(b, &w)
}

func saveWatch(st keystore.Store, w *indexWatch) error {
	b, err := json.Marshal(w)
	if err != nil {
		return err
	}
	return os.WriteFile(watchPath(st), b, 0o600)
}

// DeviceNotice is a new device of the person's, as the index booked it.
type DeviceNotice struct {
	Device, Kind, Label, Evidence, At string
	// Problem says what didn't check: its record, or its ID token.
	Problem string
}

// String is the notice as the CLI says it.
func (n DeviceNotice) String() string {
	what := n.Kind
	if n.Label != "" {
		what = n.Label + " (" + n.Kind + ")"
	}
	s := fmt.Sprintf("a new device signed in as you: %s, %s, on %s", what, n.At, n.Evidence)
	if n.Problem != "" {
		s += "; it doesn't check: " + n.Problem
	}
	return s + ". If it wasn't you, remove it from another of your devices"
}

// errNoHead is a page of the index without the head the issuer signed.
var errNoHead = errors.New("the farm served no signed head")

// ErrIndexRollback means the people's index the farm served goes back from
// a head the CLI saw signed, or forks from it.
var ErrIndexRollback = errors.New("the farm's people index goes back from what this machine saw")

// NewDevices reads the people's index from where this machine last
// stopped and returns the person's devices booked since. The first run
// reads it all, and reports only devices enrolled after this machine's own.
func NewDevices(ctx context.Context, c *Client, st keystore.Store, cfg *people.Config) ([]DeviceNotice, error) {
	mine, err := LoadCert(st, keystore.Device)
	if err != nil || mine == nil {
		return nil, err
	}
	roots, err := farmRoots(mine, cfg.Farm)
	if err != nil {
		return nil, err
	}
	w, err := loadWatch(st)
	if err != nil {
		return nil, err
	}
	first := w == nil
	if first {
		w = &indexWatch{}
	}
	if w.ProviderKeys == nil {
		w.ProviderKeys = map[string]string{}
	}
	var page people.IndexPage
	for try := 0; ; try++ {
		page = people.IndexPage{}
		if err := c.Issuer(ctx, http.MethodGet, "/v1/people/index?from="+strconv.FormatInt(w.Position, 10), nil, &page); err != nil {
			return nil, fmt.Errorf("the farm's people index: %w", err)
		}
		err := checkPage(&page, w, roots, cfg.Farm)
		if err == nil {
			break
		}
		// a head that moved between the page's two reads comes without its
		// signature once; a page that never has one is behind
		if !errors.Is(err, errNoHead) || try > 0 {
			return nil, err
		}
	}
	person := strings.TrimPrefix(mine.Identity, provenance.IdentityBase+"user/")
	var out []DeviceNotice
	for _, e := range page.Entries {
		if e.Entry == nil {
			continue
		}
		switch {
		case e.Entry.Kind == people.KindProviderKey && e.Entry.Record != "":
			w.ProviderKeys[e.Entry.Record] = e.Entry.KeyID
		case e.Entry.Kind == people.KindDevice && e.Entry.Person == person && e.Entry.Device != mine.Device &&
			(!first || e.Entry.Position > mine.Position):
			out = append(out, checkDevice(ctx, c, roots, cfg, e.Entry, w.ProviderKeys, mine))
		}
	}
	w.Position = page.Position
	if page.Head != "" {
		w.Head, w.HeadPosition = page.Head, page.Position
	}
	return out, saveWatch(st, w)
}

// farmRoots is the farm's CA, from this machine's own certificate chain,
// checked to be the farm's.
func farmRoots(mine *people.Certified, farm string) (*x509.CertPool, error) {
	if len(mine.Chain) < 2 {
		return nil, errors.New("this machine's certificate names no farm CA")
	}
	ca, err := x509.ParseCertificate(mine.Chain[len(mine.Chain)-1])
	if err != nil {
		return nil, err
	}
	if provenance.FarmID(ca) != farm {
		return nil, fmt.Errorf("this machine's key is certified at farm %.16s, and this farm is %.16s", provenance.FarmID(ca), farm)
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	return pool, nil
}

// checkPage checks a page of the index against what the CLI kept: its
// entries chain from the last one read, the head the issuer signed is
// that chain's, and it never goes back from or forks the last head seen.
func checkPage(page *people.IndexPage, w *indexWatch, roots *x509.CertPool, farm string) error {
	if page.Farm != farm {
		return fmt.Errorf("the people index is farm %.16s's, not %.16s's", page.Farm, farm)
	}
	if page.Position < w.HeadPosition {
		return fmt.Errorf("%w: its position is %d, and this machine saw %d signed", ErrIndexRollback, page.Position, w.HeadPosition)
	}
	// a page runs to the head, so it carries the head the issuer signed;
	// one without is not where this machine left the index, unless the
	// index is empty
	if page.Head == "" && (page.Position > 0 || w.HeadPosition > 0) {
		return fmt.Errorf("%w: %w at position %d", ErrIndexRollback, errNoHead, page.Position)
	}
	prev := ""
	if w.Position == w.HeadPosition {
		prev = w.Head
	}
	for k, e := range page.Entries {
		if e.Entry == nil {
			continue
		}
		if (k > 0 || prev != "") && e.Entry.Prev != prev {
			return fmt.Errorf("%w: entry %d follows %.16s, not %.16s", ErrIndexRollback, e.Entry.Position, e.Entry.Prev, prev)
		}
		prev = e.ID
	}
	if page.Head == "" {
		return nil
	}
	if len(page.Entries) > 0 && page.Head != prev {
		return fmt.Errorf("%w: the signed head is %.16s, and the last entry %.16s", ErrIndexRollback, page.Head, prev)
	}
	if page.Position == w.HeadPosition && w.Head != "" && page.Head != w.Head {
		return fmt.Errorf("%w: a head at %d other than the one it saw", ErrIndexRollback, page.Position)
	}
	v, err := provenance.VerifyBundle(page.Signed, roots)
	if err != nil {
		return fmt.Errorf("the people index's signed head: %w", err)
	}
	var h struct {
		Farm     string `json:"farm"`
		Head     string `json:"head"`
		Position int64  `json:"position"`
	}
	json.Unmarshal(v.Predicate, &h)
	switch {
	case v.PredicateType != people.PredicateIndexHead || v.Identity != provenance.ComponentIdentity("issuer"):
		return fmt.Errorf("the people index's head is signed by %s, not the people's issuer", v.Identity)
	case h.Farm != farm || h.Head != page.Head || h.Position != page.Position:
		return fmt.Errorf("%w: the head names %.16s at %d, and its signature %.16s at %d", ErrIndexRollback, page.Head, page.Position, h.Head, h.Position)
	}
	return nil
}

// checkDevice reads a new device's record and checks it: the issuer's
// device record for the person, and its ID token, where it has one,
// against the provider key the index booked and the person's own sign-in.
func checkDevice(ctx context.Context, c *Client, roots *x509.CertPool, cfg *people.Config, e *people.Entry,
	keys map[string]string, mine *people.Certified) DeviceNotice {
	n := DeviceNotice{Device: e.Device, At: e.At, Kind: "device"}
	d, err := deviceRecord(ctx, c, roots, e.Record)
	if err != nil {
		n.Problem = err.Error()
		return n
	}
	n.Kind, n.Label, n.Evidence = d.Kind, d.Label, d.Evidence
	switch {
	case d.Person != e.Person || schema.DeviceID(mustKey(d)) != e.Device:
		n.Problem = "its record names another person or key than the index"
	case d.IDToken == "":
		// enrolled on no sign-in: the installer, the owner's code, or an
		// approval with a touch
	case cfg.Provider == nil:
		n.Problem = "it was enrolled on an ID token, and the farm has no sign-in provider now"
	default:
		n.Problem = checkToken(ctx, c, roots, cfg, d, keys, mine)
	}
	return n
}

// checkToken checks a device record's ID token as a verifier does
// (people.CheckBookedToken), against the person's own sign-in: the
// subject this machine's own record was enrolled on.
func checkToken(ctx context.Context, c *Client, roots *x509.CertPool, cfg *people.Config, d *schema.DevicePredicate,
	keys map[string]string, mine *people.Certified) string {
	if _, ok := keys[d.ProviderKey]; !ok {
		return "the index books no provider key " + d.ProviderKey
	}
	var jwk json.RawMessage
	if err := c.Issuer(ctx, http.MethodGet, "/v1/people/records/"+d.ProviderKey, nil, &jwk); err != nil {
		return "its provider key: " + err.Error()
	}
	own, err := deviceRecord(ctx, c, roots, mine.Record)
	if err != nil {
		return "this machine's own record: " + err.Error()
	}
	sub := tokenSubject(own.IDToken)
	if sub == "" {
		return "this machine's own record names no sign-in to compare it with"
	}
	ins := people.StaticSignIns{d.Person: {{Issuer: cfg.Provider.Issuer, Subject: sub}}}
	if err := people.CheckBookedToken(d, jwk, cfg.Provider.Provider, ins); err != nil {
		return err.Error()
	}
	return ""
}

// deviceRecord is a device record by its C4 ID, checked to be the people's
// issuer's.
func deviceRecord(ctx context.Context, c *Client, roots *x509.CertPool, id string) (*schema.DevicePredicate, error) {
	var raw json.RawMessage
	if err := c.Issuer(ctx, http.MethodGet, "/v1/people/records/"+id, nil, &raw); err != nil {
		return nil, err
	}
	v, err := provenance.VerifyBundle(raw, roots)
	if err != nil {
		return nil, fmt.Errorf("its record: %w", err)
	}
	if err := provenance.CheckSigner(v); err != nil {
		return nil, fmt.Errorf("its record: %w", err)
	}
	var d schema.DevicePredicate
	if err := json.Unmarshal(v.Predicate, &d); err != nil {
		return nil, err
	}
	return &d, nil
}

func mustKey(d *schema.DevicePredicate) []byte {
	k, _ := d.PublicKey()
	return k
}

// tokenSubject is an ID token's subject, read without checking it: a token
// the index booked for this machine's own record.
func tokenSubject(tok string) string {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return ""
	}
	b, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var c struct {
		Sub string `json:"sub"`
	}
	json.Unmarshal(b, &c)
	return c.Sub
}
