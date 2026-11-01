package server

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/Avalanche-io/c4"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/farmbook"
	"github.com/Avalanche-io/c4-operator/internal/provenance"
	"github.com/Avalanche-io/c4-operator/internal/provenance/people"
	"github.com/Avalanche-io/c4-operator/schema"
)

// bookHUD is a HUD that signs and keeps a farm book, and the queue's
// sealer over it.
type bookHUD struct {
	s      *Server
	roots  *x509.CertPool
	farm   string
	store  *farmbook.MemoryStore
	ptr    *farmbook.MemoryPointer
	own    *farmbook.MemorySealer
	sealer *farmbook.Sealer
}

// devices admits the people's issuer's device entries as written, in place
// of its checker.
type devices struct{}

func (devices) Kinds() []string { return []string{"device"} }
func (devices) Admit(_ context.Context, raw []byte, p farmbook.Placement) ([]byte, time.Time, error) {
	var e people.Entry
	if err := farmbook.Strict(raw, &e); err != nil {
		return nil, time.Time{}, err
	}
	e.Farm, e.Position, e.Prev, e.Order = p.Farm, p.Position, p.Prev, p.Order
	b, _ := json.Marshal(&e)
	c, err := schema.Canonical(b)
	return c, time.Now(), err
}

func newBookHUD(t *testing.T, objs ...runtime.Object) *bookHUD {
	t.Helper()
	s := guarded(t)
	ca, err := provenance.NewCA(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if s.Signer, err = provenance.NewSigner(context.Background(), provenance.LocalIssuer(ca, provenance.ComponentIdentity("hud"), time.Hour)); err != nil {
		t.Fatal(err)
	}
	queue, err := provenance.NewSigner(context.Background(), provenance.LocalIssuer(ca, provenance.ComponentIdentity("queue"), time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	scheme := runtime.NewScheme()
	clientgoscheme.AddToScheme(scheme)
	c4v1.AddToScheme(scheme)
	s.Client = fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objs...).Build()
	roots := x509.NewCertPool()
	roots.AddCert(ca.Cert)
	b := &bookHUD{s: s, roots: roots, farm: provenance.FarmID(ca.Cert), store: &farmbook.MemoryStore{}, ptr: &farmbook.MemoryPointer{}, own: &farmbook.MemorySealer{}}
	s.FarmBook = &farmbook.Writer{Store: b.store, Pointer: b.ptr}
	s.FarmBookReader = &farmbook.Reader{Farm: b.farm, Roots: roots, Store: b.store, Pointer: b.ptr, Own: b.own}
	b.sealer = &farmbook.Sealer{Farm: b.farm, Roots: roots, Store: b.store, Pointer: b.ptr, Own: b.own, Signer: queue, Log: logr.Discard(),
		Checkers: func() []farmbook.Checker {
			return []farmbook.Checker{&farmbook.ControlChecker{Farm: b.farm, Roots: roots, Store: b.store}, devices{}}
		}}
	return b
}

func (b *bookHUD) seal(t *testing.T) *farmbook.SealResult {
	t.Helper()
	res, err := b.sealer.Seal(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// send sends body to path with headers.
func send(s *Server, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}

// page reads GET /book/farm?since=, the query's other values in query.
func (b *bookHUD) page(t *testing.T, since int64, query string, headers map[string]string) *farmbook.Page {
	t.Helper()
	w := send(b.s, http.MethodGet, "/api/v1/book/farm?since="+strconv.FormatInt(since, 10)+query, "", headers)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /book/farm: %d %s", w.Code, w.Body)
	}
	var p farmbook.Page
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	return &p
}

// The farm-level acts the HUD signs are booked once their write took
// effect: binding the farm, and disabling and enabling a node, which is
// now a signed control record too. The queue seals each, and the route
// serves them with their signed segments, which a client checks.
func TestFarmLevelActsBooked(t *testing.T) {
	b := newBookHUD(t, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "render-07"}})
	s := b.s
	s.SinglePerson = "noor"
	code, out := bindCall(s, http.MethodPost, "wrangler", `{"owner": "noor", "to": "laptop-1/*"}`, nil)
	if code != http.StatusOK {
		t.Fatalf("bind: %d %v", code, out)
	}
	if st, _ := b.ptr.Read(context.Background()); len(st.Pending) != 1 {
		t.Fatalf("pending after the bind: %v", st.Pending)
	}
	installer := map[string]string{InstallerHeader: testInstallerToken}
	for _, disabled := range []bool{true, false} {
		w := send(s, http.MethodPost, "/api/v1/nodes/render-07/disable?as=wrangler", `{"disabled": `+strconv.FormatBool(disabled)+`}`, installer)
		if w.Code != http.StatusOK {
			t.Fatalf("disable %v: %d %s", disabled, w.Code, w.Body)
		}
	}
	// A refused bind writes nothing, and books nothing.
	if code, _ := bindCall(s, http.MethodPost, "wrangler", `{"owner": "dana", "to": "laptop-1/*"}`, nil); code != http.StatusConflict {
		t.Fatalf("a second owner: %d", code)
	}
	if res := b.seal(t); len(res.Refused) != 0 || res.Segment.Last != 3 {
		t.Fatalf("the seal: refused %+v, sealed to %d", res.Refused, res.Segment.Last)
	}
	p := b.page(t, 0, "&as=wrangler", installer)
	var acts []string
	for _, e := range p.Entries {
		var en farmbook.Entry
		json.Unmarshal(e.Entry, &en)
		acts = append(acts, en.Act+" "+en.Subject+" "+en.Person)
	}
	if strings.Join(acts, ", ") != "bind deploymentbinding:farm noor, disable node:render-07 , enable node:render-07 " {
		t.Errorf("booked %q", acts)
	}
	if last, err := farmbook.CheckPage(b.farm, b.roots, farmbook.LastHead{}, p); err != nil || last.Position != 3 {
		t.Errorf("a client's check of the page: %+v, %v", last, err)
	}
}

// A device entry's addresses are in its private part, which goes only to
// its person and the farm's admins: everyone else reads the entry without
// it, labelled, and the page still checks, entry by entry. An entry from
// before private parts, with its address in it, is withheld whole from
// anyone else, since its ID would give the address away.
func TestFarmBookRedactsDeviceEntries(t *testing.T) {
	b := newBookHUD(t)
	signsIn(b.s)
	ctx := context.Background()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for k, person := range []string{"noor", "dana"} {
		part := []byte(`{"addr":"192.0.2.1` + strconv.Itoa(k) + `","salt":"bm9vcm9yZGFuYXNhbHQ"}`)
		id := c4id(part)
		b.store.Put(ctx, id, part)
		e := &people.Entry{Kind: "device", Device: "c4" + strings.Repeat(strconv.Itoa(k+1), 88), Person: person, Private: id, Order: farmbook.OrderPending, At: now}
		if _, err := b.s.FarmBook.Append(ctx, farmbook.Submission{Entry: e}); err != nil {
			t.Fatal(err)
		}
	}
	b.seal(t)
	dana := map[string]string{"Authorization": "Bearer dana-token"}
	read := func() map[string][]string {
		out := map[string][]string{}
		p := b.page(t, 0, "", dana)
		if _, err := farmbook.CheckPage(b.farm, b.roots, farmbook.LastHead{}, p); err != nil {
			t.Errorf("a client's check of the page: %v", err)
		}
		for _, e := range p.Entries {
			var en, part struct {
				Person string `json:"person"`
				Addr   string `json:"addr"`
			}
			json.Unmarshal(e.Entry, &en)
			json.Unmarshal(e.Private, &part)
			out[en.Person] = append([]string{part.Addr}, e.Redacted...)
		}
		return out
	}
	got := read()
	if a := got["noor"]; len(a) != 2 || a[0] != "" || a[1] != farmbook.RedactedPrivate {
		t.Errorf("dana reads noor's device entry: %v", a)
	}
	if a := got["dana"]; len(a) != 1 || a[0] != "192.0.2.11" {
		t.Errorf("dana reads her own: %v", a)
	}
	b.s.People = &people.Issuer{Admin: func(_ context.Context, p string) bool { return p == "dana" }}
	if a := read()["noor"]; len(a) != 1 || a[0] != "192.0.2.10" {
		t.Errorf("an admin reads noor's: %v", a)
	}
	// An adopted entry with its address in it.
	legacy, red, err := people.Private(ctx, b.store, []byte(`{"kind":"device","person":"noor","addr":"192.0.2.10"}`), func(string) bool { return false })
	if err != nil || legacy != nil || len(red) != 1 || red[0] != farmbook.RedactedEntry {
		t.Errorf("a legacy entry with its address: %s %v %v", legacy, red, err)
	}
}

// c4id is the C4 ID of b.
func c4id(b []byte) string { return c4.Identify(bytes.NewReader(b)).String() }

// A node's enrolment offers are booked at most once a minute, so a host
// runner offering again and again can't flood the book.
func TestEnrolmentOffersBookedOnceAMinute(t *testing.T) {
	b := newBookHUD(t)
	ctx := context.Background()
	for k := 0; k < 3; k++ {
		rec, err := provenance.SignStatement(ctx, b.s.Signer, provenance.PredicateControl,
			[]provenance.Subject{{Name: "node:offers-07", Digest: map[string]string{"sha256": strings.Repeat("0", 64)}}},
			map[string]any{"actor": "c4-host:offers-07", "action": "enroll-offer", "target": "node:offers-07", "change": map[string]any{"holder": "noor", "n": k}}, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		b.s.keepFarmRecord(ctx, "enroll-offer", rec)
		b.s.bookOffer(ctx, "offers-07", c4id(rec))
	}
	if st, _ := b.ptr.Read(ctx); len(st.Pending) != 1 {
		t.Errorf("three offers in a minute booked %d entries", len(st.Pending))
	}
}

// A private part given with a page must be the one its entry names: a
// part swapped for another, or given with an entry that names none, is
// refused by a client's check.
func TestPrivatePartCheckedAgainstItsEntry(t *testing.T) {
	b := newBookHUD(t)
	signsIn(b.s)
	ctx := context.Background()
	part := []byte(`{"addr":"192.0.2.10","salt":"bm9vcm9yZGFuYXNhbHQ"}`)
	b.store.Put(ctx, c4id(part), part)
	e := &people.Entry{Kind: "device", Device: "c4" + strings.Repeat("1", 88), Person: "dana", Private: c4id(part), Order: farmbook.OrderPending,
		At: time.Now().UTC().Format(time.RFC3339Nano)}
	if _, err := b.s.FarmBook.Append(ctx, farmbook.Submission{Entry: e}); err != nil {
		t.Fatal(err)
	}
	b.seal(t)
	p := b.page(t, 0, "", map[string]string{"Authorization": "Bearer dana-token"})
	if _, err := farmbook.CheckPage(b.farm, b.roots, farmbook.LastHead{}, p); err != nil || len(p.Entries[0].Private) == 0 {
		t.Fatalf("dana's own page: %v", err)
	}
	p.Entries[0].Private = json.RawMessage(`{"addr":"203.0.113.9","salt":"bm9vcm9yZGFuYXNhbHQ"}`)
	if _, err := farmbook.CheckPage(b.farm, b.roots, farmbook.LastHead{}, p); !errors.Is(err, farmbook.ErrBook) {
		t.Errorf("a private part swapped: %v", err)
	}
}
