package telemetry

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Avalanche-io/c4-operator/internal/telemetry/canary"
	"github.com/Avalanche-io/c4-operator/work/sched"
)

func testKey(t testing.TB, seed byte) Key {
	t.Helper()
	b := make([]byte, KeySize)
	for i := range b {
		b[i] = seed*31 + byte(i)
	}
	k, err := NewKey(b, 3)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// redactedCanary is the canary farm as a record, with its encoding.
func redactedCanary(t testing.TB, key Key) (Snapshot, []byte, *Redactor) {
	t.Helper()
	s, st, cfg := canary.Farm()
	before := st.Doc()
	d := sched.Decide(s, st, cfg)
	r := NewRedactor(key)
	rec, _ := r.Redact(s, sched.StateFrom(before), cfg, d)
	CheckFaithful(&rec, sched.Decide)
	out, err := Encode(rec)
	if err != nil {
		t.Fatal(err)
	}
	return rec, out, r
}

// recordWords are the four-character runs of the record's own JSON keys
// and enum names, computed from a record with one of everything and the
// enum tables. The scan skips them: they are written by this package, not
// taken from the work.
var recordWords = func() map[string]bool {
	words := map[string]bool{}
	b, _ := json.Marshal(fullRecord())
	canary.KeyRuns(words, b)
	for _, table := range EnumTables() {
		for _, s := range table {
			canary.AddRuns(words, s)
		}
	}
	return words
}()

// fullRecord is a record with one of everything, so its JSON shows every
// key a record can write.
func fullRecord() Snapshot {
	p := Pseudonym{Kind: KindJob}
	u := UnitID{Job: p}
	t := RelTime{ns: 1, set: true}
	return Snapshot{
		Pools:    []Pool{{Name: p, Burst: &Burst{}}},
		Projects: []Project{{Name: p, Quotas: []Quota{{Pool: p}}}},
		Jobs: []Job{{ID: p, After: []Pseudonym{p}, Pools: []Pseudonym{p}, Running: []PoolSlots{{Pool: p}},
			Runnable: []RunnableUnit{{Unit: u}}, RunningUnits: []RunningUnit{{Unit: u}},
			Sources: []PoolSources{{Pool: p, Sources: []Source{{Link: p}}}}}},
		Links: []Link{{Name: p}},
		State: State{Pending: []Pending{{Unit: u}}, PreemptLog: []PoolTimes{{Pool: p, Times: []RelTime{t}}}, Surplus: []PoolTime{{Pool: p}},
			ManualBumped: []Pseudonym{p}, Guard: []UnitTime{{Unit: u}}, GuardHit: []JobTime{{Job: p}}, Took: []Took{{Victim: p}}},
		Decisions: Decisions{Dispatch: []Dispatch{{Unit: u}}, Preempt: []Preempt{{Unit: u}}, Capacity: []PoolSlots{{Pool: p}},
			Bursts: []BurstDecision{{Slots: []PoolSlots{{Pool: p}}, Order: []Pseudonym{p}}}, Jobs: []JobState{{Job: p}}},
		Divergences: []DivergenceCount{{}},
	}
}

// No project detail of the canary farm reaches the record, in any
// encoding, nor as an unsalted hash, nor as a four-character run of a name;
// and its numbers are only the redacted ones.
func TestCanaryCorpus(t *testing.T) {
	key := testKey(t, 1)
	rec, out, r := redactedCanary(t, key)
	tokens := canary.Tokens()
	if found := canary.Scan(out, tokens, recordWords); len(found) > 0 {
		t.Fatalf("project details in the record:\n%s", strings.Join(found, "\n"))
	}
	text := string(out)
	// No wall-clock time: no date, no Unix time of the fixture's times.
	for _, tm := range []time.Time{canary.Now, canary.Deadline} {
		for _, f := range []string{tm.Format("2006-01-02"), strconv.FormatInt(tm.Unix(), 10), strconv.FormatInt(tm.UnixNano(), 10), strconv.FormatInt(tm.UnixMilli(), 10)} {
			if strings.Contains(text, f) {
				t.Errorf("a wall-clock time is in the record: %s", f)
			}
		}
	}
	// Money is the fixture's times the secret factor, never as it was.
	for _, f := range []string{"31337.42", "777.77"} {
		if strings.Contains(text, f) {
			t.Errorf("an amount of money is in the record as it was: %s", f)
		}
	}
	for _, p := range rec.Projects {
		if p.Name.Rank == rankOf(t, "saltmarsh", "saltmarsh", "NIGHTJAR_S02", "BLUE HARVEST") {
			want := canary.Budget * r.MoneyFactor
			if math.Abs(float64(p.BurstBudgetLeft)-want) > 1e-6*want {
				t.Errorf("budget %v, want the fixture's times the factor (%v)", p.BurstBudgetLeft, want)
			}
		}
	}
	// Bytes keep two significant figures.
	for _, j := range rec.Jobs {
		for _, ps := range j.Sources {
			for _, s := range ps.Sources {
				if float64(s.Bytes) != 120000000 {
					t.Errorf("source bytes %v, want two significant figures of %v", s.Bytes, canary.Bytes)
				}
			}
		}
	}
	// The fixture is contended enough to decide something.
	if len(rec.Decisions.Dispatch) == 0 {
		t.Fatal("the canary farm decided nothing; the test would scan an empty record")
	}
	if !rec.Faithful {
		t.Errorf("the canary snapshot does not replay: %+v", rec.Divergences)
	}
}

// rankOf is the rank of name among names.
func rankOf(t testing.TB, name string, names ...string) uint32 {
	t.Helper()
	n := NewNamer(Key{})
	for _, s := range names {
		n.Add(KindProject, s)
	}
	n.Freeze()
	return n.Of(KindProject, name).Rank
}

// The farm key is never in a record, in raw, hex or base64 form;
// pseudonyms under plausible wrong keys match none in the record; and two
// farms with the same names and different keys share no pseudonym.
func TestKeyNeverLeaves(t *testing.T) {
	key := testKey(t, 1)
	_, out, _ := redactedCanary(t, key)
	secret := key.secret[:]
	for _, f := range []string{string(secret), hex.EncodeToString(secret), base64.StdEncoding.EncodeToString(secret), base64.RawURLEncoding.EncodeToString(secret)} {
		if bytes.Contains(out, []byte(f)) {
			t.Fatalf("the farm key is in the record (%d bytes form)", len(f))
		}
	}
	s, _, _ := canary.Farm()
	names := func() map[Kind][]string {
		m := map[Kind][]string{}
		for _, p := range s.Pools {
			m[KindPool] = append(m[KindPool], p.Name)
		}
		for _, p := range s.Projects {
			m[KindProject] = append(m[KindProject], p.Name)
		}
		for _, j := range s.Jobs {
			m[KindJob] = append(m[KindJob], j.ID)
		}
		return m
	}()
	var wrong []Key
	for _, guess := range [][]byte{make([]byte, KeySize), padded("saltmarsh"), padded("c4-system"), padded("4e1f2c3a-uid-of-the-namespace")} {
		k, _ := NewKey(guess, 3)
		wrong = append(wrong, k)
	}
	for _, k := range wrong {
		for kind, list := range names {
			for _, nm := range list {
				p := Pseudonym{Kind: kind, sum: sum(k, kind, nm)}
				if bytes.Contains(out, []byte(p.Text())) {
					t.Fatalf("a pseudonym under a guessable key matches the record: %s", p.Text())
				}
			}
		}
	}
	_, other, _ := redactedCanary(t, testKey(t, 2))
	for kind, list := range names {
		for _, nm := range list {
			a := Pseudonym{Kind: kind, sum: sum(key, kind, nm)}.Text()
			b := Pseudonym{Kind: kind, sum: sum(testKey(t, 2), kind, nm)}.Text()
			if a == b || bytes.Contains(other, []byte(a)) {
				t.Fatalf("two farms share a pseudonym for %s", kind.prefix())
			}
		}
	}
}

func padded(s string) []byte {
	b := make([]byte, KeySize)
	copy(b, s)
	return b
}

// Every name token keeps four-character runs the scan checks: a run is
// skipped only where the record's own keys and enum names contain it, and
// no token may be made of such runs alone.
func TestCanaryRunsAreChecked(t *testing.T) {
	for _, tok := range canary.Tokens() {
		lt := strings.ToLower(tok)
		if len(lt) < 4 {
			continue
		}
		checked := 0
		for i := 0; i+4 <= len(lt); i++ {
			if !recordWords[lt[i:i+4]] && canary.HasLetter(lt[i:i+4]) {
				checked++
			}
		}
		if checked == 0 {
			t.Errorf("%q: every four-character run is one the record itself writes, so a part of it could pass unseen", tok)
		}
	}
}
