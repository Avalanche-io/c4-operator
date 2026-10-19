package process

import (
	"encoding/json"
	"math"
	"testing"
	"time"
)

func ctr(digest string, env map[string]string) []Container {
	return []Container{{Digest: digest, Command: []string{"render"}, Args: []string{"--frames", "1001"}, Env: env}}
}

func TestProcessID(t *testing.T) {
	a, ok := ProcessID(ctr("sha256:aa", map[string]string{"PASS": "beauty", "C4_JOB": "sh010-v001", "C4_ATTEMPT": "1"}), "")
	if !ok {
		t.Fatal("no process ID")
	}
	b, _ := ProcessID(ctr("sha256:aa", map[string]string{"PASS": "beauty", "C4_JOB": "sh010-v002", "C4_ATTEMPT": "2"}), "")
	if a != b {
		t.Fatal("run-naming variables changed the process ID")
	}
	c, _ := ProcessID(ctr("sha256:aa", map[string]string{"PASS": "env"}), "")
	d, _ := ProcessID(ctr("sha256:bb", map[string]string{"PASS": "beauty"}), "")
	g, _ := ProcessID(ctr("sha256:aa", map[string]string{"PASS": "beauty"}), "L40S/550.54/cuda12.4")
	if c == a || d == a || g == a {
		t.Fatal("params, image or platform did not change the process ID")
	}
	if _, ok := ProcessID(ctr("docker.io/render:latest", nil), ""); ok {
		t.Fatal("an image without a digest has a process ID")
	}
	x, _ := InputsID([]Input{{"/show", "c4a"}, {"/work", "c4b"}})
	y, _ := InputsID([]Input{{"/work", "c4b"}, {"/show", "c4a"}})
	if x != y {
		t.Fatal("input order changed the inputs ID")
	}
	if _, ok := InputsID([]Input{{"/show", ""}}); ok {
		t.Fatal("an input without a view has an inputs ID")
	}
}

func run(proc, inputs, out string, work float64, class Class, sealed bool) Run {
	return Run{Key: Key{proc, inputs}, Inputs: []Input{{"/show", inputs}}, Declared: class, Sealed: sealed,
		Output: out, Work: work, Duration: work, At: time.Unix(0, 0)}
}

// A declared-exact process is verified by matching re-runs, earns reuse,
// and is demoted to validated, permanently, by one that does not match.
func TestExactVerifiedThenFalsified(t *testing.T) {
	var b Book
	b.Record(run("p", "i1", "o1", 100, Exact, true))
	if c, v := b.Class("p"); c != Exact || v {
		t.Fatalf("one run: %s verified=%v", c, v)
	}
	if c, _ := b.Recompute(Key{"p", "i1"}, ""); c.Reuse != "" || c.Basis != "exact" {
		t.Fatalf("unverified exact offered reuse: %+v", c)
	}
	b.Record(run("p", "i1", "o1", 110, Exact, true))
	b.Record(run("p", "i2", "o2", 90, Exact, true))
	b.Record(run("p", "i2", "o2", 95, Exact, true))
	c, ok := b.Recompute(Key{"p", "i1"}, "")
	if !ok || c.Class != Exact || !c.Verified || c.Reuse != "o1" || c.Runs != 2 || math.Abs(c.Work.Mean-105) > 1e-9 || c.W90 <= c.Work.Mean {
		t.Fatalf("verified exact: %+v", c)
	}
	if k, _, in, ok := b.ProducedBy("o2"); !ok || k != (Key{"p", "i2"}) || len(in) != 1 {
		t.Fatalf("produced by: %v %v %v", k, in, ok)
	}
	if !b.NeedsVerification("p", 0.01) || b.NeedsVerification("p", 0.5) {
		t.Fatal("verification sampling")
	}
	// Another input set: the process's runs give the cost.
	if c, _ := b.Recompute(Key{"p", "i9"}, "p"); c.Basis != "program" || c.Reuse != "" || c.Runs != 4 {
		t.Fatalf("process basis: %+v", c)
	}
	// A re-run that writes another output falsifies the declaration.
	b.Record(run("p", "i1", "o1-different", 100, Exact, true))
	if c, v := b.Class("p"); c != Validated || v || b.Why("p") == "" {
		t.Fatalf("after a mismatch: %s %v %q", c, v, b.Why("p"))
	}
	if c, _ := b.Recompute(Key{"p", "i2"}, ""); c.Reuse != "" {
		t.Fatal("a falsified process offered reuse")
	}
	b.Record(run("p", "i2", "o2", 95, Exact, true))
	if c, _ := b.Class("p"); c != Validated {
		t.Fatal("matching runs undid a demotion")
	}
}

