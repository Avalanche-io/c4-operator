package telemetry

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Avalanche-io/c4-operator/work/sched"
)

// The records hold only telemetry's closed kinds: walking every type
// reachable from a root record finds no string, byte slice, interface,
// map or outside type, and every enum has a table beginning with other
// (DESIGN.md section 6.9). Adding a string field anywhere under a
// record fails here.
func TestRecordsHoldNoText(t *testing.T) {
	for _, r := range []Record{Snapshot{}} {
		if err := CheckType(reflect.TypeOf(r)); err != nil {
			t.Fatal(err)
		}
	}
	for _, e := range []Enum{Kind(0), Tier(0), Reason(0), Via(0), Channel(0), Planner(0), Surplus(0), Divergence(0), GOOS(0), GOARCH(0), GOAMD64(0)} {
		tab := e.Table()
		if len(tab) == 0 || tab[0] != "other" {
			t.Errorf("%T's table does not begin with other", e)
		}
		seen := map[string]bool{}
		for _, v := range tab {
			if seen[v] {
				t.Errorf("%T's table repeats %q", e, v)
			}
			seen[v] = true
		}
		if got := enumName(reflect.ValueOf(e).Convert(reflect.TypeOf(e)).Interface().(Enum)); got != "other" {
			t.Errorf("%T's zero value is %q, not other", e, got)
		}
	}
	// A value read from outside that is not in a table is other, never
	// its text.
	if TierOf("a tier nobody listed") != 0 || ReasonOf("render the hero plate") != 0 {
		t.Fatal("an unknown value was not other")
	}
}

// The check refuses each way a record could hold the work.
func TestCheckTypeRefuses(t *testing.T) {
	type withString struct{ ID string }
	type withBytes struct{ B []byte }
	type withMap struct{ M map[Kind]Int }
	type withAny struct{ X any }
	type withError struct{ E error }
	type withTime struct{ T reflect.Type }
	type withBigArray struct{ A [64]byte }
	for _, v := range []any{withString{}, withBytes{}, withMap{}, withAny{}, withError{}, withTime{}, withBigArray{}, []string{}, map[string]int{}} {
		if err := CheckType(reflect.TypeOf(v)); err == nil {
			t.Errorf("%T passed the check", v)
		}
	}
}

// Every output goes through the one encoder: the package's own code
// opens no file, prints nothing and imports nothing that writes, so there
// is no second way out to forget.
func TestOneWayOut(t *testing.T) {
	forbiddenImports := map[string]bool{"os": true, "io": true, "io/ioutil": true, "io/fs": true, "bufio": true, "log": true,
		"log/slog": true, "net": true, "net/http": true, "os/exec": true, "syscall": true}
	forbiddenCalls := map[string]bool{"fmt.Print": true, "fmt.Println": true, "fmt.Printf": true, "fmt.Fprint": true,
		"fmt.Fprintln": true, "fmt.Fprintf": true}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		file, err := parser.ParseFile(fset, f, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range file.Imports {
			if p := strings.Trim(imp.Path.Value, `"`); forbiddenImports[p] {
				t.Errorf("%s imports %s: telemetry writes only through Encode", f, p)
			}
		}
		ast.Inspect(file, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if id, ok := sel.X.(*ast.Ident); ok && forbiddenCalls[id.Name+"."+sel.Sel.Name] {
				t.Errorf("%s calls %s.%s: telemetry writes only through Encode", f, id.Name, sel.Sel.Name)
			}
			return true
		})
	}
}

// Every tier and preemption reason the policy writes has its own entry, so
// none is counted as other: a new one added to the policy fails here until
// its table names it.
func TestPolicyWordsAreInTheTables(t *testing.T) {
	for _, x := range []sched.Tier{sched.TierStarvation, sched.TierNeed, sched.TierGuarantee, sched.TierBoost, sched.TierInfo, sched.TierFair, sched.TierBaseline} {
		if TierOf(string(x)) == 0 {
			t.Errorf("tier %q is not in the table", x)
		}
	}
	for _, x := range []sched.PreemptReason{sched.ReasonReclaim, sched.ReasonFeasibility, sched.ReasonHoldStop, sched.ReasonWrangler, sched.ReasonGuard, sched.ReasonCapacity} {
		if ReasonOf(string(x)) == 0 {
			t.Errorf("reason %q is not in the table", x)
		}
	}
	for _, x := range []sched.Planner{sched.PlanFluid, sched.PlanPerJob} {
		if PlannerOf(string(x)) == 0 {
			t.Errorf("planner %q is not in the table", x)
		}
	}
	for _, x := range []sched.Surplus{sched.SurplusSlack, sched.SurplusFairShare} {
		if SurplusOf(string(x)) == 0 {
			t.Errorf("surplus rule %q is not in the table", x)
		}
	}
}
