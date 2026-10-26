package controller

import (
	"context"
	"k8s.io/utils/ptr"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/jobspec"
	"github.com/Avalanche-io/c4-operator/work/order"
)

// AgentBundleReconciler keeps one WorkJob of class agent for each
// AgentBundle, with one unit per member, and reports the members from the
// unit pods (docs/agents/c4-agent.md, Bundles on the farm). The queue
// schedules the units like any others; a runner on the member's machine
// resumes the member's session for each attempt.
type AgentBundleReconciler struct {
	client.Client
	// Now is the clock (tests replace it).
	Now func() time.Time
}

func (r *AgentBundleReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// Reconcile brings one bundle's WorkJob in line with its spec and writes
// the bundle's status.
func (r *AgentBundleReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var b c4v1.AgentBundle
	if err := r.Get(ctx, req.NamespacedName, &b); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	job, err := r.ensureJob(ctx, &b)
	if err != nil {
		return ctrl.Result{}, err
	}
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(b.Namespace), client.MatchingLabels{c4v1.LabelJob: job.Name}); err != nil {
		return ctrl.Result{}, err
	}
	orig := b.DeepCopy()
	b.Status = bundleStatus(&b, job, pods.Items)
	b.Status.Clock = advanceClock(orig.Status.Clock, b.Status.Phase, r.now())
	if err := r.Status().Patch(ctx, &b, client.MergeFrom(orig)); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

// JobForBundle is the WorkJob a bundle's spec calls for.
func JobForBundle(b *c4v1.AgentBundle) *c4v1.WorkJob {
	names := make([]string, len(b.Spec.Members))
	for i, m := range b.Spec.Members {
		names[i] = m.Name
	}
	t := true
	j := &c4v1.WorkJob{
		ObjectMeta: metav1.ObjectMeta{Name: b.Name, Namespace: b.Namespace,
			Labels: map[string]string{"c4.avalanche.io/agent-bundle": b.Name},
			OwnerReferences: []metav1.OwnerReference{{APIVersion: c4v1.GroupVersion.String(), Kind: "AgentBundle",
				Name: b.Name, UID: b.UID, Controller: &t, BlockOwnerDeletion: &t}}},
		Spec: c4v1.WorkJobSpec{
			Class:    "agent",
			Params:   map[string]string{"members": strings.Join(names, ","), "bundle": b.Name},
			Deadline: b.Spec.Deadline,
			// The bundle's owner owns its deadline: the farm never suggests
			// one from an ETA, which for open-ended agent work means
			// nothing. With no deadline, the agent class's long turnaround
			// applies, so a research bundle has the most slack on the farm.
			DeadlineMode: "Pinned",
			Pools:        b.Spec.Pools,
			MaxParallel:  b.Spec.MaxRunning,
			Nudge:        c4v1.WorkNudge{Hold: b.Spec.Suspend, HoldStop: b.Spec.Suspend},
			// A member that fails is tried again on another runner, up to the
			// default; a yield is never a failure.
			Retries: ptr.To[int32](2),
			// Restricted like every unit, so a namespace that enforces Pod
			// Security restricted (a local farm's projects) admits it.
			Template: jobspec.Restrict(corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"c4.avalanche.io/agent-bundle": b.Name}},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{
					// A host runner runs the member's own Claude Code session and
					// does not start this container; the image names what runs.
					Name: "member", Image: "c4-agent",
				}}},
			}),
		},
	}
	// The job is the bundle's owner's, so pools with owners and host
	// runners take its members as that person's work. The manager is a
	// delegate of the owner webhook, so the owner it names stays.
	if o := b.Annotations[c4v1.AnnotationOwner]; o != "" {
		j.Annotations = map[string]string{c4v1.AnnotationOwner: o}
		if via := b.Annotations[c4v1.AnnotationSubmittedVia]; via != "" {
			j.Annotations[c4v1.AnnotationSubmittedVia] = via
		}
		if as := b.Annotations[c4v1.AnnotationSubmittedAs]; as != "" {
			// the role the owner made the bundle as
			j.Annotations[c4v1.AnnotationSubmittedAs] = as
		}
		if g, ok := b.Annotations[c4v1.AnnotationOwnerGroups]; ok {
			// the owner's groups, as the bundle recorded them
			j.Annotations[c4v1.AnnotationOwnerGroups] = g
		}
	}
	return j
}

