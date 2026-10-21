package policy

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
)

// Values read the farm's default, then the Project, then the job, which
// may only tighten: a looser or malformed job value is ignored. Each says
// where it came from.
func TestValue(t *testing.T) {
	job := func(params map[string]string) *c4v1.WorkJob {
		return &c4v1.WorkJob{ObjectMeta: metav1.ObjectMeta{Namespace: "show", Name: "sh010"}, Spec: c4v1.WorkJobSpec{Params: params}}
	}
	max := int32(4)
	proj := &c4v1.Project{ObjectMeta: metav1.ObjectMeta{Name: "show"}, Spec: c4v1.ProjectSpec{Work: &c4v1.ProjectWork{
		Escalation: &c4v1.ProjectEscalation{PressAfter: &metav1.Duration{Duration: time.Hour},
			Recheck: &c4v1.ProjectRecheck{Max: &max}}}}}

	cases := []struct {
		name         string
		job          *c4v1.WorkJob
		project      *c4v1.Project
		key          string
		value, scope string
	}{
		{"farm", nil, nil, PressAfter, "2h", "farm"},
		{"farm margin", job(nil), proj, PressMargin, "15m", "farm"},
		{"project", job(nil), proj, PressAfter, "1h", "project"},
		{"job tightens", job(map[string]string{PressAfter: "30m"}), proj, PressAfter, "30m", "job"},
		{"job loosens", job(map[string]string{PressAfter: "3h"}), proj, PressAfter, "1h", "project"},
		{"longer margin", job(map[string]string{PressMargin: "1h"}), nil, PressMargin, "1h", "job"},
		{"shorter margin", job(map[string]string{PressMargin: "5m"}), nil, PressMargin, "15m", "farm"},
		{"fewer rechecks", job(map[string]string{RecheckMax: "1"}), proj, RecheckMax, "1", "job"},
		{"more rechecks", job(map[string]string{RecheckMax: "9"}), proj, RecheckMax, "4", "project"},
		{"malformed", job(map[string]string{RecheckMinInterval: "soon"}), nil, RecheckMinInterval, "10m", "farm"},
		{"asks by its old name", job(map[string]string{"maxAsks": "2"}), nil, AsksMax, "2", "job"},
		{"no zero asks", job(map[string]string{"maxAsks": "0"}), nil, AsksMax, "3", "farm"},
		{"answers", job(map[string]string{"answerer": "lead, sup"}), nil, Answers, "lead, sup", "job"},
	}
	values := &c4v1.Project{ObjectMeta: metav1.ObjectMeta{Name: "show"}, Spec: c4v1.ProjectSpec{Work: &c4v1.ProjectWork{
		Policy:     &c4v1.ProjectPolicy{Values: map[string]string{AsksMax: "2", Answers: "lead", RecheckSeconds: "120", PressAfter: "soon"}},
		Escalation: &c4v1.ProjectEscalation{PressAfter: &metav1.Duration{Duration: time.Hour}}}}}
	loose := &c4v1.Project{ObjectMeta: metav1.ObjectMeta{Name: "show"}, Spec: c4v1.ProjectSpec{Work: &c4v1.ProjectWork{
		Policy: &c4v1.ProjectPolicy{Values: map[string]string{AsksMax: "50", RecheckMax: "99", RecheckSeconds: "5000", PressMargin: "1m", PressAfter: "6h", YieldBackgroundWait: "2h"}}}}}
	cases = append(cases, []struct {
		name         string
		job          *c4v1.WorkJob
		project      *c4v1.Project
		key          string
		value, scope string
	}{
		{"project asks", job(nil), values, AsksMax, "2", "project"},
		{"project answers", job(nil), values, Answers, "lead", "project"},
		{"job answers", job(map[string]string{"answerer": "sup"}), values, Answers, "sup", "job"},
		{"project recheck seconds", job(nil), values, RecheckSeconds, "120", "project"},
		{"job tightens seconds", job(map[string]string{RecheckSeconds: "60"}), values, RecheckSeconds, "60", "job"},
		{"job loosens seconds", job(map[string]string{RecheckSeconds: "600"}), values, RecheckSeconds, "120", "project"},
		{"farm seconds", nil, nil, RecheckSeconds, "300", "farm"},
		{"a malformed value falls back to escalation", job(nil), values, PressAfter, "1h", "project"},
		{"asks past the farm's limit", job(nil), loose, AsksMax, "3", "farm"},
		{"rechecks past the farm's limit", job(nil), loose, RecheckMax, "5", "farm"},
		{"recheck seconds past the farm's limit", job(nil), loose, RecheckSeconds, "300", "farm"},
		{"a margin under the farm's limit", job(nil), loose, PressMargin, "15m", "farm"},
		{"within the limit, looser than the farm", job(nil), loose, PressAfter, "6h", "project"},
		{"background wait past the farm's limit", job(nil), loose, YieldBackgroundWait, "30m", "farm"},
		{"a job's shorter background wait", job(map[string]string{YieldBackgroundWait: "10m"}), loose, YieldBackgroundWait, "10m", "job"},
		{"a probe", job(map[string]string{UsageLimitProbe: "5m"}), nil, UsageLimitProbe, "5m", "job"},
		{"no zero probe", job(map[string]string{UsageLimitProbe: "0s"}), nil, UsageLimitProbe, "30m", "farm"},
	}...)
	for _, c := range cases {
		v, from := Value(c.job, c.project, -1, c.key)
		if v != c.value || from.Scope != c.scope {
			t.Errorf("%s: %s = %q from %s, want %q from %s", c.name, c.key, v, from.Scope, c.value, c.scope)
		}
	}
	if l, u := List(job(map[string]string{"answerer": "lead, sup"}), nil, -1, Answers); len(l) != 2 || l[1] != "sup" || u.From.Ref != "show/sh010" {
		t.Fatalf("list %v %+v", l, u)
	}
	d, u := Duration(nil, proj, -1, PressAfter)
	rec := Record(u)
	if d != time.Hour || len(rec) != 1 || rec[0]["key"] != PressAfter || rec[0]["from"].(map[string]any)["ref"] != "show" {
		t.Fatalf("duration %s, record %v", d, rec)
	}
	if w := Work(u); len(w) != 1 || w[0].From.Scope != "project" || w[0].Value != "1h" {
		t.Fatalf("work %+v", w)
	}
}

