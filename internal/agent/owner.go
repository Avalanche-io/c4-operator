package agent

import (
	"context"
	"fmt"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// refuses says why p may not run on this runner, or "" if it may: the
// runner runs only the work of its owners (Runner.Owners), a person's own
// sessions under their own sign-in. A pod's owner is read from the WorkJob
// that controls it, matched by the UID in the pod's controller reference,
// never from the pod: the job's owner is fixed when it is created (the
// c4-workjob-owner policy), and only the work queue creates pods a WorkJob
// controls (c4-host-pods). The answer is kept per pod, since neither can
// change.
func (r *Runner) refuses(ctx context.Context, p *corev1.Pod) (string, error) {
	if why, ok := r.owned[p.UID]; ok {
		return why, nil
	}
	why, err := r.ownerCheck(ctx, p)
	if err != nil {
		return "", err
	}
	r.owned[p.UID] = why
	return why, nil
}

func (r *Runner) ownerCheck(ctx context.Context, p *corev1.Pod) (string, error) {
	if len(r.Owners) == 0 {
		return "this runner runs no one's work: start it with --owner", nil
	}
	j, why, err := r.jobOf(ctx, p)
	if err != nil || why != "" {
		return why, err
	}
	owner := j.Owner
	if slices.Contains(r.Owners, owner) {
		return "", nil
	}
	whose := "has no owner"
	if owner != "" {
		whose = "is " + owner + "'s"
	}
	return fmt.Sprintf("job %s/%s %s; this runner runs only the jobs of %s", p.Namespace, j.Name, whose, strings.Join(r.Owners, ", ")), nil
}