// An exact declaration from an unsealed pod is capped at validated; an
// undeclared process stays volatile however often it repeats.
func TestUnsealedAndUndeclared(t *testing.T) {
	var b Book
	b.Record(run("u", "i", "o", 10, Exact, false))
	b.Record(run("u", "i", "o", 10, Exact, true))
	b.Record(run("u", "i", "o", 10, Exact, true))
	if c, _ := b.Class("u"); c != Validated || b.Why("u") == "" {
		t.Fatalf("unsealed exact: %s %q", c, b.Why("u"))
	}
	for range 5 {
		b.Record(run("v", "i", "o", 10, "", true))
	}
	c, _ := b.Class("v")
	if m, mm := b.Evidence("v"); c != Volatile || m != 4 || mm != 0 {
		t.Fatalf("undeclared: %s, evidence %d/%d", c, m, mm)
	}
	if cost, _ := b.Recompute(Key{"v", "i"}, ""); cost.Reuse != "" || cost.Class != Volatile {
		t.Fatalf("volatile reuse: %+v", cost)
	}
	if _, ok := b.Recompute(Key{"never", "i"}, "never"); ok {
		t.Fatal("a process that never ran has a cost")
	}
}

func TestBookSnapshot(t *testing.T) {
	var b Book
	for range 3 {
		b.Record(run("p", "i", "o", 100, Exact, true))
	}
	if !b.Changed() {
		t.Fatal("not changed after records")
	}
	js, err := json.Marshal(b.Snapshot())
	if err != nil || b.Changed() {
		t.Fatalf("snapshot: %v, changed %v", err, b.Changed())
	}
	var s BookSnapshot
	if err := json.Unmarshal(js, &s); err != nil {
		t.Fatal(err)
	}
	r := RestoreBook(s)
	want, _ := b.Recompute(Key{"p", "i"}, "")
	got, ok := r.Recompute(Key{"p", "i"}, "")
	if !ok || got != want {
		t.Fatalf("restored %+v, want %+v", got, want)
	}
	if k, _, _, ok := r.ProducedBy("o"); !ok || k.Process != "p" {
		t.Fatal("restored output index")
	}
}

func TestProgramPoolsEvidenceAcrossUnits(t *testing.T) {
	unit := func(frames string) []Container {
		return []Container{{Digest: "sha256:aa", Command: []string{"render"}, Env: map[string]string{"PASS": "beauty", "C4_FRAMES": frames}}}
	}
	per := map[string]bool{"C4_FRAMES": true}
	p1, _ := ProcessID(unit("1001"), "")
	p2, _ := ProcessID(unit("1002"), "")
	g1, _ := ProgramID(unit("1001"), "", per)
	g2, _ := ProgramID(unit("1002"), "", per)
	if p1 == p2 || g1 != g2 {
		t.Fatal("frames must split processes and not programs")
	}
	var b Book
	for _, p := range []string{p1, p2, p1, p2} {
		b.Record(Run{Key: Key{p, "i"}, Program: g1, Declared: Exact, Sealed: true, Output: "o" + p, Work: 1})
	}
	if c, v := b.Class(g1); c != Exact || !v {
		t.Fatalf("two frames re-run once each: %s %v", c, v)
	}
}

// The record names the unit that last ran a computation, the one a
// recompute reruns; a run with no producer (a recompute's) leaves it, and
// it survives a snapshot.
func TestBookProducer(t *testing.T) {
	var b Book
	k := Key{"p", "i"}
	if _, ok := b.ProducerOf(k); ok {
		t.Fatal("a producer before any run")
	}
	r := run("p", "i", "o", 100, Exact, true)
	r.Producer = Producer{Job: "show/sh010", Unit: 3}
	b.Record(r)
	r.Producer = Producer{Job: "show/sh011", Unit: 0}
	b.Record(r)
	b.Record(run("p", "i", "o", 100, Exact, true))
	if pr, ok := b.ProducerOf(k); !ok || pr != (Producer{Job: "show/sh011", Unit: 0}) {
		t.Fatalf("producer %+v %v", pr, ok)
	}
	js, _ := json.Marshal(b.Snapshot())
	var s BookSnapshot
	if err := json.Unmarshal(js, &s); err != nil {
		t.Fatal(err)
	}
	if pr, ok := RestoreBook(s).ProducerOf(k); !ok || pr.Job != "show/sh011" {
		t.Fatalf("restored producer %+v %v", pr, ok)
	}
}