// The farm sets its limits on project values.
func TestSetLimits(t *testing.T) {
	defer SetLimits(map[string]string{AsksMax: "10"})
	proj := &c4v1.Project{ObjectMeta: metav1.ObjectMeta{Name: "show"}, Spec: c4v1.ProjectSpec{Work: &c4v1.ProjectWork{
		Policy: &c4v1.ProjectPolicy{Values: map[string]string{AsksMax: "8"}}}}}
	if v, _ := Value(nil, proj, -1, AsksMax); v != "8" {
		t.Fatalf("within the default limit: %s", v)
	}
	SetLimits(map[string]string{AsksMax: "5", RecheckMax: "many"})
	if v, _ := Value(nil, proj, -1, AsksMax); v != "3" || Limit(RecheckMax) != "10" {
		t.Fatalf("past a farm's limit of 5: %s; a malformed limit: %s", v, Limit(RecheckMax))
	}
}

// submission.pendingFor has a floor: a project value under it would let
// the sweep take a submission still being made, so it is ignored.
func TestSubmissionPendingForFloor(t *testing.T) {
	p := &c4v1.Project{Spec: c4v1.ProjectSpec{Work: &c4v1.ProjectWork{Policy: &c4v1.ProjectPolicy{Values: map[string]string{SubmissionPendingFor: "1m"}}}}}
	if d, use := Duration(nil, p, -1, SubmissionPendingFor); d != 10*time.Minute || use.From.Scope != "farm" {
		t.Fatalf("under the floor: %v %+v", d, use)
	}
	p.Spec.Work.Policy.Values[SubmissionPendingFor] = "5m"
	if d, _ := Duration(nil, p, -1, SubmissionPendingFor); d != 5*time.Minute {
		t.Fatalf("over it: %v", d)
	}
}
