package queue

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/needs"
	"github.com/Avalanche-io/c4-operator/internal/provenance"
)

const (
	actsJob    = "9c1d2e7f-0000-4000-8000-000000000001"
	actsPodUID = "5e1f0c2a-0000-4000-8000-0000000000a3"
)

// unitActsFarm is a job owned by dana with unit 3's pod on its second
// attempt, a queue that checks against the farm's roots, and a signer for
// any unit identity.
func unitActsFarm(t *testing.T) (*Queue, *view, *corev1.Pod, func(identity string, a provenance.UnitAct) json.RawMessage) {
	t.Helper()
	now := time.Now()
	ca, err := provenance.NewCA(now)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca.Cert)
	scheme := runtime.NewScheme()
	clientgoscheme.AddToScheme(scheme)
	c4v1.AddToScheme(scheme)
	q := &Queue{Client: fake.NewClientBuilder().WithScheme(scheme).Build(), Log: zap.New(zap.WriteTo(&bytes.Buffer{})), Roots: roots}
	j := &c4v1.WorkJob{ObjectMeta: metav1.ObjectMeta{Namespace: "show-a", Name: "sh040-comp", UID: actsJob,
		Annotations: map[string]string{c4v1.AnnotationOwner: "dana"}}}
	yes := true
	p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "show-a", Name: "sh040-comp-u3", UID: actsPodUID,
		Labels:      map[string]string{c4v1.LabelJob: "sh040-comp", c4v1.LabelUnit: "3"},
		Annotations: map[string]string{c4v1.AnnotationAttempt: "2", c4v1.AnnotationUnitSpec: strings.Repeat("ab", 32)},
		OwnerReferences: []metav1.OwnerReference{{APIVersion: c4v1.GroupVersion.String(), Kind: "WorkJob", Name: "sh040-comp",
			UID: actsJob, Controller: &yes}}}}
	v := &view{job: j, key: "show-a/sh040-comp", mem: newMem(), project: "show-a"}
	sign := func(identity string, a provenance.UnitAct) json.RawMessage {
		t.Helper()
		s, err := provenance.NewSigner(context.Background(), provenance.LocalIssuer(ca, identity, time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		if a.Subject.Name == "" {
			a.Subject = provenance.ActSubject("pod", "show-a", "sh040-comp-u3", actsPodUID, strings.Repeat("ab", 32))
		}
		if a.Owner == "" {
			a.Owner = "dana"
		}
		b, err := provenance.SignUnitAct(context.Background(), s, a, now)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	return q, v, p, sign
}

func setActs(p *corev1.Pod, acts ...json.RawMessage) {
	b, _ := json.Marshal(acts)
	p.Annotations[c4v1.AnnotationUnitActs] = string(b)
}

// A unit's control is carried out signed when its act holds, unsigned
// when it has none, and not at all when its act is there and doesn't
// hold: another attempt's, another unit's, for someone else, on another
// pod, or saying something else. Each refusal is reported on the job.
func TestUnitControlsAgainstTheirActs(t *testing.T) {
	ctx, now := context.Background(), time.Now()
	q, v, p, sign := unitActsFarm(t)
	me := provenance.UnitIdentity("show-a", "sh040-comp", "3", actsJob, 2)
	ask := map[string]any{"seq": 1, "to": "unit:plan", "ask": "Which module owns the tokenizer?"}
	askAct := func(identity string, a provenance.UnitAct) json.RawMessage {
		a.Act = "ask"
		if a.Params == nil {
			a.Params = json.RawMessage(`{"seq":1,"to":"unit:plan","ask":"Which module owns the tokenizer?"}`)
		}
		return sign(identity, a)
	}

	// none: on the node's word, booked so
	r, err := q.unitControl(ctx, v, p, "ask", bySeq(1), ask, now)
	if err != nil || r["custody"] != custodyUnitUnsigned {
		t.Fatalf("no act: %v %v", r, err)
	}
	// its own act
	setActs(p, askAct(me, provenance.UnitAct{}))
	r, err = q.unitControl(ctx, v, p, "ask", bySeq(1), ask, now)
	if err != nil || r["custody"] != custodyUnitSigned || !strings.HasPrefix(r["act"].(string), "c4") {
		t.Fatalf("its own act: %v %v", r, err)
	}
	// a second ask with no act of its number goes unsigned
	if r, err := q.unitControl(ctx, v, p, "ask", bySeq(2), map[string]any{"seq": 2, "to": "", "ask": "And the lexer?"}, now); err != nil || r["custody"] != custodyUnitUnsigned {
		t.Fatalf("an ask with no act of its own: %v %v", r, err)
	}

	for name, c := range map[string]struct {
		act json.RawMessage
		why string
	}{
		"an earlier attempt's": {askAct(provenance.UnitIdentity("show-a", "sh040-comp", "3", actsJob, 1), provenance.UnitAct{}), "and pod sh040-comp-u3 is"},
		"another unit's":       {askAct(provenance.UnitIdentity("show-a", "sh040-comp", "4", actsJob, 2), provenance.UnitAct{}), "sh040-comp/4@"},
		"for someone else":     {askAct(me, provenance.UnitAct{Owner: "noor"}), `the job's owner is "dana"`},
		"on another pod": {askAct(me, provenance.UnitAct{Subject: provenance.ActSubject("pod", "show-a", "sh040-comp-u4",
			"5e1f0c2a-0000-4000-8000-0000000000a4", strings.Repeat("ab", 32))}), "on another pod or spec"},
		"saying something else": {askAct(me, provenance.UnitAct{Params: json.RawMessage(`{"seq":1,"to":"unit:plan","ask":"Delete the plates?"}`)}), "is not what it signed"},
	} {
		v.mem = newMem()
		setActs(p, c.act)
		if r, err := q.unitControl(ctx, v, p, "ask", bySeq(1), ask, now); err == nil || !strings.Contains(err.Error(), c.why) {
			t.Errorf("%s: %v %v, want refused with %q", name, r, err, c.why)
		}
		if len(v.mem.refusedControls) != 1 {
			t.Errorf("%s: reported %d times", name, len(v.mem.refusedControls))
		}
	}

	// an act that doesn't verify is reported, and holds up nothing else
	v.mem = newMem()
	setActs(p, json.RawMessage(`{"mediaType":"not a bundle"}`), askAct(me, provenance.UnitAct{}))
	if r, err := q.unitControl(ctx, v, p, "ask", bySeq(1), ask, now); err != nil || r["custody"] != custodyUnitSigned || len(v.mem.refusedControls) != 1 {
		t.Fatalf("beside a broken act: %v %v, %d reported", r, err, len(v.mem.refusedControls))
	}

	// a farm that doesn't sign checks nothing
	q.Roots = nil
	if r, err := q.unitControl(ctx, v, p, "ask", bySeq(1), ask, now); r != nil || err != nil {
		t.Fatalf("no roots: %v %v", r, err)
	}
}

// The reports a unit ends on, and its wait, are read from the pod as the
// farm reads them: needs from its termination message, a withdrawal from
// its own, a wait from its safe point; each held to its act.
func TestUnitReportsFromThePod(t *testing.T) {
	ctx, now := context.Background(), time.Now()
	q, v, p, sign := unitActsFarm(t)
	me := provenance.UnitIdentity("show-a", "sh040-comp", "3", actsJob, 2)
	p.Status = corev1.PodStatus{Phase: corev1.PodFailed, ContainerStatuses: []corev1.ContainerStatus{{State: corev1.ContainerState{
		Terminated: &corev1.ContainerStateTerminated{ExitCode: c4v1.NeedsExitCode, Message: "the delivery frame rate\nwhy: the brief gives 24 and 25\nabout: unit:schema"}}}}}
	setActs(p, sign(me, provenance.UnitAct{Act: "needs", Params: json.RawMessage(`{"what":"the delivery frame rate","why":"the brief gives 24 and 25","about":["unit:schema"]}`)}))
	if r, err := q.needsReport(ctx, v, p, now); err != nil || r["custody"] != custodyUnitSigned || !q.needsHolds(ctx, v, p, now) {
		t.Fatalf("needs: %v %v", r, err)
	}
	setActs(p, sign(me, provenance.UnitAct{Act: "needs", Params: json.RawMessage(`{"what":"nothing","why":"","about":null}`)}))
	if q.needsHolds(ctx, v, p, now) {
		t.Fatal("a needs that is not what was signed was taken")
	}
	// a needs cut to fit (needs.Fit) reads back as it was signed
	w, y, ab := needs.Fit("the delivery frame rate", strings.Repeat("the brief gives 24 here and 25 there. ", 200), []string{"unit:schema"})
	signed, _ := json.Marshal(map[string]any{"what": w, "why": y, "about": ab})
	p.Status.ContainerStatuses[0].State.Terminated.Message = needs.Message(w, y, ab)
	setActs(p, sign(me, provenance.UnitAct{Act: "needs", Params: signed}))
	if r, err := q.needsReport(ctx, v, p, now); err != nil || r["custody"] != custodyUnitSigned {
		t.Fatalf("a needs cut to fit: %v %v", r, err)
	}

	p.Status = corev1.PodStatus{Phase: corev1.PodSucceeded, ContainerStatuses: []corev1.ContainerStatus{{State: corev1.ContainerState{
		Terminated: &corev1.ContainerStateTerminated{ExitCode: 0, Message: c4v1.WithdrawnPrefix + "schema fixes 24 fps"}}}}}
	setActs(p, sign(me, provenance.UnitAct{Act: "withdraw", Params: json.RawMessage(`{"why":"schema fixes 24 fps"}`)}))
	if r, err := q.withdrawReport(ctx, v, p, now); err != nil || r["custody"] != custodyUnitSigned || !q.withdrawHolds(ctx, v, p, now) {
		t.Fatalf("withdraw: %v %v", r, err)
	}

	n := spNote{Seq: 2, Wait: json.RawMessage(`{"server":"person:dana","ask":"Approve the v3 plates?"}`)}
	setActs(p, sign(me, provenance.UnitAct{Act: "wait", Params: json.RawMessage(`{"seq":2,"server":"person:dana","ask":"Approve the v3 plates?"}`)}))
	if r, err := q.waitReport(ctx, v, p, n, now); err != nil || r["custody"] != custodyUnitSigned {
		t.Fatalf("wait: %v %v", r, err)
	}
	other := spNote{Seq: 2, Wait: json.RawMessage(`{"server":"person:noor","ask":"Approve the v3 plates?"}`)}
	if _, err := q.waitReport(ctx, v, p, other, now); err == nil {
		t.Fatal("a wait on someone the unit did not name was taken")
	}
}
