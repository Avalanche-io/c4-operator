package server

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"testing"
	"time"

	k8stypes "k8s.io/apimachinery/pkg/types"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/hud/api"
	"github.com/Avalanche-io/c4-operator/internal/bindings"
	"github.com/Avalanche-io/c4-operator/internal/farmbook"
	"github.com/Avalanche-io/c4-operator/internal/provenance"
)

// deploymentFarm is a HUD that signs, its farm bound to dana (a shared
// farm), with dana, noor and lee as development viewers who may write
// bindings, and acts taken against a scratch farm's devices.
func deploymentFarm(t *testing.T) (*Server, *actRig, map[string]string) {
	t.Helper()
	s, _ := bindFarm(t)
	g := newActRig(t, s)
	for _, p := range []string{"dana", "noor", "lee"} {
		s.DevViewers[p] = &api.Viewer{User: p, Role: "farm-admin", Projects: []string{"show-a"}}
	}
	headers := map[string]string{InstallerHeader: testInstallerToken}
	if code, out := bindCall(s, http.MethodPost, "wrangler", `{"owner": "dana", "to": "studio/*"}`, nil); code != http.StatusOK {
		t.Fatalf("bind the farm: %d %v", code, out)
	}
	return s, g, headers
}

// readBinding reads a binding by name.
func readBinding(t *testing.T, s *Server, name string) *c4v1.DeploymentBinding {
	t.Helper()
	var b c4v1.DeploymentBinding
	if err := s.Client.Get(context.Background(), k8stypes.NamespacedName{Name: name}, &b); err != nil {
		t.Fatal(err)
	}
	return &b
}

func bindingNamedIn(out map[string]any) string {
	b, _ := out["binding"].(map[string]any)
	n, _ := b["name"].(string)
	return n
}

