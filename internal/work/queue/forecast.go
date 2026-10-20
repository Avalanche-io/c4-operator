package queue

// Forecast jobs (integrations GAPS 9). A job with spec.forecast exists
// only as a forecast: work a tracker expects (a shot not yet turned over,
// its bid days) that the farm should plan for. The queue observes and
// estimates it but never releases it: it is left out of the live
// decision, so it creates no pods, reserves no quota or fair share, moves
// no real job's ETA and buys no burst capacity. A separate forecast pass,
// against a throwaway state, places it among the real work as waiting
// work and gives it its own estimate and risk, and gives the farm the
// burst capacity its forecasts would need (Queue.Forecast).

import (
	"strconv"
	"time"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/work/class"
	"github.com/Avalanche-io/c4-operator/work/estimate"
	"github.com/Avalanche-io/c4-operator/work/sched"
)

// forecastEvery spaces forecast passes: a second policy pass is not
// needed every tick.
const forecastEvery = 30 * time.Second

func isForecast(j *c4v1.WorkJob) bool { return j.Spec.Forecast != nil }

func forecastHours(s string) float64 {
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f < 0 {
		return 0
	}
	return f
}

// forecastPlan is a forecast given as slot-hours: one generator, never
// expanded, whose prior is the forecast, so the estimator spreads its work
// over as many units as it would take (not one unit's critical path).
func forecastPlan(j *c4v1.WorkJob) *class.Plan {
	f := j.Spec.Forecast
	work := forecastHours(f.SlotHours) * 3600
	sd := forecastHours(f.SD) * 3600
	if sd <= 0 {
		sd = work / 2
	}
	unit := forecastHours(f.UnitHours) * 3600
	if unit <= 0 || unit > work {
		unit = min(work, 3600)
	}
	exists := 1.0
	if e := forecastHours(f.Exists); e > 0 && e <= 1 {
		exists = e
	}
	u := class.Unit{Index: 0, Name: "forecast", Label: "forecast", Key: "forecast/" + j.Spec.Class, Slots: 1, Generator: true,
		Exists: exists, Work: work, WorkSD: sd, Params: map[string]string{"longest": strconv.FormatFloat(unit, 'f', -1, 64)}}
	return &class.Plan{Units: []class.Unit{u}, Order: []int{0}, OrderName: "forecast", Estimate: estimate.DefaultConfig()}
}

// ForecastBurst is burst capacity the forecasts would need in a pool.
type ForecastBurst struct {
	Pool  string  `json:"pool"`
	Slots int     `json:"slots"`
	Cost  float64 `json:"cost"`
}

// Forecast is what the farm's forecast jobs, among its real work, would
// need: burst capacity per pool (never requested), at its last pass.
type Forecast struct {
	At     time.Time       `json:"at"`
	Jobs   int             `json:"jobs"`
	Bursts []ForecastBurst `json:"bursts,omitempty"`
}

// Forecast is the last forecast pass's farm view.
func (q *Queue) Forecast() Forecast {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.lastForecast
}

// forecastPass places forecast jobs among the real work, as waiting work,
// on a throwaway state, and keeps its decision for their status. Nothing
// it decides is acted on.
func (q *Queue) forecastPass(now time.Time, pools map[string]*pool, live sched.Snapshot, fviews []*view, cfg sched.Config) {
	if len(fviews) == 0 {
		q.lastFdec, q.lastForecast = sched.Decisions{}, Forecast{At: now}
		return
	}
	if !q.lastFpass.IsZero() && now.Sub(q.lastFpass) < forecastEvery {
		return
	}
	q.lastFpass = now
	snap := live
	snap.Jobs = append([]sched.Job(nil), live.Jobs...)
	free := 0
	for _, p := range pools {
		free += max(0, p.slots-p.used)
	}
	for _, v := range fviews {
		v.blocked = true // waiting: never runnable, even here
		j := schedJob(v, free)
		if s := v.job.Spec.Forecast.Start; s != nil && s.After(j.Ready) {
			j.Ready = s.Time
		}
		snap.Jobs = append(snap.Jobs, j)
	}
	var st sched.State
	dec := q.Policy(snap, &st, cfg)
	q.lastFdec = dec
	fc := Forecast{At: now, Jobs: len(fviews)}
	by := map[string]*ForecastBurst{}
	var order []string
	for _, b := range dec.Bursts {
		total := 0
		for _, n := range b.Slots {
			total += n
		}
		for _, p := range b.Order {
			n := b.Slots[p]
			if n <= 0 {
				continue
			}
			fb := by[p]
			if fb == nil {
				fb = &ForecastBurst{Pool: p}
				by[p] = fb
				order = append(order, p)
			}
			fb.Slots += n
			if total > 0 {
				fb.Cost += b.Cost * float64(n) / float64(total)
			}
		}
	}
	for _, p := range order {
		fc.Bursts = append(fc.Bursts, *by[p])
	}
	q.lastForecast = fc
}

func orStr(s, d string) string {
	if s == "" {
		return d
	}
	return s
}
