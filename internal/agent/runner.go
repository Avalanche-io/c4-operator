package agent

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	coordv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
)

// TaintHostRunner keeps ordinary pods off a host runner's node; the agent
// pool's tolerations admit member pods.
const TaintHostRunner = "c4.avalanche.io/host-runner"

// Runner is the host runner (FIRST-SLICE step 2): it runs the members of
// agent bundles as the machine's own Claude Code sessions, under the
// holder's own sign-in, and plays the kubelet's part for their unit pods.
//
// It registers a Node for the machine, binds member pods that select it,
// resumes each member's session by its session ID when its pod starts,
// yields it at a turn boundary when the queue asks, and reports the pod's
// phase. The queue schedules members exactly as it schedules any unit
// that checkpoints. After a restart of the machine, pods that were running
// find their sessions stopped, and the runner resumes them: the restart of
// 2026-10-18, done by the farm.
type Runner struct {
	Client  client.Client
	Harness *Harness
	// Node is the name of the machine's Node.
	Node string
	// Labels are put on the Node; the agent pool's nodeSelector matches
	// them.
	Labels map[string]string
	// Owners are the users whose work the runner runs: a pod runs here only
	// if the WorkJob that controls it is owned by one of them (owner.go).
	// With none, it runs nothing.
	Owners []string
	// Slots is how many members may run here at once.
	Slots int
	// Capacity, when set, lowers the slots the node offers below Slots,
	// for example from the machine's power headroom; reason says why in
	// plain words. The runner writes it as the node's c4.avalanche.io/slot-limit
	// label, and the queue sheds or holds work to fit. It never raises the
	// node above Slots.
	Capacity func() (slots int, reason string)
	// RaiseAfter is how long a higher capacity must hold before the node
	// offers it (default 60 s), so a reading near a threshold never flaps
	// the node. A lower capacity applies at once: over a power line, the
	// machine sheds work now.
	RaiseAfter time.Duration
	// YieldTimeout bounds one wait for a busy member to finish its turn;
	// the runner tries again on the next tick.
	YieldTimeout time.Duration
	// HardStopAfter, when set, stops a member that has not reached a turn
	// boundary this long after it was asked to yield, losing its turn in
	// flight. Zero waits for the boundary however long it takes (the queue
	// still stops a unit that has not yielded within ten minutes).
	HardStopAfter time.Duration
	// Now is the clock.
	Now func() time.Time
	// Logf reports what the runner does.
	Logf func(format string, args ...any)
	// BlocksCwd is the folder block sessions run in: one the claude CLI
	// trusts (a background session refuses another); their work is in the
	// job's repository (C4_BLOCK_REPO). HookCommand is the Stop hook their
	// sessions run (c4-agent checkpoint --hook, with its environment).
	BlocksCwd, HookCommand string
	// UnitBin is c4-unit, for the waits the runner opens for a block
	// (default c4-unit on the PATH).
	UnitBin string
	// UnitsDir holds each running unit's directory (default the agents
	// home's units/): its output root, control directory and podinfo.
	UnitsDir string
	// Docs, when set, stores a generator member's expansion document so
	// the queue can read it (c4.avalanche.io/output-expand); DocsProject
	// names the project to store it in, else the runner reads the Project
	// that lists the pod's namespace.
	Docs        DocStore
	DocsProject string
	// Views and ViewSrc store and read units' output views (a regional
	// replica): a block's session leaves the host as its yield's
	// output-tree and comes back as its next attempt's C4_RESUME_VIEW, so it
	// can move between this host and a unit pod.
	Views   ViewStore
	ViewSrc ViewSource

	seen map[types.UID]bool
	// owned is why each pod may not run here ("" if it may).
	owned map[types.UID]string
	// refused are the pods whose refusal was logged.
	refused map[types.UID]bool
	// relays is each pod's safe-point relay.
	relays map[types.UID]*relayState
	// parts are blocks' next parts, by pod, to wake them for; since is
	// when a block's session was last woken in its attempt
	parts map[types.UID]string
	// fetched are the pods whose resume view was fetched here
	fetched map[types.UID]bool
	// waited are the pods whose block turned its hold for an answer into a
	// wait on the answerer
	waited map[types.UID]bool
	since  map[types.UID]time.Time
	// asked is when the runner first saw each pod's yield request.
	asked map[types.UID]time.Time
	// rising is a higher capacity waiting out RaiseAfter, and since when.
	rising      int
	risingSince time.Time
}

