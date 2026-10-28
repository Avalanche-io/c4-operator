package farmcli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Avalanche-io/c4-operator/hud/api"
)

// laggingHUD's snapshot shows a job only from its third read, as the
// HUD's does a moment after the farm accepted it.
type laggingHUD struct {
	mu    sync.Mutex
	reads int
}

func (h *laggingHUD) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.reads++
	s := api.Snapshot{Jobs: []api.Job{}}
	if h.reads >= 3 {
		s.Jobs = append(s.Jobs, api.Job{Namespace: "saltmarsh", Name: "sm0140-render-v001", Project: "saltmarsh"})
	}
	json.NewEncoder(w).Encode(s)
}

// A job the farm accepted a moment ago is found, though the snapshot
// lags; a job that is not there is still not found.
func TestFindJobWaitsForAJobJustMade(t *testing.T) {
	defer func(l, p time.Duration) { JobLag, jobPoll = l, p }(JobLag, jobPoll)
	JobLag, jobPoll = 2*time.Second, 10*time.Millisecond
	hud := &laggingHUD{}
	srv := httptest.NewServer(hud)
	defer srv.Close()
	c, _ := NewClient(Farm{HUD: srv.URL, As: "artist"})
	if j, err := findJob(context.Background(), c, "saltmarsh", "sm0140-render-v001"); err != nil || j.Name != "sm0140-render-v001" {
		t.Fatalf("found %v, %v after %d reads", j, err, hud.reads)
	}
	JobLag = 50 * time.Millisecond
	if _, err := findJob(context.Background(), c, "saltmarsh", "sm0150-render-v001"); err == nil || !strings.Contains(err.Error(), "no job sm0150-render-v001 in your projects") {
		t.Errorf("a missing job: %v", err)
	}
}

// A submit returns once the job it made shows in the snapshot, so ls
// straight after it lists the job.
func TestAwaitJobsReturnsOnceTheJobShows(t *testing.T) {
	defer func(l, p time.Duration) { JobLag, jobPoll = l, p }(JobLag, jobPoll)
	JobLag, jobPoll = 2*time.Second, 10*time.Millisecond
	hud := &laggingHUD{}
	srv := httptest.NewServer(hud)
	defer srv.Close()
	c, _ := NewClient(Farm{HUD: srv.URL, As: "artist"})
	start := time.Now()
	awaitJobs(context.Background(), c, "saltmarsh/sm0140-render-v001")
	if hud.reads != 3 || time.Since(start) > time.Second {
		t.Errorf("%d reads in %v", hud.reads, time.Since(start))
	}
}

// status counts the farm as up does: a burst node not rented yet is
// none of its nodes, and the slots it may rent are said beside.
func TestFarmLineCountsAsUpDoes(t *testing.T) {
	s := &api.Snapshot{
		Pools: []api.Pool{{Name: "cpu", Slots: 4}, {Name: "gpu", Slots: 2}, {Name: "cloud-gpu", Burst: &api.PoolBurst{MaxSlots: 4}}},
		Nodes: []api.Node{{Name: "w1", Pool: "cpu", State: "idle", Slots: 2}, {Name: "w2", Pool: "cpu", State: "idle", Slots: 2},
			{Name: "w3", Pool: "gpu", State: "idle", Slots: 2}, {Name: "w4", Pool: "cloud-gpu", State: "unprovisioned"}},
	}
	s.Farm.SlotsTotal = 6
	if got, want := FarmLine(s), "3 nodes · 0 of 6 slots in use · 0 units waiting · 4 more slots it rents only when a deadline needs them"; got != want {
		t.Errorf("FarmLine\n got %q\nwant %q", got, want)
	}
	s.Nodes[3].State, s.Nodes[3].Slots = "busy", 4
	s.Farm.SlotsTotal, s.Farm.SlotsUsed, s.Farm.QueuedUnits = 10, 5, 1
	if got, want := FarmLine(s), "4 nodes · 5 of 10 slots in use · 1 unit waiting · 1 of them rented for a deadline"; got != want {
		t.Errorf("FarmLine rented\n got %q\nwant %q", got, want)
	}
}
