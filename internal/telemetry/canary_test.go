package telemetry

import (
	"bytes"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"math"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/Avalanche-io/c4-operator/work/estimate"
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

// canaryTokens are the project details canaryFarm hides in its fields.
func canaryTokens(t testing.TB) []string {
	t.Helper()
	raw, err := os.ReadFile("testdata/canary/tokens.txt")
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, l := range strings.Split(string(raw), "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
			out = append(out, l)
		}
	}
	return out
}

// The fixture's distinctive numbers: money, bytes and wall-clock times
// that must not survive as they are.
var (
	canaryNow      = time.Date(2026, 11, 13, 16, 20, 0, 0, time.UTC)
	canaryDeadline = time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)
	canaryBudget   = 31337.42
	canaryLate     = 777.77
	canaryBytes    = 123456789.0
)

// canaryFarm is a small, contended farm whose every name, attribute,
// source view and class is a project detail.
func canaryFarm() (sched.Snapshot, *sched.State, sched.Config) {
	now := canaryNow
	ns := "saltmarsh"
	comp := ns + "/SM_101_0420_comp"
	hero := ns + "/sh0420-fx_hero"
	light := "NIGHTJAR_S02/seq030-lighting-dana.kim"
	review := "BLUE HARVEST/noor@studio.example-review"
	est := func(w float64) estimate.Summary {
		return estimate.Summary{W: w, Sigma: w / 4, W90: w * 1.3, W50: w, W80: w * 1.2, W95: w * 1.4, TPath: 600, Queued: 10, Calibrated: true}
	}
	units := func(job string, n int) []sched.UnitRef {
		var out []sched.UnitRef
		for i := 0; i < n; i++ {
			out = append(out, sched.UnitRef{ID: job + "#" + strconv.Itoa(i)})
		}
		return out
	}
	s := sched.Snapshot{Now: now,
		Pools: []sched.Pool{
			{Name: "bk-cpu", Slots: 8, Used: 6, CostPerSlotHour: 0.4,
				Attrs: map[string]string{"host": "render-bk-017.saltmarsh.internal", "site": "London", "image": "registry.studio.example/hydra:7"}},
			{Name: "gpu-a100-burst", Slots: 0, CostPerSlotHour: 2.5,
				Burst: &sched.Burst{MaxSlots: 16, Provisioning: 5 * time.Minute, CostPerSlotHour: 2.5}},
		},
		Projects: []sched.Project{
			{Name: "saltmarsh", Weight: 2, BurstBudgetLeft: canaryBudget, LateCostPerHour: canaryLate,
				Quotas: map[string]sched.Quota{"bk-cpu": {Nominal: 4, BorrowingLimit: -1}}},
			{Name: "NIGHTJAR_S02", Weight: 1, BurstBudgetLeft: 12, LateCostPerHour: 55.5,
				Quotas: map[string]sched.Quota{"bk-cpu": {Nominal: 2, BorrowingLimit: 2}}},
			{Name: "BLUE HARVEST", Weight: 1, LateCostPerHour: 10},
		},
		Jobs: []sched.Job{
			{ID: comp, Project: "saltmarsh", Class: "nuke-render", Submitted: now.Add(-time.Hour), Ready: now.Add(-time.Hour), Deadline: canaryDeadline,
				Pools: []string{"bk-cpu", "gpu-a100-burst"}, SlotsPerUnit: 1, Running: map[string]int{"bk-cpu": 4},
				Runnable: units(comp, 40), RunnableTotal: 40, Estimate: est(60 * 3600), Preemptible: true, Checkpoints: true,
				RunningUnits: []sched.RunningUnit{{ID: comp + "#41", Pool: "bk-cpu", Slots: 1, Started: now.Add(-20 * time.Minute), Remaining: 10 * time.Minute}},
				Sources: map[string][]sched.Source{"gpu-a100-burst": {{Via: "move", Channel: "inter-region", Link: "Examplevale Pictures",
					Bytes: canaryBytes, Latency: 20 * time.Minute, Cost: 3.5, Class: "validated",
					View: "c45ZQpdqHMDeGu6tLjk8jmz9XJ2E8Bw7D5Kyxv4Kx3j8bWDE5WgtQ6vqvPXR4ofJXWhSWMLWHmNHx1QgGWy2Bgp6aR"}}}},
			{ID: hero, Project: "saltmarsh", Class: "fx_hero", Submitted: now.Add(-3 * time.Hour), Ready: now.Add(-3 * time.Hour),
				Deadline: now.Add(2 * time.Hour), After: []string{comp}, Pools: []string{"bk-cpu"}, SlotsPerUnit: 2,
				Runnable: units(hero, 6), RunnableTotal: 6, Estimate: est(8 * 3600), Preemptible: true},
			{ID: light, Project: "NIGHTJAR_S02", Class: "lighting", Submitted: now.Add(-2 * time.Hour), Ready: now.Add(-2 * time.Hour),
				Deadline: now.Add(90 * time.Minute), Pools: []string{"bk-cpu"}, SlotsPerUnit: 1, Running: map[string]int{"bk-cpu": 2},
				Runnable: units(light, 12), RunnableTotal: 12, Estimate: est(10 * 3600), Preemptible: true,
				RunningUnits: []sched.RunningUnit{{ID: light + "#12", Pool: "bk-cpu", Slots: 1, Started: now.Add(-50 * time.Minute)},
					{ID: light + "#13", Pool: "bk-cpu", Slots: 1, Started: now.Add(-40 * time.Minute)}}},
			{ID: review, Project: "BLUE HARVEST", Class: "web-search", Submitted: now.Add(-10 * time.Minute), Ready: now.Add(-10 * time.Minute),
				RequiredRate: 1, Pools: []string{"bk-cpu"}, SlotsPerUnit: 1, Runnable: units(review, 3), RunnableTotal: 3, Estimate: est(1800)},
		},
		Links: []sched.Link{{Name: "Examplevale Pictures", BytesPerSecond: 50e6}},
	}
	st := &sched.State{}
	cfg := sched.DefaultConfig()
	cfg.Loc, _ = time.LoadLocation("America/Los_Angeles")
	return s, st, cfg
}

