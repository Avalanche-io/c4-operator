package queue

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
)

// A unit that cannot start says why on its job, in the farm's words and
// with what to do, instead of only in the manager's log: condition
// Admitted (the API server refused its pod: Pod Security, a webhook, an
// admission policy or the namespace's quota), Lease (it waits for a
// credential lease) and Images (its image cannot be pulled). Each also
// sets NeedsPerson, since none of them clears without one.

// startWait is the latest reason a job's units could not start.
type startWait struct {
	reason, msg string
	at          time.Time
}

// waitFresh is how long a wait is shown after it last held: a refusal
// recurs every tick a unit is released, so one that stops recurring is
// over.
const waitFresh = 2 * time.Minute

func (w startWait) live(now time.Time) bool {
	return w.msg != "" && now.Sub(w.at) < waitFresh
}

var (
	quotaRe   = regexp.MustCompile(`exceeded quota: ([^,]+), requested: (.+), used: (.+), limited: (.+)$`)
	podSecRe  = regexp.MustCompile(`violates PodSecurity "([^"]+)": (.+)$`)
	webhookRe = regexp.MustCompile(`admission webhook "([^"]+)" denied the request: (.+)$`)
	vapRe     = regexp.MustCompile(`ValidatingAdmissionPolicy '([^']+)' with binding '[^']+' denied request: (.+)$`)
)

// refusal reads why the API server refused a unit's pod, in the farm's
// words, with the fix. It returns no reason for errors that are not a
// refusal (a conflict, a timeout), which the next tick retries.
func refusal(err error, ns string) (reason, msg string) {
	if err == nil {
		return "", ""
	}
	text := err.Error()
	if m := quotaRe.FindStringSubmatch(text); m != nil {
		return c4v1.NeedsQuota, fmt.Sprintf("%s's quota %s is used up (asked for %s, using %s of %s); raise the namespace's ResourceQuota, or the units start as running ones finish",
			ns, m[1], m[2], m[3], m[4])
	}
	if m := podSecRe.FindStringSubmatch(text); m != nil {
		return "PodSecurity", fmt.Sprintf("Pod Security (%s) in %s refuses the job's units: %s; give the job's template a restricted security context (runAsNonRoot, allowPrivilegeEscalation: false, capabilities drop ALL, seccomp RuntimeDefault), or build its image FROM c4/runtime-base",
			strings.TrimSuffix(m[1], ":latest"), ns, m[2])
	}
	if m := webhookRe.FindStringSubmatch(text); m != nil {
		return "Webhook", fmt.Sprintf("the admission webhook %s refuses the job's units: %s; change the job's template as it says, or ask whoever runs that webhook", m[1], m[2])
	}
	if m := vapRe.FindStringSubmatch(text); m != nil {
		return "Policy", fmt.Sprintf("the farm's policy %s refuses the job's units: %s", m[1], m[2])
	}
	if apierrors.IsForbidden(err) || apierrors.IsInvalid(err) {
		return "Refused", "the API server refuses the job's units: " + text
	}
	return "", ""
}

// imageWait says which unit cannot pull its image, from the live pods.
func imageWait(v *view) string {
	idx := make([]int, 0, len(v.pods))
	for i := range v.pods {
		idx = append(idx, i)
	}
	sort.Ints(idx)
	for _, i := range idx {
		p := v.pods[i]
		for _, c := range p.Status.ContainerStatuses {
			w := c.State.Waiting
			if w == nil || !pullFailed(w.Reason) {
				continue
			}
			image := c.Image
			for _, sc := range p.Spec.Containers {
				if sc.Name == c.Name {
					image = sc.Image
				}
			}
			why := w.Message
			if why == "" {
				why = w.Reason
			}
			return fmt.Sprintf("unit %d cannot pull its image %s (%s); load or push the image (on a local farm, c4 farm image load %s), or check its name and registry",
				i, image, why, image)
		}
	}
	return ""
}

func pullFailed(reason string) bool {
	switch reason {
	case "ErrImagePull", "ImagePullBackOff", "InvalidImageName", "ErrImageNeverPull":
		return true
	}
	return false
}

// waitConditions sets or clears Admitted, Lease and Images, and returns
// what NeedsPerson should say for the first that holds.
func waitConditions(st *c4v1.WorkJobStatus, v *view, now time.Time) (reason, msg string) {
	m, gen := v.mem, v.job.Generation
	set := func(typ, why, text string, on bool) {
		if !on {
			meta.RemoveStatusCondition(&st.Conditions, typ)
			return
		}
		meta.SetStatusCondition(&st.Conditions, metav1.Condition{Type: typ, Status: metav1.ConditionFalse,
			Reason: why, Message: text, ObservedGeneration: gen})
	}
	admit, lease, image := m.admitWait.live(now), m.leaseWait.live(now), imageWait(v)
	set("Admitted", m.admitWait.reason, m.admitWait.msg, admit)
	set("Lease", "Waiting", m.leaseWait.msg, lease)
	set("Images", "PullFailed", image, image != "")
	switch {
	case admit && m.admitWait.reason == c4v1.NeedsQuota:
		return c4v1.NeedsQuota, m.admitWait.msg
	case admit:
		return c4v1.NeedsPermission, m.admitWait.msg
	case image != "":
		return c4v1.NeedsStuck, image
	case lease:
		return c4v1.NeedsPermission, "its units wait for a credential lease: " + m.leaseWait.msg
	}
	return "", ""
}
