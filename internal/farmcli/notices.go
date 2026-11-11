package farmcli

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/Avalanche-io/c4"

	"github.com/Avalanche-io/c4-operator/internal/farmbook"
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
	// Book is the last head of the farm book this machine checked, on a
	// farm that keeps one (farmbook.CheckPage).
	Book farmbook.LastHead `json:"book"`
	// pending is how many entries the last page read held waiting for
	// the book's seal: never read as the farm's record, only as a reason
	// to look again (CheckOwner). Not kept.
	pending int
	// Seen are the devices the index booked that this machine has read,
	// so a renewal is not a new device; Owner is the farm's owner key as
	// the index books it, its first device enrolled on the installer.
	Seen  map[string]bool `json:"seen,omitempty"`
	Owner string          `json:"owner,omitempty"`
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
	return keystore.WriteFile(watchPath(st), b)
}

// DeviceNotice is a new device of the person's, as the index booked it.
type DeviceNotice struct {
	Device, Kind, Label, Evidence, At string
	// Problem says what didn't check: its record, or its ID token.
	Problem string
}

// String is the notice as the CLI says it. Its label is the device's own
// claim, and every field is printable text only: a device names itself,
// and a name must not move the cursor or write the clipboard.
func (n DeviceNotice) String() string {
	what := Printable(n.Kind)
	if n.Label != "" {
		what = Printable(n.Kind) + ` that calls itself "` + Printable(n.Label) + `"`
	}
	s := fmt.Sprintf("a new device signed in as you: %s (%s), %s, on %s", what, ShortDevice(n.Device), Printable(n.At), Printable(n.Evidence))
	if n.Problem != "" {
		s += "; it doesn't check: " + Printable(n.Problem)
	}
	return s + ". If it wasn't you, remove it from another of your devices"
}

// Printable is s with nothing a terminal acts on: no control characters
// (C0, DEL, C1, so no escape sequence), no bidirectional overrides, and at
// most 80 characters.
func Printable(s string) string {
	var b strings.Builder
	n := 0
	for _, r := range s {
		switch {
		case r < 0x20 || r == 0x7f || r >= 0x80 && r <= 0x9f:
			continue
		case r >= 0x202a && r <= 0x202e || r >= 0x2066 && r <= 0x2069 || r == 0x200e || r == 0x200f || r == 0x061c:
			continue
		case unicode.Is(unicode.Cc, r) || unicode.Is(unicode.Cf, r):
			continue
		}
		if n == 80 {
			b.WriteString("…")
			break
		}
		b.WriteRune(r)
		n++
	}
	return b.String()
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
	if w.Seen == nil {
		w.Seen = map[string]bool{}
	}
	var entries []*people.Entry
	if cfg.Order == people.OrderBook {
		entries, err = readBook(ctx, c, roots, cfg.Farm, w)
	} else {
		entries, err = readIndex(ctx, c, roots, cfg.Farm, w)
	}
	if err != nil {
		return nil, err
	}
	person := strings.TrimPrefix(mine.Identity, provenance.IdentityBase+"user/")
	var out []DeviceNotice
	// On the first run the devices before this machine's own are known:
	// only those after it are new.
	mineSeen := !first
	for _, e := range entries {
		switch {
		case e.Kind == people.KindProviderKey && e.Record != "":
			w.ProviderKeys[e.Record] = e.KeyID
		case e.Kind != people.KindDevice:
		case w.Seen[e.Device]:
			// a renewal
		default:
			w.Seen[e.Device] = true
			if w.Owner == "" && e.TouchFor == "" {
				// the farm's owner key: the first device the installer
				// token enrolled, by its record
				if d, err := deviceRecord(ctx, c, roots, e.Record); err == nil && d.Evidence == schema.EvidenceInstaller && d.TouchFor == "" {
					w.Owner = e.Device
				}
			}
			switch {
			case e.Device == mine.Device:
				mineSeen = true
			case e.Person == person && mineSeen:
				out = append(out, checkDevice(ctx, c, roots, cfg, e, w.ProviderKeys, mine))
			}
		}
	}
	return out, saveWatch(st, w)
}

