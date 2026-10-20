package server

import (
	"net/http"
	"strings"

	"github.com/Avalanche-io/c4-operator/hud/api"
)

// handleCapacityForecast is what the farm's forecast jobs would need
// (DESIGN.md 10.12, Forecast jobs): burst slots per pool and the real jobs they
// would make riskier, never requested. Jobs are the viewer's projects';
// pools are farm-wide.
func (s *Server) handleCapacityForecast(w http.ResponseWriter, r *http.Request) {
	v := s.withViewer(w, r)
	if v == nil {
		return
	}
	m := s.current()
	if m == nil {
		fail(w, http.StatusServiceUnavailable, "the HUD is still reading the farm")
		return
	}
	if s.Forecast == nil {
		fail(w, http.StatusServiceUnavailable, "this HUD has no work queue to forecast with")
		return
	}
	f := s.Forecast()
	seen := func(key string) bool {
		ns, _, _ := strings.Cut(key, "/")
		p, ok := m.nsProject[ns]
		return ok && v.Sees(p)
	}
	out := api.CapacityForecast{At: f.At, Jobs: []string{}, Bursts: []api.ForecastBurst{}, Worse: []api.ForecastRisk{}}
	for _, j := range f.Jobs {
		if seen(j) {
			out.Jobs = append(out.Jobs, j)
		}
	}
	for _, x := range f.Worse {
		if seen(x.Job) {
			out.Worse = append(out.Worse, x)
		}
	}
	for _, b := range f.Bursts {
		for _, p := range m.pools {
			if p.Name == b.Pool && p.Spec.Burst != nil {
				b.CostPerSlotHour = num(p.Spec.Burst.CostPerSlotHour)
				b.ExtraCostPerHour = round2(float64(max(0, b.Slots-b.Live)) * b.CostPerSlotHour)
			}
		}
		out.Bursts = append(out.Bursts, b)
	}
	writeJSON(w, http.StatusOK, out)
}
