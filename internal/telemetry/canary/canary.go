// Package canary is the canary farm the telemetry tests redact, and the
// scan that looks for its project details in what telemetry writes
// (DESIGN.md section 6.9). Every name in it is made up and every domain is
// an example domain; tokens.txt lists them. Only tests import it.
package canary

import (
	"bytes"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf16"

	"github.com/Avalanche-io/c4-operator/work/estimate"
	"github.com/Avalanche-io/c4-operator/work/sched"
)

//go:embed tokens.txt
var tokensFile string

// Tokens are the project details Farm hides in its fields.
func Tokens() []string {
	var out []string
	for _, l := range strings.Split(tokensFile, "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
			out = append(out, l)
		}
	}
	return out
}

// The fixture's distinctive numbers: money, bytes and wall-clock times
// that must not survive as they are.
var (
	Now      = time.Date(2026, 11, 13, 16, 20, 0, 0, time.UTC)
	Deadline = time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)
	Budget   = 31337.42
	Late     = 777.77
	Bytes    = 123456789.0
)

// Farm is a small, contended farm whose every name, attribute, source view
// and class is a project detail.
func Farm() (sched.Snapshot, *sched.State, sched.Config) {
	now := Now
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
			{Name: "saltmarsh", Weight: 2, BurstBudgetLeft: Budget, LateCostPerHour: Late,
				Quotas: map[string]sched.Quota{"bk-cpu": {Nominal: 4, BorrowingLimit: -1}}},
			{Name: "NIGHTJAR_S02", Weight: 1, BurstBudgetLeft: 12, LateCostPerHour: 55.5,
				Quotas: map[string]sched.Quota{"bk-cpu": {Nominal: 2, BorrowingLimit: 2}}},
			{Name: "BLUE HARVEST", Weight: 1, LateCostPerHour: 10},
		},
		Jobs: []sched.Job{
			{ID: comp, Project: "saltmarsh", Class: "nuke-render", Submitted: now.Add(-time.Hour), Ready: now.Add(-time.Hour), Deadline: Deadline,
				Pools: []string{"bk-cpu", "gpu-a100-burst"}, SlotsPerUnit: 1, Running: map[string]int{"bk-cpu": 4},
				Runnable: units(comp, 40), RunnableTotal: 40, Estimate: est(60 * 3600), Preemptible: true, Checkpoints: true,
				RunningUnits: []sched.RunningUnit{{ID: comp + "#41", Pool: "bk-cpu", Slots: 1, Started: now.Add(-20 * time.Minute), Remaining: 10 * time.Minute}},
				Sources: map[string][]sched.Source{"gpu-a100-burst": {{Via: "move", Channel: "inter-region", Link: "Examplevale Pictures",
					Bytes: Bytes, Latency: 20 * time.Minute, Cost: 3.5, Class: "validated",
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

// Encodings is every form DESIGN.md's canary test looks for a token in.
func Encodings(tok string) []string {
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

// ScanWhole reports every token found whole in out, in any encoding.
func ScanWhole(out []byte, tokens []string) []string {
	var found []string
	for _, tok := range tokens {
		for _, f := range Encodings(tok) {
			if f != "" && bytes.Contains(out, []byte(f)) {
				found = append(found, tok+" as "+strconv.Quote(f))
			}
		}
	}
	return found
}

// Scan reports every token found in out, in any encoding, or as a run of
// four or more characters of a token with a letter in it, except runs in
// skip: the four-character runs of the record's own keys and enum names, which telemetry
// writes itself.
func Scan(out []byte, tokens []string, skip map[string]bool) []string {
	found := ScanWhole(out, tokens)
	lower := strings.ToLower(string(out))
	for _, tok := range tokens {
		lt := strings.ToLower(tok)
		for i := 0; i+4 <= len(lt); i++ {
			if sub := lt[i : i+4]; strings.Contains(lower, sub) && !skip[sub] && HasLetter(sub) {
				found = append(found, tok+" through "+strconv.Quote(sub))
				break
			}
		}
	}
	return found
}

// HasLetter says a run has a letter. A run of digits alone ("0420" of
// sh0420) is not looked for: a record's numbers are counts, scaled money
// and relative times, which can hold any four digits, and the tests check
// them semantically instead (no fixture date, Unix time or unscaled
// amount).
func HasLetter(s string) bool { return strings.ContainsFunc(s, unicode.IsLetter) }

// AddRuns adds s's four-character runs, lowercased, to words.
func AddRuns(words map[string]bool, s string) {
	s = strings.ToLower(s)
	for i := 0; i+4 <= len(s); i++ {
		words[s[i:i+4]] = true
	}
}

// KeyRuns adds the four-character runs of every object key in the JSON
// document b to words.
func KeyRuns(words map[string]bool, b []byte) {
	var v any
	if json.Unmarshal(b, &v) != nil {
		return
	}
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, y := range x {
				AddRuns(words, k)
				walk(y)
			}
		case []any:
			for _, y := range x {
				walk(y)
			}
		}
	}
	walk(v)
}
