package server

import (
	"net/http"
	"slices"
	"testing"
	"time"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/hud/api"
)

// The farm's forecast as a viewer sees it: forecast jobs and the real
// jobs they worsen in the viewer's projects only; the pools farm-wide,
// with what the extra burst would cost an hour. Without a queue, 503.
func TestCapacityForecast(t *testing.T) {
	s := farm(t)
	if code := get(t, s, "/api/v1/capacity/forecast?as=artist", nil); code != http.StatusServiceUnavailable {
		t.Fatalf("no queue: %d", code)
	}
	s.current().pools[0].Spec.Burst = &c4v1.PoolBurst{CostPerSlotHour: "0.50"}
	at := time.Date(2026, 10, 23, 9, 58, 0, 0, time.UTC)
	s.Forecast = func() api.CapacityForecast {
		return api.CapacityForecast{At: &at, Jobs: []string{"show-a/sh050-light-forecast", "show-b/ep105-crowd-forecast"},
			Bursts: []api.ForecastBurst{{Pool: "cpu", Slots: 12, Live: 4}},
			Worse: []api.ForecastRisk{{Job: "show-a/sh040-light-v007", Risk: "high", Live: "none"},
				{Job: "show-b/ep104-crowd", Risk: "medium", Live: "none"}}}
	}
	var f api.CapacityForecast
	if code := get(t, s, "/api/v1/capacity/forecast?as=artist", &f); code != http.StatusOK {
		t.Fatalf("forecast: %d", code)
	}
	if f.At == nil || !f.At.Equal(at) || !slices.Equal(f.Jobs, []string{"show-a/sh050-light-forecast"}) ||
		len(f.Worse) != 1 || f.Worse[0].Job != "show-a/sh040-light-v007" {
		t.Fatalf("maya sees show-a's forecast and worsened jobs only: %+v", f)
	}
	if len(f.Bursts) != 1 || f.Bursts[0].Slots != 12 || f.Bursts[0].Live != 4 || f.Bursts[0].CostPerSlotHour != 0.5 ||
		f.Bursts[0].ExtraCostPerHour != 4 {
		t.Fatalf("eight more slots at $0.50: %+v", f.Bursts)
	}
	if code := get(t, s, "/api/v1/capacity/forecast?as=wrangler", &f); code != http.StatusOK || len(f.Jobs) != 2 || len(f.Worse) != 2 {
		t.Fatalf("a farm-wide viewer: %d %+v", code, f)
	}
	// No forecast pass yet: no time, empty lists.
	s.Forecast = func() api.CapacityForecast { return api.CapacityForecast{} }
	f = api.CapacityForecast{}
	if code := get(t, s, "/api/v1/capacity/forecast?as=artist", &f); code != http.StatusOK || f.At != nil ||
		f.Jobs == nil || f.Bursts == nil || f.Worse == nil {
		t.Fatalf("no pass: %d %+v", code, f)
	}
}