// redactedCanary is the canary farm as a record, with its encoding.
func redactedCanary(t testing.TB, key Key) (Snapshot, []byte, *Redactor) {
	t.Helper()
	s, st, cfg := canaryFarm()
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

// encodings is every form DESIGN.md's canary test looks for a token in.
func encodings(tok string) []string {
	forms := []string{tok, strings.ToLower(tok), strings.ToUpper(tok)}
	if j, err := json.Marshal(tok); err == nil {
		forms = append(forms, strings.Trim(string(j), `"`))
	}
	forms = append(forms, url.QueryEscape(tok), url.PathEscape(tok),
		base64.StdEncoding.EncodeToString([]byte(tok)), base64.URLEncoding.EncodeToString([]byte(tok)),
		base64.RawStdEncoding.EncodeToString([]byte(tok)), hex.EncodeToString([]byte(tok)))
	for _, order := range []binary.ByteOrder{binary.LittleEndian, binary.BigEndian} {
		var b bytes.Buffer
		for _, r := range utf16.Encode([]rune(tok)) {
			var x [2]byte
			order.PutUint16(x[:], r)
			b.Write(x[:])
		}
		forms = append(forms, b.String())
	}
	m, s1, s2 := md5.Sum([]byte(tok)), sha1.Sum([]byte(tok)), sha256.Sum256([]byte(tok))
	for _, h := range [][]byte{m[:], s1[:], s2[:]} {
		forms = append(forms, hex.EncodeToString(h), base64.StdEncoding.EncodeToString(h))
	}
	return forms
}

// scan reports every token found in out, in any encoding, or as a
// substring of four or more characters of a name token.
func scan(out []byte, tokens []string) []string {
	var found []string
	lower := strings.ToLower(string(out))
	for _, tok := range tokens {
		for _, f := range encodings(tok) {
			if f != "" && bytes.Contains(out, []byte(f)) {
				found = append(found, tok+" as "+strconv.Quote(f))
			}
		}
		lt := strings.ToLower(tok)
		for i := 0; i+4 <= len(lt); i++ {
			if sub := lt[i : i+4]; strings.Contains(lower, sub) && !commonWord(sub) {
				found = append(found, tok+" through "+strconv.Quote(sub))
				break
			}
		}
	}
	return found
}

// commonWord is a four-character run of a token that is also part of a
// record's own field or enum names (written by this package, not taken
// from the work): "plat" in "plate" and "Pl" fields, and the like. Each
// is checked below to be such a name, so this cannot hide a leak.
func commonWord(sub string) bool {
	return recordWords[sub]
}

// recordWords are the four-character runs of the record's own JSON keys
// and enum names, computed from an empty record and the enum tables.
var recordWords = func() map[string]bool {
	words := map[string]bool{}
	add := func(s string) {
		s = strings.ToLower(s)
		for i := 0; i+4 <= len(s); i++ {
			words[s[i:i+4]] = true
		}
	}
	b, _ := json.Marshal(fullRecord())
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, y := range x {
				add(k)
				walk(y)
			}
		case []any:
			for _, y := range x {
				walk(y)
			}
		}
	}
	var v any
	_ = json.Unmarshal(b, &v)
	walk(v)
	for _, table := range [][]string{kindTable, tierTable, reasonTable, viaTable, channelTable, plannerTable, surplusTable, divergenceTable, goosTable, goarchTable, goamd64Table} {
		for _, s := range table {
			add(s)
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
	tokens := canaryTokens(t)
	if found := scan(out, tokens); len(found) > 0 {
		t.Fatalf("project details in the record:\n%s", strings.Join(found, "\n"))
	}
	text := string(out)
	// No wall-clock time: no date, no Unix time of the fixture's times.
	for _, tm := range []time.Time{canaryNow, canaryDeadline} {
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
			want := canaryBudget * r.MoneyFactor
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
					t.Errorf("source bytes %v, want two significant figures of %v", s.Bytes, canaryBytes)
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
	s, _, _ := canaryFarm()
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
	for _, tok := range canaryTokens(t) {
		lt := strings.ToLower(tok)
		if len(lt) < 4 {
			continue
		}
		checked := 0
		for i := 0; i+4 <= len(lt); i++ {
			if !recordWords[lt[i:i+4]] {
				checked++
			}
		}
		if checked == 0 {
			t.Errorf("%q: every four-character run is one the record itself writes, so a part of it could pass unseen", tok)
		}
	}
}