// A farm admin binds one deployment of the farm to its owner, overriding
// the farm binding for it: to names exactly one deployment of this farm;
// for only narrows what the signer policy lets it sign; a deployment
// bound already takes a binding that follows it; components are the farm
// owner's. A new owner follows it and ends it at the same instant; its
// owner ends it silently, a farm admin with a touch; GET lists them with
// whether each is in force and standing.
func TestDeploymentBindings(t *testing.T) {
	s, g, headers := deploymentFarm(t)
	post := func(as, path string, body any, act string) (int, map[string]any, *actRecord) {
		return g.send(t, http.MethodPost, path+"?as="+as, body, act, "show-a", headers)
	}
	for _, c := range []struct {
		name, as string
		body     map[string]any
		code     int
	}{
		{"not the farm owner", "noor", map[string]any{"owner": "noor", "to": "studio/node/laptop-1"}, http.StatusForbidden},
		{"a node signing control records", "dana", map[string]any{"owner": "noor", "to": "studio/node/laptop-1", "for": []string{"task/v1", "control/v1"}}, http.StatusBadRequest},
		{"the whole farm", "dana", map[string]any{"owner": "noor", "to": "studio/*"}, http.StatusBadRequest},
		{"a prefix", "dana", map[string]any{"owner": "noor", "to": "studio/node"}, http.StatusBadRequest},
		{"another farm", "dana", map[string]any{"owner": "noor", "to": "kestrel/node/laptop-1"}, http.StatusBadRequest},
		{"a component to another owner", "dana", map[string]any{"owner": "noor", "to": "studio/component/hud"}, http.StatusBadRequest},
		{"an end in the past", "dana", map[string]any{"owner": "noor", "to": "studio/node/laptop-1", "end": "2020-01-01T00:00:00Z"}, http.StatusBadRequest},
	} {
		if code, out, _ := post(c.as, "/api/v1/bindings", c.body, ""); code != c.code {
			t.Errorf("%s: %d %v, want %d", c.name, code, out, c.code)
		}
	}
	code, out, rec := post("dana", "/api/v1/bindings", map[string]any{"owner": "noor", "to": "studio/node/laptop-1"}, "")
	if code != http.StatusOK || rec == nil || rec.Action != "bind" || rec.Target != farmbook.BindingSubject(bindingNamedIn(out)) {
		t.Fatalf("bind node/laptop-1: %d %v %+v", code, out, rec)
	}
	node := bindingNamedIn(out)
	b := readBinding(t, s, node)
	if b.Spec.Owner != "noor" || !slices.Equal(b.Spec.For, []string{"task/v1"}) || b.Spec.Record != out["record"] {
		t.Errorf("binding %+v", b.Spec)
	}
	if code, out, _ := post("dana", "/api/v1/bindings", map[string]any{"owner": "lee", "to": "studio/node/laptop-1"}, ""); code != http.StatusConflict {
		t.Errorf("a second binding of node/laptop-1: %d %v", code, out)
	}
	if code, out, _ := post("dana", "/api/v1/bindings", map[string]any{"owner": "dana", "to": "studio/component/hud"}, ""); code != http.StatusOK {
		t.Errorf("the HUD bound to the farm owner: %d %v", code, out)
	}

	// a new owner follows it
	code, out, rec = post("dana", "/api/v1/bindings/"+node+"/rotate", map[string]any{"owner": "lee"}, "")
	if code != http.StatusOK {
		t.Fatalf("rotate: %d %v", code, out)
	}
	lee := bindingNamedIn(out)
	ch, _ := rec.Change.(map[string]any)
	if ch["follows"] != b.Spec.Record || ch["ends"] != farmbook.BindingSubject(node) {
		t.Errorf("the rotation's change: %v", ch)
	}
	old, next := readBinding(t, s, node), readBinding(t, s, lee)
	if old.Ended == nil || old.Ended.FollowedBy != next.Spec.Record || !old.Ended.At.Equal(&next.Spec.Start) || next.Spec.Follows != old.Spec.Record {
		t.Errorf("the changeover: old ended %+v, new %+v", old.Ended, next.Spec)
	}
	if code, out, _ := post("dana", "/api/v1/bindings/"+node+"/rotate", map[string]any{"owner": "noor"}, ""); code != http.StatusConflict {
		t.Errorf("following a binding no longer in force: %d %v", code, out)
	}

	var list api.BindingsResponse
	code, got, _ := g.send(t, http.MethodGet, "/api/v1/bindings?as=noor", nil, "", "show-a", headers)
	raw, _ := json.Marshal(got)
	json.Unmarshal(raw, &list)
	if code != http.StatusOK || list.Farm != "studio" {
		t.Fatalf("list: %d %+v", code, list)
	}
	inForce := map[string]bool{}
	for _, v := range list.Bindings {
		inForce[v.Name] = v.InForce
		if v.Name == lee && !v.Standing || v.Name == node && v.Standing {
			t.Errorf("standing: %+v", v)
		}
	}
	if list.Bindings[0].Deployment != "*" || !inForce["farm"] || inForce[node] || !inForce[lee] {
		t.Errorf("listed %+v", list.Bindings)
	}

	// ending it: the wrong owner named, someone else's, then a farm admin
	// without and with a touch
	on := readBinding(t, s, lee)
	withUID(t, s, on, "b1d0e5a2-0000-4000-8000-000000000001")
	on = readBinding(t, s, lee)
	subject := actOn(bindingSubject(on))
	if code, out, _ := post("dana", "/api/v1/bindings/"+lee+"/end", map[string]any{"owner": "noor"}, ""); code != http.StatusBadRequest {
		t.Errorf("naming another owner: %d %v", code, out)
	}
	if code, out, _ := post("noor", "/api/v1/bindings/"+lee+"/end", map[string]any{"owner": "lee"}, ""); code != http.StatusForbidden {
		t.Errorf("noor ends lee's binding: %d %v", code, out)
	}
	danaKey := g.f.Person(t, "dana", true)
	body := map[string]any{"owner": "lee"}
	if code, out, _ := post("dana", "/api/v1/bindings/"+lee+"/end", body, g.act(t, danaKey, subject, "end-binding", body, false)); code != http.StatusForbidden {
		t.Errorf("the farm owner ends lee's binding without a touch: %d %v", code, out)
	}
	code, out, rec = post("dana", "/api/v1/bindings/"+lee+"/end", body, g.act(t, danaKey, subject, "end-binding", body, true))
	cites(t, "end-binding", code, out, rec, subject, CustodyConfirmed)
	if e := readBinding(t, s, lee).Ended; e == nil || e.Record != out["record"] || e.FollowedBy != "" {
		t.Errorf("ended %+v", e)
	}
	if code, out, _ := post("lee", "/api/v1/bindings/"+lee+"/end", body, ""); code != http.StatusConflict {
		t.Errorf("ending it twice: %d %v", code, out)
	}
	if code, out, _ := post("dana", "/api/v1/bindings/farm/end", map[string]any{"owner": "dana"}, ""); code != http.StatusConflict {
		t.Errorf("ending the farm binding: %d %v", code, out)
	}
}