func (r *Runner) logf(format string, args ...any) {
	if r.Logf != nil {
		r.Logf(format, args...)
	}
}

func (r *Runner) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// Run ticks until ctx ends.
func (r *Runner) Run(ctx context.Context, every time.Duration) error {
	for {
		if err := r.Tick(ctx); err != nil {
			r.logf("tick: %v", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(every):
		}
	}
}

// Tick does one round: the node's heartbeat, binding, and each pod.
func (r *Runner) Tick(ctx context.Context) error {
	if r.seen == nil {
		r.seen = map[types.UID]bool{}
		r.asked = map[types.UID]time.Time{}
		r.relays = map[types.UID]*relayState{}
		r.parts = map[types.UID]string{}
		r.fetched = map[types.UID]bool{}
		r.waited = map[types.UID]bool{}
		r.since = map[types.UID]time.Time{}
	}
	if r.owned == nil {
		r.owned, r.refused = map[types.UID]string{}, map[types.UID]bool{}
	}
	if err := r.heartbeat(ctx); err != nil {
		return fmt.Errorf("node %s: %w", r.Node, err)
	}
	var pods corev1.PodList
	if err := r.Client.List(ctx, &pods, client.HasLabels{c4v1.LabelJob}); err != nil {
		return err
	}
	sel := labels.SelectorFromSet(r.Labels)
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.Spec.NodeName == "" && p.Status.Phase == corev1.PodPending && p.DeletionTimestamp == nil &&
			sel.Matches(labels.Set(p.Spec.NodeSelector)) && memberOf(p) != "" {
			why, err := r.refuses(ctx, p)
			if err != nil {
				r.logf("%s/%s: %v", p.Namespace, p.Name, err)
				continue
			}
			if why != "" {
				if !r.refused[p.UID] {
					r.refused[p.UID] = true
					r.logf("not binding %s/%s: %s", p.Namespace, p.Name, why)
				}
				continue
			}
			if err := r.bind(ctx, p); err != nil {
				r.logf("bind %s/%s: %v", p.Namespace, p.Name, err)
			}
			continue
		}
		if p.Spec.NodeName != r.Node {
			continue
		}
		if p.Status.Phase != corev1.PodSucceeded && p.Status.Phase != corev1.PodFailed {
			why, err := r.refuses(ctx, p)
			if err != nil {
				r.logf("%s/%s: %v", p.Namespace, p.Name, err)
				continue
			}
			if why != "" {
				// bound here by someone else: never run it
				r.logf("refusing %s/%s: %s", p.Namespace, p.Name, why)
				if err := r.finish(ctx, p, corev1.PodFailed, "NotTheOwner", why, 1); err != nil {
					r.logf("%s/%s: %v", p.Namespace, p.Name, err)
				}
				continue
			}
		}
		if err := r.handle(ctx, p); err != nil {
			r.logf("%s/%s: %v", p.Namespace, p.Name, err)
		}
	}
	return nil
}

// memberOf is the member a unit pod runs, from its environment: the one
// it names, or a block's own.
func memberOf(p *corev1.Pod) string {
	for _, c := range p.Spec.Containers {
		for _, e := range c.Env {
			if e.Name == "C4_AGENT_MEMBER" {
				return e.Value
			}
		}
	}
	if isBlock(p) {
		return blockMember(p)
	}
	return ""
}

func (r *Runner) bind(ctx context.Context, p *corev1.Pod) error {
	b := &corev1.Binding{ObjectMeta: metav1.ObjectMeta{Name: p.Name, Namespace: p.Namespace},
		Target: corev1.ObjectReference{Kind: "Node", Name: r.Node}}
	if err := r.Client.SubResource("binding").Create(ctx, p, b); err != nil {
		return err
	}
	r.logf("bound %s/%s (member %s)", p.Namespace, p.Name, memberOf(p))
	return nil
}

