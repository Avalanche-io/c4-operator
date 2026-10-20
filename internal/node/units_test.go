package node

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	authnv1 "k8s.io/api/authentication/v1"
	authzv1 "k8s.io/api/authorization/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kfake "k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"

	"github.com/Avalanche-io/c4-operator/internal/auth"
)

// fakeReviewer accepts tier tokens "manager" and "node" (audience c4-tier)
// and API tokens "scraper" and "nobody"; only the scraper may get
// /metrics.
func fakeReviewer() *auth.Reviewer {
	kube := kfake.NewSimpleClientset()
	kube.PrependReactor("create", "tokenreviews", func(a ktesting.Action) (bool, runtime.Object, error) {
		tr := a.(ktesting.CreateAction).GetObject().(*authnv1.TokenReview)
		tier := len(tr.Spec.Audiences) == 1 && tr.Spec.Audiences[0] == auth.AudienceTier
		users := map[string]string{"scraper": "monitoring:prometheus", "nobody": "show-a:default"}
		if tier {
			users = map[string]string{"manager": "c4-system:c4-manager", "node": "c4-system:c4-node"}
		}
		u, ok := users[tr.Spec.Token]
		if !ok {
			return true, &authnv1.TokenReview{}, nil
		}
		return true, &authnv1.TokenReview{Status: authnv1.TokenReviewStatus{Authenticated: true, Audiences: tr.Spec.Audiences,
			User: authnv1.UserInfo{Username: "system:serviceaccount:" + u}}}, nil
	})
	kube.PrependReactor("create", "subjectaccessreviews", func(a ktesting.Action) (bool, runtime.Object, error) {
		sar := a.(ktesting.CreateAction).GetObject().(*authzv1.SubjectAccessReview)
		na := sar.Spec.NonResourceAttributes
		sar.Status.Allowed = sar.Spec.User == "system:serviceaccount:monitoring:prometheus" && na != nil && na.Path == "/metrics" && na.Verb == "get"
		return true, sar, nil
	})
	return &auth.Reviewer{Client: kube, Tier: "node"}
}

// What units report reaches the manager on /units and nobody else; the
// node's metrics go only to a scraper allowed to get /metrics, and name
// no pod.
func TestUnitsAndMetricsArePrivate(t *testing.T) {
	d := &Daemon{Cfg: Config{Root: t.TempDir(), Namespace: "c4-system"}, Reviewer: fakeReviewer()}
	v := &Volume{ID: "vol-u3", Partition: "p0", Namespace: "saltmarsh", Pod: "sm0140-light-v001-u3-x", PodUID: "u3", Capture: true}
	os.MkdirAll(d.volDir(), 0o755)
	if err := d.saveVolume(v); err != nil {
		t.Fatal(err)
	}
	upper, _ := d.volumeDirs(v.Partition, v.ID)
	os.MkdirAll(filepath.Join(upper, ".c4"), 0o755)
	os.WriteFile(filepath.Join(upper, ".c4", "progress"), []byte("0.4\n"), 0o644)
	os.WriteFile(filepath.Join(upper, ".c4", "safepoint"), []byte("seq 3\nat 2026-10-22T14:00:00Z\n"), 0o644)
	d.progressOnce()

	h := d.peerHandler(http.NotFoundHandler())
	do := func(path, token string) (int, string) {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code, w.Body.String()
	}

	for token, want := range map[string]int{"": 401, "forged": 401, "node": 403, "scraper": 401} {
		if code, _ := do("/units", token); code != want {
			t.Errorf("/units with %q: %d, want %d", token, code, want)
		}
	}
	code, body := do("/units", "manager")
	var got []UnitReport
	if code != 200 || json.Unmarshal([]byte(body), &got) != nil || len(got) != 1 {
		t.Fatalf("/units for the manager: %d %s", code, body)
	}
	if u := got[0]; u.Pod != v.Pod || u.Namespace != "saltmarsh" || u.Progress == nil || *u.Progress != 0.4 || u.Safepoint == nil || u.Safepoint.Seq != 3 {
		t.Fatalf("report %+v", u)
	}

	// The manager's tier token is not an API token, and an API token
	// without the metrics-reader role is refused.
	for token, want := range map[string]int{"": 401, "manager": 401, "nobody": 403} {
		if code, _ := do("/metrics", token); code != want {
			t.Errorf("/metrics with %q: %d, want %d", token, code, want)
		}
	}
	code, body = do("/metrics", "scraper")
	if code != 200 || !strings.Contains(body, "c4_tier_auth_total") {
		t.Fatalf("/metrics for the scraper: %d %.200s", code, body)
	}
	if strings.Contains(body, "c4_task_") || strings.Contains(body, "sm0140") || strings.Contains(body, `pod="`) {
		t.Fatal("the node's metrics name a unit")
	}

	// A unit that stops reporting leaves /units.
	os.Remove(filepath.Join(upper, ".c4", "progress"))
	os.Remove(filepath.Join(upper, ".c4", "safepoint"))
	d.progressOnce()
	if _, body := do("/units", "manager"); strings.TrimSpace(body) != "[]" {
		t.Fatalf("/units after the unit stopped reporting: %s", body)
	}
}
