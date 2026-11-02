package server

import (
	"bytes"
	"context"
	"fmt"
	"time"

	"github.com/Avalanche-io/c4"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/hud/api"
	"github.com/Avalanche-io/c4-operator/internal/provenance"
	"github.com/Avalanche-io/c4-operator/schema"
)

// A unit signs its acts as itself (PEOPLE-KEYS 5.1): a key that never
// leaves its pod, certified by the workload issuer on the pod's own token
// as unit/<ns>/<job>/<unit>@<job UID>/<attempt>. The HUD takes a unit's
// act only from that unit (10.4 item 18): the request comes with the
// unit's own pod-bound token, and the act's signer is exactly that unit's
// attempt, the job's whole UID included, which the act's chain carries
// only in part. The unit acts for its job's owner, under the job's
// ownership; the verifier refuses the kinds a unit may never make.

// citeUnitAct is citeAct for an act whose chain is by a unit: data the
// bundle as sent, vr its verified envelope, p its predicate, v the
// request's viewer.
func (s *Server) citeUnitAct(ctx context.Context, data []byte, vr *provenance.Verified, p schema.ActPredicate, v *api.Viewer, subject provenance.Subject) (*citedAct, error) {
	if v == nil || v.Unit == nil {
		return nil, fmt.Errorf("%w is a unit's, and is taken only from that unit's own token: this request is no unit's", errAct)
	}
	u := v.Unit
	if u.JobUID == "" || u.Attempt < 1 {
		return nil, fmt.Errorf("%w is a unit's, and the sending unit's job or attempt is not known here", errAct)
	}
	if want := provenance.UnitIdentity(u.Namespace, u.Job, u.Unit, u.JobUID, u.Attempt); vr.Identity != want {
		return nil, fmt.Errorf("%w is signed by %s, and this request comes from %s", errAct, vr.Identity, want)
	}
	owner, err := s.unitJobOwner(ctx, u)
	if err != nil {
		return nil, err
	}
	if p.Chain.For.Party != owner {
		return nil, fmt.Errorf("%w is for %s, and its unit acts for its job's owner, %s", errAct, p.Chain.For.Party, owner)
	}
	if _, _, err := provenance.VerifyAct(data, s.trust(ctx).roots, time.Now(), s.actsSeen()); err != nil {
		return nil, fmt.Errorf("%w: %w", errAct, err)
	}
	return &citedAct{cite: &schema.ActCitation{Payload: provenance.PayloadHash(vr.Payload), Record: c4.Identify(bytes.NewReader(data)).String()},
		params: p.Params, grade: p.Chain.By.Grade, on: subject}, nil
}

// unitJobOwner is the owner of the job a unit viewer runs in, that job
// and no other of its name (by UID).
func (s *Server) unitJobOwner(ctx context.Context, u *api.UnitViewer) (string, error) {
	m := s.current()
	if m == nil {
		return "", fmt.Errorf("%w is a unit's, and the HUD is still reading the farm", errAct)
	}
	rec, err := s.lookup(ctx, m, u.Namespace, u.Job)
	if err != nil || rec == nil || string(rec.job.UID) != u.JobUID {
		return "", fmt.Errorf("%w is a unit's, and its job %s/%s is not here", errAct, u.Namespace, u.Job)
	}
	owner := rec.job.Annotations[c4v1.AnnotationOwner]
	if owner == "" {
		return "", fmt.Errorf("%w is a unit's, and its job %s/%s has no owner on record", errAct, u.Namespace, u.Job)
	}
	return owner, nil
}