func (r *AgentBundleReconciler) ensureJob(ctx context.Context, b *c4v1.AgentBundle) (*c4v1.WorkJob, error) {
	want := JobForBundle(b)
	var have c4v1.WorkJob
	err := r.Get(ctx, types.NamespacedName{Namespace: b.Namespace, Name: want.Name}, &have)
	if apierrors.IsNotFound(err) {
		if err := r.Create(ctx, want); err != nil {
			return nil, err
		}
		return want, nil
	}
	if err != nil {
		return nil, err
	}
	orig := have.DeepCopy()
	// Members, cap, deadline, pools and the suspension follow the bundle.
	// The template and the rest stay as created: changing a template does
	// not reach units already planned.
	have.Spec.Params, have.Spec.Deadline, have.Spec.Pools = want.Spec.Params, want.Spec.Deadline, want.Spec.Pools
	have.Spec.MaxParallel, have.Spec.Nudge, have.Spec.DeadlineMode = want.Spec.MaxParallel, want.Spec.Nudge, want.Spec.DeadlineMode
	if err := r.Patch(ctx, &have, client.MergeFrom(orig)); err != nil {
		return nil, err
	}
	return &have, nil
}

// bundleStatus derives a bundle's status from its WorkJob and unit pods.
func bundleStatus(b *c4v1.AgentBundle, j *c4v1.WorkJob, pods []corev1.Pod) c4v1.AgentBundleStatus {
	st := c4v1.AgentBundleStatus{ObservedGeneration: b.Generation, Job: j.Name, Conditions: b.Status.Conditions}
	// The newest pod of each member tells its state.
	newest := map[int]*corev1.Pod{}
	for i := range pods {
		p := &pods[i]
		idx, err := strconv.Atoi(p.Labels[c4v1.LabelUnit])
		if err != nil {
			continue
		}
		if q := newest[idx]; q == nil || p.CreationTimestamp.After(q.CreationTimestamp.Time) {
			newest[idx] = p
		}
	}
	resume := j.Status.Resume
	succeeded := map[int]bool{}
	if set, err := order.DecodeSet(j.Status.UnitStates["succeeded"]); err == nil {
		for _, i := range set {
			succeeded[i] = true
		}
	}
	running, yielding := 0, 0
	for idx, m := range b.Spec.Members {
		ms := c4v1.AgentMemberStatus{Name: m.Name, State: "queued", Checkpoint: resume[strconv.Itoa(idx)]}
		switch {
		case succeeded[idx]:
			ms.State = "done"
		case ms.Checkpoint != "":
			// Saved at a turn boundary and not running: parked until the
			// queue releases it again.
			ms.State = "parked"
		}
		if p := newest[idx]; p != nil {
			ms.Node = p.Spec.NodeName
			ms.Epoch = p.Annotations[c4v1.AnnotationOutputEpoch]
			ms.Attempts, _ = parseInt32(p.Annotations[c4v1.AnnotationAttempt])
			if cp := p.Annotations[c4v1.AnnotationOutputCheckpoint]; cp != "" {
				ms.Checkpoint = cp
			}
			switch {
			case p.Status.Phase == corev1.PodFailed:
				ms.State = "failed"
			case p.Status.Phase == corev1.PodSucceeded:
				ms.State = "parked"
			case p.Annotations[c4v1.AnnotationYield] != "" || p.DeletionTimestamp != nil:
				ms.State, yielding = "yielding", yielding+1
			case p.Status.Phase == corev1.PodRunning:
				ms.State, running = "running", running+1
			case p.Status.Phase == corev1.PodPending:
				ms.State = "queued"
			}
		}
		st.Members = append(st.Members, ms)
	}
	saved := 0
	for _, ms := range st.Members {
		if ms.State == "parked" {
			saved++
		}
	}
	switch {
	case b.Spec.Suspend && running+yielding > 0:
		st.Phase = "Holding"
	case b.Spec.Suspend:
		st.Phase = "Held"
	case running+yielding > 0:
		st.Phase = "Running"
	case j.Status.Phase == "Succeeded":
		// every member finished: the bundle follows its job
		st.Phase = "Done"
	case j.Status.Phase == "Failed":
		st.Phase = "Failed"
	case saved > 0:
		// Every member that ran was stopped at a turn boundary by the
		// queue (capacity, a power cap): the bundle is suspended, and its
		// clock stops until a member runs again.
		st.Phase = "Suspended"
	default:
		st.Phase = "Pending"
	}
	// What keeps the job's units from starting keeps the members from
	// starting: the bundle says so, and is not Ready while it holds.
	var blocked *metav1.Condition
	for _, typ := range memberWaits {
		c := meta.FindStatusCondition(j.Status.Conditions, typ)
		if c == nil || c.Status != metav1.ConditionFalse {
			meta.RemoveStatusCondition(&st.Conditions, typ)
			continue
		}
		meta.SetStatusCondition(&st.Conditions, metav1.Condition{Type: typ, Status: metav1.ConditionFalse,
			Reason: c.Reason, Message: c.Message, ObservedGeneration: b.Generation})
		if blocked == nil {
			blocked = c
		}
	}
	if blocked != nil {
		meta.SetStatusCondition(&st.Conditions, metav1.Condition{Type: "Ready", Status: metav1.ConditionFalse,
			Reason: blocked.Type, Message: "its members cannot start: " + blocked.Message, ObservedGeneration: b.Generation})
		return st
	}
	meta.SetStatusCondition(&st.Conditions, metav1.Condition{Type: "Ready", Status: metav1.ConditionTrue,
		Reason: "JobReconciled", Message: "the bundle's WorkJob " + j.Name + " follows its spec", ObservedGeneration: b.Generation})
	return st
}

