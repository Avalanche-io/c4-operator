package server

// GET /book?since=<seq>: the transition log (internal/work/queue/book.go,
// integrations GAPS 5), for a client that was away to learn what changed,
// in order. Any replica serves it: the queue alone writes the log, and
// sealed segments never change, so they are cached here by C4 ID.

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"sync"

	"github.com/Avalanche-io/c4"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Avalanche-io/c4-operator/hud/api"
	c4view "github.com/Avalanche-io/c4-operator/internal/view"
	"github.com/Avalanche-io/c4-operator/internal/work/queue"
)

var segments sync.Map // C4 ID -> *queue.BookSegmentObject

func (s *Server) segment(ctx context.Context, project, id string) (*queue.BookSegmentObject, error) {
	if seg, ok := segments.Load(id); ok {
		return seg.(*queue.BookSegmentObject), nil
	}
	cid, err := c4.Parse(id)
	if err != nil {
		return nil, err
	}
	st, err := s.Stores(ctx, project)
	if err != nil {
		return nil, err
	}
	raw, err := c4view.ReadObject(st, cid)
	if err != nil {
		return nil, err
	}
	var seg queue.BookSegmentObject
	if err := json.Unmarshal(raw, &seg); err != nil {
		return nil, err
	}
	segments.Store(id, &seg)
	return &seg, nil
}

func (s *Server) handleTransitions(w http.ResponseWriter, r *http.Request, v *api.Viewer, since int64, limit int) {
	ctx := r.Context()
	var cm corev1.ConfigMap
	err := s.Client.Get(ctx, client.ObjectKey{Namespace: s.Namespace, Name: queue.BookConfigMap}, &cm)
	if apierrors.IsNotFound(err) {
		writeJSON(w, http.StatusOK, api.Transitions{Entries: []api.Transition{}, Next: since})
		return
	}
	if err != nil {
		fail(w, http.StatusServiceUnavailable, "the transition log: %v", err)
		return
	}
	h, err := queue.ReadBookHead(cm.Data)
	if err != nil {
		fail(w, http.StatusInternalServerError, "the transition log: %v", err)
		return
	}
	projects := map[string]bool{}
	for p := range h.Tails {
		projects[p] = true
	}
	for p := range h.Segments {
		projects[p] = true
	}
	var out []api.Transition
	for p := range projects {
		if !v.Sees(p) {
			continue
		}
		if h.Dropped[p] && since+1 < h.Oldest(p) {
			writeJSON(w, http.StatusGone, map[string]any{"error": "the log no longer reaches " + strconv.FormatInt(since+1, 10) + " for project " + p + ": read the jobs again, then go on from oldest",
				"oldest": h.Oldest(p)})
			return
		}
		for _, sg := range h.Segments[p] {
			if sg.Last <= since {
				continue
			}
			seg, err := s.segment(ctx, p, sg.ID)
			if err != nil {
				fail(w, http.StatusServiceUnavailable, "the transition log of %s: %v", p, err)
				return
			}
			for _, t := range seg.Entries {
				if t.Seq > since {
					out = append(out, t)
				}
			}
		}
		for _, t := range h.Tails[p] {
			if t.Seq > since {
				out = append(out, t)
			}
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Seq < out[b].Seq })
	res := api.Transitions{Entries: out, Next: max(since, h.Next-1)}
	if len(out) > limit {
		res.Entries, res.Next, res.More = out[:limit], out[limit-1].Seq, true
	}
	if res.Entries == nil {
		res.Entries = []api.Transition{}
	}
	writeJSON(w, http.StatusOK, res)
}
