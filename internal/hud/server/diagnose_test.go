package server

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Avalanche-io/c4-operator/hud/api"
	"github.com/Avalanche-io/c4-operator/internal/telemetry"
	"github.com/Avalanche-io/c4-operator/internal/telemetry/bundle"
	"github.com/Avalanche-io/c4-operator/internal/telemetry/ring"
	"github.com/Avalanche-io/c4-operator/work/estimate"
	"github.com/Avalanche-io/c4-operator/work/sched"
)

func diagnoseServer(key func(context.Context) (telemetry.Key, error), tier ring.Tier) (*Server, *ring.Ring) {
	r := ring.New(tier, telemetry.PolicyObjective, key)
	now := time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)
	for i, n := range []int{3, 2} {
		j := sched.Job{ID: "show-a/sh010", Project: "show-a", Ready: now.Add(-time.Hour), Deadline: now.Add(2 * time.Hour),
			Pools: []string{"cpu"}, SlotsPerUnit: 1, RunnableTotal: n, Estimate: estimate.Summary{W: 3600, W90: 4000, TPath: 300}}
		for u := 0; u < n; u++ {
			j.Runnable = append(j.Runnable, sched.UnitRef{ID: j.ID + "#" + strconv.Itoa(u)})
		}
		s := sched.Snapshot{Now: now.Add(time.Duration(i) * time.Minute), Pools: []sched.Pool{{Name: "cpu", Slots: 4}},
			Projects: []sched.Project{{Name: "show-a", Weight: 1}}, Jobs: []sched.Job{j}}
		st := &sched.State{}
		before := st.Doc()
		cfg := sched.DefaultConfig()
		r.Note(s, before, cfg, sched.Decide(s, st, cfg))
	}
	srv := &Server{SinglePerson: "dana", DevViewers: map[string]*api.Viewer{
		"admin":    {User: "noor", All: true, Role: "farm-admin"},
		"wrangler": {User: "sam", All: true, Role: "wrangler"},
		"owner":    {User: "dana", All: true, Role: "artist"},
		"unit":     {User: "unit", Role: "farm-admin", Unit: &api.UnitViewer{Namespace: "show-a", Job: "sh010", Unit: "0", Grant: "project"}},
	}, Telemetry: r}
	return srv, r
}

func goodKey(context.Context) (telemetry.Key, error) {
	return telemetry.NewKey(bytes.Repeat([]byte{5}, telemetry.KeySize), 2)
}

// The farm admin, or a single-person farm's installer, takes a bundle:
// the ring's decisions, written as one tgz named by its ID, that reads
// back and replays. A wrangler and a unit are refused in words.
func TestDiagnose(t *testing.T) {
	srv, _ := diagnoseServer(goodKey, ring.TierBundles)
	h := srv.Handler()
	get := func(path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		return w
	}
	for _, as := range []string{"admin", "owner"} {
		w := get("/api/v1/diagnose?as=" + as)
		if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "application/gzip" {
			t.Fatalf("%s: %d %s", as, w.Code, w.Body.String())
		}
		c, err := bundle.Read(bytes.NewReader(w.Body.Bytes()))
		if err != nil {
			t.Fatal(err)
		}
		if want := `attachment; filename="` + bundle.FileName(c.ID) + `"`; w.Header().Get("Content-Disposition") != want || w.Header().Get("X-C4-Bundle") != c.ID.String() {
			t.Fatalf("headers %v", w.Header())
		}
		if len(c.Snapshots) != 2 || c.Snapshots[0].KeyEpoch != 2 || w.Header().Get("X-C4-Bundle-Tier") != "ring" {
			t.Fatalf("%d snapshots, epoch %d, tier %s", len(c.Snapshots), c.Snapshots[0].KeyEpoch, w.Header().Get("X-C4-Bundle-Tier"))
		}
		for _, s := range c.Snapshots {
			if !s.Faithful {
				t.Fatalf("a snapshot does not replay: %+v", s.Divergences)
			}
		}
	}
	for as, words := range map[string]string{"wrangler": "takes the farm admin role", "unit": "a unit does not take diagnostic bundles"} {
		w := get("/api/v1/diagnose?as=" + as)
		if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), words) {
			t.Errorf("%s: %d %s", as, w.Code, w.Body.String())
		}
	}
	if w := get("/api/v1/diagnose?as=admin&window=soon"); w.Code != http.StatusBadRequest {
		t.Errorf("a window that is no duration: %d", w.Code)
	}
	if w := get("/api/v1/diagnose?as=admin&window=30s"); w.Code != http.StatusOK {
		t.Errorf("a 30s window: %d %s", w.Code, w.Body.String())
	} else if c, err := bundle.Read(w.Body); err != nil || len(c.Snapshots) != 1 {
		t.Errorf("a 30s window of ticks a minute apart kept more than the latest: %v", err)
	}
}

// At tier off the bundle holds the present moment alone; and without the
// farm key nothing of a bundle is written, only the reason.
func TestDiagnoseFailsClosed(t *testing.T) {
	srv, _ := diagnoseServer(goodKey, ring.TierOff)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/diagnose?as=admin", nil))
	if c, err := bundle.Read(w.Body); err != nil || len(c.Snapshots) != 1 || w.Header().Get("X-C4-Bundle-Tier") != "present" {
		t.Fatalf("tier off: %v, %s", err, w.Header().Get("X-C4-Bundle-Tier"))
	}
	broken := func(context.Context) (telemetry.Key, error) {
		return telemetry.Key{}, errors.New("secrets is forbidden")
	}
	srv, _ = diagnoseServer(broken, ring.TierBundles)
	w = httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/diagnose?as=admin", nil))
	if w.Code != http.StatusServiceUnavailable || w.Header().Get("Content-Type") != "application/json" || !strings.Contains(w.Body.String(), "no bundle was written") {
		t.Fatalf("without the key: %d %s %s", w.Code, w.Header().Get("Content-Type"), w.Body.String())
	}
	srv.Telemetry = nil
	w = httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/diagnose?as=admin", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("without a ring: %d", w.Code)
	}
}
