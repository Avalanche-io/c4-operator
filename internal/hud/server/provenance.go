package server

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
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

// KeysConfigMap holds the farm's trusted task-record keys: key ID to
// base64 Ed25519 public key, in the operator's namespace.
const KeysConfigMap = "c4-provenance"

// trustedKeys reads the farm's public keys (cached for a minute).
func (s *Server) trustedKeys(ctx context.Context) map[string]ed25519.PublicKey {
	s.mu.RLock()
	if s.keys != nil && time.Now().Before(s.keysUntil) {
		k := s.keys
		s.mu.RUnlock()
		return k
	}
	s.mu.RUnlock()
	out := map[string]ed25519.PublicKey{}
	if s.Kube != nil {
		if cm, err := s.Kube.CoreV1().ConfigMaps(s.Namespace).Get(ctx, KeysConfigMap, metav1.GetOptions{}); err == nil {
			for id, text := range cm.Data {
				if pub, err := provenance.ParsePublic(text); err == nil && provenance.KeyID(pub) == id {
					out[id] = pub
				}
			}
		}
	}
	s.mu.Lock()
	s.keys, s.keysUntil = out, time.Now().Add(time.Minute)
	s.mu.Unlock()
	return out
}

// traced is a verified task record and what it implies.
type traced struct {
	Record    string                `json:"record"`
	Verified  bool                  `json:"verified"`
	Signer    string                `json:"signer,omitempty"`
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
	st, err := s.Stores(ctx, rec.project)
	if err != nil || st == nil {
		return nil, err
	}
	id, err := c4.Parse(recordID)
	if err != nil {
		return nil, err
	}
	data, err := c4view.ReadObject(st, id)
	if err != nil {
		return nil, err
	}
	out := &traced{Record: recordID, Lineage: []map[string]any{}}
	stmt, signer, err := provenance.Verify(data, s.trustedKeys(ctx))
	if err != nil {
		out.Problem = err.Error()
		return out, nil
	}
	out.Verified, out.Signer, out.Statement = true, signer, stmt
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
// verified against the farm's keys, with its lineage and the file's C2PA
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
