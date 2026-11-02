package agent

import (
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/provenance"
	"github.com/Avalanche-io/c4-operator/schema"
)

// A host runner holds its unit's key: certified at the HUD on a request
// it signs as itself, written with what the unit's acts name to the unit's
// directory for the unit's c4-unit, used to sign the needs it carries out
// for the block (on the pod before the needs is), and dropped when the pod
// ends. The unit's acts verify as the unit's, naming the runner.
func TestHostRunnerHoldsItsUnitsKeys(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	ca, err := provenance.NewCA(now)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca.Cert)
	const jobUID = "9c1d2e7f-0000-4000-8000-000000000001"
	unit := provenance.UnitIdentity("show-a", "team", "2", jobUID, 1)
	seen := provenance.NewMemorySeen()
	asked := 0
	hud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		data, _ := base64.StdEncoding.DecodeString(r.Header.Get(schema.RequestHeader))
		v, req, err := provenance.VerifyRequest(data, roots, time.Now(), seen)
		if err != nil || req.Matches(r.Method, r.RequestURI, body) != nil || v.Identity != provenance.RunnerIdentity("host") ||
			r.URL.Path != "/workload/v1/unit-certificate" {
			http.Error(w, "not the runner's own request", http.StatusUnauthorized)
			return
		}
		var in struct {
			PublicKey []byte
			Namespace string
			Pod       string
		}
		json.Unmarshal(body, &in)
		leaf, err := ca.IssueHostUnit(ed25519.PublicKey(in.PublicKey), unit, "host", time.Hour, time.Now())
		if err != nil || in.Pod != "lead-0" {
			http.Error(w, "no", http.StatusForbidden)
			return
		}
		asked++
		json.NewEncoder(w).Encode(map[string]any{"identity": unit, "chain": [][]byte{leaf.Raw, ca.Cert.Raw}})
	}))
	defer hud.Close()

	e := newEnv(t)
	m := e.register("lead", sess)
	yes := true
	p := hostPod()
	p.UID = "5e1f0c2a-0000-4000-8000-0000000000a2"
	p.Labels[c4v1.LabelUnit] = "2"
	p.Annotations[c4v1.AnnotationAttempt] = "1"
	p.Annotations[c4v1.AnnotationUnitSpec] = strings.Repeat("ab", 32)
	p.OwnerReferences = []metav1.OwnerReference{{APIVersion: c4v1.GroupVersion.String(), Kind: "WorkJob", Name: "team", UID: jobUID, Controller: &yes}}
	r, c := hostRunner(t, e, p)
	if err := c.Create(ctx, &c4v1.WorkJob{ObjectMeta: metav1.ObjectMeta{Namespace: "show-a", Name: "team", UID: types.UID(jobUID),
		Annotations: map[string]string{c4v1.AnnotationOwner: "dana"}}}); err != nil {
		t.Fatal(err)
	}
	if r.Key, err = provenance.NewSigner(ctx, provenance.LocalIssuer(ca, provenance.RunnerIdentity("host"), time.Hour)); err != nil {
		t.Fatal(err)
	}
	r.HUD = hud.URL
	p = getPod(t, c)
	u, err := r.prepareUnit(p, m)
	if err != nil {
		t.Fatal(err)
	}

	if err := r.relay(ctx, p, u); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(unitKeyPath(u)); err != nil || info.Mode().Perm() != 0o600 || asked != 1 {
		t.Fatalf("the unit's key file: %v %v, asked %d", info, err, asked)
	}
	env := strings.Join(UnitEnv(u), "\n")
	for _, want := range []string{"C4_UNIT_KEY=" + unitKeyPath(u), "C4_JOB=team", "C4_POD=lead-0", "C4_POD_UID=" + string(p.UID), "C4_OWNER=dana", "C4_JOB_UID=" + jobUID} {
		if !strings.Contains(env, want) {
			t.Fatalf("the unit's environment lacks %s", want)
		}
	}
	// a second pass keeps the key it holds
	if err := r.relay(ctx, p, u); err != nil || asked != 1 {
		t.Fatalf("renewed before half its life: %v, asked %d", err, asked)
	}

	// the unit's own c4-unit signs with it
	if _, err := unitOf(u).Sign(u.Out, "ask", map[string]any{"seq": 1, "to": "", "ask": "Which module?"}); err != nil {
		t.Fatal(err)
	}
	// the runner signs the block's needs, and it is on the pod first
	if err := r.needsInformation(ctx, p, m, "the delivery frame rate\nwhy: the brief gives 24 and 25"); err != nil {
		t.Fatal(err)
	}
	got := getPod(t, c)
	var acts []json.RawMessage
	json.Unmarshal([]byte(got.Annotations[c4v1.AnnotationUnitActs]), &acts)
	if len(acts) != 2 || got.Status.Reason != "NeedsInformation" {
		t.Fatalf("%d acts on the pod, reason %q", len(acts), got.Status.Reason)
	}
	for i, want := range []string{"ask", "needs"} {
		v, a, err := provenance.VerifyUnitReport(acts[i], roots, time.Now())
		if err != nil || a.Predicate.Act != want || v.Identity != unit || v.Runner != "host" || a.Predicate.Chain.For.Party != "dana" {
			t.Fatalf("act %d: %v %s by %s through %q", i, err, a.Predicate.Act, v.Identity, v.Runner)
		}
	}
	if _, err := os.Stat(unitKeyPath(u)); err == nil {
		t.Fatal("the unit's key outlived its pod")
	}
}
