package tracker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"time"
)

// Presenting a link's state is the adapter's: GitHub shows a job as a
// comment, a check run and a commit status; Flow as a Task status and two
// fields. The loop decides what the state is and when a write is due; the
// adapter decides what the write looks like. Building the adapters showed
// that presentation is per tracker, so it moved out of the loop.

// LinkState is one link's state as the loop knows it.
type LinkState struct {
	Link       string // the TrackerLink, namespace/name
	Ref        Ref    // the tracker entity the link serves (an issue, a task)
	Review     *Ref   // what is under review, once there is one
	ReviewHead string // its head commit, when it has heads
	Job        string // the job, namespace/name
	JobURL     string
	State      string // a summary state (syncer's Summary states)
	Words      string // the state as a reader sees it
	Title      string // one line: the state, the ETA, the risk
	Why        string // why a person is needed, or the job failed
	Active     bool   // the work is still going
	ETA        *time.Time
	AtRisk     bool
	Deadline   *time.Time
	// DeadlineSource says where the deadline came from, in words.
	DeadlineSource string
	Wait           *WaitState
	// Loc is the zone the tracker's readers read times in (the project's),
	// for When; nil is UTC.
	Loc *time.Location
	// Spend is what the link's jobs have cost so far, every version, in
	// the pools' currency: slot time at their pools' rates and model
	// tokens at their prices. 0: not priced.
	Spend float64
	// Parent is the entity the tracker entity belongs to (a Task's Shot),
	// when the link has recorded it, and ParentSpend the total of every
	// link on it: a total that names no one person.
	Parent      *Ref
	ParentSpend float64
}

// WaitState is the open wait the tracker serves.
type WaitState struct {
	Server    string // person:<id> or party:<name>
	Choices   []string
	Escalated bool
}

// Decisions are what a person may decide on the wait, as the queue and
// the HUD take them (internal/work/queue/wait.go): a wait with choices
// takes one of them (or fail); a wait without takes approve; an escalated
// unit takes accept or rework.
func (w *WaitState) Decisions() []string {
	if w == nil {
		return nil
	}
	if w.Escalated {
		return []string{"accept", "rework"}
	}
	if len(w.Choices) == 0 {
		return []string{"approve"}
	}
	return append([]string(nil), w.Choices...)
}

// Written is what the loop last wrote for one field.
type Written struct {
	Value string // a fingerprint
	Ref   string // the tracker entity the write made or changed
}

// FieldWrite is one field of a link's presentation. The loop writes it
// when Value changes, or when only Continuous changes and the Tracker's
// fieldSeconds have passed; Build makes the write from what was last
// written (to update an entity in place rather than make another).
type FieldWrite struct {
	Field      string
	Value      string
	Continuous string
	Build      func(prev Written) Write
}

// PresentConfig is the Tracker's say in presentation.
type PresentConfig struct {
	States       map[string]string // summary state to the tracker's status
	CheckName    string
	CommitStatus bool
}

// ParentPresenter is a presenter that writes on the tracker entity's
// parent (the spend total on a Flow Shot), so the syncer records each
// link's parent and totals what it presents there.
type ParentPresenter interface {
	PresentsOnParent() bool
}

// Presenter is how an adapter shows a link's state. Every adapter is also
// a Presenter.
type Presenter interface {
	Present(s LinkState, c PresentConfig) []FieldWrite
}

// ReviewFinder finds what is under review for links whose review the
// tracker makes, not C4 (a pull request someone opens). Adapters whose
// review C4 publishes (PublishForReview) need not implement it.
type ReviewFinder interface {
	FindReviews(ctx context.Context, links []LinkState) (map[string]Ref, error)
}

// Verdicter gives an adapter's own default C4 decision for one of its
// native words where the Tracker's verdictsIn names none ("" for no
// decision): Kitsu reads it from the status's flags (is_done, is_retake)
// rather than from names a studio may change. Adapters whose defaults are
// the loop's need not implement it.
type Verdicter interface {
	Verdict(native string) string
}

// WriteKey is the idempotency key of a write: the link, the field and
// what it writes. A crash between a write and the loop remembering it is
// found by the adapter's probe under the same key.
func WriteKey(link, field, value string) string {
	h := sha256.Sum256([]byte(link + "/" + field + "/" + value))
	return hex.EncodeToString(h[:12])
}

// When is a time as a tracker's readers read it: in their zone, with UTC
// beside it ("Wed 21 Oct 16:30 PDT (23:30 UTC)"), as the rest of C4 shows
// times; in UTC alone when the zone is UTC or not known.
func When(t time.Time, loc *time.Location) string { return when(t, loc, "Mon 2 Jan 15:04") }

// WhenShort is When without the date, for a one-line status.
func WhenShort(t time.Time, loc *time.Location) string { return when(t, loc, "Mon 15:04") }

func when(t time.Time, loc *time.Location, layout string) string {
	u := t.UTC()
	if loc == nil || loc.String() == "UTC" {
		return u.Format(layout + " UTC")
	}
	return t.In(loc).Format(layout+" MST") + " (" + u.Format("15:04") + " UTC)"
}
