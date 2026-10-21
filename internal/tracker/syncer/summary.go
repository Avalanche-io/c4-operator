package syncer

import (
	"fmt"
	"strings"
	"time"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/hud/api"
	"github.com/Avalanche-io/c4-operator/internal/tracker"
)

// Summary states, the work states c4-sync writes (docs/c4-sync.md, "What it
// writes, and what it never writes"). A tracker
// status changes only when the summary state does; continuous values (the
// ETA) go to text that notifies no one.
const (
	StateQueued       = "queued"
	StateRunning      = "running"
	StateNeedsPerson  = "needs-person"
	StateInReview     = "in-review"
	StateRework       = "rework"
	StateHeld         = "held"
	StateDone         = "done"
	StateDoneAccepted = "done-accepted"
	StateFailed       = "failed"
)

// Summary is a job reduced to what a tracker can show.
type Summary struct {
	State string
	// Wait is the open wait the tracker serves, in-review only.
	Wait *api.Wait
	// Why is the reason a person is needed, or the job failed.
	Why string
	// Question says the person needed is asked for information a unit
	// stopped for (NeedsInformation), which Why then is.
	Question bool
	// ETA is the finish a person is shown (p80 when the band has it, else
	// p90); nil when the job has none.
	ETA      *time.Time
	Risk     string
	Deadline *time.Time
	Source   string // the deadline's source: spec, suggested, class
	Mode     string // Pinned, Suggested or Tracking
	Output   string // the output view's C4 ID, once there is one
}

// Summarize reduces a job. review names the job's wait the tracker serves;
// reworking says the last relayed decision was a rework that has not come
// back for review yet.
func Summarize(j *api.Job, review string, reworking bool) Summary {
	s := Summary{
		Risk:     j.Estimate.Risk,
		Deadline: j.Objective.Deadline,
		Source:   j.Objective.Source,
		Mode:     j.Objective.Mode,
	}
	if j.Output != nil {
		s.Output = j.Output.View
	}
	if j.Estimate.NoETA == "" {
		switch {
		case j.Estimate.EtaP80 != nil:
			t := *j.Estimate.EtaP80
			s.ETA = &t
		case !j.Estimate.EtaP90.IsZero():
			t := j.Estimate.EtaP90
			s.ETA = &t
		}
	}
	for i := range j.Waits {
		w := &j.Waits[i]
		if w.Do == "" && w.Since != nil && review != "" && w.Name == review {
			s.Wait = w
		}
	}
	switch {
	case j.Phase == "Succeeded" && j.Units.Accepted > 0:
		s.State = StateDoneAccepted
	case j.Phase == "Succeeded":
		s.State = StateDone
	case j.Phase == "Failed":
		s.State, s.Why = StateFailed, needsWhy(j)
	case s.Wait != nil:
		s.State = StateInReview
	case j.NeedsPerson != nil:
		s.State, s.Why = StateNeedsPerson, needsWhy(j)
		s.Question = j.NeedsPerson.Reason == strings.ToLower(c4v1.NeedsInformation)
	case j.Phase == "Held":
		s.State = StateHeld
	case reworking && j.Phase == "Running":
		s.State = StateRework
	case j.Phase == "Running":
		s.State = StateRunning
	default:
		s.State = StateQueued
	}
	return s
}

func needsWhy(j *api.Job) string {
	if j.NeedsPerson == nil {
		return ""
	}
	if j.NeedsPerson.Message != "" {
		return j.NeedsPerson.Message
	}
	return j.NeedsPerson.Reason
}

// Words is the state as a tracker reader sees it.
func (s Summary) Words() string {
	switch s.State {
	case StateQueued:
		return "Queued"
	case StateRunning:
		return "Running"
	case StateNeedsPerson:
		if s.Question {
			return "Blocked: needs information"
		}
		return "Needs a person"
	case StateInReview:
		if who := strings.TrimPrefix(s.Wait.WaitsFor, "person:"); who != s.Wait.WaitsFor {
			return "Waiting for review by " + who
		}
		return "Waiting for review"
	case StateRework:
		return "Reworking"
	case StateHeld:
		return "Held"
	case StateDone:
		return "Done"
	case StateDoneAccepted:
		return "Done, with accepted notes"
	case StateFailed:
		return "Failed"
	}
	return s.State
}

// Active is whether the work is still going (a check run in progress).
func (s Summary) Active() bool {
	switch s.State {
	case StateQueued, StateRunning, StateInReview, StateRework, StateHeld:
		return true
	}
	return false
}

// AtRisk is the HUD's orchid: the deadline is at risk, will be missed, or
// was. A field, never a status.
func (s Summary) AtRisk() bool {
	return s.Risk == "atRisk" || s.Risk == "willMiss" || s.Risk == "missed"
}

// Title is a one-line status: the state, the ETA in the readers' zone
// (loc; nil is UTC) and the risk.
func (s Summary) Title(loc *time.Location) string {
	t := s.Words()
	if s.Active() && s.ETA != nil {
		t += " · ETA " + tracker.WhenShort(*s.ETA, loc)
	}
	if s.AtRisk() {
		t += " · deadline " + map[string]string{"atRisk": "at risk", "willMiss": "will be missed", "missed": "missed"}[s.Risk]
	}
	return t
}

// Key is what changes a status write: the state, why, and risk. The ETA
// is left out; it is written on its own clock (fieldSeconds).
func (s Summary) Key() string {
	return fmt.Sprintf("%s|%s|%s|%t", s.State, s.Why, waitKey(s.Wait), s.AtRisk())
}

func waitKey(w *api.Wait) string {
	if w == nil {
		return ""
	}
	return fmt.Sprintf("%d:%s", w.Unit, w.WaitsFor)
}
