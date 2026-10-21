package main

import (
	"context"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/auth"
	"github.com/Avalanche-io/c4-operator/internal/provenance"
)

// The issuer certifies a workload only if a project names it as an
// importer: any other service account (a unit's, a project's default) is
// refused, so nothing but a project's importers can sign a record that
// verifies as a source of its content.
func TestIssuerCertifiesOnlyImporters(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := c4v1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	show := &c4v1.Project{ObjectMeta: metav1.ObjectMeta{Name: "show-a"}, Spec: c4v1.ProjectSpec{
		Namespaces: []string{"show-a", "show-a-plates"},
		Importers:  []c4v1.Importer{{Namespace: "show-a", ServiceAccount: "import-git"}, {Namespace: "show-a-plates", ServiceAccount: "ingest"}},
	}}
	other := &c4v1.Project{ObjectMeta: metav1.ObjectMeta{Name: "show-b"}, Spec: c4v1.ProjectSpec{Namespaces: []string{"show-b"}}}
	r := fake.NewClientBuilder().WithScheme(scheme).WithObjects(show, other).Build()
	sa := func(ns, name string) *auth.Identity {
		return &auth.Identity{User: "system:serviceaccount:" + ns + ":" + name, Namespace: ns, Account: name}
	}
	ctx := context.Background()
	for _, c := range []struct {
		id   *auth.Identity
		want string
		life time.Duration
		err  string
	}{
		{id: sa("show-a", "import-git"), want: provenance.ServiceAccountIdentity("show-a", "import-git"), life: time.Hour},
		{id: sa("show-a-plates", "ingest"), want: provenance.ServiceAccountIdentity("show-a-plates", "ingest"), life: time.Hour},
		{id: sa("show-a", "default"), err: "is not an importer of a project"},
		{id: sa("show-a", "ingest"), err: "is not an importer of a project"},
		{id: sa("show-b", "import-git"), err: "is not an importer of a project"},
		{id: sa("elsewhere", "import-git"), err: "is not an importer of a project"},
		{id: &auth.Identity{User: "system:serviceaccount:c4-system:c4-node", Namespace: "c4-system", Account: "c4-node", NodeName: "w1"},
			want: provenance.NodeIdentity("w1"), life: 24 * time.Hour},
		{id: sa("c4-system", "c4-node"), err: "bound to its pod"},
		{id: sa("c4-system", "c4-manager"), err: "does not sign records"},
		{id: &auth.Identity{User: "alice"}, err: "not a service account"},
	} {
		got, life, err := signerIdentity(ctx, r, c.id, "c4-system")
		if c.err != "" {
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Errorf("%s: certified as %q (err %v), want refused with %q", c.id.User, got, err, c.err)
			}
			continue
		}
		if err != nil || got != c.want || life != c.life {
			t.Errorf("%s: %q for %v (err %v), want %q for %v", c.id.User, got, life, err, c.want, c.life)
		}
	}
}

// A HUD is loopback-only when its listen address says so, never when a
// request does: an empty host or a routable address listens to the network.
func TestLoopbackOnly(t *testing.T) {
	for addr, want := range map[string]bool{
		"127.0.0.1:8090": true, "[::1]:8090": true, "localhost:8090": true, "127.0.0.2:8090": true,
		":8090": false, "0.0.0.0:8090": false, "[::]:8090": false, "10.0.0.5:8090": false, "hud.example.com:8090": false, "": false,
	} {
		if got := loopbackOnly(addr); got != want {
			t.Errorf("%q: %v, want %v", addr, got, want)
		}
	}
}
