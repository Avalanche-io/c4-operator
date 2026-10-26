package localfarm

import (
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// A failed tool test is removed only when a newer test of the same tool
// in the same namespace has run: the newest test stays whatever its end,
// a person's job is never touched, and another project's tests are its own.
func TestSupersededTests(t *testing.T) {
	at := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	job := func(ns, name, tool, phase string, age time.Duration) unstructured.Unstructured {
		var u unstructured.Unstructured
		u.SetNamespace(ns)
		u.SetName(name)
		u.SetCreationTimestamp(metav1.NewTime(at.Add(age)))
		if tool != "" {
			u.SetLabels(map[string]string{toolTestLabel: tool})
		}
		unstructured.SetNestedField(u.Object, phase, "status", "phase")
		return u
	}
	jobs := []unstructured.Unstructured{
		job("saltmarsh", "comp-farmtest-207760c4", "comp", "Failed", 0),            // the old failed test
		job("saltmarsh", "comp-farmtest-9a8b7c6d", "comp", "Succeeded", time.Hour), // the newer one
		job("saltmarsh", "render-farmtest-11111111", "render", "Failed", 0),        // the newest render test: stays
		job("glassworks", "comp-farmtest-22222222", "comp", "Failed", 0),           // another project's: stays
		job("saltmarsh", "sm0140-comp-v001", "", "Failed", -time.Hour),             // a person's: never
		job("saltmarsh", "notes-farmtest-33333333", "notes", "Succeeded", 0),       // passed, older
		job("saltmarsh", "notes-farmtest-44444444", "notes", "Failed", time.Hour),  // the newest, failed: stays
	}
	var got []string
	for _, j := range supersededTests(jobs) {
		got = append(got, j.GetNamespace()+"/"+j.GetName())
	}
	if strings.Join(got, ",") != "saltmarsh/comp-farmtest-207760c4" {
		t.Errorf("superseded %v", got)
	}
}
