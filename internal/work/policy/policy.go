// Package policy reads the farm's thresholds for how the queue treats
// people and agents: when an escalation turns pressing, how often a block
// re-checks its question, how many questions a unit may have open, who
// answers them. Every value is read through Value, which says where it
// came from (the farm's default, the job's Project, the job itself), so a
// decision a value made can cite it in its signed record and in the book.
//
// A job may tighten a value its project or the farm sets, never loosen it:
// a shorter pressAfter, a longer pressMargin or recheck.minInterval, a
// lower recheck.max, recheck.seconds or asks.max. A looser job value is
// ignored.
package policy

import (
	"strconv"
	"strings"
	"time"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
)

// The keys.
const (
	// PressAfter is how long an escalation to a person may stay open
	// before it is pressing, whatever the plan says (2h).
	PressAfter = "escalation.pressAfter"
	// PressMargin is how long before its answer-by time an escalation is
	// pressing (15m).
	PressMargin = "escalation.pressMargin"
	// RecheckMax is how many times a block may re-check one question (5).
	RecheckMax = "escalation.recheck.max"
	// RecheckMinInterval is the least time between two re-checks of one
	// question (10m).
	RecheckMinInterval = "escalation.recheck.minInterval"
	// RecheckSeconds is how long one re-check may run, in seconds (300):
	// the block's turn ($C4_RECHECK_SECONDS) and the pod's deadline.
	RecheckSeconds = "escalation.recheck.seconds"
	// UsageLimitProbe is how long a unit that hit a model's usage limit
	// waits before it tries again when the limit did not say when it
	// resets (30m).
	UsageLimitProbe = "usage.limitProbe"
	// YieldBackgroundWait is how long a member's yield waits for its
	// background work to report before it stops anyway
	// ($C4_YIELD_BACKGROUND_WAIT, 30m).
	YieldBackgroundWait = "yield.backgroundWait"
	// AsksMax is how many questions a unit may have open (3).
	AsksMax = "asks.max"
	// Answers is who answers a unit's questions, in order: unit names of
	// the job.
	Answers = "answers"
	// SubmissionPendingFor is how long a submission's jobs may stay held
	// while it makes the rest (10m): one still held past it was left by a
	// submission that never finished, and is deleted with its attempt.
	SubmissionPendingFor = "submission.pendingFor"
	// SubmissionSweepEvery is how often the HUD looks for such jobs (1m).
	SubmissionSweepEvery = "submission.sweepEvery"
	// SubmissionKeyFor is how long a submission's key answers a retry
	// (24h): after it, the same key makes a new submission.
	SubmissionKeyFor = "submission.keyFor"
)

// SubmissionPendingFloor is the least submission.pendingFor: the HUD's
// two minutes to make a submission and thirty seconds to roll it back,
// with room to spare. A smaller value is ignored.
const SubmissionPendingFloor = 3 * time.Minute

// RecheckInFlight is how many re-checks of one question may run at once:
// a rule, not a policy value.
const RecheckInFlight = 1

// From is where a value came from: its scope (farm, project, job or
// unit), what in that scope (the project's or job's name), and, once
// policies are objects, who set it and its record.
type From struct {
	Scope  string `json:"scope"`
	Ref    string `json:"ref,omitempty"`
	SetBy  string `json:"setBy,omitempty"`
	Record string `json:"record,omitempty"`
}

// Use is a value that decided something, for a signed record or a book
// entry: policy: [{key, value, from}].
type Use struct {
	Key   string `json:"key"`
	Value string `json:"value"`
	From  From   `json:"from"`
}

func (f From) String() string {
	if f.Ref == "" {
		return f.Scope
	}
	return f.Scope + " " + f.Ref
}

// tighter says whether a job's value a may replace b: a stricter value.
var tighter = map[string]func(a, b string) bool{
	PressAfter:          func(a, b string) bool { return dur(a) <= dur(b) },
	PressMargin:         func(a, b string) bool { return dur(a) >= dur(b) },
	RecheckMinInterval:  func(a, b string) bool { return dur(a) >= dur(b) },
	RecheckMax:          func(a, b string) bool { return num(a) <= num(b) },
	RecheckSeconds:      func(a, b string) bool { return num(a) <= num(b) },
	YieldBackgroundWait: func(a, b string) bool { return dur(a) <= dur(b) },
	AsksMax:             func(a, b string) bool { return num(a) <= num(b) },
}

var defaults = map[string]string{
	PressAfter:          "2h",
	PressMargin:         "15m",
	RecheckMax:          "5",
	RecheckMinInterval:  "10m",
	RecheckSeconds:      "300",
	UsageLimitProbe:     "30m",
	YieldBackgroundWait: "30m",
	AsksMax:             "3",
	Answers:             "",
	// A submission's, read for its project; the sweep's, for the farm.
	SubmissionPendingFor: "10m",
	SubmissionSweepEvery: "1m",
	SubmissionKeyFor:     "24h",
}

// limits are the farm's limits on a project's values, in each key's
// looser direction: a project may set a value up to its limit (a pressAfter
// of at most 24h, a recheck.max of at most 10), never past it; a value past
// it is ignored. A job only tightens, so needs none. SetLimits changes them
// for the farm.
var limits = map[string]string{
	PressAfter:         "24h",
	PressMargin:        "5m",
	RecheckMinInterval: "2m",
	RecheckMax:         "10",
	RecheckSeconds:     "900",
	AsksMax:            "10",
	// a member's slot is held while it waits: no project waits longer
	// than the farm
	YieldBackgroundWait: "30m",
}

