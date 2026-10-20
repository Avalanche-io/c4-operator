package queue

import (
	"context"
	"encoding/json"
	"sort"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
)

// Steering (agent-catalog G-steer): a message to a unit while it runs,
// without ending it. A person sends it through the HUD or MCP (signed, as
// a request in AnnotationSteer); the queue numbers it for its unit and
// puts every message the unit has not read on the unit's pod
// (AnnotationUnitSteer). The node daemon, or the host runner, writes them
// to the pod's control directory, where the unit reads them at its safe
// points (c4-unit steer) and says how far it read (.c4/steer-seen, relayed
// as AnnotationSteerSeen). A unit that is not running gets its unread
// messages on its next pod. Messages are text: a running pod cannot mount
// a new view.

// maxSteersKept bounds the messages a job keeps.
const maxSteersKept = 64

type steerRec struct {
	unit, seq int
	actor     string
	text      string
	record    string
	at        time.Time
	delivered time.Time
	seen      bool
}

// unitSteer is a message as the unit's pod carries it.
type unitSteer struct {
	Seq   int       `json:"seq"`
	At    time.Time `json:"at"`
	Actor string    `json:"actor,omitempty"`
	Text  string    `json:"text"`
}

// applySteers carries out the job's new messages, in token order, each
// once.
func applySteers(j *c4v1.WorkJob, m *jobMem) {
	a := j.Annotations[c4v1.AnnotationSteer]
	if a == "" || m.plan == nil {
		return
	}
	var reqs []c4v1.WorkSteerRequest
	if err := json.Unmarshal([]byte(a), &reqs); err != nil {
		return
	}
	sort.SliceStable(reqs, func(x, y int) bool { return tokenValue(reqs[x].Token) < tokenValue(reqs[y].Token) })
	last := tokenValue(m.steered)
	for _, r := range reqs {
		if tokenValue(r.Token) <= last {
			continue
		}
		m.steered, last = r.Token, tokenValue(r.Token)
		i, ok := unitByRef(m.plan, r.Unit)
		if !ok || r.Text == "" || len(r.Text) > c4v1.MaxSteerText {
			continue
		}
		seq := 1
		for _, s := range m.steers {
			if s.unit == i {
				seq = max(seq, s.seq+1)
			}
		}
		m.steers = append(m.steers, &steerRec{unit: i, seq: seq, actor: r.Actor, text: r.Text, record: r.Record, at: r.At.Time})
	}
	if n := len(m.steers); n > maxSteersKept {
		m.steers = m.steers[n-maxSteersKept:]
	}
}

// unread is unit i's messages its unit has not read, in order.
func (m *jobMem) unread(i int) []*steerRec {
	var out []*steerRec
	for _, s := range m.steers {
		if s.unit == i && !s.seen {
			out = append(out, s)
		}
	}
	return out
}

// steerAnnotation is the AnnotationUnitSteer value carrying msgs, "" for
// none.
func steerAnnotation(msgs []*steerRec) string {
	if len(msgs) == 0 {
		return ""
	}
	out := make([]unitSteer, len(msgs))
	for k, s := range msgs {
		out[k] = unitSteer{Seq: s.seq, At: s.at.UTC(), Actor: s.actor, Text: s.text}
	}
	b, _ := json.Marshal(out)
	return string(b)
}

// deliverSteers notes what each running unit read, and puts its unread
// messages on its pod.
func (q *Queue) deliverSteers(ctx context.Context, now time.Time, views []*view) {
	for _, v := range views {
		m := v.mem
		if len(m.steers) == 0 {
			continue
		}
		for idx, pod := range v.pods {
			if _, running := v.running[idx]; !running || pod.DeletionTimestamp != nil {
				continue
			}
			if seen, err := strconv.Atoi(pod.Annotations[c4v1.AnnotationSteerSeen]); err == nil {
				for _, s := range m.steers {
					if s.unit == idx && s.seq <= seen {
						s.seen = true
					}
				}
			}
			msgs := m.unread(idx)
			want := steerAnnotation(msgs)
			if pod.Annotations[c4v1.AnnotationUnitSteer] == want {
				continue
			}
			patch := client.MergeFrom(pod.DeepCopy())
			if pod.Annotations == nil {
				pod.Annotations = map[string]string{}
			}
			if want == "" {
				delete(pod.Annotations, c4v1.AnnotationUnitSteer)
			} else {
				pod.Annotations[c4v1.AnnotationUnitSteer] = want
			}
			if err := q.Client.Patch(ctx, pod, patch); err != nil {
				q.Log.Error(err, "steer", "job", v.key, "unit", idx)
				continue
			}
			for _, s := range msgs {
				if s.delivered.IsZero() {
					s.delivered = now
				}
			}
		}
	}
}

// steerPod gives a new pod of unit idx the messages its unit has not read.
func steerPod(m *jobMem, pod *corev1.Pod, idx int, now time.Time) {
	msgs := m.unread(idx)
	if len(msgs) == 0 {
		return
	}
	pod.Annotations[c4v1.AnnotationUnitSteer] = steerAnnotation(msgs)
	for _, s := range msgs {
		if s.delivered.IsZero() {
			s.delivered = now
		}
	}
}

// steerStatus lists the job's messages for status.steers.
func steerStatus(m *jobMem) []c4v1.WorkSteer {
	var out []c4v1.WorkSteer
	for _, s := range m.steers {
		w := c4v1.WorkSteer{Unit: int32(s.unit), Seq: int32(s.seq), Actor: s.actor, At: metav1.Time{Time: s.at.Truncate(time.Second)},
			Text: s.text, Record: s.record, Seen: s.seen}
		if m.plan != nil && s.unit < len(m.plan.Units) {
			w.Name = m.plan.Units[s.unit].Name
		}
		if !s.delivered.IsZero() {
			w.Delivered = &metav1.Time{Time: s.delivered.Truncate(time.Second)}
		}
		out = append(out, w)
	}
	return out
}

// loadSteers rebuilds the messages from status.
func (m *jobMem) loadSteers(st *c4v1.WorkJobStatus) {
	m.steered = st.Steered
	for _, w := range st.Steers {
		s := &steerRec{unit: int(w.Unit), seq: int(w.Seq), actor: w.Actor, text: w.Text, record: w.Record, at: w.At.Time, seen: w.Seen}
		if w.Delivered != nil {
			s.delivered = w.Delivered.Time
		}
		m.steers = append(m.steers, s)
	}
}
