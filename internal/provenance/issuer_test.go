package provenance

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// An issuer that finds a signer asks too often answers 429 with when to
// come back, and certifies nothing.
func TestIssuerSaysTooMany(t *testing.T) {
	ca, err := NewCA(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	iss := &Issuer{CA: ca, Identify: func(context.Context, string) (string, time.Duration, error) {
		return "", 0, TooMany{Who: "pod show-a/sh040-u3", Wait: 7 * time.Second}
	}}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/certificate", strings.NewReader(`{"publicKey":"AAAA"}`))
	r.Header.Set("Authorization", "Bearer t")
	iss.ServeHTTP(w, r)
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") != "8" || !strings.Contains(w.Body.String(), "too often") {
		t.Fatalf("%d, Retry-After %q: %s", w.Code, w.Header().Get("Retry-After"), w.Body.String())
	}
}