// SetLimits sets the farm's limits on project values, by key; a malformed
// limit is ignored.
func SetLimits(l map[string]string) {
	for k, v := range l {
		if _, ok := limits[k]; ok && valid(k, v) {
			limits[k] = v
		}
	}
}

// Limit is the farm's limit on key's project value, or "" for none.
func Limit(key string) string { return limits[key] }

// withinLimit reports whether a project may set key to s.
func withinLimit(key, s string) bool {
	l, t := limits[key], tighter[key]
	return l == "" || t == nil || t(s, l)
}

// jobParam is where a job sets each key; the older names stay the
// params jobs already use.
var jobParam = map[string]string{
	PressAfter:          PressAfter,
	PressMargin:         PressMargin,
	RecheckMax:          RecheckMax,
	RecheckMinInterval:  RecheckMinInterval,
	RecheckSeconds:      RecheckSeconds,
	UsageLimitProbe:     UsageLimitProbe,
	YieldBackgroundWait: YieldBackgroundWait,
	AsksMax:             "maxAsks",
	Answers:             "answerer",
}

// Value is key's value for unit of job (unit -1 for the job), and where
// it came from: the farm's default, then the job's Project (its
// spec.work.policy.values, or spec.work.escalation) within the farm's
// limit, then the job, which may only tighten what is above it.
func Value(job *c4v1.WorkJob, project *c4v1.Project, unit int, key string) (string, From) {
	v, from := defaults[key], From{Scope: "farm"}
	if p := projectValue(project, key); p != "" && valid(key, p) && withinLimit(key, p) {
		v, from = p, From{Scope: "project", Ref: project.Name}
	}
	if job != nil {
		if s := strings.TrimSpace(job.Spec.Params[jobParam[key]]); s != "" && valid(key, s) {
			if t := tighter[key]; t == nil || t(s, v) {
				v, from = s, From{Scope: "job", Ref: job.Namespace + "/" + job.Name}
			}
		}
	}
	return v, from
}

// Duration is a duration key's value.
func Duration(job *c4v1.WorkJob, project *c4v1.Project, unit int, key string) (time.Duration, Use) {
	v, from := Value(job, project, unit, key)
	return dur(v), Use{Key: key, Value: v, From: from}
}

// Int is a count key's value.
func Int(job *c4v1.WorkJob, project *c4v1.Project, unit int, key string) (int, Use) {
	v, from := Value(job, project, unit, key)
	return num(v), Use{Key: key, Value: v, From: from}
}

// List is a list key's value, in order.
func List(job *c4v1.WorkJob, project *c4v1.Project, unit int, key string) ([]string, Use) {
	v, from := Value(job, project, unit, key)
	var out []string
	for _, s := range strings.Split(v, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out, Use{Key: key, Value: v, From: from}
}

// Record is uses as a signed record's or a book entry's policy field.
func Record(uses ...Use) []map[string]any {
	var out []map[string]any
	for _, u := range uses {
		f := map[string]any{"scope": u.From.Scope}
		if u.From.Ref != "" {
			f["ref"] = u.From.Ref
		}
		out = append(out, map[string]any{"key": u.Key, "value": u.Value, "from": f})
	}
	return out
}

// projectValue is key's value for project p: spec.work.policy.values, any
// key, else spec.work.escalation for the escalation keys.
func projectValue(p *c4v1.Project, key string) string {
	if p == nil || p.Spec.Work == nil {
		return ""
	}
	if pol := p.Spec.Work.Policy; pol != nil {
		if s := strings.TrimSpace(pol.Values[key]); s != "" && valid(key, s) {
			return s
		}
	}
	e := p.Spec.Work.Escalation
	if e == nil {
		return ""
	}
	switch key {
	case PressAfter:
		if e.PressAfter != nil {
			return short(e.PressAfter.Duration)
		}
	case PressMargin:
		if e.PressMargin != nil {
			return short(e.PressMargin.Duration)
		}
	case RecheckMax:
		if r := e.Recheck; r != nil && r.Max != nil {
			return strconv.Itoa(int(*r.Max))
		}
	case RecheckMinInterval:
		if r := e.Recheck; r != nil && r.MinInterval != nil {
			return short(r.MinInterval.Duration)
		}
	}
	return ""
}

// short is d as a policy value reads: 1h, 1h30m, 90s as 1m30s.
func short(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

func valid(key, s string) bool {
	switch key {
	case PressAfter, PressMargin, RecheckMinInterval:
		d, err := time.ParseDuration(s)
		return err == nil && d >= 0
	case SubmissionPendingFor:
		// At least the time a submission may take to be made and rolled
		// back, or the sweep takes one still being made.
		d, err := time.ParseDuration(s)
		return err == nil && d >= SubmissionPendingFloor
	case SubmissionSweepEvery, SubmissionKeyFor, UsageLimitProbe, YieldBackgroundWait:
		d, err := time.ParseDuration(s)
		return err == nil && d > 0
	case RecheckMax:
		n, err := strconv.Atoi(s)
		return err == nil && n >= 0
	case AsksMax, RecheckSeconds:
		n, err := strconv.Atoi(s)
		return err == nil && n > 0
	}
	return true
}

func dur(s string) time.Duration {
	d, _ := time.ParseDuration(s)
	return d
}

func num(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

// Work is uses as a status decision's policy field.
func Work(uses ...Use) []c4v1.WorkPolicyUse {
	var out []c4v1.WorkPolicyUse
	for _, u := range uses {
		out = append(out, c4v1.WorkPolicyUse{Key: u.Key, Value: u.Value,
			From: c4v1.WorkPolicyFrom{Scope: u.From.Scope, Ref: u.From.Ref, SetBy: u.From.SetBy, Record: u.From.Record}})
	}
	return out
}
