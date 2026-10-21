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

	"github.com/Avalanche-io/c4"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/provenance"
	"github.com/Avalanche-io/c4-operator/internal/storeurl"
)

// With signing on, the queue carries out a control only on a record the
// HUD signed for that very request: this job, this action, this actor,
// this token and content. A request with no record, someone else's, a
// record for other content, one not in the store, or one another
// component signed is left undone and reported once as a Warning event.
func TestControlsNeedTheirRecord(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 20, 17, 0, 0, 0, time.UTC)
	st, err := storeurl.OpenSharded([]storeurl.Spec{{Name: "s0", URL: t.TempDir()}}, storeurl.Credentials{})
	if err != nil {
		t.Fatal(err)
	}
	ca, err := provenance.NewCA(now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	signer := func(as string) *provenance.Signer {
		s, err := provenance.NewSigner(ctx, provenance.LocalIssuer(ca, provenance.ComponentIdentity(as), time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	hud, queueSigner := signer("hud"), signer("queue")
	sign := func(s *provenance.Signer, action, actor string, request map[string]any) string {
		pred := map[string]any{"actor": actor, "action": action, "target": "workjob:show/sh010",
			"change": map[string]any{"request": request}}
		rec, err := provenance.SignStatement(ctx, s, provenance.PredicateControl, []provenance.Subject{{Name: "workjob:show/sh010",
			Digest: map[string]string{"sha256": strings.Repeat("0", 64)}}}, pred, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		id := c4.Identify(bytes.NewReader(rec))
		if err := st.Put(ctx, id, bytes.NewReader(rec), int64(len(rec))); err != nil {
			t.Fatal(err)
		}
		return id.String()
	}
	approve := sign(hud, "decide", "dana", map[string]any{"unit": "ruling", "do": "approve", "token": "t1"})
	byAgent := sign(hud, "decide", "dana", map[string]any{"unit": "ruling", "do": "approve", "token": "t6"})
	byQueue := sign(queueSigner, "decide", "dana", map[string]any{"unit": "ruling", "do": "approve", "token": "t7"})
	approveT4 := sign(hud, "decide", "dana", map[string]any{"unit": "ruling", "do": "approve", "token": "t4"})
	steer := sign(hud, "steer", "dana", map[string]any{"unit": "2", "text": "warmer", "token": "s1"})
	steerS2 := sign(hud, "steer", "dana", map[string]any{"unit": "2", "text": "warmer", "token": "s2"})
	notStored := c4.Identify(strings.NewReader("never stored")).String()
	decide := []c4v1.WorkDecideRequest{
		{Token: "t1", Unit: "ruling", Do: "approve", Actor: "dana", Record: approve},                        // vouched for
		{Token: "t2", Unit: "ruling", Do: "approve", Actor: "mallory", Record: approve},                     // someone else's record
		{Token: "t3", Unit: "ruling", Do: "approve", Actor: "dana"},                                         // no record
		{Token: "t4", Unit: "ruling", Do: "fail", Actor: "dana", Record: approveT4},                         // other content
		{Token: "t5", Unit: "ruling", Do: "approve", Actor: "dana", Record: notStored},                      // not in the store
		{Token: "t6", Unit: "ruling", Do: "approve", Actor: "agent/wrangler-bot for dana", Record: byAgent}, // an agent for dana
		{Token: "t7", Unit: "ruling", Do: "approve", Actor: "dana", Record: byQueue},                        // not the HUD's
		{Token: "t8", Unit: "ruling", Do: "approve", Actor: "dana", Record: approve},                        // another request's
	}
	steers := []c4v1.WorkSteerRequest{{Token: "s1", Unit: "2", Text: "warmer", Actor: "dana", Record: steer},
		{Token: "s2", Unit: "2", Text: "cooler", Actor: "dana", Record: steerS2}}
	db, _ := json.Marshal(decide)
	sb, _ := json.Marshal(steers)
	j := &c4v1.WorkJob{ObjectMeta: metav1.ObjectMeta{Namespace: "show", Name: "sh010", UID: "u1",
		Annotations: map[string]string{c4v1.AnnotationDecide: string(db), c4v1.AnnotationSteer: string(sb)}}}

	scheme := runtime.NewScheme()
	clientgoscheme.AddToScheme(scheme)
	c4v1.AddToScheme(scheme)
	kc := fake.NewClientBuilder().WithScheme(scheme).Build()
	roots := x509.NewCertPool()
	roots.AddCert(ca.Cert)
	q := &Queue{Client: kc, Log: zap.New(zap.WriteTo(&bytes.Buffer{})), Roots: roots,
		Stores: func(context.Context, string) (*storeurl.Sharded, error) { return st, nil }}
	v := &view{job: j, key: "show/sh010", mem: newMem(), project: "show"}

	tokens := func(job *c4v1.WorkJob, key string) string {
		var reqs []struct {
			Token string `json:"token"`
		}
		json.Unmarshal([]byte(job.Annotations[key]), &reqs)
		var out []string
		for _, r := range reqs {
			out = append(out, r.Token)
		}
		return strings.Join(out, ",")
	}
	for range 2 { // a second tick reports nothing again
		ctl := q.trustedControls(ctx, v, now)
		if got := tokens(ctl, c4v1.AnnotationDecide); got != "t1,t6" {
			t.Fatalf("decisions carried out: %s, want t1,t6", got)
		}
		if got := tokens(ctl, c4v1.AnnotationSteer); got != "s1" {
			t.Fatalf("steers carried out: %s, want s1", got)
		}
	}
	if tokens(j, c4v1.AnnotationDecide) != "t1,t2,t3,t4,t5,t6,t7,t8" {
		t.Fatal("the job's own annotation changed")
	}
	var evs corev1.EventList
	if err := kc.List(ctx, &evs); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"t2": "is by dana, not mallory", "t3": "names no signed record", "t4": `says do "approve", the request "fail"`,
		"t5": "is not in project show's store", "t7": "the policy wants", "t8": "for another request", "s2": `says text "warmer"`}
	if len(evs.Items) != len(want) {
		t.Fatalf("%d events, want %d: %+v", len(evs.Items), len(want), evs.Items)
	}
	for _, e := range evs.Items {
		ok := false
		for token, says := range want {
			if e.Type == corev1.EventTypeWarning && e.Reason == "ControlRefused" && strings.Contains(e.Message, "(token "+token+")") && strings.Contains(e.Message, says) {
				ok = true
			}
		}
		if !ok {
			t.Errorf("event %s: %s", e.Reason, e.Message)
		}
	}

	// Signing off, the requests are carried out as written.
	q.Roots = nil
	if got := q.trustedControls(ctx, v, now); got != j {
		t.Fatal("without roots the job is not used as written")
	}
}

// Requeues, allowed nodes and acceptances of a validated input are
// carried out on their records too, as the other controls are. With
// signing on, a request with no record, or one whose record says other
// content, is left undone and reported; so is the older plain form, which
// names no record. With signing off, the plain forms are still read.
func TestRequeueAllowAcceptNeedTheirRecords(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 20, 19, 0, 0, 0, time.UTC)
	st, err := storeurl.OpenSharded([]storeurl.Spec{{Name: "s0", URL: t.TempDir()}}, storeurl.Credentials{})
	if err != nil {
		t.Fatal(err)
	}
	ca, err := provenance.NewCA(now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	hud, err := provenance.NewSigner(ctx, provenance.LocalIssuer(ca, provenance.ComponentIdentity("hud"), time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	sign := func(action string, request map[string]any) string {
		pred := map[string]any{"actor": "dana", "action": action, "target": "workjob:show/sh010", "change": map[string]any{"request": request}}
		rec, err := provenance.SignStatement(ctx, hud, provenance.PredicateControl, []provenance.Subject{{Name: "workjob:show/sh010",
			Digest: map[string]string{"sha256": strings.Repeat("0", 64)}}}, pred, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		id := c4.Identify(bytes.NewReader(rec))
		if err := st.Put(ctx, id, bytes.NewReader(rec), int64(len(rec))); err != nil {
			t.Fatal(err)
		}
		return id.String()
	}
	list := func(v any) string { b, _ := json.Marshal(v); return string(b) }
	requeue := list([]c4v1.WorkRequeueRequest{
		{Token: "r1", Units: "0-3", Actor: "dana", Record: sign("requeue", map[string]any{"units": "0-3", "token": "r1"})},
		{Token: "r2", Units: "0-9", Actor: "dana", Record: sign("requeue", map[string]any{"units": "4", "token": "r2"})}, // other units
		{Token: "r3", Units: "5", Actor: "dana"}, // no record
	})
	allow := list([]c4v1.WorkAllowNodeRequest{
		{Token: "a1", Node: "w1", Actor: "dana", Record: sign("allow-node", map[string]any{"node": "w1", "token": "a1"})},
		{Token: "a2", Node: "w2", Actor: "dana", Record: sign("requeue", map[string]any{"node": "w2", "token": "a2"})}, // another action's
	})
	accept := list([]c4v1.WorkAcceptValidatedRequest{
		{Token: "v1", Accept: false, Actor: "dana", Record: sign("accept-validated", map[string]any{"accept": false, "token": "v1"})},
		{Token: "v2", Accept: true, Actor: "dana"}, // the newest, but unsigned
	})
	j := &c4v1.WorkJob{ObjectMeta: metav1.ObjectMeta{Namespace: "show", Name: "sh010", UID: "u1", Annotations: map[string]string{
		c4v1.AnnotationRequeue: requeue, c4v1.AnnotationAllowNodes: allow, c4v1.AnnotationAcceptValidated: accept}}}
	scheme := runtime.NewScheme()
	clientgoscheme.AddToScheme(scheme)
	c4v1.AddToScheme(scheme)
	kc := fake.NewClientBuilder().WithScheme(scheme).Build()
	roots := x509.NewCertPool()
	roots.AddCert(ca.Cert)
	q := &Queue{Client: kc, Log: zap.New(zap.WriteTo(&bytes.Buffer{})), Roots: roots,
		Stores: func(context.Context, string) (*storeurl.Sharded, error) { return st, nil }}
	v := &view{job: j, key: "show/sh010", mem: newMem(), project: "show"}

	ctl := q.trustedControls(ctx, v, now)
	var got []string
	for _, r := range requeues(ctl, v.mem, false) {
		got = append(got, r.Token+":"+r.Units)
	}
	if strings.Join(got, ",") != "r1:0-3" {
		t.Errorf("requeues carried out: %v, want r1:0-3", got)
	}
	if nodes := allowedNodes(ctl, v.mem, false); strings.Join(nodes, ",") != "w1" {
		t.Errorf("nodes allowed: %v, want w1", nodes)
	}
	if acceptsValidated(ctl, false) {
		t.Error("an unsigned acceptance was taken over the newest signed one, which declines")
	}
	var evs corev1.EventList
	kc.List(ctx, &evs)
	want := map[string]string{"r2": `says units "4", the request "0-9"`, "r3": "names no signed record", "a2": "is a requeue, not a allow-node", "v2": "names no signed record"}
	if len(evs.Items) != len(want) {
		t.Fatalf("%d events, want %d: %+v", len(evs.Items), len(want), evs.Items)
	}
	for _, e := range evs.Items {
		ok := false
		for token, says := range want {
			if e.Reason == "ControlRefused" && strings.Contains(e.Message, "(token "+token+")") && strings.Contains(e.Message, says) {
				ok = true
			}
		}
		if !ok {
			t.Errorf("event: %s", e.Message)
		}
	}

	// The older plain forms name no record: refused with signing on.
	plain := &c4v1.WorkJob{ObjectMeta: metav1.ObjectMeta{Namespace: "show", Name: "sh010", UID: "u1", Annotations: map[string]string{
		c4v1.AnnotationRequeue: "t1:0-3", c4v1.AnnotationAllowNodes: "w1,w2", c4v1.AnnotationAcceptValidated: "true"}}}
	pv := &view{job: plain, key: "show/sh010", mem: newMem(), project: "show"}
	pctl := q.trustedControls(ctx, pv, now)
	if len(requeues(pctl, pv.mem, false)) != 0 || len(allowedNodes(pctl, pv.mem, false)) != 0 || acceptsValidated(pctl, false) {
		t.Error("a plain request was carried out with signing on")
	}
	evs = corev1.EventList{}
	kc.List(ctx, &evs)
	plainRefused := 0
	for _, e := range evs.Items {
		if strings.Contains(e.Message, "it is not a list of signed requests") {
			plainRefused++
		}
	}
	if plainRefused != 3 {
		t.Errorf("%d plain requests reported, want 3 (requeue, allow-nodes, accept-validated)", plainRefused)
	}
	// Signing off, they are read as written.
	q.Roots = nil
	pv.mem = newMem()
	pctl = q.trustedControls(ctx, pv, now)
	if r := requeues(pctl, pv.mem, true); len(r) != 1 || r[0].Units != "0-3" {
		t.Errorf("an unsigned farm's requeue: %v", r)
	}
	if n := allowedNodes(pctl, pv.mem, true); strings.Join(n, ",") != "w1,w2" {
		t.Errorf("an unsigned farm's allowed nodes: %v", n)
	}
	if !acceptsValidated(pctl, true) {
		t.Error("an unsigned farm's acceptance")
	}
}
