package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestProgressReadings(t *testing.T) {
	a, b := 0.4, 0.9
	at := time.Date(2026, 10, 22, 14, 0, 0, 0, time.UTC)
	fm := farmMetrics{units: map[[2]string]unitReport{
		{"show-a", "u-1"}: {Progress: &a},
		{"show-b", "u-2"}: {Progress: &b},
		{"show-b", "u-3"}: {Safepoint: &struct {
			Seq int       `json:"seq"`
			At  time.Time `json:"at"`
		}{3, at}},
	}}
	got := progressReadings(fm)
	if len(got) != 2 || got[[2]string{"show-a", "u-1"}] != 0.4 || got[[2]string{"show-b", "u-2"}] != 0.9 {
		t.Fatalf("readings %v", got)
	}
	sp := safepointReadings(fm)
	if len(sp) != 1 || sp[[2]string{"show-b", "u-3"}].Seq != 3 || !sp[[2]string{"show-b", "u-3"}].At.Equal(at) {
		t.Fatalf("safe points %v", sp)
	}
}

// The HUD reads what units report from a node's /units with its tier
// token, and the node's metrics with its API token.
func TestScrapeSendsTokens(t *testing.T) {
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/units" && r.Header.Get("Authorization") == "Bearer tier":
			fmt.Fprint(w, `[{"namespace":"show-a","pod":"u-1","progress":0.4,"safepoint":{"seq":2,"at":"2026-10-22T14:00:00Z"}}]`)
		case r.URL.Path == "/metrics" && r.Header.Get("Authorization") == "Bearer api":
			fmt.Fprint(w, "c4_node_cache_bytes 7\n")
		default:
			http.Error(w, "no", http.StatusUnauthorized)
		}
	}))
	defer node.Close()
	ctx, hc := context.Background(), node.Client()
	us, err := fetchUnits(ctx, hc, node.URL+"/units", "tier")
	if err != nil || len(us) != 1 || us[0].Pod != "u-1" || *us[0].Progress != 0.4 || us[0].Safepoint.Seq != 2 {
		t.Fatalf("units %+v %v", us, err)
	}
	ss, err := fetchMetrics(ctx, hc, node.URL+"/metrics", "api")
	if v, ok := ss.first("c4_node_cache_bytes"); err != nil || !ok || v != 7 {
		t.Fatalf("metrics %v %v", ss, err)
	}
	// A refusal is an error, not an empty scrape.
	if _, err := fetchUnits(ctx, hc, node.URL+"/units", "api"); err == nil {
		t.Fatal("units read with the wrong token")
	}
	if _, err := fetchMetrics(ctx, hc, node.URL+"/metrics", ""); err == nil {
		t.Fatal("metrics read without a token")
	}
}
