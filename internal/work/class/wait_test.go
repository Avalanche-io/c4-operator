package class

import (
	"strings"
	"testing"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
)

// A document's wait units: a person, a party or a clock; a predicate is
// refused until it is built, as are a planned wait, a bad clock and an
// unknown server.
func TestDocumentWaits(t *testing.T) {
	job := &c4v1.WorkJob{}
	gen := Unit{Index: 0, Name: "root", Key: "graph", Generator: true}
	ok := `{"version": 1, "units": [
		{"name": "ruling", "wait": {"server": "person:owner", "ask": "split?", "expect": 46080, "choices": ["split", "keep"]}},
		{"name": "client", "wait": {"server": "party:studio"}},
		{"name": "dailies", "wait": {"server": "clock:2026-10-20T15:00:00Z"}}]}`
	x, err := ExpandDocument(job, gen, []byte(ok), map[string]int{"root": 0}, 1)
	if err != nil {
		t.Fatal(err)
	}
	reply := `{"version": 1, "units": [
		{"name": "brief", "wait": {"server": "person:owner", "reply": true}},
		{"name": "draft", "after": ["brief"], "inputs": [{"fromUnit": "brief", "mountPath": "/brief"}]}]}`
	if y, err := ExpandDocument(job, gen, []byte(reply), map[string]int{"root": 0}, 1); err != nil || !y.Units[0].Wait.Reply {
		t.Fatalf("a reply wait read by a unit: %v", err)
	}
	r, c, d := x.Units[0].Wait, x.Units[1].Wait, x.Units[2].Wait
	if r == nil || r.Server != "person:owner" || r.Expect != 46080 || len(r.Choices) != 2 || c == nil || d == nil || d.At.IsZero() {
		t.Fatalf("waits %+v %+v %+v", r, c, d)
	}
	for doc, want := range map[string]string{
		`{"name": "w", "wait": {"server": "predicate:tables-pass"}}`:                                                                     "designed, not built",
		`{"name": "w", "planned": true, "wait": {"server": "person:a"}}`:                                                                 "both a wait and planned",
		`{"name": "w", "wait": {"server": "clock:tomorrow"}}`:                                                                            "clock",
		`{"name": "w", "wait": {"server": "oracle"}}`:                                                                                    "want person",
		`{"name": "w", "wait": {"server": "person:a", "choices": ["fail"]}}`:                                                             "choice",
		`{"name": "w", "wait": {"server": "clock:2026-10-20T15:00:00Z", "reply": true}}`:                                                 "cannot reply",
		`{"name": "w", "wait": {"server": "person:a"}}, {"name": "r", "after": ["w"], "inputs": [{"fromUnit": "w", "mountPath": "/w"}]}`: "carries no reply",
	} {
		_, err := ExpandDocument(job, gen, []byte(`{"version": 1, "units": [`+doc+`]}`), map[string]int{"root": 0}, 1)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", doc, err, want)
		}
	}
}
