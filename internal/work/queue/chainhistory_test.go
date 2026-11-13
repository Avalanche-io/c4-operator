package queue

import (
	"context"
	"crypto/x509"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Avalanche-io/c4"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/provenance"
	"github.com/Avalanche-io/c4-operator/internal/storeurl"
	c4view "github.com/Avalanche-io/c4-operator/internal/view"
)

func historyReached(t *testing.T, st *storeurl.Sharded, o *output) map[c4.ID]bool {
	t.Helper()
	ctx := context.Background()
	histID, err := st.PutBytes(ctx, o.historyBytes())
	if err != nil {
		t.Fatal(err)
	}
	heads, err := readIDList(st, histID)
	if err != nil {
		t.Fatal(err)
	}
	reached := map[c4.ID]bool{}
	for _, h := range heads {
		for cur := h; !cur.IsNil(); {
			d, err := c4view.ReadObject(st, cur)
			if err != nil {
				t.Fatal(err)
			}
			ch, err := c4view.ReadChain(d)
			if err != nil {
				break
			}
			reached[cur] = true
			cur = ch.Base
		}
	}
	return reached
}

func restartOutput(t *testing.T, st *storeurl.Sharded, o *output) *output {
	t.Helper()
	ctx := context.Background()
	if err := o.saveIndex(ctx, st, "ns/job"); err != nil {
		t.Fatal(err)
	}
	re := newOutput()
	if err := re.load(st, chainField+o.id.String(), o.view.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := re.loadIndex(ctx, st, o.index.String(), "ns/job", nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	re.count, re.folded, re.writer = o.count, o.folded, o.writer
	return re
}

// Everything: 70 folds (a periodic rest), a restart, a rest right after,
// two restarts with no fold, a refold superseding unit 0, more folds, a
// restart, a refold after the restart. Every block ever stored reachable.
func TestTheHistoryHoldsOnEveryPath(t *testing.T) {
	ctx := context.Background()
	st, _ := foldStore(t)
	q := &Queue{Stores: func(context.Context, string) (*storeurl.Sharded, error) { return st, nil }, Log: logr.Discard()}
	in := inputView(t, st, map[string]string{"in.txt": "input"})
	o := newOutput()
	stored := map[c4.ID]bool{}
	unit := 0
	fold := func(n int) {
		for k := 0; k < n; k++ {
			cs := unitChangeset(t, st, in, map[string]string{fmt.Sprintf("d/f%03d.txt", unit): fmt.Sprint(unit)})
			if err := q.fold(ctx, st, o, Fold{Unit: unit, Changeset: cs, Run: fmt.Sprint("r", unit)}); err != nil {
				t.Fatal(err)
			}
			stored[o.id] = true
			unit++
		}
	}
	refold := func(sup ...int) {
		m := &jobMem{out: o, superseded: map[int]bool{}}
		for _, s := range sup {
			m.superseded[s] = true
		}
		v := &view{key: "ns/job", project: "show", mem: m}
		if !q.refoldOutput(ctx, v) {
			t.Fatal("refold retry")
		}
		o = m.out
		stored[o.id] = true
	}
	fold(65)                    // the 65th rests periodically
	o = restartOutput(t, st, o) // a restart right after a rest
	fold(1)
	o = restartOutput(t, st, o)
	o = restartOutput(t, st, o) // two in a row
	fold(2)
	refold(0)
	fold(3)
	o = restartOutput(t, st, o)
	refold(1)
	o = restartOutput(t, st, o)
	reached := historyReached(t, st, o)
	miss := 0
	for b := range stored {
		if !reached[b] {
			miss++
		}
	}
	if miss > 0 {
		t.Fatalf("%d of %d stored blocks not reachable (cut %d)", miss, len(stored), len(o.cut))
	}
	if o.input != in {
		t.Fatalf("input %s want %s", o.input, in)
	}
	t.Logf("blocks %d, cut %d", len(stored), len(o.cut))
}

// A chain from before blocks: whole, unprefixed in the status, no Input in
// the index (none at all here). Its ID is cut and kept; the input is its base.
func TestAChainFromBeforeBlocksStaysInTheHistory(t *testing.T) {
	ctx := context.Background()
	st, _ := foldStore(t)
	q := &Queue{}
	in := inputView(t, st, map[string]string{"in.txt": "input"})
	o1 := newOutput()
	var secs []c4view.Section
	for i := 0; i < 3; i++ {
		before := o1.view
		if before.IsNil() {
			before = in
		}
		if err := q.fold(ctx, st, o1, Fold{Unit: i, Changeset: unitChangeset(t, st, in, map[string]string{fmt.Sprint("a", i): "x"}), Run: fmt.Sprint(i)}); err != nil {
			t.Fatal(err)
		}
		_, blk := readChain(t, st, o1.id)
		sec := blk.Sections[0]
		sec.Base = before
		secs = append(secs, sec)
	}
	old, err := (&c4view.Chain{Base: in, Sections: secs}).Encode()
	if err != nil {
		t.Fatal(err)
	}
	oldID, _ := st.PutBytes(ctx, old)
	re := newOutput()
	if err := re.load(st, oldID.String(), o1.view.String()); err != nil {
		t.Fatal(err)
	}
	if err := q.fold(ctx, st, re, Fold{Unit: 9, Changeset: unitChangeset(t, st, in, map[string]string{"z": "z"}), Run: "9"}); err != nil {
		t.Fatal(err)
	}
	h := string(re.historyBytes())
	if !strings.Contains(h, oldID.String()) {
		t.Fatalf("old whole chain not in history:\n%s", h)
	}
	if re.input != in {
		t.Fatalf("the input after an old chain with no index is %s, want its base %s", re.input, in)
	}
}

// sealJob with no store this tick, and with a chain that cannot be read.
func TestTheJobRecordNamesTheHistory(t *testing.T) {
	ctx := context.Background()
	ca, _ := provenance.NewCA(time.Now())
	sg, _ := provenance.NewSigner(ctx, provenance.LocalIssuer(ca, provenance.ComponentIdentity("queue"), time.Hour))
	st, _ := foldStore(t)
	roots := x509.NewCertPool()
	roots.AddCert(ca.Cert)
	in := inputView(t, st, map[string]string{"in.txt": "input"})
	j := &c4v1.WorkJob{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "job"}}

	// 1. The store is not there this tick (Stores gives nil): sealed, no chain.
	qn := &Queue{Signer: sg, Log: logr.Discard(), Stores: func(context.Context, string) (*storeurl.Sharded, error) { return nil, nil }}
	m := newJobMemWithOutput()
	m.outLoad, m.outView = chainField+in.String(), in.String()
	v := &view{job: j, key: "ns/job", project: "p", mem: m}
	status := &c4v1.WorkJobStatus{Phase: "Succeeded", Output: &c4v1.WorkOutput{View: in.String(), Chain: chainField + in.String()}}
	qn.sealJob(ctx, v, status, time.Now())
	t.Logf("no store: jobRecord=%q", m.jobRecord)
	if m.jobRecord != "" {
		data, err := c4view.ReadObject(st, mustParse(t, m.jobRecord))
		if err == nil {
			rv, _ := provenance.VerifyBundle(data, roots)
			t.Logf("no store: chain subject %q", rv.Subject("chain"))
		} else {
			t.Logf("no store: record not in st (stored nowhere?): %v", err)
		}
	}

	// 2. The chain the status names is not in the store: never sealed.
	q := &Queue{Signer: sg, Log: logr.Discard(), Stores: func(context.Context, string) (*storeurl.Sharded, error) { return st, nil }}
	gone := c4.Identify(strings.NewReader("gone"))
	m2 := newJobMemWithOutput()
	m2.outLoad, m2.outView = chainField+gone.String(), in.String()
	v2 := &view{job: j, key: "ns/job", project: "p", mem: m2}
	for i := 0; i < 3 && m2.jobRecord == ""; i++ {
		q.sealJob(ctx, v2, status, time.Now())
	}
	if m2.jobRecord == "" {
		t.Fatal("a chain that cannot be read held the job record for 3 ticks")
	}
	data, err := c4view.ReadObject(st, mustParse(t, m2.jobRecord))
	if err != nil {
		t.Fatal(err)
	}
	if rv, err := provenance.VerifyBundle(data, roots); err != nil || rv.Subject("chain") != "" {
		t.Fatalf("a record sealed without its history names a chain %q: %v", rv.Subject("chain"), err)
	}

	// 3. A job folded past a rest: the record's chain is its whole history,
	// every head the rest cut off and then the head, not the head alone.
	m3 := newJobMemWithOutput()
	for i := 0; i < chainRestEvery+3; i++ {
		cs := unitChangeset(t, st, in, map[string]string{fmt.Sprintf("r/f%03d.txt", i): fmt.Sprint(i)})
		if err := q.fold(ctx, st, m3.out, Fold{Unit: i, Changeset: cs, Run: fmt.Sprint("r", i)}); err != nil {
			t.Fatal(err)
		}
	}
	if len(m3.out.cut) == 0 {
		t.Fatal("no head was cut, so this shows nothing")
	}
	v3 := &view{job: j, key: "ns/job3", project: "p", mem: m3}
	st3 := &c4v1.WorkJobStatus{Phase: "Succeeded", Output: &c4v1.WorkOutput{View: m3.out.view.String(), Chain: chainField + m3.out.id.String()}}
	q.sealJob(ctx, v3, st3, time.Now())
	data, err = c4view.ReadObject(st, mustParse(t, m3.jobRecord))
	if err != nil {
		t.Fatal(err)
	}
	rv, err := provenance.VerifyBundle(data, roots)
	if err != nil {
		t.Fatal(err)
	}
	hist, err := readIDList(st, mustParse(t, rv.Subject("chain")))
	if err != nil {
		t.Fatalf("the record's chain is not a stored ID list: %v", err)
	}
	want := append(append([]c4.ID(nil), m3.out.cut...), m3.out.id)
	if len(hist) != len(want) {
		t.Fatalf("the record's chain lists %d heads, want %d (the cut and the head)", len(hist), len(want))
	}
	for i := range want {
		if hist[i] != want[i] {
			t.Fatalf("the record's chain, entry %d: %s, want %s", i, hist[i], want[i])
		}
	}
}

func newJobMemWithOutput() *jobMem {
	m := &jobMem{superseded: map[int]bool{}}
	m.out = newOutput()
	return m
}

func mustParse(t *testing.T, s string) c4.ID {
	id, err := c4.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return id
}
