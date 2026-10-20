package queue

// Cause groups (DESIGN.md 10.14, Causes). A job's failures and flags, grouped
// by a cause the queue has the facts for: a unit's exit (code, the
// container's reason, the first line of its message), a node that failed
// the job's units up to its limit, a review's reason, information a brief
// lacked. Each group keeps the units it touched, its first and last time,
// and what the farm did, so the HUD shows one incident per cause and a
// tracker one comment per cause, updated as it grows. Status is the only
// record: it is loaded back after a restart.

import (
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/work/order"
)

const (
	causesKept   = 16
	causeNodes   = 4
	causeTitle   = 120
	causeUnitSet = 256
)

type causeRec struct {
	c4v1.WorkCause
	units map[int]bool
}

var causeDigits = regexp.MustCompile(`\d+`)

// causeText is a message as a cause sees it: its first line, numbers
// blurred, bounded, so the same failure on frame 1001 and 1002 is one cause.
func causeText(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	s = causeDigits.ReplaceAllString(s, "#")
	if len(s) > 80 {
		s = s[:80]
	}
	return s
}

// exitCause is the cause of a failed pod: the first container that ended
// in error (a setup container's marked so), its code, its reason
// (OOMKilled, Error) and the first line of its message; else the pod's
// own reason.
func exitCause(p *corev1.Pod) (key, title string) {
	look := func(sts []corev1.ContainerStatus, setup bool) (string, string, bool) {
		for _, s := range sts {
			t := s.State.Terminated
			if t == nil {
				t = s.LastTerminationState.Terminated
			}
			if t == nil || t.ExitCode == 0 {
				continue
			}
			msg := causeText(t.Message)
			reason := t.Reason
			prefix := ""
			if setup {
				prefix = "setup "
			}
			key := fmt.Sprintf("exit:%s%d:%s", prefix, t.ExitCode, reason)
			title := fmt.Sprintf("%sexit %d", prefix, t.ExitCode)
			if reason != "" && reason != "Error" {
				title += " " + reason
			}
			if msg != "" && (reason == "" || reason == "Error") {
				key += ":" + msg
				title += ": " + msg
			}
			return key, title, true
		}
		return "", "", false
	}
	if k, t, ok := look(p.Status.InitContainerStatuses, true); ok {
		return k, t
	}
	if k, t, ok := look(p.Status.ContainerStatuses, false); ok {
		return k, t
	}
	if r := p.Status.Reason; r != "" {
		return "exit:pod:" + r, "failed: " + r + causeSuffix(causeText(p.Status.Message))
	}
	return "exit:unknown", "failed (no exit recorded)"
}

func causeSuffix(s string) string {
	if s == "" {
		return ""
	}
	return ": " + s
}

// noteCause adds an attempt of unit to the cause, making it if new; did
// is what the farm did about it this time.
func (m *jobMem) noteCause(kind, key, title string, unit int, node string, at time.Time, did ...string) *causeRec {
	if m.causes == nil {
		m.causes = map[string]*causeRec{}
	}
	c := m.causes[key]
	if c == nil {
		if len(m.causes) >= causesKept {
			m.dropOldestCause()
		}
		if len(title) > causeTitle {
			title = title[:causeTitle]
		}
		c = &causeRec{WorkCause: c4v1.WorkCause{Kind: kind, Key: key, Title: title, Sample: int32(unit),
			First: metav1.NewTime(at.UTC().Truncate(time.Second))}, units: map[int]bool{}}
		m.causes[key] = c
	}
	c.units[unit] = true
	c.Count++
	c.Last = metav1.NewTime(at.UTC().Truncate(time.Second))
	if node != "" && !slices.Contains(c.Nodes, node) && len(c.Nodes) < causeNodes {
		c.Nodes = append(c.Nodes, node)
	}
	c.addDid(did...)
	return c
}

func (c *causeRec) addDid(what ...string) {
	for _, d := range what {
		if d != "" && !slices.Contains(c.Did, d) {
			c.Did = append(c.Did, d)
		}
	}
}

func (m *jobMem) dropOldestCause() {
	var oldest string
	for k, c := range m.causes {
		if oldest == "" || c.Last.Before(&m.causes[oldest].Last) {
			oldest = k
		}
	}
	delete(m.causes, oldest)
	m.causesDropped++
}

// causesDid marks what the farm (or a person) did about the given units
// on every cause that touched them (accepted, supplied).
func (m *jobMem) causesDid(units map[int]bool, what string) {
	for _, c := range m.causes {
		for u := range units {
			if c.units[u] {
				c.addDid(what)
				break
			}
		}
	}
}

// causeStatus is the job's causes, oldest first by their last time.
func (m *jobMem) causeStatus() []c4v1.WorkCause {
	if len(m.causes) == 0 {
		return nil
	}
	out := make([]c4v1.WorkCause, 0, len(m.causes))
	for _, c := range m.causes {
		w := c.WorkCause
		idx := make([]int, 0, len(c.units))
		for u := range c.units {
			idx = append(idx, u)
		}
		w.Units = order.EncodeSet(idx)
		if len(w.Units) > causeUnitSet {
			w.Units = fmt.Sprint(w.Sample) // too many to list: the count says how many
		}
		w.Nodes = slices.Clone(c.Nodes)
		w.Did = slices.Clone(c.Did)
		out = append(out, w)
	}
	sort.Slice(out, func(a, b int) bool {
		if !out[a].Last.Equal(&out[b].Last) {
			return out[a].Last.Before(&out[b].Last)
		}
		return out[a].Key < out[b].Key
	})
	return out
}

// loadCauses restores the causes from status after a restart.
func (m *jobMem) loadCauses(st *c4v1.WorkJobStatus) {
	m.causesDropped = int(st.CausesDropped)
	for _, w := range st.Causes {
		if m.causes == nil {
			m.causes = map[string]*causeRec{}
		}
		c := &causeRec{WorkCause: w, units: map[int]bool{}}
		c.Nodes, c.Did = slices.Clone(w.Nodes), slices.Clone(w.Did)
		if idx, err := order.DecodeSet(w.Units); err == nil {
			for _, u := range idx {
				c.units[u] = true
			}
		}
		m.causes[w.Key] = c
	}
}