// readIndex reads the people's own index from where this machine stopped:
// each entry hashed from what was served and checked against its ID, the
// page against the head the issuer signed, and the head against the last
// one seen.
func readIndex(ctx context.Context, c *Client, roots *x509.CertPool, farm string, w *indexWatch) ([]*people.Entry, error) {
	var page people.IndexPage
	var raw rawEntries
	for try := 0; ; try++ {
		var b json.RawMessage
		if err := c.Issuer(ctx, http.MethodGet, "/v1/people/index?from="+strconv.FormatInt(w.Position, 10), nil, &b); err != nil {
			return nil, fmt.Errorf("the farm's people index: %w", err)
		}
		page, raw = people.IndexPage{}, rawEntries{}
		if err := json.Unmarshal(b, &page); err != nil {
			return nil, fmt.Errorf("the farm's people index: %w", err)
		}
		if err := json.Unmarshal(b, &raw); err != nil || len(raw.Entries) != len(page.Entries) {
			return nil, fmt.Errorf("the farm's people index: its entries don't read: %v", err)
		}
		err := checkPage(&page, w, roots, farm)
		if err == nil {
			break
		}
		// a head that moved between the page's two reads comes without its
		// signature once; a page that never has one is behind
		if !errors.Is(err, errNoHead) || try > 0 {
			return nil, err
		}
	}
	var out []*people.Entry
	for k, e := range raw.Entries {
		if e.ID != page.Entries[k].ID {
			return nil, fmt.Errorf("%w: entry %d's ID", ErrIndexRollback, k)
		}
		if len(e.Entry) == 0 || string(e.Entry) == "null" {
			continue
		}
		// the entry is what its ID names, byte for byte as the issuer
		// wrote it: a HUD that rewrote it is refused, not believed
		canon, err := schema.Canonical(e.Entry)
		if err != nil || c4.Identify(bytes.NewReader(canon)).String() != e.ID {
			return nil, fmt.Errorf("%w: entry %.16s is not the entry its ID names", ErrIndexRollback, e.ID)
		}
		var pe people.Entry
		if err := json.Unmarshal(e.Entry, &pe); err != nil {
			return nil, err
		}
		out = append(out, &pe)
	}
	w.Position = page.Position
	if page.Head != "" {
		w.Head, w.HeadPosition = page.Head, page.Position
	}
	return out, nil
}

// rawEntries are an index page's entries as served, to hash.
type rawEntries struct {
	Entries []struct {
		ID    string          `json:"id"`
		Entry json.RawMessage `json:"entry"`
	} `json:"entries"`
}

// readBook reads a farm book from the last head this machine checked, as
// a verifier does (farmbook.CheckPage): every segment the queue's and this
// farm's, following the last by number, link and time, every entry the
// one its segment seals and hashing to its ID. It returns the people's
// entries the book sealed since; pending ones count once sealed.
func readBook(ctx context.Context, c *Client, roots *x509.CertPool, farm string, w *indexWatch) ([]*people.Entry, error) {
	var out []*people.Entry
	for {
		var p farmbook.Page
		if err := c.Do(ctx, http.MethodGet, "/book/farm?since="+strconv.FormatInt(w.Book.Position, 10), nil, &p); err != nil {
			return nil, fmt.Errorf("the farm book: %w", err)
		}
		last, err := farmbook.CheckPage(farm, roots, w.Book, &p)
		if errors.Is(err, farmbook.ErrRollback) {
			return nil, fmt.Errorf("%w: %v", ErrIndexRollback, err)
		}
		if err != nil {
			return nil, fmt.Errorf("the farm book doesn't check: %w", err)
		}
		for _, pe := range p.Entries {
			if len(pe.Entry) == 0 || string(pe.Entry) == "null" {
				continue
			}
			var e people.Entry
			if json.Unmarshal(pe.Entry, &e) != nil || e.Kind == "" {
				continue
			}
			out = append(out, &e)
		}
		w.Book = last
		if !p.More {
			w.pending = len(p.Pending)
			return out, nil
		}
	}
}

