package syncer_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/hud/api"
	"github.com/Avalanche-io/c4-operator/internal/effect"
	"github.com/Avalanche-io/c4-operator/internal/tracker"
	"github.com/Avalanche-io/c4-operator/internal/tracker/github"
	"github.com/Avalanche-io/c4-operator/internal/tracker/syncer"
)

// lossyGitHub is GitHub with comments that nothing guards (as a tracker
// with no idempotency key and no way to find a comment by marker), whose
// answer to a comment is lost while loseAnswers is set: the comment is
// made, and the caller gets an error.
type lossyGitHub struct{ *github.Adapter }

var loseAnswers atomic.Bool

func (a *lossyGitHub) Guard(w tracker.Write) effect.Guard {
	if w.Kind == "comment" {
		return effect.Unguarded
	}
	return a.Adapter.Guard(w)
}

func (a *lossyGitHub) Apply(ctx context.Context, w tracker.Write) (tracker.Ref, error) {
	ref, err := a.Adapter.Apply(ctx, w)
	if err == nil && w.Kind == "comment" && loseAnswers.Load() {
		return tracker.Ref{}, errors.New("connection reset by peer")
	}
	return ref, err
}

func init() {
	tracker.Register("github-lossy", func(c tracker.Config) (tracker.Adapter, error) {
		a, err := github.New(c)
		if err != nil {
			return nil, err
		}
		return &lossyGitHub{a}, nil
	})
}

// An unguarded write whose answer was lost is held for a person, never
// sent again on a guess, and the link says so; once a person confirms it,
// writes go on and the link stops saying so.
func TestUnguardedWriteHeld(t *testing.T) {
	e := setup(t, func(tr *c4v1.Tracker) { tr.Spec.Kind = "github-lossy" })
	ctx := context.Background()
	loseAnswers.Store(true)
	t.Cleanup(func() { loseAnswers.Store(false) })
	if err := e.sync.Sync(ctx, e.tracker()); err == nil {
		t.Fatal("a lost answer is an error for the pass")
	}
	if n := len(e.gh.Comments(7)); n != 1 {
		t.Fatalf("%d comments, want the one whose answer was lost", n)
	}
	if w := e.link().Status.Written["comment"]; w.State != "sending" {
		t.Fatalf("journaled before it was sent, and still sending: %+v", w)
	}
	loseAnswers.Store(false)
	e.pass()
	e.pass()
	if n := len(e.gh.Comments(7)); n != 1 {
		t.Fatalf("%d comments: an unguarded write was sent again on a guess", n)
	}
	l := e.link()
	if w := l.Status.Written["comment"]; w.State != "unconfirmed" {
		t.Fatalf("held: %+v", w)
	}
	if c := meta.FindStatusCondition(l.Status.Conditions, syncer.CondUnconfirmed); c == nil || c.Status != metav1.ConditionTrue {
		t.Fatalf("the link says it is held: %+v", l.Status.Conditions)
	}

	// A person confirms it happened; the job moves on, and its next
	// comment is written.
	before := l.DeepCopy()
	w := l.Status.Written["comment"]
	w.State = ""
	l.Status.Written["comment"] = w
	if err := e.k8s.Status().Patch(ctx, l, client.MergeFrom(before)); err != nil {
		t.Fatal(err)
	}
	e.job(func(j *api.Job) { j.Phase = "Running" })
	e.pass()
	l = e.link()
	if c := meta.FindStatusCondition(l.Status.Conditions, syncer.CondUnconfirmed); c == nil || c.Status != metav1.ConditionFalse {
		t.Fatalf("confirmed, and written since: %+v", l.Status.Conditions)
	}
	if w := l.Status.Written["comment"]; w.State != "" || w.Value == before.Status.Written["comment"].Value {
		t.Fatalf("the next comment: %+v", w)
	}
}

// A person says a held write did not happen (the HUD clears its value and
// state, keeping the entity it was on): the next pass writes it again,
// once.
func TestDeniedWriteWrittenAgain(t *testing.T) {
	e := setup(t, func(tr *c4v1.Tracker) { tr.Spec.Kind = "github-lossy" })
	ctx := context.Background()
	loseAnswers.Store(true)
	t.Cleanup(func() { loseAnswers.Store(false) })
	if err := e.sync.Sync(ctx, e.tracker()); err == nil {
		t.Fatal("a lost answer is an error for the pass")
	}
	loseAnswers.Store(false)
	e.pass()
	l := e.link()
	if w := l.Status.Written["comment"]; w.State != "unconfirmed" {
		t.Fatalf("held: %+v", w)
	}
	before := l.DeepCopy()
	l.Status.Written["comment"] = c4v1.TrackerWritten{Ref: l.Status.Written["comment"].Ref, At: l.Status.Written["comment"].At}
	if err := e.k8s.Status().Patch(ctx, l, client.MergeFrom(before)); err != nil {
		t.Fatal(err)
	}
	e.pass()
	e.pass()
	if n := len(e.gh.Comments(7)); n != 2 {
		t.Fatalf("%d comments: a write a person said did not happen is written again, once", n)
	}
	if w := e.link().Status.Written["comment"]; w.State != "" || w.Value == "" {
		t.Fatalf("written: %+v", w)
	}
}
