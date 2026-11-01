package server

import (
	"context"
	"crypto/x509"
	"encoding/json"
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

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/farmbook"
	"github.com/Avalanche-io/c4-operator/internal/provenance"
	"github.com/Avalanche-io/c4-operator/internal/provenance/people"
)

// bookHUD is a HUD that signs and keeps a farm book, and the queue's
// sealer over it.
type bookHUD struct {
	s      *Server
	roots  *x509.CertPool
	farm   string
	store  *farmbook.MemoryStore
	ptr    *farmbook.MemoryPointer
	sealer *farmbook.Sealer
}

// devices admits the people's issuer's device entries as written, in place
// of its checker.
type devices struct{}

func (devices) Kinds() []string { return []string{"device"} }
func (devices) Admit(context.Context, *farmbook.Entry, []byte) (time.Time, error) {
	return time.Time{}, nil
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
	b := &bookHUD{s: s, roots: roots, farm: provenance.FarmID(ca.Cert), store: &farmbook.MemoryStore{}, ptr: &farmbook.MemoryPointer{}}
	s.FarmBook = &farmbook.Writer{Store: b.store, Pointer: b.ptr}
	s.FarmBookReader = &farmbook.Reader{Farm: b.farm, Roots: roots, Store: b.store, Pointer: b.ptr}
	b.sealer = &farmbook.Sealer{Farm: b.farm, Roots: roots, Store: b.store, Pointer: b.ptr, Signer: queue, Log: logr.Discard(),
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

// Device entries' addresses go only to their person and the farm's
// admins; everyone else reads them redacted, labelled, by the people's
// issuer's own rule.
func TestFarmBookRedactsDeviceEntries(t *testing.T) {
	b := newBookHUD(t)
	signsIn(b.s)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for _, person := range []string{"noor", "dana"} {
		e := map[string]any{"kind": "device", "device": "c4" + strings.Repeat("1", 88), "person": person, "addr": "192.0.2.10", "order": farmbook.OrderPending, "at": now}
		if _, err := b.s.FarmBook.Append(context.Background(), farmbook.Submission{Entry: e}); err != nil {
			t.Fatal(err)
		}
	}
	b.seal(t)
	dana := map[string]string{"Authorization": "Bearer dana-token"}
	read := func() map[string][]string {
		out := map[string][]string{}
		for _, e := range b.page(t, 0, "", dana).Entries {
			var en struct {
				Person string `json:"person"`
				Addr   string `json:"addr"`
			}
			json.Unmarshal(e.Entry, &en)
			out[en.Person] = append([]string{en.Addr}, e.Redacted...)
		}
		return out
	}
	got := read()
	if a := got["noor"]; len(a) != 2 || a[0] != "" || a[1] != "addr" {
		t.Errorf("dana reads noor's device entry: %v", a)
	}
	if a := got["dana"]; len(a) != 1 || a[0] != "192.0.2.10" {
		t.Errorf("dana reads her own: %v", a)
	}
	b.s.People = &people.Issuer{Admin: func(_ context.Context, p string) bool { return p == "dana" }}
	if a := read()["noor"]; len(a) != 1 || a[0] != "192.0.2.10" {
		t.Errorf("an admin reads noor's: %v", a)
	}
}
