package controller

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/Avalanche-io/c4"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/provenance"
	"github.com/Avalanche-io/c4-operator/internal/storeurl"
	"github.com/Avalanche-io/c4-operator/internal/view"
)

// recordRef points at another signed record.
type recordRef struct {
	Record string `json:"record"`
	Signer string `json:"signer"`
	Kind   string `json:"kind"`
}

// resolution is the controller's resolution record (PredicateResolution).
type resolution struct {
	Project           string      `json:"project"`
	Namespace         string      `json:"namespace"`
	Materialization   string      `json:"materialization"`
	Generation        int64       `json:"generation"`
	ProjectGeneration int64       `json:"projectGeneration"`
	Source            string      `json:"source"`
	Include           []string    `json:"include,omitempty"`
	Grant             *c4v1.Grant `json:"grant,omitempty"`
	View              string      `json:"view"`
	Layers            []string    `json:"layers"`
	Files             int64       `json:"files"`
	Objects           int64       `json:"objects"`
	Bytes             int64       `json:"bytes"`
	// Preflight says what was checked: every object of the view was found
	// in the project's own store.
	Preflight    string     `json:"preflight"`
	SourceRecord *recordRef `json:"sourceRecord,omitempty"`
}

// sourceRecord reads and checks the record a Materialization names for its
// source: it must verify against the farm's roots, be signed by whom the
// policy says (an import or ingest by a workload of this project, or the
// queue's record of the job that produced the source), and have the source
// among its subjects.
func (r *MaterializationReconciler) sourceRecord(store *storeurl.Sharded, p *c4v1.Project, id string, source c4.ID) (*provenance.Verified, error) {
	rid, err := c4.Parse(id)
	if err != nil {
		return nil, fmt.Errorf("source record %q is not a C4 ID", id)
	}
	data, err := view.ReadObject(store, rid)
	if err != nil {
		return nil, fmt.Errorf("source record: %w", err)
	}
	if r.Roots == nil {
		return nil, fmt.Errorf("source record %s: this controller has no signing roots to check it", id)
	}
	v, err := provenance.VerifyBundle(data, r.Roots)
	if err != nil {
		return nil, fmt.Errorf("source record %s: %w", id, err)
	}
	if err := provenance.CheckSigner(v); err != nil {
		return nil, fmt.Errorf("source record %s: %w", id, err)
	}
	switch v.PredicateType {
	case provenance.PredicateImport, provenance.PredicateIngest:
		// A workload may vouch only for content of its own project.
		if ns := provenance.SignerNamespace(v.Identity); !slices.Contains(p.Spec.Namespaces, ns) {
			return nil, fmt.Errorf("source record %s is signed by %s, which is not a workload of project %s", id, v.Identity, p.Name)
		}
	case provenance.PredicateJob:
	default:
		return nil, fmt.Errorf("source record %s is a %s record, not an import, ingest or job record", id, v.PredicateType)
	}
	if !v.HasSubject(source.String()) {
		return nil, fmt.Errorf("source record %s does not name the source %s", id, source)
	}
	return v, nil
}

// signResolution signs and stores the resolution record.
func (r *MaterializationReconciler) signResolution(ctx context.Context, store *storeurl.Sharded, p *c4v1.Project, mat *c4v1.Materialization,
	source c4.ID, g *c4v1.Grant, v *view.View, t view.Totals, res *view.Resolved, src *provenance.Verified) (string, error) {
	pred := resolution{
		Project: p.Name, Namespace: mat.Namespace, Materialization: mat.Name, Generation: mat.Generation,
		ProjectGeneration: p.Generation, Source: source.String(), Include: mat.Spec.Include, Grant: g,
		View: v.ID.String(), Files: int64(t.Files), Objects: int64(t.UniqueObjects), Bytes: t.Bytes,
		Preflight: "every object of the view is in the project's store",
	}
	for _, lv := range res.Chain {
		pred.Layers = append(pred.Layers, lv.ID.String())
	}
	if src != nil {
		pred.SourceRecord = &recordRef{Record: mat.Annotations[c4v1.AnnotationSourceRecord], Signer: src.Identity, Kind: src.PredicateType}
	}
	subjects := []provenance.Subject{provenance.SubjectFor("view", v.ID), provenance.SubjectFor("source", source)}
	rec, err := provenance.SignStatement(ctx, r.Signer, provenance.PredicateResolution, subjects, pred, time.Now())
	if err != nil {
		return "", err
	}
	id := c4.Identify(bytes.NewReader(rec))
	if err := store.Put(ctx, id, bytes.NewReader(rec), int64(len(rec))); err != nil {
		return "", err
	}
	return id.String(), nil
}
