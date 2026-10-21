package queue

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/work/class"
)

// A graph job whose unit table was stored with it at submit runs that
// table: no root generator, its units in their order and after their
// edges, each with its own env. A dry run plans it from the table given,
// or from the store once it is there.
func TestStoredTable(t *testing.T) {
	ns, q, now, st := graphFarm(t)
	ctx := context.Background()
	doc := `{"version": 1, "units": [
	  {"name": "scan", "env": {"C4_BLOCK_PROMPT": "port scan"}, "reasoning": {"does": "model"}},
	  {"name": "format", "after": ["scan"], "env": {"C4_BLOCK_PROMPT": "port format"}, "reasoning": {"does": "model"}}]}`
	id := putDoc(t, st, doc)
	params := map[string]string{class.TableParam: id, "key": "agent"}

	if err := k8s.Create(ctx, job(ns, "port", "graph", "cpu-"+ns, params, now.Add(4*time.Hour))); err != nil {
		t.Fatal(err)
	}
	tick(t, q)
	d, err := q.DryRunTables(ctx, []*c4v1.WorkJob{job(ns, "draft", "graph", "cpu-"+ns, params, now.Add(4*time.Hour))}, "", nil, nil)
	if err != nil || len(d) != 1 || d[0].Units != 2 {
		t.Fatalf("dry run from the stored table: %+v %v", d, err)
	}
	other := map[string]string{class.TableParam: viewID("not stored"), "key": "agent"}
	if _, err := q.DryRunTables(ctx, []*c4v1.WorkJob{job(ns, "draft2", "graph", "cpu-"+ns, other, now.Add(4*time.Hour))}, "", nil,
		map[string][]byte{viewID("not stored"): []byte(`{"version": 1, "units": [{"name": "only"}]}`)}); err != nil {
		t.Fatalf("dry run from a table given: %v", err)
	}

	pods := unitPods(t, ns, "port")
	if len(pods) != 1 {
		t.Fatalf("released %d pods; want scan alone, format waits on it", len(pods))
	}
	if n, p := envOf(pods[0].Spec.Containers[0], "C4_UNIT_NAME"), envOf(pods[0].Spec.Containers[0], "C4_BLOCK_PROMPT"); n != "scan" || p != "port scan" {
		t.Fatalf("first unit: %s %q", n, p)
	}
	*now = now.Add(time.Minute)
	finish(t, &pods[0], corev1.PodSucceeded, *now, time.Minute, nil)
	tick(t, q)
	p := podOfUnit(t, ns, "port", 1)
	if envOf(p.Spec.Containers[0], "C4_BLOCK_PROMPT") != "port format" {
		t.Fatalf("second unit: %+v", p.Spec.Containers[0].Env)
	}

	// A table the store does not have yet leaves the job waiting, not
	// failed.
	missing := map[string]string{class.TableParam: viewID("never stored"), "key": "agent"}
	if err := k8s.Create(ctx, job(ns, "early", "graph", "cpu-"+ns, missing, now.Add(4*time.Hour))); err != nil {
		t.Fatal(err)
	}
	tick(t, q)
	if j := get(t, ns, "early"); j.Status.Phase == "Failed" || len(unitPods(t, ns, "early")) != 0 {
		t.Fatalf("a job whose table is not stored yet: %s, %d pods", j.Status.Phase, len(unitPods(t, ns, "early")))
	}
}