func (r *Runner) handle(ctx context.Context, p *corev1.Pod) error {
	first := !r.seen[p.UID]
	r.seen[p.UID] = true
	if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
		return nil
	}
	name := memberOf(p)
	if name == "" {
		return r.finish(ctx, p, corev1.PodFailed, "NotAMember", "the pod names no agent member (C4_AGENT_MEMBER)", 1)
	}
	if err := r.resumeFromView(ctx, p, name); err != nil {
		return err
	}
	m, err := r.Harness.Store.Get(name)
	if errors.Is(err, ErrNoMember) && isBlock(p) && p.Status.Phase == corev1.PodPending && p.DeletionTimestamp == nil {
		return r.startBlock(ctx, p, name)
	}
	if errors.Is(err, ErrNoMember) && isBlock(p) && p.DeletionTimestamp != nil {
		// a block withdrawn before its session started: let it go
		zero := int64(0)
		return client.IgnoreNotFound(r.Client.Delete(ctx, p, &client.DeleteOptions{GracePeriodSeconds: &zero}))
	}
	if errors.Is(err, ErrNoMember) {
		return r.finish(ctx, p, corev1.PodFailed, "MemberNotRegistered",
			fmt.Sprintf("member %s is not registered with c4-agent on %s", name, r.Node), 1)
	}
	if err != nil {
		return err
	}
	if p.DeletionTimestamp != nil {
		return r.deleted(ctx, p, m)
	}
	u, err := r.prepareUnit(p, m)
	if err != nil {
		return err
	}
	if p.Annotations[c4v1.AnnotationYield] != "" {
		if err := r.relay(ctx, p, u); err != nil {
			r.logf("%s: safe points: %v", m.Name, err)
		}
		if !m.Block {
			return r.yield(ctx, p, m)
		}
		if wait, err := r.blockYield(ctx, p, m); wait || err != nil {
			return err
		}
		// the farm answered continue at the block's safe point: go on
	}
	live, act, err := r.Harness.live(ctx, m)
	if err != nil {
		return err
	}
	if live {
		if err := r.relay(ctx, p, u); err != nil {
			r.logf("%s: safe points: %v", m.Name, err)
		}
	}
	// a re-check past its time: stop it there, still needed
	if live && m.Block && recheck(p) && p.Status.Phase == corev1.PodRunning && p.Status.StartTime != nil &&
		r.now().Sub(p.Status.StartTime.Time) > recheckLimit(p) {
		if err := r.Harness.Claude.Stop(ctx, m.ConfigDir, m.Session); err != nil {
			return err
		}
		r.logf("%s: its re-check ran past %s: still needed", m.Name, recheckLimit(p))
		return r.needsInformation(ctx, p, m, neededText(p))
	}
	// A block's turn ends at a safe point: stop its idle session. If the
	// farm said stop there, the pod yields below. A part done with more to
	// do (.c4/more) wakes it again for the next part, in this attempt. A
	// task it finished ends the pod below as a member whose session ended
	// on its own; one it gave up on (the CLI reports it blocked, not done)
	// fails the attempt, for the job's retries or a person. (The CLI's
	// state at a turn's end says how the model left its task: done,
	// blocked, or working when it ended a part with more to come.)
	if live && m.Block && p.Status.Phase == corev1.PodRunning && act.Idle() && r.turnSince(m, p) {
		if err := r.Harness.Claude.Stop(ctx, m.ConfigDir, m.Session); err != nil {
			return err
		}
		live = false
		// a re-check decides only: withdrawn, or still needed
		if recheck(p) {
			return r.recheckDone(ctx, p, m, u)
		}
		// missing information first: it stops the block whatever the farm
		// said at the safe point
		if msg, ok, err := takeNeeds(u); ok {
			if err != nil {
				return r.finish(ctx, p, corev1.PodFailed, "BlockGaveUp", "the block reported missing information, but "+err.Error(), 1)
			}
			return r.needsInformation(ctx, p, m, msg)
		}
		stopped := r.relays[p.UID] != nil && r.relays[p.UID].stopped
		_, asking := ReadAsking(u.Out)
		switch note, more := takeMore(u); {
		case answerOnly(p):
			// a wake to answer: its task was already done
			r.logf("%s: answered what it was asked; stopped its session", m.Name)
			return r.finish(ctx, p, corev1.PodSucceeded, "Completed", "the block answered the questions it was woken for", 0)
		case asking && !stopped:
			// it asked another unit: it keeps its slot for the answer
			r.logf("%s: asked a question; holds for the answer", m.Name)
		case stopped:
			// its session knows the part it noted; the next attempt resumes it
			_ = note
			r.logf("%s: the farm said stop at this safe point; stopped its session", m.Name)
		case more:
			r.parts[p.UID] = note
			r.logf("%s: a part is done; next: %s", m.Name, note)
		case !act.Done() && !takeDone(u):
			r.logf("%s: ended its turn without finishing its task (%s); stopped its session", m.Name, act.State)
			return r.finish(ctx, p, corev1.PodFailed, "BlockGaveUp",
				fmt.Sprintf("the block ended its turn without finishing its task or noting a next part (the CLI reports it %s); its transcript says why", act.State), 1)
		default:
			r.logf("%s: its task is done; stopped its session", m.Name)
		}
	}
	// a block holding for the answer to its question
	if a, ok := ReadAsking(u.Out); ok && !live && m.Block && p.Status.Phase == corev1.PodRunning && r.parts[p.UID] == "" && !(r.relays[p.UID] != nil && r.relays[p.UID].stopped) {
		return r.holdForAnswer(ctx, p, m, u, a)
	}
	// the next part, in this attempt: wake the block's session by its ID
	if !live && m.Block && p.Status.Phase == corev1.PodRunning && r.parts[p.UID] != "" && !(r.relays[p.UID] != nil && r.relays[p.UID].stopped) {
		note := r.parts[p.UID]
		if _, err := r.Harness.Resume(ctx, name, partPrompt(note, u)); err != nil {
			return err
		}
		delete(r.parts, p.UID)
		r.since[p.UID] = r.now()
		r.logf("%s: woke for its next part", m.Name)
		return nil
	}
	switch {
	case p.Status.Phase == corev1.PodPending:
		if !live {
			prompt := ""
			if m.Block {
				if err := r.fetchReply(ctx, p, u); err != nil {
					return err
				}
				prompt = reworkPrompt(p, u)
			}
			res, err := r.Harness.Resume(ctx, name, prompt)
			if errors.Is(err, ErrForked) {
				return r.finish(ctx, p, corev1.PodFailed, "CopyStarted", err.Error(), 4)
			}
			if err != nil {
				return err
			}
			m = res.Member
			r.logf("resumed %s (session %s, epoch %d)", m.Name, m.Session, m.Epoch)
		}
		return r.running(ctx, p, m)
	case !live && first:
		// The runner restarted and finds the member's session stopped: the
		// machine was restarted. Resume it from its last turn.
		prompt := ""
		if m.Block {
			if err := r.fetchReply(ctx, p, u); err != nil {
				return err
			}
			prompt = reworkPrompt(p, u)
		}
		res, err := r.Harness.Resume(ctx, name, prompt)
		if errors.Is(err, ErrForked) {
			return r.finish(ctx, p, corev1.PodFailed, "CopyStarted", err.Error(), 4)
		}
		if err != nil {
			return err
		}
		r.logf("recovered %s after a restart (epoch %d)", name, res.Member.Epoch)
		return r.annotateEpoch(ctx, p, res.Member)
	case !live:
		cp, err := r.Harness.Store.Last(name)
		if err != nil {
			return err
		}
		if cp != nil {
			if err := r.annotate(ctx, p, map[string]string{c4v1.AnnotationOutputCheckpoint: point(m, cp)}); err != nil {
				return err
			}
		}
		// The session ended after the farm said stop at a safe point: the
		// member yielded there, with its turn saved, and resumes later.
		if st := r.relays[p.UID]; st != nil && st.stopped {
			r.logf("%s stopped at a safe point (%s)", m.Name, st.why)
			if err := r.saveSession(ctx, p, m); err != nil {
				return err
			}
			return r.finish(ctx, p, corev1.PodSucceeded, "Yielded", "stopped at a safe point: "+st.why, 0)
		}
		// The session ended on its own: the member finished. A generator
		// member leaves its expansion document for the queue, a review
		// member its verdict.
		if err := r.storeControlDocs(ctx, p, m, u); err != nil {
			return err
		}
		return r.finish(ctx, p, corev1.PodSucceeded, "Completed", "the member's session ended", 0)
	}
	return nil
}

