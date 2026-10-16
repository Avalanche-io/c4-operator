package server

import (
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Avalanche-io/c4"

	"github.com/Avalanche-io/c4-operator/internal/provenance"
	c4view "github.com/Avalanche-io/c4-operator/internal/view"
	"github.com/Avalanche-io/c4-operator/internal/work/queue"
)

// KeysConfigMap is the farm's trust bundle, in the operator's namespace:
// ca.crt holds the signing roots (PEM), and any other key is a legacy
// key ID mapped to a base64 Ed25519 public key, for records signed before
// signers had certificates.
const KeysConfigMap = "c4-provenance"

// trustSet is what records are checked against.
type trustSet struct {
	roots  *x509.CertPool
	legacy map[string]ed25519.PublicKey
}

// trust reads the farm's trust bundle (cached for a minute).
func (s *Server) trust(ctx context.Context) trustSet {
	s.mu.RLock()
	if s.trusted.legacy != nil && time.Now().Before(s.keysUntil) {
		t := s.trusted
		s.mu.RUnlock()
		return t
	}
	s.mu.RUnlock()
	t := trustSet{roots: x509.NewCertPool(), legacy: map[string]ed25519.PublicKey{}}
	if s.Kube != nil {
		if cm, err := s.Kube.CoreV1().ConfigMaps(s.Namespace).Get(ctx, KeysConfigMap, metav1.GetOptions{}); err == nil {
			for id, text := range cm.Data {
				if id == "ca.crt" {
					t.roots.AppendCertsFromPEM([]byte(text))
					continue
				}
				if pub, err := provenance.ParsePublic(text); err == nil && provenance.KeyID(pub) == id {
					t.legacy[id] = pub
				}
			}
		}
	}
	s.mu.Lock()
	s.trusted, s.keysUntil = t, time.Now().Add(time.Minute)
	s.mu.Unlock()
	return t
}

// verify reads a record from a project's store and checks its signature,
// its certificate chain and the farm's signer policy.
func (s *Server) verify(ctx context.Context, project, recordID string) (*provenance.Verified, error) {
	if s.Stores == nil {
		return nil, errors.New("no stores")
	}
	st, err := s.Stores(ctx, project)
	if err != nil || st == nil {
		return nil, fmt.Errorf("project %s store: %v", project, err)
	}
	id, err := c4.Parse(recordID)
	if err != nil {
		return nil, fmt.Errorf("record %q is not a C4 ID", recordID)
	}
	data, err := c4view.ReadObject(st, id)
	if err != nil {
		return nil, fmt.Errorf("record %s: %w", recordID, err)
	}
	t := s.trust(ctx)
	v, err := provenance.VerifyRecord(data, t.roots, t.legacy)
	if err != nil {
		return nil, err
	}
	return v, nil
}

// traced is a verified task record and what it implies.
type traced struct {
	Record   string `json:"record"`
	Verified bool   `json:"verified"`
	// Signer is the identity that signed: this node's URI, or key:ID for
	// a record signed with a farm key before signers had certificates.
	Signer string `json:"signer,omitempty"`
	// Policy is empty when the signer is the one the farm's policy wants
	// for this record (the node the record says the task ran on).
	Policy    string                `json:"policy,omitempty"`
	SignedAt  *time.Time            `json:"signedAt,omitempty"`
	Problem   string                `json:"problem,omitempty"`
	Statement *provenance.Statement `json:"statement,omitempty"`
	File      *fileRef              `json:"file,omitempty"`
	// Lineage names the jobs whose outputs this task's inputs were.
	Lineage []map[string]any `json:"lineage"`
	C2PA    map[string]any   `json:"c2pa,omitempty"`
	OMC     map[string]any   `json:"omc,omitempty"`
}

type fileRef struct {
	Path string `json:"path"`
	ID   string `json:"id"`
}