// farmOwner is the farm's owner key as its people's index books it: the
// first device the installer token enrolled, read from the start and
// checked as each run's reading is, without moving what this machine
// keeps; "" when the index books none. With it, how many entries the farm
// book held pending its seal when it was read (0 on a farm without a
// book). A device entry
// whose record does not load is an error, not an entry that books no
// owner: when no owner is found it is the answer, so a read that failed
// never reads as a farm with no owner.
func farmOwner(ctx context.Context, c *Client, st keystore.Store, cfg *people.Config) (string, int, error) {
	mine, err := LoadCert(st, keystore.Device)
	if err != nil || mine == nil {
		return "", 0, err
	}
	roots, err := farmRoots(mine, cfg.Farm)
	if err != nil {
		return "", 0, err
	}
	w := &indexWatch{}
	var entries []*people.Entry
	if cfg.Order == people.OrderBook {
		entries, err = readBook(ctx, c, roots, cfg.Farm, w)
	} else {
		entries, err = readIndex(ctx, c, roots, cfg.Farm, w)
	}
	if err != nil {
		return "", 0, err
	}
	var unread error
	for _, e := range entries {
		if e.Kind != people.KindDevice || e.TouchFor != "" {
			continue
		}
		d, err := deviceRecord(ctx, c, roots, e.Record)
		if err != nil {
			if unread == nil {
				unread = fmt.Errorf("device %.16s: %w", e.Device, err)
			}
			continue
		}
		if d.Evidence == schema.EvidenceInstaller && d.TouchFor == "" {
			return e.Device, w.pending, nil
		}
	}
	return "", w.pending, unread
}

// CheckOwner checks the farm's owner key, as its index books it, against
// the one this machine pinned when up enrolled it: a farm whose owner key
// is another is refused, whatever its HUD says. Nothing is pinned on a
// farm up didn't make.
//
// On a farm with a farm book, an entry is the farm's record only once the
// book seals it, seconds after it is written (farmbook.DefaultSealEvery):
// up checks the owner it has just enrolled before the seal, and a farm
// that books no owner yet while entries wait for the seal is read again
// until it seals them, for at most ownerSealWait. The entries waiting are
// never read as the record, only as the reason to look again; a farm that
// books another owner, or none with nothing waiting, is refused at once.
func CheckOwner(ctx context.Context, c *Client, st keystore.Store, cfg *people.Config, pinned string) error {
	if pinned == "" {
		return nil
	}
	deadline := time.Now().Add(ownerSealWait)
	owner, pending, err := farmOwner(ctx, c, st, cfg)
	for err == nil && owner == "" && pending > 0 && time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return fmt.Errorf("the farm's owner key: %w", ctx.Err())
		case <-time.After(ownerSealPoll):
		}
		owner, pending, err = farmOwner(ctx, c, st, cfg)
	}
	if err != nil {
		return fmt.Errorf("the farm's owner key: %w", err)
	}
	if owner == "" && pending > 0 {
		return fmt.Errorf("%w: its farm book sealed no owner within %s, with entries still waiting for its seal (%d), and this machine pinned %s when up enrolled it; run up again once the book has sealed",
			ErrOwnerKey, ownerSealWait, pending, pinned)
	}
	if owner != pinned {
		if owner == "" {
			owner = "none"
		}
		return fmt.Errorf("%w: its index books %s, and this machine pinned %s when up enrolled it; if the farm was made again, remove its keys (%s) and run up again",
			ErrOwnerKey, owner, pinned, st.Dir)
	}
	return nil
}

// ownerSealWait is how long CheckOwner waits for a farm book to seal the
// entries it holds pending: twice the longest seal interval a farm may
// set. ownerSealPoll is how often it looks.
var (
	ownerSealWait = 2 * farmbook.MaxSealEvery
	ownerSealPoll = 250 * time.Millisecond
)

// certFarm is the farm a certificate was issued at: its CA's farm ID,
// from the chain itself, never the HUD's word.
func certFarm(cert *people.Certified) (string, error) {
	if cert == nil || len(cert.Chain) < 2 {
		return "", errors.New("this machine's certificate names no farm CA")
	}
	ca, err := x509.ParseCertificate(cert.Chain[len(cert.Chain)-1])
	if err != nil {
		return "", err
	}
	return provenance.FarmID(ca), nil
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
	if err := c.Issuer(ctx, http.MethodGet, "/v1/people/records/"+url.PathEscape(d.ProviderKey), nil, &jwk); err != nil {
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
	if err := c.Issuer(ctx, http.MethodGet, "/v1/people/records/"+url.PathEscape(id), nil, &raw); err != nil {
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
