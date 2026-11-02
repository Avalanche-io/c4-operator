package agent

import (
	"context"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Avalanche-io/c4-operator/internal/provenance"
	"github.com/Avalanche-io/c4-operator/schema"
)

// An enrolled machine's runner gets its key certified as runner/<node> at
// the HUD, on its own issuer token, and signs its calls there with it: the
// HUD verifies each as the very request it came with. Its token goes too,
// for a HUD that takes no signed request yet.
func TestRunnerSignsItsCallsToTheHUD(t *testing.T) {
	ca, err := provenance.NewCA(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca.Cert)
	iss := &provenance.Issuer{CA: ca, Identify: func(_ context.Context, token string) (string, time.Duration, error) {
		if token != "signer-tok" {
			return "", 0, errors.New("not a host runner's token")
		}
		return provenance.RunnerIdentity("laptop-1"), time.Hour, nil
	}}
	seen := provenance.NewMemorySeen()
	var signer, bearer string
	mux := http.NewServeMux()
	mux.Handle("POST /workload/v1/certificate", http.StripPrefix("/workload", iss))
	mux.HandleFunc("POST /api/v1/nodes/{node}/enroll-offer", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bearer = r.Header.Get("Authorization")
		data, _ := base64.StdEncoding.DecodeString(r.Header.Get(schema.RequestHeader))
		v, req, err := provenance.VerifyRequest(data, roots, time.Now(), seen)
		if err != nil || req.Matches(r.Method, r.RequestURI, body) != nil {
			http.Error(w, fmt.Sprintf("not signed: %v", err), http.StatusUnauthorized)
			return
		}
		signer = v.Identity
		w.Write([]byte(`{"node":"laptop-1","code":"ABCD-EFGH","message":"offered"}`))
	})
	hud := httptest.NewServer(mux)
	defer hud.Close()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "signer"), []byte("signer-tok\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	key := HostKey(hud.URL, TokenFile(dir, "signer"))
	o, err := OfferMachine(context.Background(), nil, hud.URL, "hud-tok", key, "laptop-1", "noor")
	if err != nil || o.Code != "ABCD-EFGH" {
		t.Fatalf("the signed offer: %+v %v", o, err)
	}
	if signer != provenance.RunnerIdentity("laptop-1") || bearer != "Bearer hud-tok" {
		t.Fatalf("signed by %q, token %q", signer, bearer)
	}

	// a key the issuer will not certify: the call goes with its token,
	// and the runner says so once, not at every call
	var logs []string
	r := &Runner{Key: HostKey(hud.URL, filepath.Join(dir, "missing")), Logf: func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }}
	for range 3 {
		req, _ := http.NewRequest(http.MethodGet, hud.URL+"/api/v1/nodes/laptop-1/pods", nil)
		r.signHUD(context.Background(), req, nil)
		if req.Header.Get(schema.RequestHeader) != "" {
			t.Fatal("signed without a certificate")
		}
	}
	if len(logs) != 1 || !strings.Contains(logs[0], "with its token alone") {
		t.Fatalf("logs: %q", logs)
	}
	// once certified again, it says so and signs
	if err := os.WriteFile(filepath.Join(dir, "missing"), []byte("signer-tok"), 0o600); err != nil {
		t.Fatal(err)
	}
	r.hudKey.after = time.Time{}
	req, _ := http.NewRequest(http.MethodGet, hud.URL+"/api/v1/nodes/laptop-1/pods", nil)
	r.signHUD(context.Background(), req, nil)
	if req.Header.Get(schema.RequestHeader) == "" || len(logs) != 2 || !strings.Contains(logs[1], "certified again") {
		t.Fatalf("after the issuer certifies it: %q", logs)
	}
}
