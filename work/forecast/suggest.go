package forecast

import "time"

// State is where a suggested deadline is in its life.
type State string

const (
	// Estimating: the suggestion may still move later if it turns out to
	// be at risk.
	Estimating State = "Estimating"
	// Settled: fixed. The system never moves it again; only a person does.
	Settled State = "Settled"
)

// Reasons a suggestion took a value.
const (
	ReasonFirst  = "first"
	ReasonAtRisk = "at-risk"
)

// SettleUnits is the number of finished units after which a suggestion
// settles: by then binary order has a sample in every 32nd of the axis.
const SettleUnits = 33

// SettleShare is the share of the time from the first suggestion to the
// deadline after which a suggestion settles, however few units finished.
const SettleShare = 0.25

// InfoPoint reports whether the finished-unit count n is an information
// event of binary order: the first unit, and each count 2^k+1 up to
// SettleUnits, when the next level of the order is complete (every half,
// quarter, eighth... of the axis has a sample).
func InfoPoint(n int) bool {
	if n == 1 {
		return true
	}
	if n < 3 || n > SettleUnits {
		return false
	}
	m := n - 1
	return m&(m-1) == 0
}

// Revision is one value the suggestion took, and why.
type Revision struct {
	At       time.Time
	Deadline time.Time
	Finished int // units finished when it was set
	Reason   string
}

// Suggestion is a job's suggested deadline.
//
// Suggestions are generous (Policy.Quantile, 0.95 by default) and the
// system never moves one earlier: a deadline met early is still met, and
// tightening it is the person's act, made with the deadline-options
// readout. The only move the system makes is later, and only when the
// suggestion itself is at risk: at an information event, if the band's
// point at the policy quantile is after the deadline, the suggestion moves
// to the new candidate when that is later by more than max(MinMoveFrac of
// the time left, MinMove). Such a move is recorded with ReasonAtRisk, so
// it is shown, not silent. After MaxRevisions moves, after SettleUnits
// finished units, or once SettleShare of the time to the deadline has
// passed, the suggestion is Settled; a settled suggestion that becomes at
// risk stays put and the job's feasibility warning says so.
type Suggestion struct {
	Deadline  time.Time
	State     State
	Revisions int
	Start     time.Time
	History   []Revision
}

// Event is what the caller observes at one moment.
type Event struct {
	Now time.Time
	// Finished and Total count the job's units.
	Finished, Total int
	// Info marks an information event (InfoPoint, or a generator that
	// just expanded).
	Info bool
	// Candidate is the policy's deadline for the band's finish at the
	// policy quantile now (Policy.Earliest). It is consulted only when the
	// suggestion is at risk.
	Candidate time.Time
	// Band is the band shown now.
	Band Band
}

// AtRisk reports whether the band's point at quantile q is after the
// suggested deadline.
func (s *Suggestion) AtRisk(q float64, b Band) bool {
	return !s.Deadline.IsZero() && !b.IsZero() && b.At(q).After(s.Deadline)
}

// Observe applies an event and reports whether Deadline changed.
func (s *Suggestion) Observe(p Policy, ev Event) bool {
	if s.State == Settled {
		return false
	}
	if s.Deadline.IsZero() {
		if ev.Candidate.IsZero() {
			return false
		}
		s.Deadline, s.Start, s.State = ev.Candidate, ev.Now, Estimating
		s.History = append(s.History, Revision{At: ev.Now, Deadline: ev.Candidate, Finished: ev.Finished, Reason: ReasonFirst})
		s.settle(p, ev)
		return true
	}
	changed := false
	if ev.Info && s.Revisions < p.MaxRevisions && s.AtRisk(p.Quantile, ev.Band) && s.later(p, ev) {
		s.Deadline = ev.Candidate
		s.Revisions++
		s.History = append(s.History, Revision{At: ev.Now, Deadline: ev.Candidate, Finished: ev.Finished, Reason: ReasonAtRisk})
		changed = true
	}
	s.settle(p, ev)
	return changed
}

// later reports whether the candidate is later than the deadline by more
// than the policy's minimum move. An earlier candidate never moves it.
func (s *Suggestion) later(p Policy, ev Event) bool {
	if ev.Candidate.IsZero() {
		return false
	}
	left := s.Deadline.Sub(ev.Now)
	if left < 0 {
		left = 0
	}
	min := time.Duration(p.MinMoveFrac * float64(left))
	if min < p.MinMove {
		min = p.MinMove
	}
	return ev.Candidate.Sub(s.Deadline) > min
}

func (s *Suggestion) settle(p Policy, ev Event) {
	switch {
	case s.Revisions >= p.MaxRevisions,
		ev.Finished >= SettleUnits,
		ev.Total > 0 && ev.Finished >= ev.Total,
		ev.Now.Sub(s.Start) >= time.Duration(SettleShare*float64(s.Deadline.Sub(s.Start))):
		s.State = Settled
	}
}
