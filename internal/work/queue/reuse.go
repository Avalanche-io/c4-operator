package queue

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"sort"
	"strconv"
	"time"

	"github.com/Avalanche-io/c4-operator/internal/work/class"
	"github.com/Avalanche-io/c4-operator/work/order"
	"github.com/Avalanche-io/c4-operator/work/process"
)

// Reuse: C4-native memoization (INTEGRATE 3.4, where Argo's memoization
// is skipped: its key is a string the user builds). A procedural unit
// ready to run whose computation ran before in its project, the same
// process (images by digest, command, arguments, environment, platform)
// on the same input views, reuses that run's output instead of running:
// its changeset is folded into the job's output with the earlier run's
// task record, and the unit succeeds having held no slot. The book offers
// it only for a program declared exact, sealed, and verified by re-runs
// that wrote the same bytes (process.Book.ReuseOf), to a job that itself
// declares its process exact; a share of the
// opportunities (VerifyRate) runs anyway, so the declaration stays
// falsifiable. Each reuse is signed (decision/v1, kind reuse) and the unit
// is shown reused.
//
// Not reused: model units (their outputs are not functions of their
// inputs), generators and gates, and units whose run would differ from
// the recorded one in what the key does not see: inputs from other units,
// a rework's changes, a resumed attempt.

// reuseUnits reuses what it can among v's runnable units, and reports
// whether it reused any.
func (q *Queue) reuseUnits(ctx context.Context, v *view, now time.Time) bool {
	b := q.bookOf(v.project)
	if b == nil || b.Len() == 0 || v.driver == nil || v.blocked {
		return false
	}
	if process.ParseClass(v.job.Annotations[AnnotationDerivationClass]) != process.Exact {
		// Reuse is the job's to accept: it declares its process exact.
		return false
	}
	m, plan := v.mem, v.mem.plan
	var inID string
	resolved, reused := false, false
	for _, idx := range v.runnable {
		u := plan.Units[idx]
		if class.DoesOf(v.driver, u) != class.DoesProcedure || u.Generator || u.Gate != "" || len(u.Inputs) > 0 ||
			m.rework(idx) != nil || m.resume[idx] != "" || isWait(plan, idx) ||
			// what its process key does not see: a tool profile, supplied
			// information, a decision it resumes with
			u.Reasoning.Tools != "" || suppliedEnv(m, idx) != nil || m.blockDecision(idx) != nil {
			continue
		}
		cs := unitContainers(v.job, u, q.digests)
		proc, ok := process.ProcessID(cs, q.lastPlatform[v.key])
		if !ok {
			continue
		}
		program, _ := process.ProgramID(cs, q.lastPlatform[v.key], perUnit(u))
		if !b.Has(program) {
			continue
		}
		if !resolved {
			resolved = true
			inputs, ok := q.inputViews(ctx, v.job)
			if !ok {
				return reused
			}
			if inID, ok = process.InputsID(inputs); !ok {
				return reused
			}
		}
		if inID == "" {
			return reused
		}
		ru, ok := b.ReuseOf(process.Key{Process: proc, Inputs: inID}, program)
		if !ok || b.NeedsVerification(program, draw(unitID(v.key, idx), m.attempts[idx])) {
			continue
		}
		if !q.foldChangeset(ctx, v, idx, ru.Changeset, ru.Record) {
			continue
		}
		m.succeeded[idx] = true
		m.ended[idx] = now
		m.unitOut[idx] = ru.Output
		if m.reused == nil {
			m.reused = map[int]string{}
		}
		m.reused[idx] = ru.Output
		q.recordDecision(ctx, v, idx, "reuse", "the same computation ran before, exact and verified", "", now,
			map[string]any{"output": ru.Output, "program": program, "inputs": inID, "record": ru.Record})
		q.Log.Info("reused", "job", v.key, "unit", idx, "output", ru.Output)
		reused = true
	}
	return reused
}

// draw is a reproducible uniform draw for a unit's attempt, for the
// verification sample.
func draw(unit string, attempt int) float64 {
	h := sha256.Sum256([]byte(unit + "#" + strconv.Itoa(attempt)))
	return float64(binary.BigEndian.Uint64(h[:8])>>11) / (1 << 53)
}

func reusedStatus(m *jobMem) (string, int32) {
	var idx []int
	for i := range m.reused {
		if m.succeeded[i] {
			idx = append(idx, i)
		}
	}
	sort.Ints(idx)
	return order.EncodeSet(idx), int32(len(idx))
}
