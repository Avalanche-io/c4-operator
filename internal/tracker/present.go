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
// adapter decides what the write looks like. (ADAPTER.md section 9: the
// build showed presentation is per tracker, so it moved out of the loop.)

// LinkState is one link's state as the loop knows it.
type LinkState struct {
	Link       string // the TrackerLink, namespace/name
	Ref        Ref    // the tracker entity the link serves (an issue, a task)
	Review     *Ref   // what is under review, once there is one
	ReviewHead string // its head commit, when it has heads
	Job        string // the job, namespace/name
	JobURL     string
	State      string // a summary state (MAPPING.md section 2)
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
}

// WaitState is the open wait the tracker serves.
type WaitState struct {
	Server    string // person:<id> or party:<name>
	Choices   []string
	Escalated bool
}

// Decisions are what a person may decide on the wait: approve and its
// choices, or for an escalated unit accept and rework.
func (w *WaitState) Decisions() []string {
	if w == nil {
		return nil
	}
	if w.Escalated {
		return []string{"accept", "rework"}
	}
	out := []string{"approve"}
	for _, c := range w.Choices {
		if c != "approve" {
			out = append(out, c)
		}
	}
	return out
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

// WriteKey is the idempotency key of a write: the link, the field and
// what it writes. A crash between a write and the loop remembering it is
// found by the adapter's probe under the same key.
func WriteKey(link, field, value string) string {
	h := sha256.Sum256([]byte(link + "/" + field + "/" + value))
	return hex.EncodeToString(h[:12])
}