// trace reads and verifies one task record.
func (s *Server) trace(ctx context.Context, m *model, rec *jobRec, recordID, path string) (*traced, error) {
	out := &traced{Record: recordID, Lineage: []map[string]any{}}
	v, err := s.verify(ctx, rec.project, recordID)
	if err != nil {
		out.Problem = err.Error()
		return out, nil
	}
	stmt, err := taskStatement(v)
	if err != nil {
		out.Problem = err.Error()
		return out, nil
	}
	at := v.SignedAt
	out.Verified, out.Signer, out.Statement, out.SignedAt = true, v.Identity, stmt, &at
	if err := provenance.CheckSigner(v); err != nil {
		out.Policy = err.Error()
	}
	for _, in := range stmt.Predicate.Inputs {
		l := map[string]any{"materialization": in.Materialization, "view": in.View}
		for _, other := range m.jobs {
			if o := other.job.Status.Output; o != nil && o.View == in.View {
				l["producedBy"] = other.key
			}
		}
		out.Lineage = append(out.Lineage, l)
	}
	if path != "" {
		for _, sub := range stmt.Subject {
			if sub.Name == path {
				out.File = &fileRef{Path: path, ID: sub.Digest["c4"]}
				out.C2PA = provenance.C2PAManifest(stmt, recordID, path, out.File.ID)
				out.OMC = provenance.OMCAsset(stmt, recordID, path, out.File.ID)
			}
		}
	}
	return out, nil
}

// index reads a job's provenance index.
func (s *Server) index(ctx context.Context, rec *jobRec) (*queue.Index, error) {
	o := rec.job.Status.Output
	if o == nil || o.Provenance == "" || s.Stores == nil {
		return nil, nil
	}
	st, err := s.Stores(ctx, rec.project)
	if err != nil || st == nil {
		return nil, err
	}
	id, err := c4.Parse(o.Provenance)
	if err != nil {
		return nil, err
	}
	data, err := c4view.ReadObject(st, id)
	if err != nil {
		return nil, err
	}
	var ix queue.Index
	return &ix, json.Unmarshal(data, &ix)
}

// handleTrace is GET /trace/{ns}/{name}?path=P or ?unit=N: the signed task
// record of the unit that wrote a file of the job's output (or of a unit),
// verified against the farm's roots and signer policy, with its lineage and the file's C2PA
// manifest definition and OMC asset.
func (s *Server) handleTrace(w http.ResponseWriter, r *http.Request) {
	m, rec, _ := s.job(w, r)
	if rec == nil {
		return
	}
	ix, err := s.index(r.Context(), rec)
	if err != nil {
		fail(w, http.StatusServiceUnavailable, "provenance index: %v", err)
		return
	}
	if ix == nil {
		fail(w, http.StatusNotFound, "%s has no task records yet: none of its units has finished with signing on", rec.key)
		return
	}
	path := r.URL.Query().Get("path")
	recordID := ""
	switch {
	case path != "":
		recordID = ix.Records[path]
		if recordID == "" {
			var some []string
			for p := range ix.Records {
				some = append(some, p)
			}
			sort.Strings(some)
			fail(w, http.StatusNotFound, "%s wrote no file %q; it wrote %d files, for example %v", rec.key, path, len(some), some[:min(5, len(some))])
			return
		}
	default:
		u, err := strconv.Atoi(r.URL.Query().Get("unit"))
		if err != nil {
			fail(w, http.StatusBadRequest, "give path= (a file the job wrote) or unit= (a unit index)")
			return
		}
		recordID = ix.Units[u]
		if recordID == "" {
			fail(w, http.StatusNotFound, "unit %d of %s has no task record", u, rec.key)
			return
		}
	}
	t, err := s.trace(r.Context(), m, rec, recordID, path)
	if err != nil {
		fail(w, http.StatusServiceUnavailable, "%v", err)
		return
	}
	writeJSON(w, http.StatusOK, t)
}

// taskStatement reads a verified task record as a task Statement.
func taskStatement(v *provenance.Verified) (*provenance.Statement, error) {
	if v.PredicateType != provenance.PredicateTask {
		return nil, fmt.Errorf("a %s record, not a task record", v.PredicateType)
	}
	st := &provenance.Statement{Type: provenance.StatementType, Subject: v.Subjects, PredicateType: v.PredicateType}
	return st, json.Unmarshal(v.Predicate, &st.Predicate)
}
