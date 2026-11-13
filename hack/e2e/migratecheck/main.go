// Command migratecheck checks one project after `c4-operator migrate-pin
// --write` and the switch to the new operator, before the new pin's e2e
// phases change anything (hack/e2e/migrate.sh runs it). Every rename the
// dry run listed resolves through the table the Project's status names,
// and its line is checked by identifying the old ID's records again; grants
// on renamed trees have the new entry beside them; every Materialization
// Ready before the migration is Ready again at the view's ID now; a
// git-import mapping reads with no old IDs and its chains open to their
// recorded heads. It prints "ok ..." and "FAIL ..." lines and exits 1 on
// any FAIL.
//
//	migratecheck -project P -shard s0=URL ... -dry dry.out -pre mats.json [-mapping m.jsonl]
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Avalanche-io/c4"
	"github.com/Avalanche-io/c4/store"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/gitimport"
	"github.com/Avalanche-io/c4-operator/internal/project"
	"github.com/Avalanche-io/c4-operator/internal/storeurl"
	"github.com/Avalanche-io/c4-operator/internal/view"
)

type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }

var failed bool

func ok(f string, a ...any) { fmt.Printf("ok "+f+"\n", a...) }
func bad(f string, a ...any) {
	failed = true
	fmt.Printf("FAIL "+f+"\n", a...)
}

func main() {
	name := flag.String("project", "", "")
	dry := flag.String("dry", "", "")
	pre := flag.String("pre", "", "")
	mapping := flag.String("mapping", "", "")
	var shards multi
	flag.Var(&shards, "shard", "")
	flag.Parse()
	ctx := context.Background()
	scheme := runtime.NewScheme()
	clientgoscheme.AddToScheme(scheme)
	c4v1.AddToScheme(scheme)
	c, err := client.New(ctrl.GetConfigOrDie(), client.Options{Scheme: scheme})
	if err != nil {
		panic(err)
	}
	var specs []storeurl.Spec
	for _, s := range shards {
		specs = append(specs, storeurl.ParseSpec(s))
	}
	st, err := storeurl.OpenShardedWith(specs, store.Ambient(false))
	if err != nil {
		panic(err)
	}
	var p c4v1.Project
	if err := c.Get(ctx, types.NamespacedName{Name: *name}, &p); err != nil {
		panic(err)
	}

	// The renames the dry run listed.
	renames := map[c4.ID]c4.ID{}
	f, err := os.Open(*dry)
	if err != nil {
		panic(err)
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		w := strings.Fields(sc.Text())
		if len(w) >= 3 && w[0] == "rename" {
			old, err1 := c4.Parse(w[1])
			cur, err2 := c4.Parse(w[2])
			if err1 != nil || err2 != nil {
				panic(sc.Text())
			}
			renames[old] = cur
		}
	}
	f.Close()
	var table c4.ID
	if len(renames) > 0 {
		if table, err = c4.Parse(p.Status.Renames); err != nil {
			bad("%s: status.renames %q, and the dry run listed %d renames", *name, p.Status.Renames, len(renames))
		}
	}
	r, err := view.ReadRenames(st, table)
	if err != nil {
		bad("%s: the table %s: %v", *name, table, err)
	}
	for old, cur := range renames {
		got, err := view.ResolveOld(ctx, st, table, old)
		if err != nil || got != cur {
			bad("%s: %s resolves to %s (%v), the dry run said %s", *name, old, got, err, cur)
			continue
		}
		if got, err := project.Current(ctx, c, st, *name, old); err != nil || got != cur {
			bad("%s: project.Current of %s is %s (%v)", *name, old, got, err)
		}
		if _, err := view.Load(ctx, cur, view.StoreRecords(st), 16); err != nil {
			bad("%s: %s, renamed from %s, does not load: %v", *name, cur, old, err)
			continue
		}
		if v, err := view.Reidentify(ctx, old, view.StoreRecords(st), 16); err != nil || v.ID != cur {
			bad("%s: the table line %s %s does not verify: %v", *name, old, cur, err)
		}
	}
	ok("%s: %d renames, table %s with %d lines", *name, len(renames), table, len(r))

	// Grants.
	have := map[string]bool{}
	for _, g := range p.Spec.Grants {
		have[g.Tree+" "+strings.Join(g.Paths, ",")] = true
	}
	n := 0
	for _, g := range p.Spec.Grants {
		id, err := c4.Parse(g.Tree)
		if err != nil {
			continue
		}
		if cur := r.Current(id); cur != id {
			n++
			if !have[cur.String()+" "+strings.Join(g.Paths, ",")] {
				bad("%s: grant %s %v has no entry for %s", *name, g.Tree, g.Paths, cur)
			}
		}
	}
	ok("%s: %d grants on renamed trees, each with its new entry", *name, n)

	// Materializations Ready before the migration.
	var before struct {
		Items []c4v1.Materialization `json:"items"`
	}
	data, err := os.ReadFile(*pre)
	if err != nil {
		panic(err)
	}
	if err := json.Unmarshal(data, &before); err != nil {
		panic(err)
	}
	in := map[string]bool{}
	for _, ns := range p.Spec.Namespaces {
		in[ns] = true
	}
	checked := 0
	for _, m := range before.Items {
		if !in[m.Namespace] || m.Status.Phase != c4v1.PhaseReady {
			continue
		}
		checked++
		oldView, _ := c4.Parse(m.Status.ViewID)
		want := r.Current(oldView).String()
		var now c4v1.Materialization
		deadline := time.Now().Add(3 * time.Minute)
		for {
			err := c.Get(ctx, types.NamespacedName{Namespace: m.Namespace, Name: m.Name}, &now)
			if err == nil && now.Status.Phase == c4v1.PhaseReady && now.Status.ObservedGeneration == now.Generation && now.Status.ViewID == want {
				break
			}
			if time.Now().After(deadline) {
				bad("%s: Materialization %s/%s is %s at %s, want Ready at %s (was %s)", *name, m.Namespace, m.Name, now.Status.Phase, now.Status.ViewID, want, m.Status.ViewID)
				break
			}
			time.Sleep(time.Second)
		}
	}
	ok("%s: %d Materializations Ready before, checked", *name, checked)

	if *mapping != "" {
		m, err := gitimport.ReadMapping(*mapping)
		if err != nil {
			bad("mapping %s: %v", *mapping, err)
		} else {
			old := 0
			refs := map[string]bool{}
			for _, l := range m {
				for _, id := range []c4.ID{l.View, l.ParentView, l.ChainBase} {
					if r.Current(id) != id {
						old++
					}
				}
				if l.Ref != "" {
					refs[l.Ref] = true
				}
			}
			if old > 0 {
				bad("mapping %s still reads %d old IDs", *mapping, old)
			}
			tmp, _ := os.MkdirTemp("", "pincheck-")
			defer os.RemoveAll(tmp)
			for ref := range refs {
				rc, recorded := gitimport.ChainRecorded(m, ref)
				if !recorded {
					continue
				}
				ch, _, err := gitimport.OpenRecordedChain(ctx, filepath.Join(tmp, gitimport.ChainFileName(ref)), view.StoreRecords(st), m, ref)
				if err != nil || ch.Head != rc.Head {
					bad("mapping %s: ref %s's chain: %v (%v)", *mapping, ref, ch, err)
				}
			}
			ok("mapping %s: %d lines, %d refs, no old IDs, chains made again", *mapping, len(m), len(refs))
		}
	}
	if failed {
		os.Exit(1)
	}
}