func point(m *Member, cp *Checkpoint) string {
	return m.Session + "#" + strconv.Itoa(cp.Seq) + ":" + cp.Head
}

func (r *Runner) yield(ctx context.Context, p *corev1.Pod, m *Member) error {
	why := p.Annotations[c4v1.AnnotationYield]
	if why == "" {
		why = "the pod was deleted"
	}
	if _, ok := r.asked[p.UID]; !ok {
		r.asked[p.UID] = r.now()
	}
	res, err := r.Harness.Yield(ctx, m.Name, r.YieldTimeout)
	if errors.Is(err, ErrBusy) {
		if r.HardStopAfter > 0 && r.now().Sub(r.asked[p.UID]) >= r.HardStopAfter {
			cp, err := r.Harness.ForceStop(ctx, m.Name)
			if err != nil {
				return err
			}
			if cp != nil {
				if err := r.annotate(ctx, p, map[string]string{c4v1.AnnotationOutputCheckpoint: point(m, cp)}); err != nil {
					return err
				}
			}
			r.logf("stopped %s mid-turn after %s waiting for a turn boundary (%s)", m.Name, r.HardStopAfter, why)
			if err := r.markDrained(ctx, p); err != nil {
				return err
			}
			return r.finish(ctx, p, corev1.PodSucceeded, "StoppedMidTurn",
				"stopped mid-turn after waiting "+r.HardStopAfter.String()+" for a turn boundary: "+why, 0)
		}
		r.logf("%s still busy; will try again", m.Name)
		return nil
	}
	if err != nil {
		return err
	}
	cp := res.Checkpoint
	if cp == nil {
		if cp, err = r.Harness.Store.Last(m.Name); err != nil {
			return err
		}
	}
	if cp != nil {
		if err := r.annotate(ctx, p, map[string]string{c4v1.AnnotationOutputCheckpoint: point(m, cp)}); err != nil {
			return err
		}
	}
	r.logf("yielded %s (%s)", m.Name, why)
	if err := r.saveSession(ctx, p, m); err != nil {
		return err
	}
	if err := r.markDrained(ctx, p); err != nil {
		return err
	}
	return r.finish(ctx, p, corev1.PodSucceeded, "Yielded", "yielded at a turn boundary: "+why, 0)
}

