package telemetry

import (
	"encoding/json"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/Avalanche-io/c4-operator/internal/telemetry/canary"
	"github.com/Avalanche-io/c4-operator/work/sched"
)

// leakyRecord is a record a careless change might write: the redacted
// snapshot plus the job's own ID.
type leakyRecord struct {
	Snapshot
	ID string
}

// The tests catch a deliberate leak: a record that copies a job's ID
// fails the structural check, and, written anyway, the canary scan finds
// the ID. If either stopped catching it, the canary would be blind.
func TestTheTestsCatchALeak(t *testing.T) {
	if err := CheckType(reflect.TypeOf(leakyRecord{})); err == nil {
		t.Fatal("the structural check let a string field through")
	}
	if _, err := Encode(leakyRecord{}); err == nil {
		t.Fatal("the encoder wrote a record with a string field")
	}
	rec, _, _ := redactedCanary(t, testKey(t, 1))
	s, _, _ := canary.Farm()
	leaky := leakyRecord{Snapshot: rec, ID: s.Jobs[0].ID}
	out, err := json.Marshal(leaky) // bypassing Encode, as a careless exporter would
	if err != nil {
		t.Fatal(err)
	}
	if found := canary.Scan(out, canary.Tokens(), recordWords); len(found) == 0 {
		t.Fatal("the canary scan did not find a job ID copied into the output")
	}
}

// A replay that differs is reported (the faithfulness check is not blind):
// moving a job's deadline in the record makes its replay decide otherwise.
func TestFaithfulnessCheckSeesADifference(t *testing.T) {
	rec, _, _ := redactedCanary(t, testKey(t, 1))
	if !rec.Faithful {
		t.Fatalf("the untouched canary is unfaithful: %+v", rec.Divergences)
	}
	for i := range rec.Jobs {
		rec.Jobs[i].Deadline = RelTime{ns: 1, set: true}
		rec.Jobs[i].Hold = true
	}
	CheckFaithful(&rec, sched.Decide)
	if rec.Faithful || len(rec.Divergences) == 0 {
		t.Fatal("a tampered record replayed as faithful")
	}
}

// Pseudonyms are stable under one key, ranked in name order, and only the
// key's holder can resolve them.
func TestPseudonyms(t *testing.T) {
	key := testKey(t, 1)
	n := NewNamer(key)
	for _, s := range []string{"saltmarsh/b", "saltmarsh/a", "saltmarsh/a2"} {
		n.Add(KindJob, s)
	}
	n.Freeze()
	a, b, a2 := n.Of(KindJob, "saltmarsh/a"), n.Of(KindJob, "saltmarsh/b"), n.Of(KindJob, "saltmarsh/a2")
	if !(a.Rank < a2.Rank && a2.Rank < b.Rank) {
		t.Fatalf("ranks %d %d %d do not follow name order", a.Rank, a2.Rank, b.Rank)
	}
	if !(a.Synthetic() < a2.Synthetic() && a2.Synthetic() < b.Synthetic()) {
		t.Fatal("synthetic names do not sort as the names did")
	}
	if n.Of(KindJob, "saltmarsh/a") != a || n.Of(KindPool, "saltmarsh/a") == a {
		t.Fatal("a pseudonym is not stable, or kinds share one")
	}
	if got, ok := Resolve(key, a, []string{"saltmarsh/b", "saltmarsh/a"}); !ok || got != "saltmarsh/a" {
		t.Fatalf("resolve: %q %v", got, ok)
	}
	if _, ok := Resolve(testKey(t, 9), a, []string{"saltmarsh/a"}); ok {
		t.Fatal("another key resolved the pseudonym")
	}
	if txt := a.Text(); !strings.HasPrefix(txt, "j.") || len(txt) != 18 {
		t.Fatalf("pseudonym text %q", txt)
	}
}

// FuzzRedact fills every string the redactor reads with arbitrary text: no
// input string of four bytes or more reaches the record, unless it
// is already in the record redacted from neutral names, which a string the
// record writes for itself would be.
func FuzzRedact(f *testing.F) {
	for _, seed := range [][4]string{{"saltmarsh", "sh0420/comp", "bk-cpu", "nuke-render"}, {"NIGHTJAR_S02", "seq030#7", "London", "x"}} {
		f.Add(seed[0], seed[1], seed[2], seed[3])
	}
	key := testKey(f, 1)
	baseline := func() string {
		s, st, cfg := fuzzFarm("pa", "ja", "pl", "cl")
		rec, _ := NewRedactor(key).Redact(s, st, cfg, sched.Decide(s, st, cfg))
		out, _ := Encode(rec)
		return string(out)
	}()
	f.Fuzz(func(t *testing.T, project, job, pool, class string) {
		s, st, cfg := fuzzFarm(project, job, pool, class)
		rec, _ := NewRedactor(key).Redact(s, st, cfg, sched.Decide(s, st, cfg))
		out, err := Encode(rec)
		if err != nil {
			t.Fatal(err)
		}
		for _, in := range []string{project, job, pool, class} {
			// An input with no letter is indistinguishable from the record's
			// own numbers; numbers are checked semantically (canary_test.go).
			if len(in) < 4 || !utf8.ValidString(in) || !strings.ContainsFunc(in, unicode.IsLetter) || strings.Contains(baseline, in) {
				continue
			}
			if strings.Contains(string(out), in) {
				t.Fatalf("input %q reached the record", in)
			}
			if q := strconv.Quote(in); strings.Contains(string(out), q[1:len(q)-1]) {
				t.Fatalf("input %q reached the record escaped", in)
			}
		}
	})
}

func fuzzFarm(project, job, pool, class string) (sched.Snapshot, *sched.State, sched.Config) {
	s, _, cfg := canary.Farm()
	s.Projects[0].Name = project
	s.Jobs[0].ID = job
	s.Jobs[0].Project = project
	s.Jobs[0].Class = class
	s.Pools[0].Name = pool
	s.Pools[0].Attrs = map[string]string{pool: job}
	for i := range s.Jobs {
		s.Jobs[i].Pools = append([]string{pool}, s.Jobs[i].Pools...)
	}
	for i := range s.Jobs[0].Runnable {
		s.Jobs[0].Runnable[i].ID = job + "#" + strconv.Itoa(i)
	}
	return s, &sched.State{}, cfg
}
