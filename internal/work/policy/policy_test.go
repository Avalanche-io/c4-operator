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
		{"asks past the farm's limit, held to it", job(nil), loose, AsksMax, "10", "farm"},
		{"rechecks past the farm's limit, held to it", job(nil), loose, RecheckMax, "10", "farm"},
		{"recheck seconds past the farm's limit, held to it", job(nil), loose, RecheckSeconds, "900", "farm"},
		{"a margin under the farm's limit, held to it", job(nil), loose, PressMargin, "5m", "farm"},
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

// The farm sets its values and limits; a project's value past a limit is
// held to it, and says why; a malformed farm policy changes nothing.
func TestSetFarm(t *testing.T) {
	defer func(a, r Setting) {
		if err := SetFarm(map[string]Setting{AsksMax: a, RecheckMax: r}); err != nil {
			t.Fatal(err)
		}
	}(Farm(AsksMax), Farm(RecheckMax))
	proj := &c4v1.Project{ObjectMeta: metav1.ObjectMeta{Name: "show"}, Spec: c4v1.ProjectSpec{Work: &c4v1.ProjectWork{
		Policy: &c4v1.ProjectPolicy{Values: map[string]string{AsksMax: "50"}}}}}
	if err := SetFarm(map[string]Setting{AsksMax: {Value: "4", Limit: "8"}, RecheckMax: {Value: "2"}}); err != nil {
		t.Fatal(err)
	}
	if v, from := Value(nil, nil, -1, AsksMax); v != "4" || from.Scope != "farm" {
		t.Fatalf("the farm's value: %s from %+v", v, from)
	}
	v, from := Value(nil, proj, -1, AsksMax)
	if v != "8" || from.Scope != "farm" || from.Ref != "limit" || from.Note != "project show asked for 50, past the farm's limit of 8" {
		t.Fatalf("past the limit: %s from %+v", v, from)
	}
	_, u := Int(nil, proj, -1, AsksMax)
	if r := Record(u); r[0]["from"].(map[string]any)["note"] == nil || Work(u)[0].From.Note == "" {
		t.Fatalf("the record does not say why: %v", r)
	}
	for _, bad := range []map[string]Setting{
		{"asks.many": {Value: "1"}},
		{AsksMax: {Value: "lots"}},
		{AsksMax: {Limit: "0"}},
		{Answers: {Limit: "lead"}},
	} {
		if err := SetFarm(bad); err == nil {
			t.Errorf("%v taken", bad)
		}
	}
	if s := Farm(AsksMax); s.Value != "4" || s.Limit != "8" {
		t.Fatalf("a refused policy changed the farm: %+v", s)
	}
}

// Values are read while the farm's policy changes (go test -race).
func TestSetFarmRaces(t *testing.T) {
	done := make(chan bool)
	go func() {
		for k := 0; k < 200; k++ {
			_ = SetFarm(map[string]Setting{RecheckSeconds: {Value: "300", Limit: "900"}})
		}
		close(done)
	}()
	for {
		select {
		case <-done:
			return
		default:
			Value(nil, nil, -1, RecheckSeconds)
		}
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

// agents.submit is lead unless a project says anyone; any other value is
// ignored, and the farm refuses one. lead is the tighter, so a farm limit
// of lead holds a project to it.
func TestAgentsSubmit(t *testing.T) {
	if v, from := Value(nil, nil, -1, AgentsSubmit); v != SubmitLead || from.Scope != "farm" {
		t.Fatalf("default: %s %+v", v, from)
	}
	p := &c4v1.Project{ObjectMeta: metav1.ObjectMeta{Name: "show"},
		Spec: c4v1.ProjectSpec{Work: &c4v1.ProjectWork{Policy: &c4v1.ProjectPolicy{Values: map[string]string{AgentsSubmit: SubmitAnyone}}}}}
	if v, from := Value(nil, p, -1, AgentsSubmit); v != SubmitAnyone || from.Scope != "project" {
		t.Fatalf("anyone: %s %+v", v, from)
	}
	p.Spec.Work.Policy.Values[AgentsSubmit] = "everyone"
	if v, _ := Value(nil, p, -1, AgentsSubmit); v != SubmitLead {
		t.Fatalf("an unknown value: %s", v)
	}
	if err := SetFarm(map[string]Setting{AgentsSubmit: {Value: "everyone"}}); err == nil {
		t.Fatal("the farm took an unknown value")
	}
	if !tighter[AgentsSubmit](SubmitLead, SubmitAnyone) || tighter[AgentsSubmit](SubmitAnyone, SubmitLead) {
		t.Fatal("lead is the tighter")
	}
}
