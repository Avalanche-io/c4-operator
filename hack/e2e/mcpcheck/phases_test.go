package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"testing"
)

// The phases P6 runs after, in the order CONTRIBUTING runs them.
var beforeP6 = []string{"p1.sh", "p2.sh", "p3.sh", "p5.sh", "p4.sh", "p7.sh"}

// jobsMade are the WorkJobs the phases before P6 make, as "namespace/name":
// P5's `workjob NS NAME ...` calls and the WorkJob manifests in P7.
func jobsMade(t *testing.T) map[string]bool {
	t.Helper()
	made := map[string]bool{}
	call := regexp.MustCompile(`(?m)^workjob (\S+) (\S+) `)
	inline := regexp.MustCompile(`metadata: \{name: ([a-z0-9-]+), namespace: ([a-z0-9-]+)\}`)
	block := regexp.MustCompile(`(?m)^kind: WorkJob\nmetadata:\n  name: ([a-z0-9-]+)\n  namespace: ([a-z0-9-]+)`)
	for _, f := range beforeP6 {
		b, err := os.ReadFile(filepath.Join("..", f))
		if err != nil {
			t.Fatal(err)
		}
		s := string(b)
		for _, m := range call.FindAllStringSubmatch(s, -1) {
			made[m[1]+"/"+m[2]] = true
		}
		for _, m := range inline.FindAllStringSubmatch(s, -1) {
			made[m[2]+"/"+m[1]] = true
		}
		for _, m := range block.FindAllStringSubmatch(s, -1) {
			made[m[2]+"/"+m[1]] = true
		}
	}
	if len(made) < 5 {
		t.Fatalf("found only %d jobs in the phases before P6 (%v), so this test reads them wrongly", len(made), made)
	}
	return made
}

// Every job P6 asks the MCP server about is one a phase before it makes,
// and so is every job the evaluation set it answers names: a phase that
// renames its jobs and a P6 that still asks for the old names fail here,
// naming the call, not at the end of a farm run as "a tool call failed".
func TestP6AsksOnlyAboutJobsTheFarmMakes(t *testing.T) {
	made := jobsMade(t)
	b, err := os.ReadFile(filepath.Join("..", "p6.sh"))
	if err != nil {
		t.Fatal(err)
	}
	// The calls: 'c4_tool {json arguments}' arguments to mcpcheck.
	calls := regexp.MustCompile(`'(c4_[a-z_]+) (\{[^']*\})'`).FindAllStringSubmatch(string(b), -1)
	asked := 0
	for _, c := range calls {
		var args struct{ Namespace, Name string }
		if err := json.Unmarshal([]byte(c[2]), &args); err != nil {
			t.Fatalf("p6.sh: %s %s: %v", c[1], c[2], err)
		}
		if args.Namespace == "" || args.Name == "" {
			continue
		}
		asked++
		if job := args.Namespace + "/" + args.Name; !made[job] {
			t.Errorf("p6.sh calls %s for %s, which no phase before it makes (they make %v)", c[1], job, keys(made))
		}
	}
	if asked == 0 {
		t.Fatal("found no call in p6.sh that names a job, so this test reads it wrongly")
	}
	eval, err := os.ReadFile(filepath.Join("..", "..", "..", "docs", "agents", "eval.xml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range regexp.MustCompile(`show-[ab]/[a-z0-9-]+`).FindAllString(string(eval), -1) {
		if !made[m] {
			t.Errorf("docs/agents/eval.xml names %s, which no phase before P6 makes", m)
		}
	}
}

func keys(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
