package forecast

import "time"

// State is where a suggested deadline is in its life.
type State string

const (
	// Estimating: the first suggestion, from history and policy, before
	// any revision.
	Estimating State = "Estimating"
	// Settling: revised at least once, still allowed to move.
	Settling State = "Settling"
	// Settled: fixed. The system never moves it again; only a person does.
	Settled State = "Settled"
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

// Revision is one value the suggestion took.
type Revision struct {
	At       time.Time
	Deadline time.Time
	Finished int // units finished when it was set
}

// Suggestion is a job's suggested deadline and the rules that let it move
// at most a few times over the first binary-order units and then settle.
//
// A revision happens only at an information event, only when the policy's
// earliest deadline has moved by more than max(MinMoveFrac of the time
// left, MinMove), and only when the new band is narrower than the band
// behind the current suggestion by MinNarrowing: the suggestion moves
// when the system knows more, not when noise moves the center. After
// MaxRevisions revisions, after SettleUnits finished units, or once
// SettleShare of the time to the deadline has passed, it is Settled and
// never moves again.
type Suggestion struct {
	Deadline  time.Time
	State     State
	Revisions int
	// Width is the band width (Band.Width from Start) behind Deadline.
	Width   float64
	Start   time.Time
	History []Revision
}

// Event is what the caller observes at one moment.
type Event struct {
	Now time.Time
	// Finished and Total count the job's units (Total includes units not
	// yet revealed by generators only as far as they are known).
	Finished, Total int
	// Info marks an information event (InfoPoint, or a generator that
	// just expanded).
	Info bool
	// Candidate is the policy's earliest deadline for the band's finish at
	// the policy quantile (Policy.Earliest).
	Candidate time.Time
	Band      Band
}

// Observe applies an event and reports whether Deadline changed.
func (s *Suggestion) Observe(p Policy, ev Event) bool {
	if ev.Candidate.IsZero() || s.State == Settled {
		return false
	}
	if s.Deadline.IsZero() {
		s.Deadline, s.Start, s.State = ev.Candidate, ev.Now, Estimating
		s.Width = ev.Band.Width(ev.Now)
		s.History = append(s.History, Revision{At: ev.Now, Deadline: ev.Candidate, Finished: ev.Finished})
		s.settle(p, ev)
		return true
	}
	changed := false
	if ev.Info && s.Revisions < p.MaxRevisions && s.moves(p, ev) {
		s.Deadline = ev.Candidate
		s.Width = ev.Band.Width(s.Start)
		s.Revisions++
		s.State = Settling
		s.History = append(s.History, Revision{At: ev.Now, Deadline: ev.Candidate, Finished: ev.Finished})
		changed = true
	}
	s.settle(p, ev)
	return changed
}

func (s *Suggestion) moves(p Policy, ev Event) bool {
	left := s.Deadline.Sub(ev.Now)
	if left < 0 {
		left = 0
	}
	min := time.Duration(p.MinMoveFrac * float64(left))
	if min < p.MinMove {
		min = p.MinMove
	}
	d := ev.Candidate.Sub(s.Deadline)
	if d < 0 {
		d = -d
	}
	if d <= min {
		return false
	}
	return ev.Band.Width(s.Start) <= (1-p.MinNarrowing)*s.Width
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
