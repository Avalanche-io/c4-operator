package server

// A real job that replaces a forecast (integrations GAPS 9) names it
// (c4.avalanche.io/replaces). The HUD deletes the forecast when it submits
// the job; a delete that fails then is tried again from the collect loop,
// with backoff, until the forecast is gone, so a forecast and the real
// job that replaced it are never both planned for long. Every replica
// sweeps; the delete is preconditioned on the forecast's UID, so one
// succeeds and books it, and the others find it gone.

import (
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
)

const (
	retireFirst = 5 * time.Second
	retireMost  = 5 * time.Minute
)

// retireTry is when to try a replaced forecast's delete again, and how
// long to wait after that one fails.
type retireTry struct {
	at   time.Time
	wait time.Duration
}

// retireForecast deletes forecast f, replaced by job by, and books it as
// replaced. A forecast gone already is not an error and is not booked
// again.
func (s *Server) retireForecast(ctx context.Context, f *c4v1.WorkJob, by, user, actor string) error {
	err := s.Client.Delete(ctx, f, client.Preconditions{UID: &f.UID})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	s.record(ctx, f, "WorkJob", user, "Replaced", fmt.Sprintf("%s: replaced by %s", f.Name, by),
		bookNote{actor: actor, kind: "replaced"})
	return nil
}

// sweepReplaced deletes the forecasts real jobs name as replaced that are
// still there. Only a forecast made before the job that replaced it goes:
// one made later (the entity was a forecast again) is new work.
func (s *Server) sweepReplaced(ctx context.Context, now time.Time) {
	m := s.current()
	if m == nil {
		return
	}
	pending := map[string]bool{}
	for _, rec := range m.jobs {
		name := rec.job.Annotations[c4v1.AnnotationReplaces]
		if name == "" || rec.job.Spec.Forecast != nil {
			continue
		}
		key := rec.job.Namespace + "/" + name
		f := m.byKey[key]
		if f == nil || f.job.Spec.Forecast == nil || f.job.CreationTimestamp.After(rec.job.CreationTimestamp.Time) {
			continue
		}
		pending[key] = true
		try := s.retire[key]
		if now.Before(try.at) {
			continue
		}
		owner := rec.job.Annotations[c4v1.AnnotationOwner]
		err := s.retireForecast(ctx, f.job.DeepCopy(), rec.job.Name, owner, owner)
		if err == nil {
			delete(s.retire, key)
			delete(pending, key)
			continue
		}
		try.wait = min(max(2*try.wait, retireFirst), retireMost)
		try.at = now.Add(try.wait)
		if s.retire == nil {
			s.retire = map[string]retireTry{}
		}
		s.retire[key] = try
		s.Log.Error(err, "delete the replaced forecast; will try again", "job", key, "replacedBy", rec.job.Name, "in", try.wait)
	}
	for key := range s.retire {
		if !pending[key] {
			delete(s.retire, key)
		}
	}
}
