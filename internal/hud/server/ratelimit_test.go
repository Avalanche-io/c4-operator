package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A route takes a burst from one address, then one per its pace, and a
// pace from everyone together; another address has its own bucket.
func TestRouteLimits(t *testing.T) {
	b := hostIssuerBudget
	var q routeLimits
	now := time.Now()
	for i := range b.burst {
		if ok, _ := q.allow(b, "10.0.0.1", now); !ok {
			t.Fatalf("request %d of a burst refused", i+1)
		}
	}
	if ok, wait := q.allow(b, "10.0.0.1", now); ok || wait <= 0 || wait > b.perClient {
		t.Fatalf("past the burst: %v, wait %v", ok, wait)
	}
	if ok, _ := q.allow(b, "10.0.0.2", now); !ok {
		t.Fatal("another address shares the first's bucket")
	}
	if ok, _ := q.allow(b, "10.0.0.1", now.Add(b.perClient)); !ok {
		t.Fatal("refused after waiting")
	}
	n := 0
	for i := range 100 {
		if ok, _ := q.allow(b, fmt.Sprintf("10.0.1.%d", i), now.Add(b.perClient)); ok {
			n++
		}
	}
	if n > b.allBurst {
		t.Fatalf("%d addresses at once were taken, past everyone's burst of %d", n, b.allBurst)
	}
}

// Both issuer routes are limited, 429 with Retry-After, before the issuer
// sees the request. The people's route takes a studio's sign-in burst from
// one address: two hundred people, five calls each.
func TestIssuerRoutesAreLimited(t *testing.T) {
	s := hostFarm(t)
	host, people := 0, 0
	s.HostIssuer = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { host++ })
	s.People = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { people++ })
	h := s.Handler()
	send := func(path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, path, strings.NewReader("{}")))
		return w
	}
	var w *httptest.ResponseRecorder
	for range hostIssuerBudget.burst + 1 {
		w = send("/workload/v1/certificate")
	}
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") == "" || host != hostIssuerBudget.burst {
		t.Fatalf("the host runners' issuer past its burst: %d, Retry-After %q, %d reached it", w.Code, w.Header().Get("Retry-After"), host)
	}
	for i := range 200 * 5 {
		if w = send("/issuer/v1/people/enroll"); w.Code == http.StatusTooManyRequests {
			t.Fatalf("a studio's sign-ins refused at call %d", i+1)
		}
	}
	for range peopleBudget.burst {
		w = send("/issuer/v1/people/enroll")
	}
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") == "" || people >= 2*peopleBudget.burst {
		t.Fatalf("the people's issuer past its burst: %d, Retry-After %q, %d reached it", w.Code, w.Header().Get("Retry-After"), people)
	}
}
