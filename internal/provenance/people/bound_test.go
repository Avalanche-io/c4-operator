package people

import (
	"context"
	"crypto/x509"
	"testing"
	"time"

	"github.com/Avalanche-io/c4-operator/internal/provenance"
)

// The people's issuer's own key (component/issuer), which signs device
// records and the index's head, is certified under the deployment binding
// in force for it, like the manager's other components, and re-certifies
// when that binding changes.
func TestIssuerKeyCertifiedUnderItsBinding(t *testing.T) {
	ca, err := provenance.NewCA(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	bound := provenance.Bound{}
	stale := false
	i := &Issuer{CA: ca,
		Bind: func(_ context.Context, identity string) (provenance.Bound, error) {
			if identity != provenance.ComponentIdentity("issuer") {
				t.Errorf("asked for the binding of %s", identity)
			}
			return bound, nil
		},
		Stale: func(*x509.Certificate) bool { return stale }}
	binding := func() string {
		t.Helper()
		s, err := i.sign(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		rec, err := provenance.SignStatement(context.Background(), s, provenance.PredicateDevice, []provenance.Subject{{Name: "d", Digest: map[string]string{"sha256": "00"}}},
			map[string]any{"probe": true}, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		pool := x509.NewCertPool()
		pool.AddCert(ca.Cert)
		v, err := provenance.VerifyBundle(rec, pool)
		if err != nil {
			t.Fatal(err)
		}
		return v.Binding
	}
	if got := binding(); got != "" {
		t.Fatalf("unbound, the leaf names %q", got)
	}
	bound, stale = provenance.Bound{ID: "c4farmbinding"}, true
	if got := binding(); got != "c4farmbinding" {
		t.Errorf("once bound, the leaf names %q", got)
	}
}
