package server

import (
	"context"
	"sync"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
)

// freshMax bounds the jobs read past the model; past it the oldest go,
// and a read past the model plans its job again.
const freshMax = 256

// freshJobs holds jobs read from the API server before the model has
// collected them, by key, one record per resource version: a burst of
// reads of a job just made plans it once.
type freshJobs struct {
	mu   sync.Mutex
	recs map[string]*jobRec
	keys []string // oldest first
}

// lookup finds a job by namespace and name: the model's record, else, for
// a job made since the model was collected, one read from the API server,
// so a job just made is answered at once and never as missing. A nil
// record with a nil error is a job that does not exist.
func (s *Server) lookup(ctx context.Context, m *model, ns, name string) (*jobRec, error) {
	if rec := m.byKey[ns+"/"+name]; rec != nil {
		return rec, nil
	}
	var j c4v1.WorkJob
	if err := s.reader().Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &j); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return s.fresh.record(s, m, j), nil
}

func (f *freshJobs) record(s *Server, m *model, j c4v1.WorkJob) *jobRec {
	key := j.Namespace + "/" + j.Name
	f.mu.Lock()
	defer f.mu.Unlock()
	if rec := f.recs[key]; rec != nil && rec.job.ResourceVersion == j.ResourceVersion {
		return rec
	}
	if f.recs == nil {
		f.recs = map[string]*jobRec{}
	}
	// what the model holds now is read from it
	kept := f.keys[:0]
	for _, k := range f.keys {
		if m.byKey[k] != nil || k == key {
			delete(f.recs, k)
			continue
		}
		kept = append(kept, k)
	}
	f.keys = kept
	for len(f.keys) >= freshMax {
		delete(f.recs, f.keys[0])
		f.keys = f.keys[1:]
	}
	rec := s.jobRecord(m, nil, j)
	rec.fresh = true
	rec.work, rec.hasWork = jobWork(m, rec), true
	f.recs[key] = rec
	f.keys = append(f.keys, key)
	return rec
}