// yieldDisruption is the queue's mark on a drained pod (work/queue's
// yieldDrain): it reads a pod that ends with it as yielded, not done.
const yieldDisruption = "disruption"

// markDrained marks a pod the farm deleted, and that yields before the
// farm answered a safe point (a member idle between parts stops at once),
// the way the queue marks a drain, so its end is a yield: the next attempt
// resumes it. The host policy allows this mark only on a deleting pod.
func (r *Runner) markDrained(ctx context.Context, p *corev1.Pod) error {
	if p.DeletionTimestamp == nil || p.Annotations[c4v1.AnnotationYield] != "" {
		return nil
	}
	return r.annotate(ctx, p, map[string]string{c4v1.AnnotationYield: yieldDisruption})
}

// deleted handles a pod the queue deleted without a yield (a disruption,
// a hard stop): the member stops at a turn boundary, its pod is given its
// phase (Yielded with its checkpoint, as a yield is), and only then is the
// pod removed. The unit-pod finalizer keeps a deleting pod until the queue
// has recorded how it ended, so while the member is still mid-turn the
// runner tries again next tick; the farm's stop also reaches it at its
// next safe point (the decision relay).
func (r *Runner) deleted(ctx context.Context, p *corev1.Pod, m *Member) error {
	if p.Status.Phase != corev1.PodSucceeded && p.Status.Phase != corev1.PodFailed {
		if m.Block {
			// a block yields at its next safe point, where the farm
			// answers stop for a pod being deleted (its grace is the wait)
			u, err := r.prepareUnit(p, m)
			if err != nil {
				return err
			}
			if err := r.relay(ctx, p, u); err != nil {
				r.logf("%s: safe points: %v", m.Name, err)
			}
			if wait, err := r.blockYield(ctx, p, m); err != nil {
				return err
			} else if !wait {
				if err := r.yield(ctx, p, m); err != nil {
					return err
				}
			}
		} else if err := r.yield(ctx, p, m); err != nil {
			return err
		}
		if p.Status.Phase != corev1.PodSucceeded && p.Status.Phase != corev1.PodFailed {
			return nil // still mid-turn
		}
	}
	zero := int64(0)
	err := r.Client.Delete(ctx, p, &client.DeleteOptions{GracePeriodSeconds: &zero})
	return client.IgnoreNotFound(err)
}

