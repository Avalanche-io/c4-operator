package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/hud/api"
	"github.com/Avalanche-io/c4-operator/internal/farmcli"
	"github.com/Avalanche-io/c4-operator/internal/keystore"
	"github.com/Avalanche-io/c4-operator/internal/provenance/people"
	"github.com/Avalanche-io/c4-operator/internal/provenance/provtest"
)

// The CLI's own signer, end to end: noor's key, enrolled by c4 farm up's
// step at the farm's people's issuer, signs her revoke of a grant through
// farmcli, and the HUD takes the act and cites it, signed. Her grant,
// which takes a touch, goes unsigned from a machine with no touch key, as
// before signed acts.
func TestTheCLIsActsAreTaken(t *testing.T) {
	s := grantFarm(t)
	s.DevViewers["noor"] = &api.Viewer{User: "noor", Projects: []string{"show-a"}}
	sf := provtest.NewFarm(t, time.Now())
	g := newActRigOn(t, s, sf)
	idx := &people.Index{Farm: sf.ID, Roots: sf.Roots, Store: &people.MemoryStore{}, Head: &people.MemoryHead{}}
	s.PeopleIndex = idx
	const installer = "5d2e8a1c7b3f4e6a9c0d1e2f3a4b5c6d7e8f9a0b1c2d3e4f5a6b7c8d9e0f1a2b"
	iss := &people.Issuer{CA: sf.CA, Index: idx, Owner: func(context.Context) string { return "noor" },
		InstallerToken: []byte(installer), LocalOnly: true, Admin: func(_ context.Context, p string) bool { return p == "noor" }}
	mux := http.NewServeMux()
	mux.Handle("/issuer/v1/people/", http.StripPrefix("/issuer", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		iss.ServeHTTP(w, r.WithContext(people.WithCaller(r.Context(), "noor")))
	})))
	// the revoke's answer, which names its record
	var revoked []byte
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			s.Handler().ServeHTTP(w, r)
			return
		}
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, r)
		revoked = rec.Body.Bytes()
		for k, v := range rec.Header() {
			w.Header()[k] = v
		}
		w.WriteHeader(rec.Code)
		w.Write(revoked)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	ctx := context.Background()
	c, err := farmcli.NewClient(farmcli.Farm{HUD: srv.URL, As: "noor"})
	if err != nil {
		t.Fatal(err)
	}
	c.Installer = installer
	st := keystore.Store{Dir: filepath.Join(t.TempDir(), "keys"), FileOnly: true}
	if _, err := farmcli.EnrolOwner(ctx, c, st, "noor"); err != nil {
		t.Fatal(err)
	}
	// the HUD's development viewer needs no installer token here
	c.Installer = ""
	var said strings.Builder
	c.Acts = &farmcli.Acts{Store: st, Present: true, Say: func(f string, v ...any) { said.WriteString(f) }}

	o := farmcli.Out{W: &strings.Builder{}}
	if err := farmcli.Grant(ctx, c, o, "show-a", api.GrantRequest{Tracker: "github", Pools: []string{"cpu"}, Until: "7d"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(said.String(), "takes your touch") {
		t.Errorf("the grant went without saying why it is unsigned: %q", said.String())
	}
	gs := grants(t, s, "?as=noor")
	if len(gs) != 1 {
		t.Fatalf("grants %+v", gs)
	}
	// the API server gives every object a UID; the fake doesn't
	withUID(t, s, &c4v1.SubmitGrant{ObjectMeta: metav1.ObjectMeta{Namespace: gs[0].Namespace, Name: gs[0].Name}}, "9b0e0001-0000-4000-8000-000000000000")
	if gs = grants(t, s, "?as=noor"); gs[0].ActOn == nil {
		t.Fatalf("grants %+v", gs)
	}
	on := gs[0].ActOn
	if err := farmcli.Revoke(ctx, c, o, gs[0].Namespace+"/"+gs[0].Name); err != nil {
		t.Fatalf("the CLI's signed revoke: %v", err)
	}
	// the revoke's record, from the project's store, cites the act
	var out api.GrantResponse
	if err := json.Unmarshal(revoked, &out); err != nil || out.Record == "" {
		t.Fatalf("the revoke's answer: %v %s", err, revoked)
	}
	cites(t, "the CLI's revoke", http.StatusOK, nil, g.predicate(t, "show-a", out.Record), on, CustodySigned)
}