// memberWaits are the job conditions that keep its units from starting
// (Schedulable, and the queue's waits.go), in the order a bundle names
// them.
var memberWaits = []string{"Schedulable", "Admitted", "Images", "Lease", "Tools"}

// advanceClock moves a bundle's virtual clock to now. The clock starts the
// first time the bundle runs, stops when it becomes Suspended (by the
// queue) or Held (by a person), and resumes, without jumping, when it runs
// again.
func advanceClock(c c4v1.AgentClock, phase string, now time.Time) c4v1.AgentClock {
	if c.Start == nil {
		if phase != "Running" && phase != "Holding" {
			return c
		}
		t := metav1.NewTime(now)
		c.Start = &t
	}
	stopped := phase == "Suspended" || phase == "Held"
	by := "queue"
	if phase == "Held" {
		by = "person"
	}
	switch {
	case stopped && c.SuspendedSince != nil && c.SuspendedBy != by:
		// Held after the queue had stopped it, or the reverse: one gap,
		// whose cause is the latest.
		c.SuspendedBy = by
	case stopped && c.SuspendedSince == nil:
		t := metav1.NewTime(now)
		c.SuspendedSince, c.SuspendedBy = &t, by
	case !stopped && c.SuspendedSince != nil:
		c.SuspendedSeconds += int64(now.Sub(c.SuspendedSince.Time).Seconds())
		c.Suspensions = append(c.Suspensions, c4v1.AgentSuspension{From: *c.SuspendedSince, To: metav1.NewTime(now), By: c.SuspendedBy})
		if n := len(c.Suspensions); n > 16 {
			c.Suspensions = append([]c4v1.AgentSuspension(nil), c.Suspensions[n-16:]...)
		}
		c.SuspendedSince, c.SuspendedBy = nil, ""
	}
	suspended := time.Duration(c.SuspendedSeconds) * time.Second
	if c.SuspendedSince != nil {
		suspended += now.Sub(c.SuspendedSince.Time)
	}
	v := metav1.NewTime(now.Add(-suspended).Truncate(time.Second))
	c.VirtualNow = &v
	c.DriftSeconds = int64(suspended.Seconds())
	return c
}

func parseInt32(s string) (int32, error) {
	n, err := strconv.Atoi(s)
	return int32(n), err
}

// SetupWithManager registers the controller. It also reconciles a bundle
// when its WorkJob or a unit pod changes.
func (r *AgentBundleReconciler) SetupWithManager(mgr ctrl.Manager) error {
	toBundle := handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, o client.Object) []reconcile.Request {
		name := o.GetLabels()["c4.avalanche.io/agent-bundle"]
		if name == "" {
			return nil
		}
		return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: o.GetNamespace(), Name: name}}}
	})
	return ctrl.NewControllerManagedBy(mgr).For(&c4v1.AgentBundle{}).
		Watches(&c4v1.WorkJob{}, toBundle).
		Watches(&corev1.Pod{}, toBundle).
		Complete(r)
}