func (r *Runner) annotateEpoch(ctx context.Context, p *corev1.Pod, m *Member) error {
	return r.annotate(ctx, p, map[string]string{c4v1.AnnotationOutputEpoch: strconv.Itoa(m.Epoch)})
}

func (r *Runner) annotate(ctx context.Context, p *corev1.Pod, ann map[string]string) error {
	orig := p.DeepCopy()
	if p.Annotations == nil {
		p.Annotations = map[string]string{}
	}
	for k, v := range ann {
		p.Annotations[k] = v
	}
	return r.Client.Patch(ctx, p, client.MergeFrom(orig))
}

func (r *Runner) running(ctx context.Context, p *corev1.Pod, m *Member) error {
	if err := r.annotateEpoch(ctx, p, m); err != nil {
		return err
	}
	now := metav1.NewTime(r.now())
	orig := p.DeepCopy()
	p.Status.Phase = corev1.PodRunning
	p.Status.StartTime = &now
	p.Status.Conditions = podConditions(corev1.ConditionTrue, now)
	p.Status.ContainerStatuses = containerStatuses(p, corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: now}}, true)
	return r.Client.Status().Patch(ctx, p, client.MergeFrom(orig))
}

func (r *Runner) finish(ctx context.Context, p *corev1.Pod, phase corev1.PodPhase, reason, msg string, code int32) error {
	now := metav1.NewTime(r.now())
	started := now
	if p.Status.StartTime != nil {
		started = *p.Status.StartTime
	}
	orig := p.DeepCopy()
	p.Status.Phase, p.Status.Reason, p.Status.Message = phase, reason, msg
	if p.Status.StartTime == nil {
		p.Status.StartTime = &now
	}
	p.Status.Conditions = podConditions(corev1.ConditionFalse, now)
	p.Status.ContainerStatuses = containerStatuses(p, corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
		ExitCode: code, Reason: reason, Message: msg, StartedAt: started, FinishedAt: now}}, false)
	return r.Client.Status().Patch(ctx, p, client.MergeFrom(orig))
}

func podConditions(ready corev1.ConditionStatus, at metav1.Time) []corev1.PodCondition {
	return []corev1.PodCondition{
		{Type: corev1.PodScheduled, Status: corev1.ConditionTrue, LastTransitionTime: at},
		{Type: corev1.PodInitialized, Status: corev1.ConditionTrue, LastTransitionTime: at},
		{Type: corev1.ContainersReady, Status: ready, LastTransitionTime: at},
		{Type: corev1.PodReady, Status: ready, LastTransitionTime: at},
	}
}

func containerStatuses(p *corev1.Pod, state corev1.ContainerState, ready bool) []corev1.ContainerStatus {
	var out []corev1.ContainerStatus
	for _, c := range p.Spec.Containers {
		out = append(out, corev1.ContainerStatus{Name: c.Name, Image: c.Image, ImageID: "host-runner://" + c.Image,
			Ready: ready, State: state})
	}
	return out
}

