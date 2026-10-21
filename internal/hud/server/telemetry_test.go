package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kfake "k8s.io/client-go/kubernetes/fake"

	"github.com/Avalanche-io/c4-operator/hud/api"
	"github.com/Avalanche-io/c4-operator/internal/provenance"
	"github.com/Avalanche-io/c4-operator/internal/telemetry"
	"github.com/Avalanche-io/c4-operator/internal/telemetry/farmkey"
	"github.com/Avalanche-io/c4-operator/internal/telemetry/ring"
)

func telemetryServer(t *testing.T) (*Server, *kfake.Clientset) {
	t.Helper()
	ctx := context.Background()
	kube := kfake.NewSimpleClientset()
	srv, _ := diagnoseServer(func(ctx context.Context) (telemetry.Key, error) {
		return farmkey.LoadOrCreate(ctx, kube, "c4-system")
	}, ring.TierOff)
	ca, err := provenance.NewCA(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	sig, err := provenance.NewSigner(ctx, provenance.LocalIssuer(ca, provenance.ComponentIdentity("hud"), time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	srv.Kube, srv.Namespace, srv.Signer = kube, "c4-system", sig
	return srv, kube
}

func tcall(t *testing.T, srv *Server, method, path, body string, header map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	for k, v := range header {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	return w
}

// The farm admin reads the farm's telemetry, raises the tier only as
// themselves, lowers it by any route, and each change is a signed record
// kept with the setting and booked. A wrangler may do none of it.
func TestTelemetryTier(t *testing.T) {
	srv, kube := telemetryServer(t)
	ctx := context.Background()
	mcp := map[string]string{"X-C4-Client": "mcp"}
	var st api.Telemetry
	w := tcall(t, srv, "GET", "/api/v1/telemetry?as=admin", "", nil)
	if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil || w.Code != 200 || st.Tier != "a" || st.Epoch != 1 || st.Ring.Window != "2h0m0s" {
		t.Fatalf("status: %d %s", w.Code, w.Body.String())
	}
	if w := tcall(t, srv, "GET", "/api/v1/telemetry?as=wrangler", "", nil); w.Code != http.StatusForbidden {
		t.Fatalf("a wrangler read the farm's telemetry: %d", w.Code)
	}
	if w := tcall(t, srv, "POST", "/api/v1/telemetry/tier?as=admin", `{"tier":"b"}`, mcp); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "not an agent (MCP)") {
		t.Fatalf("raised through MCP: %d %s", w.Code, w.Body.String())
	}
	if w := tcall(t, srv, "POST", "/api/v1/telemetry/tier?as=unit", `{"tier":"b"}`, nil); w.Code != http.StatusForbidden {
		t.Fatalf("raised by a unit: %d %s", w.Code, w.Body.String())
	}
	if w := tcall(t, srv, "POST", "/api/v1/telemetry/tier?as=admin", `{"tier":"c"}`, nil); w.Code != http.StatusBadRequest {
		t.Fatalf("tier c: %d", w.Code)
	}
	var ch api.TelemetryChange
	w = tcall(t, srv, "POST", "/api/v1/telemetry/tier?as=admin", `{"tier":"b"}`, nil)
	if err := json.Unmarshal(w.Body.Bytes(), &ch); err != nil || w.Code != 200 || ch.Tier != "b" || !strings.HasPrefix(ch.Record, "c4") {
		t.Fatalf("raised directly: %d %s", w.Code, w.Body.String())
	}
	if srv.Telemetry.Tier() != ring.TierBundles {
		t.Fatal("the ring did not follow the tier")
	}
	cm, err := kube.CoreV1().ConfigMaps("c4-system").Get(ctx, farmkey.Setting, metav1.GetOptions{})
	if err != nil || cm.Data["tier"] != "b" || cm.Data["record"] != ch.Record || !strings.Contains(cm.Data["record.json"], "dsseEnvelope") {
		t.Fatalf("the setting: %v %v", err, cm)
	}
	evs, _ := kube.CoreV1().Events("c4-system").List(ctx, metav1.ListOptions{})
	if len(evs.Items) != 1 || evs.Items[0].InvolvedObject.Name != farmkey.Setting || evs.Items[0].Annotations[annoRecord] != ch.Record {
		t.Fatalf("the change was not booked: %+v", evs.Items)
	}
	w = tcall(t, srv, "POST", "/api/v1/telemetry/tier?as=admin", `{"tier":"a"}`, mcp)
	if err := json.Unmarshal(w.Body.Bytes(), &ch); err != nil || w.Code != 200 || ch.Tier != "a" {
		t.Fatalf("lowered through MCP: %d %s", w.Code, w.Body.String())
	}
	if w := tcall(t, srv, "POST", "/api/v1/telemetry/tier?as=wrangler", `{"tier":"a"}`, nil); w.Code != http.StatusForbidden {
		t.Fatalf("a wrangler set the tier: %d", w.Code)
	}
	t.Setenv("C4_TELEMETRY", "off")
	if w := tcall(t, srv, "POST", "/api/v1/telemetry/tier?as=admin", `{"tier":"b"}`, nil); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "C4_TELEMETRY=off") {
		t.Fatalf("raised past the kill switch: %d %s", w.Code, w.Body.String())
	}
}

// A pseudonym resolves on the farm, for a person acting as themselves,
// among the names the farm holds now; after a rotation the old one names
// nothing, and the ring's records under the old key are gone.
func TestTelemetryResolveAndRotate(t *testing.T) {
	srv, kube := telemetryServer(t)
	ctx := context.Background()
	key, err := farmkey.LoadOrCreate(ctx, kube, "c4-system")
	if err != nil {
		t.Fatal(err)
	}
	n := telemetry.NewNamer(key)
	n.Add(telemetry.KindJob, "show-a/sh010")
	n.Freeze()
	text := n.Of(telemetry.KindJob, "show-a/sh010").Text()
	body := `{"pseudonym":"` + text + `"}`
	var name api.TelemetryName
	w := tcall(t, srv, "POST", "/api/v1/telemetry/resolve?as=admin", body, nil)
	if err := json.Unmarshal(w.Body.Bytes(), &name); err != nil || w.Code != 200 || name != (api.TelemetryName{Kind: "job", Name: "show-a/sh010"}) {
		t.Fatalf("resolve: %d %s", w.Code, w.Body.String())
	}
	if w := tcall(t, srv, "POST", "/api/v1/telemetry/resolve?as=admin", body, map[string]string{"X-C4-Client": "mcp"}); w.Code != http.StatusForbidden {
		t.Fatalf("resolved through MCP: %d", w.Code)
	}
	if w := tcall(t, srv, "POST", "/api/v1/telemetry/resolve?as=admin", `{"pseudonym":"sh010"}`, nil); w.Code != http.StatusBadRequest {
		t.Fatalf("not a pseudonym: %d", w.Code)
	}
	var ch api.TelemetryChange
	w = tcall(t, srv, "POST", "/api/v1/telemetry/rotate?as=admin", "", nil)
	if err := json.Unmarshal(w.Body.Bytes(), &ch); err != nil || w.Code != 200 || ch.Epoch != 2 || ch.Record == "" {
		t.Fatalf("rotate: %d %s", w.Code, w.Body.String())
	}
	if w := tcall(t, srv, "POST", "/api/v1/telemetry/resolve?as=admin", body, nil); w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "epoch 2") {
		t.Fatalf("the old pseudonym after a rotation: %d %s", w.Code, w.Body.String())
	}
	var st api.Telemetry
	w = tcall(t, srv, "GET", "/api/v1/telemetry?as=admin", "", nil)
	if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil || st.Epoch != 2 || st.Ring.Snapshots != 0 {
		t.Fatalf("status after a rotation: %s", w.Body.String())
	}
	if w := tcall(t, srv, "POST", "/api/v1/telemetry/rotate?as=wrangler", "", nil); w.Code != http.StatusForbidden {
		t.Fatalf("a wrangler rotated the key: %d", w.Code)
	}
}
