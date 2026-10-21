package queue

import (
	"encoding/json"
	"sort"
	"strings"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
)

// Requests a person makes of a job that the queue carries out once each,
// in token order, as trustedControls leaves them (controltrust.go). On a
// farm with signing off, the older plain forms are read too (unsigned is
// true): "TOKEN:SET" for a requeue, a comma-separated node list for allowed
// nodes, "true" for an acceptance.

// requeues are j's requeue requests the queue has not carried out yet.
func requeues(j *c4v1.WorkJob, m *jobMem, unsigned bool) []c4v1.WorkRequeueRequest {
	raw := j.Annotations[c4v1.AnnotationRequeue]
	if raw == "" {
		return nil
	}
	var reqs []c4v1.WorkRequeueRequest
	if err := json.Unmarshal([]byte(raw), &reqs); err != nil {
		token, set, _ := strings.Cut(raw, ":")
		if !unsigned || token == m.requeued {
			return nil
		}
		return []c4v1.WorkRequeueRequest{{Token: token, Units: set}}
	}
	sort.SliceStable(reqs, func(x, y int) bool { return tokenValue(reqs[x].Token) < tokenValue(reqs[y].Token) })
	last := tokenValue(m.requeued)
	out := reqs[:0]
	for _, r := range reqs {
		if tokenValue(r.Token) > last {
			out = append(out, r)
		}
	}
	return out
}

// allowedNodes are the nodes j's new allow-node requests take off its
// blacklist; it marks them carried out.
func allowedNodes(j *c4v1.WorkJob, m *jobMem, unsigned bool) []string {
	raw := j.Annotations[c4v1.AnnotationAllowNodes]
	if raw == "" {
		return nil
	}
	var reqs []c4v1.WorkAllowNodeRequest
	if err := json.Unmarshal([]byte(raw), &reqs); err != nil {
		if !unsigned {
			return nil
		}
		var out []string
		for _, n := range strings.Split(raw, ",") {
			if n = strings.TrimSpace(n); n != "" {
				out = append(out, n)
			}
		}
		return out
	}
	sort.SliceStable(reqs, func(x, y int) bool { return tokenValue(reqs[x].Token) < tokenValue(reqs[y].Token) })
	last := tokenValue(m.allowed)
	var out []string
	for _, r := range reqs {
		if tokenValue(r.Token) > last && r.Node != "" {
			out = append(out, r.Node)
			m.allowed, last = r.Token, tokenValue(r.Token)
		}
	}
	return out
}

// acceptsValidated is whether j accepts a validated recompute of its
// inputs: the newest acceptance request says so.
func acceptsValidated(j *c4v1.WorkJob, unsigned bool) bool {
	raw := j.Annotations[c4v1.AnnotationAcceptValidated]
	var reqs []c4v1.WorkAcceptValidatedRequest
	if err := json.Unmarshal([]byte(raw), &reqs); err != nil {
		return unsigned && raw == "true"
	}
	accept, newest := false, int64(-1)
	for _, r := range reqs {
		if t := tokenValue(r.Token); t > newest {
			accept, newest = r.Accept, t
		}
	}
	return accept
}
