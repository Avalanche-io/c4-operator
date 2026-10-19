package mlflow

import (
	"time"

	"github.com/Avalanche-io/c4-operator/internal/tracker"
)

// runStatus is MLflow's own RunStatus for a summary state. The run is the
// work, so its finishing is a work state: in-review means the training
// finished and its output waits for a person. needs-person and held keep
// the run's last status; the tags say why.
var runStatus = map[string]string{
	"queued":        "SCHEDULED",
	"running":       "RUNNING",
	"rework":        "RUNNING",
	"in-review":     "FINISHED",
	"done":          "FINISHED",
	"done-accepted": "FINISHED",
	"failed":        "FAILED",
}

// Present shows a link's state on its run: the status, when the state has
// one (the Tracker's statusOut.states may override), and the c4.* tags:
// the state, the job (c4.job, <namespace>/<name>, the tag intake adopts by)
// and its HUD link (c4.job_url), why a person is needed, and the ETA on
// its own clock. The approve alias is never written: approval is the registry's.
func (a *Adapter) Present(s tracker.LinkState, c tracker.PresentConfig) []tracker.FieldWrite {
	if s.Ref.Type != "Run" {
		return nil
	}
	status := runStatus[s.State]
	if v, ok := c.States[s.State]; ok {
		status = v
	}
	eta := ""
	if s.ETA != nil && s.Active {
		eta = s.ETA.UTC().Format(time.RFC3339)
	}
	why := s.Why
	if why == "" {
		why = "-" // a tag cannot be emptied by an empty write; "-" clears the reason
	}
	fields := map[string]string{"status": status, "c4.state": s.State, "c4.job": s.Job, "c4.job_url": s.JobURL,
		"c4.why": why, "c4.eta": eta}
	return []tracker.FieldWrite{{
		Field: "run", Value: s.State + "|" + status + "|" + why, Continuous: eta,
		Build: func(tracker.Written) tracker.Write {
			return tracker.Write{On: s.Ref, Kind: "run", Fields: fields}
		},
	}}
}
