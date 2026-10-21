package queue

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/Avalanche-io/c4"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/provenance"
	"github.com/Avalanche-io/c4-operator/internal/storeurl"
	c4view "github.com/Avalanche-io/c4-operator/internal/view"
)

// TestQueueSignsDecisionsAndJobs covers DESIGN.md section 9.2: the queue
// signs each preemption or yield it decides, and seals each finished job
// in a record that names its output and every decision and control.
func TestQueueSignsDecisionsAndJobs(t *testing.T) {
	ctx := context.Background()
	ca, err := provenance.NewCA(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	sg, err := provenance.NewSigner(ctx, provenance.LocalIssuer(ca, provenance.ComponentIdentity("queue"), time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	st, err := storeurl.OpenSharded([]storeurl.Spec{{Name: "s0", URL: t.TempDir()}}, storeurl.Credentials{})
	if err != nil {
		t.Fatal(err)
	}
	q := &Queue{Signer: sg, PolicyName: "objective", Log: zap.New(zap.WriteTo(&strings.Builder{})),
		Stores: func(context.Context, string) (*storeurl.Sharded, error) { return st, nil }}
	j := &c4v1.WorkJob{ObjectMeta: metav1.ObjectMeta{Namespace: "show-a", Name: "train",
		Annotations: map[string]string{c4v1.AnnotationControlRecords: "c4control1,c4control2",
			c4v1.AnnotationOwner: "noor", c4v1.AnnotationSubmittedVia: "c4-sync", c4v1.AnnotationGrant: "c4grantrecord"}},
		Spec: c4v1.WorkJobSpec{Class: "ml"}}
	v := &view{job: j, key: "show-a/train", project: "show-a", mem: &jobMem{}}
	now := time.Now()
	q.recordDecision(ctx, v, 3, "yield", "reclaim-nominal", "show-a/sh010", now)
	if len(v.mem.decisions) != 1 || v.mem.decisions[0].Record == "" {
		t.Fatalf("decision not recorded: %+v", v.mem.decisions)
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca.Cert)
	read := func(id string) *provenance.Verified {
		t.Helper()
		cid, err := c4.Parse(id)
		if err != nil {
			t.Fatal(err)
		}
		data, err := c4view.ReadObject(st, cid)
		if err != nil {
			t.Fatal(err)
		}
		rv, err := provenance.VerifyBundle(data, roots)
		if err != nil {
			t.Fatal(err)
		}
		if err := provenance.CheckSigner(rv); err != nil {
			t.Fatal(err)
		}
		return rv
	}
	d := read(v.mem.decisions[0].Record)
	if d.PredicateType != provenance.PredicateDecision || !strings.Contains(string(d.Predicate), `"reclaim-nominal"`) {
		t.Fatalf("decision record %s %s", d.PredicateType, d.Predicate)
	}

	out := c4.Identify(strings.NewReader("output view"))
	status := &c4v1.WorkJobStatus{Phase: "Succeeded", Finished: &metav1.Time{Time: now},
		Output: &c4v1.WorkOutput{View: out.String(), Chain: c4.Identify(strings.NewReader("chain")).String()}}
	q.sealJob(ctx, v, status, now)
	if v.mem.jobRecord == "" {
		t.Fatal("no job record")
	}
	jr := read(v.mem.jobRecord)
	var p struct {
		Job       string           `json:"job"`
		Decisions []map[string]any `json:"decisions"`
		Controls  []string         `json:"controls"`
		Owner     string           `json:"owner"`
		Via       string           `json:"submittedVia"`
		Grant     string           `json:"grant"`
	}
	if err := json.Unmarshal(jr.Predicate, &p); err != nil {
		t.Fatal(err)
	}
	if jr.Subject("output") != out.String() || p.Job != "show-a/train" || len(p.Decisions) != 1 ||
		p.Decisions[0]["record"] != v.mem.decisions[0].Record || len(p.Controls) != 2 ||
		p.Owner != "noor" || p.Via != "c4-sync" || p.Grant != "c4grantrecord" {
		t.Fatalf("job record %s", jr.Predicate)
	}
	// Sealed once: a later status pass keeps the same record.
	first := v.mem.jobRecord
	q.sealJob(ctx, v, status, now.Add(time.Minute))
	if v.mem.jobRecord != first {
		t.Fatal("the job was sealed twice")
	}
}