// A bind needs the farm admin's touch, and so does a rotation, which ends
// another owner's binding (the end-binding rule); a binding's owner ends
// their own binding with a signed act and no touch, like a revoke.
func TestBindingActs(t *testing.T) {
	s, g, headers := deploymentFarm(t)
	on, ok := s.farmSubject(context.Background())
	if !ok {
		t.Fatal("no farm subject")
	}
	farmOn := actOn(on)
	dana, noor := g.f.Person(t, "dana", true), g.f.Person(t, "noor", true)
	body := map[string]any{"owner": "noor", "to": "studio/runner/laptop-1"}
	send := func(as, path string, body any, act string) (int, map[string]any, *actRecord) {
		return g.send(t, http.MethodPost, path+"?as="+as, body, act, "show-a", headers)
	}
	if code, out, _ := send("dana", "/api/v1/bindings", body, g.act(t, dana, farmOn, "bind", body, false)); code != http.StatusForbidden {
		t.Errorf("a bind without the touch: %d %v", code, out)
	}
	code, out, rec := send("dana", "/api/v1/bindings", body, g.act(t, dana, farmOn, "bind", body, true))
	cites(t, "bind", code, out, rec, farmOn, CustodyConfirmed)
	name := bindingNamedIn(out)
	b := readBinding(t, s, name)
	withUID(t, s, b, "b1d0e5a2-0000-4000-8000-000000000002")
	b = readBinding(t, s, name)
	subject := actOn(bindingSubject(b))
	rotate := map[string]any{"owner": "lee"}
	if code, out, _ := send("dana", "/api/v1/bindings/"+name+"/rotate", rotate, g.act(t, dana, subject, "bind", rotate, false)); code != http.StatusForbidden {
		t.Errorf("the farm owner rotates noor's binding without a touch: %d %v", code, out)
	}
	code, out, rec = send("dana", "/api/v1/bindings/"+name+"/rotate", rotate, g.act(t, dana, subject, "bind", rotate, true))
	cites(t, "rotate", code, out, rec, subject, CustodyConfirmed)
	if ch, _ := rec.Change.(map[string]any); ch["follows"] != b.Spec.Record {
		t.Errorf("the rotation follows %v", ch["follows"])
	}

	// noor's own binding of another machine, ended by noor
	body = map[string]any{"owner": "noor", "to": "studio/runner/laptop-2"}
	code, out, _ = send("dana", "/api/v1/bindings", body, g.act(t, dana, farmOn, "bind", body, true))
	if code != http.StatusOK {
		t.Fatalf("bind laptop-2: %d %v", code, out)
	}
	name = bindingNamedIn(out)
	withUID(t, s, readBinding(t, s, name), "b1d0e5a2-0000-4000-8000-000000000003")
	subject = actOn(bindingSubject(readBinding(t, s, name)))
	end := map[string]any{"owner": "noor"}
	code, out, rec = send("noor", "/api/v1/bindings/"+name+"/end", end, g.act(t, noor, subject, "end-binding", end, false))
	cites(t, "noor ends her own binding", code, out, rec, subject, CustodySigned)
}

// The loop end to end: the HUD's own certificate names no binding until
// the farm is bound, then, re-certified, the farm binding; GET /book/farm
// follows each booked record's signer to its binding and owner, labelling
// the one signed unbound.
func TestBookedRecordsFollowedToTheirOwner(t *testing.T) {
	b := newBookHUD(t)
	s := b.s
	s.DevViewers["dana"] = &api.Viewer{User: "dana", Role: "farm-admin", Projects: []string{"show-a"}}
	r := &bindings.Resolver{Client: s.Client, Book: &farmbook.Reader{Farm: b.farm, Roots: b.roots, Store: b.store, Pointer: b.ptr, Own: b.own}}
	hud, err := provenance.NewSigner(context.Background(), provenance.BoundLocalIssuer(b.ca, provenance.ComponentIdentity("hud"), time.Hour, r.Bind))
	if err != nil {
		t.Fatal(err)
	}
	hud.RenewWhen(r.Stale(0))
	s.Signer = hud
	if code, out := bindCall(s, http.MethodPost, "wrangler", `{"owner": "dana", "to": "studio/*"}`, nil); code != http.StatusOK {
		t.Fatalf("bind the farm: %d %v", code, out)
	}
	installer := map[string]string{InstallerHeader: testInstallerToken}
	if w := send(s, http.MethodPost, "/api/v1/bindings?as=dana", `{"owner": "noor", "to": "studio/node/laptop-1"}`, installer); w.Code != http.StatusOK {
		t.Fatalf("bind a node: %d %s", w.Code, w.Body)
	}
	if res := b.seal(t); len(res.Refused) != 0 {
		t.Fatalf("refused %+v", res.Refused)
	}
	p := b.page(t, 0, "&as=dana", installer)
	if len(p.Entries) != 2 {
		t.Fatalf("booked %d entries", len(p.Entries))
	}
	if g := p.Entries[0].Binding; g == nil || g.Label != farmbook.LabelUnattributed {
		t.Errorf("the farm's bind, signed before any binding: %+v", g)
	}
	if g := p.Entries[1].Binding; g == nil || g.Label != "" || g.Owner != "dana" || g.Deployment != "*" {
		t.Errorf("the node's bind, signed under the farm binding: %+v", g)
	}
}
