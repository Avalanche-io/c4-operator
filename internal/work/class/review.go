package class

import (
	"fmt"
	"strings"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
)

// ReviewName is the job's final review unit (spec.review, integrations
// GAPS 7). No expansion document may name it, so nothing waits on it.
const ReviewName = "@review"

// Review returns the index of the job's final review unit.
func (p *Plan) Review() (int, bool) { return p.reviewAt - 1, p.reviewAt > 0 }

// WithReview adds the job's review, when its spec asks for one: a wait
// for the reviewer after every unit the class planned. Plan.Apply joins
// every unit an expansion adds to it, so it is always last.
func WithReview(job *c4v1.WorkJob, p *Plan) (*Plan, error) {
	r := job.Spec.Review
	if r == nil {
		return p, nil
	}
	if _, ok := p.Review(); ok {
		return p, nil
	}
	w, err := docWait(ReviewName, &DocWait{Server: r.Server, Ask: r.Ask, Choices: r.Choices, Expect: float64(r.ExpectSeconds)}, false)
	if err != nil {
		return nil, fmt.Errorf("review: %w", err)
	}
	if !strings.HasPrefix(w.Server, "person:") && !strings.HasPrefix(w.Server, "party:") {
		return nil, fmt.Errorf("review: a person or a party reviews, not %s", w.Server)
	}
	i := len(p.Units)
	u := Unit{Index: i, Name: ReviewName, Label: "review", Key: "review", Slots: 1, Exists: 1, Wait: w}
	if n := len(p.Units); n > 0 {
		u.Pos = p.Units[n-1].Pos
	}
	for j := range p.Units {
		u.After = append(u.After, j)
	}
	p.Units = append(p.Units, u)
	p.Order = append(p.Order, i)
	p.reviewAt = i + 1
	return p, nil
}
