package queue

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/work/sched"
)

// capacity writes the policy's burst decisions as CapacityRequests: one
// per burst pool holding the total slots wanted (named after the pool),
// and one per suggestion that needs a person's approval. An approved
// suggestion is added to its pool's request once, then removed.
func (q *Queue) capacity(ctx context.Context, now time.Time, dec sched.Decisions, pools map[string]*pool, crs []c4v1.CapacityRequest) {
	byName := map[string]*c4v1.CapacityRequest{}
	for i := range crs {
		byName[crs[i].Name] = &crs[i]
	}
	auto := map[string][]sched.BurstSuggestion{}
	for _, b := range dec.Bursts {
		if !b.Auto {
			continue
		}
		for pool := range b.Slots {
			auto[pool] = append(auto[pool], b)
		}
	}
	for _, c := range dec.Capacity {
		p := pools[c.Pool]
		if p == nil || p.spec.Spec.Burst == nil {
			continue
		}
		slots, reason := c.Slots, c.Reason
		for name, cr := range byName {
			if cr.Spec.Pool != c.Pool || !strings.HasPrefix(name, c.Pool+"-approve-") || !cr.Spec.Approved {
				continue
			}
			slots += int(cr.Spec.Slots)
			reason = fmt.Sprintf("+%d approved for %s", cr.Spec.Slots, strings.Join(cr.Spec.Jobs, ", "))
			if err := q.Client.Delete(ctx, cr); err != nil && !apierrors.IsNotFound(err) {
				q.Log.Error(err, "fold approved capacity request", "name", name)
			}
		}
		slots = min(slots, int(p.spec.Spec.Burst.MaxNodes)*p.perNode)
		spec := c4v1.CapacityRequestSpec{Pool: c.Pool, Slots: int32(slots), Reason: reason, Approved: true}
		cost := 0.0
		var until time.Time
		for _, b := range auto[c.Pool] {
			spec.Jobs = append(spec.Jobs, b.Job)
			cost += b.Cost
		}
		if old := byName[c.Pool]; old != nil && len(spec.Jobs) == 0 && slots > 0 {
			// Keep naming the jobs the capacity was requested for while it
			// is still held.
			spec.Jobs, spec.EstimatedCost = old.Spec.Jobs, old.Spec.EstimatedCost
		} else if cost > 0 {
			spec.EstimatedCost = fmt.Sprintf("%.2f", cost)
		}
		for _, j := range spec.Jobs {
			if st, ok := dec.Jobs[j]; ok && st.ETA90.After(until) {
				until = st.ETA90
			}
		}
		if !until.IsZero() {
			spec.Until = &metav1.Time{Time: until.Truncate(time.Second)}
		}
		q.upsert(ctx, byName[c.Pool], c.Pool, spec)
	}
	wanted := map[string]bool{}
	for _, b := range dec.Bursts {
		if b.Auto || !b.Closes {
			continue
		}
		why := b.Why
		if b.Margin {
			why = "margin against estimate uncertainty; " + why
		}
		for pool, n := range b.Slots {
			name := pool + "-approve-" + shortHash(b.Job)
			wanted[name] = true
			spec := c4v1.CapacityRequestSpec{Pool: pool, Slots: int32(n), Reason: why, Jobs: []string{b.Job},
				EstimatedCost: fmt.Sprintf("%.2f", b.Cost)}
			if old := byName[name]; old != nil {
				spec.Approved = old.Spec.Approved
			}
			q.upsert(ctx, byName[name], name, spec)
		}
	}
	for name, cr := range byName {
		if strings.Contains(name, "-approve-") && !wanted[name] && !cr.Spec.Approved {
			q.Client.Delete(ctx, cr)
		}
	}
}

func (q *Queue) upsert(ctx context.Context, old *c4v1.CapacityRequest, name string, spec c4v1.CapacityRequestSpec) {
	sort.Strings(spec.Jobs)
	if old == nil {
		cr := &c4v1.CapacityRequest{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: spec}
		if err := q.Client.Create(ctx, cr); err != nil && !apierrors.IsAlreadyExists(err) {
			q.Log.Error(err, "create capacity request", "name", name)
			return
		}
		cr.Status.State = "Provisioning"
		if !spec.Approved {
			cr.Status.State = "PendingApproval"
		}
		q.Client.Status().Update(ctx, cr)
		return
	}
	if equalSpec(old.Spec, spec) {
		return
	}
	patch := client.MergeFrom(old.DeepCopy())
	old.Spec = spec
	if err := q.Client.Patch(ctx, old, patch); err != nil {
		q.Log.Error(err, "update capacity request", "name", name)
	}
}

func equalSpec(a, b c4v1.CapacityRequestSpec) bool {
	if a.Pool != b.Pool || a.Slots != b.Slots || a.Reason != b.Reason || a.Approved != b.Approved ||
		a.EstimatedCost != b.EstimatedCost || strings.Join(a.Jobs, ",") != strings.Join(b.Jobs, ",") {
		return false
	}
	if (a.Until == nil) != (b.Until == nil) {
		return false
	}
	return a.Until == nil || a.Until.Equal(b.Until)
}

func shortHash(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:5])
}
