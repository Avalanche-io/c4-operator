package queue

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
)

// The tick that folds a unit saves the index, and then the job's status
// write fails. A restart reads the status as it was, and the index it
// names; the unit's pod, still listed (it is deleted only once the status
// recording it is written), is folded, and its write is in the output.
func TestAStatusWriteThatFailsAfterTheIndexLosesNoFold(t *testing.T) {
	ns, q, now, st := graphFarm(t)
	ctx := context.Background()
	if err := k8s.Create(ctx, job(ns, "bon", "graph", "cpu-"+ns, nil, now.Add(8*time.Hour))); err != nil {
		t.Fatal(err)
	}
	tick(t, q)
	doc := putDoc(t, st, `{"version": 1, "units": [{"name": "a1"}, {"name": "b"}]}`)
	finishUnit(t, now, ns, "bon", 0, map[string]string{c4v1.AnnotationOutputExpand: doc})
	tick(t, q)

	p := podOfUnit(t, ns, "bon", 1)
	p.Finalizers = append(p.Finalizers, "test.c4.avalanche.io/hold")
	if err := k8s.Update(ctx, p); err != nil {
		t.Fatal(err)
	}
	defer func() {
		var held corev1.Pod
		if k8s.Get(ctx, client.ObjectKeyFromObject(p), &held) == nil {
			held.Finalizers = nil
			k8s.Update(ctx, &held)
		}
	}()

	// The job's status writes fail while this is set, after the queue has
	// saved the index (report saves it before it writes the status).
	var failing atomic.Bool
	var refused atomic.Int32
	q.Client = failingStatus{Client: k8s, failing: &failing, refused: &refused}
	finishUnit(t, now, ns, "bon", 1, map[string]string{c4v1.AnnotationOutputChangeset: writeChangeset(t, st, "a1.txt", "unit one's file")})
	failing.Store(true)
	tick(t, q)
	failing.Store(false)
	if refused.Load() == 0 {
		t.Fatal("no status write was refused, so this test shows nothing")
	}
	// What the failed tick left: an index, saved, whose fold log lists unit
	// 1's run, and a status that does not name it.
	stale := get(t, ns, "bon")
	m := q.jobs[stale.UID]
	if m == nil || m.out == nil || m.out.index.IsNil() {
		t.Fatal("the failed tick saved no index, so this test shows nothing")
	}
	ix, err := ReadIndex(st, m.out.index)
	if err != nil {
		t.Fatal(err)
	}
	logged := false
	for _, f := range ix.Folds {
		logged = logged || (f.Unit == 1 && f.Run == string(p.UID))
	}
	if !logged {
		t.Fatal("the saved index does not log unit 1's fold, so this test shows nothing")
	}
	if stale.Status.Output != nil && stale.Status.Output.Provenance == m.out.index.String() {
		t.Fatal("the status names the new index after all, so this test shows nothing")
	}

	// The restart: a new queue with no memory, the real client.
	q = &Queue{Client: k8s, Policy: q.Policy, Cfg: q.Cfg, Log: q.Log, Now: q.Now, CaptureWait: q.CaptureWait, Stores: q.Stores}
	tick(t, q)
	tick(t, q)
	got := get(t, ns, "bon")
	if got.Status.Output == nil {
		t.Fatal("no output after the restart")
	}
	files := outputFiles(t, st, got)
	found := false
	for _, f := range files {
		found = found || f == "a1.txt"
	}
	if !found {
		t.Fatalf("unit 1's write is lost: the output holds %v", files)
	}
}

// failingStatus refuses a WorkJob's status patch while failing is set.
type failingStatus struct {
	client.Client
	failing *atomic.Bool
	refused *atomic.Int32
}

func (c failingStatus) Status() client.SubResourceWriter {
	return failingWriter{c.Client.Status(), c}
}

type failingWriter struct {
	client.SubResourceWriter
	c failingStatus
}

func (w failingWriter) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
	if _, ok := obj.(*c4v1.WorkJob); ok && w.c.failing.Load() {
		w.c.refused.Add(1)
		return errors.New("injected: the status write failed")
	}
	return w.SubResourceWriter.Patch(ctx, obj, patch, opts...)
}
