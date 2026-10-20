package api

import "time"

// CapacityForecast answers GET /api/v1/capacity/forecast: what the farm's
// forecast jobs (spec.forecast, work a tracker expects that is not yet
// submitted) would need among the real work, as the queue's last forecast
// pass planned it (DESIGN.md 10.12, Forecast jobs). Nothing of it is requested.
type CapacityForecast struct {
	// At is the last forecast pass; absent when there has been none (no
	// forecast jobs).
	At *time.Time `json:"at,omitempty"`
	// Jobs are the forecast jobs planned, namespace/name, in projects the
	// viewer sees.
	Jobs []string `json:"jobs"`
	// Bursts are the pools the forecast work would want more burst slots
	// of. Pools are farm-wide, as in the snapshot.
	Bursts []ForecastBurst `json:"bursts"`
	// Worse are the real jobs, in projects the viewer sees, whose risk the
	// forecast work worsens.
	Worse []ForecastRisk `json:"worse"`
}

// ForecastBurst is the burst slots suggested in a pool with the forecast
// work (Slots) and as the farm plans it now (Live), and what the
// difference would cost an hour at the pool's burst price.
type ForecastBurst struct {
	Pool             string  `json:"pool"`
	Slots            int     `json:"slots"`
	Live             int     `json:"live"`
	CostPerSlotHour  float64 `json:"costPerSlotHour,omitempty"`
	ExtraCostPerHour float64 `json:"extraCostPerHour,omitempty"`
}

// ForecastRisk is a real job's risk with the forecast work (Risk) and as
// the farm plans it now (Live).
type ForecastRisk struct {
	Job  string `json:"job"`
	Risk string `json:"risk"`
	Live string `json:"live"`
}
