package farmbook_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/Avalanche-io/c4-operator/internal/farmbook"
)

// CheckPage never panics on a page an attacker wrote, whatever its JSON
// (review item 7). The seed corpus (testdata/fuzz/FuzzCheckPage) holds a
// real page, the review's page with no new segment, and its fuzzed finds.
func FuzzCheckPage(f *testing.F) {
	r := newRig(&testing.T{})
	r.book(r.control(r.hud, "disable", "node:n1", map[string]any{"unschedulable": true}, r.now), nil)
	r.seal()
	r.book(r.control(r.hud, "enable", "node:n1", map[string]any{"unschedulable": false}, r.now.Add(time.Second)), nil)
	r.seal()
	good, _ := json.Marshal(r.page(0))
	f.Add(good, int64(0), "", int64(0))
	f.Add(good, int64(1), "", int64(1))
	f.Add([]byte(`{"farm":"`+r.farm.ID+`","since":1,"head":{"number":1},"segments":[],"entries":[{"id":"","position":2,"entry":{}}]}`), int64(1), "", int64(1))
	f.Fuzz(func(t *testing.T, raw []byte, number int64, segment string, position int64) {
		var p farmbook.Page
		if json.Unmarshal(raw, &p) != nil {
			return
		}
		farmbook.CheckPage(r.farm.ID, r.farm.Roots, farmbook.LastHead{Number: number, Segment: segment, Position: position}, &p)
	})
}