// heartbeat creates or refreshes the machine's Node and its Lease, so the
// cluster sees it Ready and keeps scheduling to it.
func (r *Runner) heartbeat(ctx context.Context) error {
	now := metav1.NewTime(r.now())
	var n corev1.Node
	err := r.Client.Get(ctx, types.NamespacedName{Name: r.Node}, &n)
	if apierrors.IsNotFound(err) {
		n = corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: r.Node, Labels: map[string]string{}},
			Spec: corev1.NodeSpec{Taints: []corev1.Taint{{Key: TaintHostRunner, Value: "true", Effect: corev1.TaintEffectNoSchedule}}}}
		for k, v := range r.Labels {
			n.Labels[k] = v
		}
		n.Labels["kubernetes.io/hostname"] = r.Node
		if err := r.Client.Create(ctx, &n); err != nil {
			return err
		}
		r.logf("registered node %s", r.Node)
	} else if err != nil {
		return err
	}
	if err := r.capacity(ctx, &n); err != nil {
		return err
	}
	orig := n.DeepCopy()
	if n.Status.Capacity == nil {
		n.Status.Capacity = corev1.ResourceList{}
	}
	if n.Status.Allocatable == nil {
		n.Status.Allocatable = corev1.ResourceList{}
	}
	pods := resource.MustParse(strconv.Itoa(max(r.Slots, 1) * 4))
	n.Status.Capacity[corev1.ResourcePods], n.Status.Allocatable[corev1.ResourcePods] = pods, pods
	n.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue,
		Reason: "HostRunnerReady", Message: "c4-agent runner is up", LastHeartbeatTime: now, LastTransitionTime: now}}
	if err := r.Client.Status().Patch(ctx, &n, client.MergeFrom(orig)); err != nil {
		return err
	}
	return r.renewLease(ctx, now)
}

// capacity writes the node's slot label and its reason from Capacity.
func (r *Runner) capacity(ctx context.Context, n *corev1.Node) error {
	slots, reason := r.Slots, ""
	if r.Capacity != nil {
		slots, reason = r.Capacity()
		slots = max(0, min(slots, r.Slots))
	}
	if have, err := strconv.Atoi(n.Labels[c4v1.LabelSlotLimit]); err == nil && slots > have {
		// Raise only once the higher value has held for RaiseAfter.
		hold := r.RaiseAfter
		if hold == 0 {
			hold = time.Minute
		}
		if r.rising != slots {
			r.rising, r.risingSince = slots, r.now()
		}
		if r.now().Sub(r.risingSince) < hold {
			slots = have
			reason = n.Annotations[c4v1.AnnotationCapacityReason]
		}
	} else {
		r.rising = 0
	}
	want := strconv.Itoa(slots)
	if n.Labels[c4v1.LabelSlotLimit] == want && n.Annotations[c4v1.AnnotationCapacityReason] == reason {
		return nil
	}
	orig := n.DeepCopy()
	if n.Labels == nil {
		n.Labels = map[string]string{}
	}
	if n.Annotations == nil {
		n.Annotations = map[string]string{}
	}
	n.Labels[c4v1.LabelSlotLimit] = want
	if reason == "" {
		delete(n.Annotations, c4v1.AnnotationCapacityReason)
	} else {
		n.Annotations[c4v1.AnnotationCapacityReason] = reason
	}
	if err := r.Client.Patch(ctx, n, client.MergeFrom(orig)); err != nil {
		return err
	}
	if before := orig.Labels[c4v1.LabelSlotLimit]; before != want {
		r.logf("capacity %s -> %s slots %s", before, want, reason)
	}
	return nil
}

func (r *Runner) renewLease(ctx context.Context, now metav1.Time) error {
	micro := metav1.NewMicroTime(now.Time)
	secs := int32(40)
	var l coordv1.Lease
	err := r.Client.Get(ctx, types.NamespacedName{Namespace: corev1.NamespaceNodeLease, Name: r.Node}, &l)
	if apierrors.IsNotFound(err) {
		l = coordv1.Lease{ObjectMeta: metav1.ObjectMeta{Namespace: corev1.NamespaceNodeLease, Name: r.Node},
			Spec: coordv1.LeaseSpec{HolderIdentity: &r.Node, LeaseDurationSeconds: &secs, RenewTime: &micro}}
		err = r.Client.Create(ctx, &l)
		if apierrors.IsNotFound(err) {
			return nil // no node-lease namespace (a bare API server, as in tests)
		}
		return err
	}
	if err != nil {
		return err
	}
	orig := l.DeepCopy()
	l.Spec.RenewTime = &micro
	return r.Client.Patch(ctx, &l, client.MergeFrom(orig))
}
