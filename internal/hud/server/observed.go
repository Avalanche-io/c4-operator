package server

import (
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"

	"github.com/Avalanche-io/c4-operator/hud/api"
)

// Observed sessions: each host runner reports the Claude Code sessions on
// its machine (c4-agent observe), farm members or not, so the person whose
// machine it is sees them beside the farm's work. The rows quote what a
// session asks and name the person's folders, so only the runner's owners
// read them (its --owner users, who may be several), and of those only
// people who also own a pool the node is in, so a runner cannot put rows
// before someone who never took its node. No one else sees them, not a
// farm admin, not as a count. They live in memory only, never in a
// resource or the book, and are served only in a viewer's own answers
// (the snapshot, needs-me, MCP through them); nothing that leaves the
// farm, such as a diagnostic bundle, may read them.

const (
	// observedFor is how long a report stands: the runner posts every
	// minute, so one that stops leaves nothing stale.
	observedFor = 5 * time.Minute
	// observedMax is the most sessions one report may carry.
	observedMax = 200
	// observedBody is the largest report body.
	observedBody = 4 << 20
)

type observedReport struct {
	// owners are the runner's own (its --owner users).
	owners     []string
	machine    string
	observedAt time.Time
	postedAt   time.Time
	sessions   []api.ObservedSession
}

// handleObserved is POST /nodes/{node}/observed: a host runner's report of
// its machine's sessions, as its own node only. It answers whom they are
// shown to; an empty list says the node is in no pool with owners.
func (s *Server) handleObserved(w http.ResponseWriter, r *http.Request) {
	v := s.withViewer(w, r)
	if v == nil {
		return
	}
	node := r.PathValue("node")
	host, ok := strings.CutPrefix(v.User, HostAccountPrefix)
	if !ok || host != node {
		fail(w, http.StatusForbidden, "only node %s's own host runner reports its sessions", node)
		return
	}
	var body api.ObservedReport
	if err := decodeBodyUpTo(r, &body, observedBody); err != nil {
		failBody(w, err)
		return
	}
	if len(body.Sessions) > observedMax {
		fail(w, http.StatusRequestEntityTooLarge, "%d sessions: at most %d in one report", len(body.Sessions), observedMax)
		return
	}
	now := time.Now().UTC()
	s.obsMu.Lock()
	if s.observed == nil {
		s.observed = map[string]observedReport{}
	}
	s.observed[node] = observedReport{owners: body.Owners, machine: body.Machine, observedAt: body.ObservedAt.UTC(), postedAt: now,
		sessions: body.Sessions}
	s.obsMu.Unlock()
	shown := shownTo(body.Owners, nodeOwners(s.current(), node))
	out := map[string]any{"shownTo": shown}
	switch {
	case len(body.Owners) == 0:
		out["why"] = "the runner names no owner: start it with --owner"
	case len(shown) == 0:
		out["why"] = "its node is in no pool its owners own"
	}
	writeJSON(w, http.StatusOK, out)
}

// shownTo are the runner's owners who also own a pool its node is in,
// sorted; never nil.
func shownTo(runner, pools []string) []string {
	out := []string{}
	for _, o := range runner {
		if slices.Contains(pools, o) && !slices.Contains(out, o) {
			out = append(out, o)
		}
	}
	sort.Strings(out)
	return out
}

// nodeOwners are the owners of the pools node is in (Pool spec.owners,
// over the pools whose selector matches it), sorted; never nil.
func nodeOwners(m *model, node string) []string {
	out := []string{}
	if m == nil {
		return out
	}
	var n *corev1.Node
	for i := range m.nodes {
		if m.nodes[i].Name == node {
			n = &m.nodes[i]
		}
	}
	if n == nil {
		return out
	}
	for _, p := range m.pools {
		if len(p.Spec.Owners) == 0 || len(p.Spec.NodeSelector) == 0 {
			continue
		}
		if !labels.SelectorFromSet(p.Spec.NodeSelector).Matches(labels.Set(n.Labels)) {
			continue
		}
		for _, o := range p.Spec.Owners {
			if !slices.Contains(out, o) {
				out = append(out, o)
			}
		}
	}
	sort.Strings(out)
	return out
}

// inUse are the words of a session in use outside the queue: it holds the
// machine's attention or its model account, whatever the farm does.
var inUse = map[string]bool{"working": true, "needs you": true, "at its limit": true}

// usedOutside counts, by node, the observed sessions in use outside the
// queue, from reports that still stand. Only a count leaves here, for any
// viewer of the node: it names no one. The queue never reads it.
func (s *Server) usedOutside(now time.Time) map[string]int {
	out := map[string]int{}
	s.obsMu.Lock()
	defer s.obsMu.Unlock()
	for node, rep := range s.observed {
		if now.Sub(rep.postedAt) > observedFor {
			continue
		}
		for _, o := range rep.sessions {
			if inUse[o.Word] {
				out[node]++
			}
		}
	}
	return out
}

// observedSessions are the sessions v may see: those of every node whose
// runner v owns (and v owns one of its pools), from reports that still
// stand, by node then session.
func (s *Server) observedSessions(m *model, v *api.Viewer, now time.Time) []api.ObservedSession {
	if v == nil || v.Unit != nil || v.User == "" {
		return nil
	}
	s.obsMu.Lock()
	defer s.obsMu.Unlock()
	var nodes []string
	for node, rep := range s.observed {
		if now.Sub(rep.postedAt) > observedFor {
			delete(s.observed, node)
			continue
		}
		nodes = append(nodes, node)
	}
	sort.Strings(nodes)
	var out []api.ObservedSession
	for _, node := range nodes {
		rep := s.observed[node]
		owners := shownTo(rep.owners, nodeOwners(m, node))
		if !slices.Contains(owners, v.User) {
			continue
		}
		for _, o := range rep.sessions {
			o.Machine, o.Node, o.Owners, o.ObservedAt, o.PostedAt = rep.machine, node, owners, rep.observedAt, rep.postedAt
			out = append(out, o)
		}
	}
	return out
}
