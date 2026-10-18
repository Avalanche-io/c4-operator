package tier

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/Avalanche-io/c4"

	"github.com/Avalanche-io/c4-operator/internal/fill"
	"github.com/Avalanche-io/c4-operator/internal/objcache"
)

// HeaderCached set to "only" on a HEAD asks whether this tier holds the
// object itself: 404 on a miss, never the store's answer. Staging uses it
// to learn what a site already has.
const HeaderCached = "X-C4-Cached"

// WarmPolicy admits POST /warm: the operator asking this tier (a site's
// regional replica) to fetch objects ahead of work staged to its site.
// Warming spends money at the sending end (egress), so it is never
// anonymous: the request must pass Authorize and its caller must be one of
// Callers (the operator's own identity). Each UTC day it may fetch at most
// BytesPerDay, at most BytesPerSecond at a time, and every request is
// recorded, admitted or refused.
type WarmPolicy struct {
	// Callers are the identities (Authorize's caller, such as
	// system:serviceaccount:c4-system:c4-manager) that may warm.
	Callers []string
	// BytesPerDay bounds what warms fetch per UTC day (0: none may).
	BytesPerDay int64
	// BytesPerSecond paces warm fetches (0: unpaced).
	BytesPerSecond int64
	// Record receives every request: who asked, the objects, the bytes,
	// and why it was refused if it was. Nil records to the log only.
	Record func(WarmRecord)

	mu    sync.Mutex
	day   string
	spent int64
	queue chan warmJob
	once  sync.Once
	now   func() time.Time
}

// WarmRecord is one warm request.
type WarmRecord struct {
	At      time.Time `json:"at"`
	Caller  string    `json:"caller"`
	Project string    `json:"project"`
	IDs     []string  `json:"ids"`
	Bytes   int64     `json:"bytes"`
	Refused string    `json:"refused,omitempty"`
}

// WarmRequest is POST /warm's body: the objects to fetch and their sizes
// (from the views that list them). Objects the tier holds are skipped and
// not charged.
type WarmRequest struct {
	Objects []WarmObject `json:"objects"`
}

// WarmObject is one object to fetch.
type WarmObject struct {
	ID   string `json:"id"`
	Size int64  `json:"size"`
}

// WarmResponse says what the tier will fetch.
type WarmResponse struct {
	Fetching int   `json:"fetching"`
	Bytes    int64 `json:"bytes"`
	Held     int   `json:"held"`
}

type warmJob struct {
	project, partition string
	cache              *objcache.Cache
	ids                []c4.ID
}

func (p *WarmPolicy) clock() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now()
}

func (p *WarmPolicy) allows(caller string) bool {
	for _, c := range p.Callers {
		if c == caller {
			return true
		}
	}
	return false
}

// charge takes bytes from today's budget, or says why it cannot.
func (p *WarmPolicy) charge(bytes int64) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	day := p.clock().UTC().Format("2006-01-02")
	if day != p.day {
		p.day, p.spent = day, 0
	}
	if p.spent+bytes > p.BytesPerDay {
		return fmt.Sprintf("over today's warm budget (%d of %d bytes used)", p.spent, p.BytesPerDay)
	}
	p.spent += bytes
	return ""
}

// warm serves POST /warm.
func (s *Server) warm(w http.ResponseWriter, r *http.Request, caller, project, partition string, cache *objcache.Cache) {
	p := s.Warm
	rec := WarmRecord{At: time.Now().UTC(), Caller: caller, Project: project}
	refuse := func(status int, why string) {
		rec.Refused = why
		s.recordWarm(rec)
		http.Error(w, why, status)
	}
	switch {
	case p == nil:
		refuse(http.StatusNotFound, "this tier does not warm")
		return
	case s.Authorize == nil || caller == "":
		refuse(http.StatusForbidden, "warming requires an authenticated caller")
		return
	case !p.allows(caller):
		refuse(http.StatusForbidden, caller+" may not warm this tier")
		return
	}
	var req WarmRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<20)).Decode(&req); err != nil {
		refuse(http.StatusBadRequest, "invalid warm request: "+err.Error())
		return
	}
	var ids []c4.ID
	var bytes int64
	held := 0
	for _, o := range req.Objects {
		id, err := c4.Parse(o.ID)
		if err != nil || o.Size < 0 {
			refuse(http.StatusBadRequest, "invalid object "+o.ID)
			return
		}
		rec.IDs = append(rec.IDs, o.ID)
		if cache.Has(id) {
			held++
			continue
		}
		ids = append(ids, id)
		bytes += o.Size
	}
	rec.Bytes = bytes
	if why := p.charge(bytes); why != "" {
		refuse(http.StatusTooManyRequests, why)
		return
	}
	s.recordWarm(rec)
	p.once.Do(func() {
		p.queue = make(chan warmJob, 256)
		go s.warmWorker()
	})
	select {
	case p.queue <- warmJob{project: project, partition: partition, cache: cache, ids: ids}:
	default:
		http.Error(w, "warm queue full", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(WarmResponse{Fetching: len(ids), Bytes: bytes, Held: held})
}

func (s *Server) recordWarm(rec WarmRecord) {
	s.Log.Info("warm", "caller", rec.Caller, "project", rec.Project, "objects", len(rec.IDs), "bytes", rec.Bytes, "refused", rec.Refused)
	if s.Warm != nil && s.Warm.Record != nil {
		s.Warm.Record(rec)
	}
}

// warmWorker fetches warm requests one object at a time, paced to
// BytesPerSecond, through the same fills as GET so a warm and a read of the
// same object share one fetch.
func (s *Server) warmWorker() {
	p := s.Warm
	for job := range p.queue {
		for _, id := range job.ids {
			if job.cache.Has(id) {
				continue
			}
			start := time.Now()
			// The route is the one a GET of the object would take.
			req, _ := http.NewRequest(http.MethodGet, "/"+id.String(), nil)
			req.Header.Set(HeaderProject, job.project)
			route := s.Route(req, job.project, id)
			if route == nil {
				s.Log.V(1).Info("warm skipped: no route", "id", id.String())
				continue
			}
			key := job.partition + "/" + id.String()
			flight, _, err := s.Fills.Join(key, job.cache.TempFile, func(f *fill.Flight) {
				s.run(f, job.project, id, job.cache, route)
			})
			if err != nil {
				s.Log.V(1).Info("warm skipped", "id", id.String(), "err", err.Error())
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
			err = flight.Wait(ctx)
			cancel()
			flight.Release()
			if err != nil && !errors.Is(err, context.DeadlineExceeded) {
				s.Log.V(1).Info("warm fetch failed", "id", id.String(), "err", err.Error())
			}
			if p.BytesPerSecond > 0 {
				if n, ok := job.cache.Stat(id); ok {
					if d := time.Duration(float64(n)/float64(p.BytesPerSecond)*float64(time.Second)) - time.Since(start); d > 0 {
						time.Sleep(d)
					}
				}
			}
		}
	}
}
