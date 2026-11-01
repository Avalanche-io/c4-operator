package farmcli

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Avalanche-io/c4-operator/hud/api"
	"github.com/Avalanche-io/c4-operator/internal/keystore"
	"github.com/Avalanche-io/c4-operator/internal/provenance"
	"github.com/Avalanche-io/c4-operator/internal/provenance/provtest"
	"github.com/Avalanche-io/c4-operator/schema"
)

// A person's act from the CLI carries its signed act in X-C4-Act, as the
// HUD checks it: verified against the farm's roots, by the person's
// device, on the subject the HUD served, of the act named, with the
// request's body as its params byte for byte. Nobody present signs
// nothing; an act that takes a touch goes unsigned, saying so once, when
// the machine has no touch key.
func TestActsCarryTheSignedAct(t *testing.T) {
	o := newOwnerIssuer(t)
	ctx := context.Background()
	st := keystore.Store{Dir: filepath.Join(t.TempDir(), "keys"), FileOnly: true}
	if _, err := EnrolOwner(ctx, o.client, st, "noor"); err != nil {
		t.Fatal(err)
	}
	acts, _ := provenance.ActNeedsList()
	needs := api.ActNeeds{Acts: map[string]api.ActNeed{}}
	for k, n := range acts {
		needs.Acts[k] = api.ActNeed{Person: n.Person, Touch: n.Touch, Relayed: n.Relayed}
	}
	o.mux.HandleFunc("GET /api/v1/acts/needs", func(w http.ResponseWriter, r *http.Request) { json.NewEncoder(w).Encode(needs) })
	var got []string
	var bodies [][]byte
	o.mux.HandleFunc("POST /api/v1/x", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got, bodies = append(got, r.Header.Get(ActHeader)), append(bodies, b)
		w.Write([]byte("{}"))
	})
	sub := provenance.ActSubject("submitgrant", "show-a", "dana", provtest.JobUID, provtest.JobSpecDigest)
	on := &api.ActOn{Name: sub.Name, Digest: sub.Digest}
	body := map[string]any{"reason": "she left the show", "n": 2}

	var said strings.Builder
	o.client.Acts = &Acts{Store: st, Present: true, Say: func(f string, v ...any) { fmt.Fprintf(&said, f, v...) }}
	if err := o.client.DoAct(ctx, "POST", "/x", "revoke", on, body, nil); err != nil {
		t.Fatal(err)
	}
	raw, err := base64.StdEncoding.DecodeString(got[0])
	if err != nil {
		t.Fatalf("no act: %q", got[0])
	}
	vr, err := provenance.VerifyBundle(raw, o.scratch.Roots)
	if err != nil {
		t.Fatal(err)
	}
	a, errs := schema.ParseAct(vr.Payload)
	if errs != nil {
		t.Fatal(errs)
	}
	want, _ := schema.ParamsID(a.Predicate.Params)
	sent, _ := schema.ParamsID(bodies[0])
	if a.Predicate.Act != "revoke" || a.Subject[0].Name != on.Name || a.Subject[0].Digest["sha256"] != on.Digest["sha256"] ||
		want != sent || a.Predicate.Chain.By.Person != "noor" || vr.Identity != provenance.UserIdentity("noor") {
		t.Errorf("the act: %+v %+v (params %s, body %s)", a.Predicate, a.Subject, want, sent)
	}

	// bind takes a touch, and a file store has no touch key
	for range 2 {
		if err := o.client.DoAct(ctx, "POST", "/x", "bind", on, body, nil); err != nil {
			t.Fatal(err)
		}
	}
	if got[1] != "" || got[2] != "" || strings.Count(said.String(), "bind takes your touch") != 1 || !strings.Contains(said.String(), "c4 farm touch-key") {
		t.Errorf("a touch act with no touch key: %q %q, said %q", got[1], got[2], said.String())
	}
	// nobody at the terminal: nothing signed
	o.client.Acts.Present = false
	if err := o.client.DoAct(ctx, "POST", "/x", "revoke", on, body, nil); err != nil || got[3] != "" {
		t.Errorf("signed with no one present: %v %q", err, got[3])
	}
}
