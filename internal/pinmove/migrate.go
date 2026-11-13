package pinmove

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/Avalanche-io/c4"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/gitimport"
	"github.com/Avalanche-io/c4-operator/internal/view"
)

// Options say which project to migrate, and how.
type Options struct {
	Client  client.Client
	Project string
	// Store opens the project's store (project.Registry.Store).
	Store func(ctx context.Context, p *c4v1.Project) (Store, error)
	// Mappings are git-import mapping files of the project's store, on
	// this machine: each gains a renamed line.
	Mappings []string
	// FarmNamespace is the operator's namespace, whose Tools are the
	// farm's: their versions published from this project are migrated.
	FarmNamespace string
	// Named are views named outside the cluster (a tracker's link, an ID
	// handed to a client after an ingest): no holder here names them, and
	// the store is not listed, so they are migrated only when named.
	Named []c4.ID
	// Write does the migration. Without it nothing is written anywhere:
	// the report says what a run would do.
	Write bool
	// Out is the report, one line per thing done (or, without Write, to
	// do): "rename <old> <new> <holder> <path>", "missing <id> <holder>
	// <path> [record <id>]", "notaview <id> <holder> <path> <why>",
	// "unfinished <ns>/<name> <phase>", "table <prior> <table>",
	// "rematerialize <ns>/<name>", "tool <ns>/<name> <old> <new>",
	// "grant <project> <tree> <paths>", "mapping <path> <views>". Log
	// narrates.
	Out, Log io.Writer
	Workers  int
}

// Migrate migrates one project: every view its holders name re-identified,
// the records of each renamed one stored under its new ID, and then the
// renames table, named in the Project's status; that status write is the
// commit point. After it, the Materializations whose status names a
// renamed view are cleared to resolve again, the Tools' versions name the
// new IDs (a version is the farm's, read in every project it is spread
// to, so it is translated, not mapped by each reader), the Project's
// grants gain the new IDs, and the mappings gain a renamed line. A run that stops part way
// is resumed by running again.
//
// Run it once per project at a time, with the operator stopped: two runs
// at once write the status last-writer-wins (a run after them repairs it).
//
// It refuses while a WorkJob in the project's namespaces has not finished:
// a live output's chain rests on views as they are stored, and its next
// fold would build on them. Drain those, then run again.
func Migrate(ctx context.Context, o Options) error {
	if o.Workers <= 0 {
		o.Workers = 16
	}
	say := func(format string, args ...any) { fmt.Fprintf(o.Log, format+"\n", args...) }
	report := func(format string, args ...any) { fmt.Fprintf(o.Out, format+"\n", args...) }
	var p c4v1.Project
	if err := o.Client.Get(ctx, types.NamespacedName{Name: o.Project}, &p); err != nil {
		return err
	}
	st, err := o.Store(ctx, &p)
	if err != nil {
		return fmt.Errorf("project %s store: %w", p.Name, err)
	}
	var prior c4.ID
	if p.Status.Renames != "" {
		if prior, err = c4.Parse(p.Status.Renames); err != nil {
			return fmt.Errorf("project %s status.renames: %w", p.Name, err)
		}
	}
	priorTable, err := view.ReadRenames(st, prior)
	if err != nil {
		return err
	}

	held, err := IDsIn("Project "+p.Name, p.Spec)
	if err != nil {
		return err
	}
	var unfinished []string
	var mats []c4v1.Materialization
	for _, ns := range p.Spec.Namespaces {
		var ml c4v1.MaterializationList
		if err := o.Client.List(ctx, &ml, client.InNamespace(ns)); err != nil {
			return err
		}
		for i := range ml.Items {
			m := &ml.Items[i]
			h, err := IDsIn("Materialization "+ns+"/"+m.Name, m)
			if err != nil {
				return err
			}
			held = append(held, h...)
			mats = append(mats, *m)
		}
		var jl c4v1.WorkJobList
		if err := o.Client.List(ctx, &jl, client.InNamespace(ns)); err != nil {
			return err
		}
		for i := range jl.Items {
			j := &jl.Items[i]
			if !Finished(j) {
				unfinished = append(unfinished, ns+"/"+j.Name)
				report("unfinished %s/%s %s", ns, j.Name, orDash(j.Status.Phase))
				continue
			}
			h, err := IDsIn("WorkJob "+ns+"/"+j.Name, j)
			if err != nil {
				return err
			}
			held = append(held, h...)
		}
	}
	tools, err := toolsOf(ctx, o.Client, &p, o.FarmNamespace)
	if err != nil {
		return err
	}
	for _, t := range tools {
		for _, at := range toolTrees(&t, p.Name, o.FarmNamespace) {
			if id, err := c4.Parse(*at.view); err == nil {
				held = append(held, Held{ID: id, Holder: "Tool " + t.Namespace + "/" + t.Name, Path: at.path})
			}
		}
	}
	refusal := func() error {
		return fmt.Errorf("%d WorkJobs in project %s have not finished: %s. Drain these, then run again", len(unfinished), p.Name, strings.Join(unfinished, ", "))
	}
	if len(unfinished) > 0 && o.Write {
		return refusal()
	}
	for i, id := range o.Named {
		held = append(held, Held{ID: id, Holder: "named", Path: fmt.Sprintf("[%d]", i)})
	}
	for _, path := range o.Mappings {
		m, err := gitimport.ReadMapping(path)
		if err != nil {
			return fmt.Errorf("mapping %s: %w", path, err)
		}
		for i, c := range m {
			for field, id := range map[string]c4.ID{"view": c.View, "parentView": c.ParentView, "chainBase": c.ChainBase} {
				if !id.IsNil() {
					held = append(held, Held{ID: id, Holder: "mapping " + path, Path: fmt.Sprintf(".line%d.%s", i+1, field)})
				}
			}
		}
	}

	plan, err := Survey(ctx, st, priorTable, held, o.Workers)
	if err != nil {
		return err
	}
	var notViews int
	for _, f := range plan.Findings {
		switch {
		case f.Missing:
			under := ""
			if !f.MissingRecord.IsNil() && f.MissingRecord != f.ID {
				under = " record " + f.MissingRecord.String()
			}
			for _, h := range f.Held {
				report("missing %s %s %s%s", f.ID, h.Holder, h.Path, under)
			}
		case f.NotAView != "":
			notViews++
			for _, h := range f.Held {
				report("notaview %s %s %s %s", f.ID, h.Holder, h.Path, f.NotAView)
			}
		case f.New != f.ID && priorTable.Current(f.ID) == f.ID:
			// Renamed by this run; one an earlier run renamed is in the
			// count below, its holders left naming the ID as stored.
			for _, h := range f.Held {
				report("rename %s %s %s %s", f.ID, f.New, h.Holder, h.Path)
			}
		}
	}
	renames := plan.Renames()
	say("project %s: %d C4 IDs named, %d not views, %d views renamed by this run, %d by earlier runs", p.Name, len(plan.Findings), notViews, len(renames), len(priorTable))
	table := prior
	if len(renames) > 0 {
		table = c4.Identify(strings.NewReader(string(plan.Table.Bytes())))
		report("table %s %s", idOrDash(prior), table)
	}
	if !o.Write {
		defer say("dry run: nothing written")
	}

	// Materializations whose status names a view as stored: resolved
	// again, which gives the view's new ID.
	var stale []*c4v1.Materialization
	for i := range mats {
		m := &mats[i]
		if renamedIn(plan.Table, append([]string{m.Status.ViewID}, m.Status.Layers...)) {
			stale = append(stale, m)
			report("rematerialize %s/%s", m.Namespace, m.Name)
		}
	}
	var retool []*c4v1.Tool
	for i := range tools {
		t := &tools[i]
		changes := translateTool(t.DeepCopy(), plan.Table, p.Name, o.FarmNamespace)
		for _, ch := range changes {
			report("tool %s/%s %s %s", t.Namespace, t.Name, ch[0], ch[1])
		}
		if len(changes) > 0 {
			retool = append(retool, t)
		}
	}
	_, added := ExtendGrants(p.Spec.Grants, plan.Table)
	for _, g := range added {
		report("grant %s %s %s", p.Name, g.Tree, orDash(strings.Join(g.Paths, ",")))
	}
	var lines []string
	for _, path := range o.Mappings {
		m, err := gitimport.ReadMapping(path)
		if err != nil {
			return err
		}
		if line := gitimport.Renamed(m, plan.Table); len(line.Renamed) > 0 {
			report("mapping %s %d", path, len(line.Renamed))
			lines = append(lines, path)
		}
	}
	if !o.Write {
		if len(unfinished) > 0 {
			return refusal()
		}
		return nil
	}

	// Nothing is written unless the API server keeps what the commit
	// point writes: a Project CRD from before status.renames prunes it.
	if err := CheckSchema(ctx, o.Client); err != nil {
		return err
	}
	// The records, then the table, then the status that names it.
	written, err := plan.WriteTable(ctx, st, prior)
	if err != nil {
		return err
	}
	if written != table {
		return fmt.Errorf("the table stored as %s, and it is %s", written, table)
	}
	if table != prior {
		orig := p.DeepCopy()
		p.Status.Renames = table.String()
		if err := o.Client.Status().Patch(ctx, &p, client.MergeFrom(orig)); err != nil {
			return fmt.Errorf("project %s status: %w", p.Name, err)
		}
		if err := readBack(ctx, o.Client, p.Name, table); err != nil {
			return err
		}
		say("project %s names the renames table %s", p.Name, table)
	}
	for _, m := range stale {
		orig := m.DeepCopy()
		m.Status.ObservedGeneration = 0
		if err := o.Client.Status().Patch(ctx, m, client.MergeFrom(orig)); err != nil {
			return fmt.Errorf("materialization %s/%s: %w", m.Namespace, m.Name, err)
		}
	}
	for _, t := range retool {
		err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			var cur c4v1.Tool
			if err := o.Client.Get(ctx, types.NamespacedName{Namespace: t.Namespace, Name: t.Name}, &cur); err != nil {
				return err
			}
			if len(translateTool(&cur, plan.Table, p.Name, o.FarmNamespace)) == 0 {
				return nil
			}
			return o.Client.Status().Update(ctx, &cur)
		})
		if err != nil {
			return fmt.Errorf("tool %s/%s: %w", t.Namespace, t.Name, err)
		}
	}
	if len(added) > 0 {
		err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			var cur c4v1.Project
			if err := o.Client.Get(ctx, types.NamespacedName{Name: p.Name}, &cur); err != nil {
				return err
			}
			grants, more := ExtendGrants(cur.Spec.Grants, plan.Table)
			if len(more) == 0 {
				return nil
			}
			cur.Spec.Grants = grants
			return o.Client.Update(ctx, &cur)
		})
		if err != nil {
			return fmt.Errorf("project %s grants: %w", p.Name, err)
		}
	}
	for _, path := range lines {
		if err := appendRenamed(path, plan.Table); err != nil {
			return fmt.Errorf("mapping %s: %w", path, err)
		}
	}
	return nil
}

// appendRenamed appends the renamed line the mapping needs, under the
// mapping's lock, read again under it: a line another run (or an import)
// appended meanwhile is seen.
func appendRenamed(path string, r view.Renames) error {
	w, err := gitimport.AppendMapping(path)
	if err != nil {
		return err
	}
	m, err := gitimport.ReadMapping(path)
	if err == nil {
		if line := gitimport.Renamed(m, r); len(line.Renamed) > 0 {
			err = w.Write(line)
		}
	}
	if cerr := w.Close(); err == nil {
		err = cerr
	}
	return err
}

func renamedIn(r view.Renames, ids []string) bool {
	for _, s := range ids {
		if id, err := c4.Parse(s); err == nil && r.Current(id) != id {
			return true
		}
	}
	return false
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func idOrDash(id c4.ID) string {
	if id.IsNil() {
		return "-"
	}
	return id.String()
}

// toolsOf are the Tools whose versions the project's migration translates:
// those in its namespaces, and the farm's whose versions were published
// from its store.
func toolsOf(ctx context.Context, c client.Client, p *c4v1.Project, farm string) ([]c4v1.Tool, error) {
	var out []c4v1.Tool
	for _, ns := range p.Spec.Namespaces {
		var l c4v1.ToolList
		if err := c.List(ctx, &l, client.InNamespace(ns)); err != nil {
			return nil, err
		}
		out = append(out, l.Items...)
	}
	if farm == "" {
		return out, nil
	}
	var l c4v1.ToolList
	if err := c.List(ctx, &l, client.InNamespace(farm)); err != nil {
		return nil, err
	}
	for _, t := range l.Items {
		for _, v := range t.Status.Versions {
			if v.Project == p.Name {
				out = append(out, t)
				break
			}
		}
	}
	return out, nil
}

// toolTree is one version tree of a Tool's status, and its path there.
type toolTree struct {
	view *string
	path string
}

// toolTrees are the version trees of t the project's migration holds and
// translates. A tool of the project's own: every version and earlier one.
// A farm tool: only the versions published from the project's store. The
// others are other projects', translated by their own runs, whose stores
// hold the new records; and an earlier version names no project, so it is
// left as it is (a reader maps it through the table of the project that
// reads it).
func toolTrees(t *c4v1.Tool, project, farm string) []toolTree {
	var out []toolTree
	for i := range t.Status.Versions {
		if t.Namespace != farm || t.Status.Versions[i].Project == project {
			out = append(out, toolTree{&t.Status.Versions[i].View, fmt.Sprintf(".status.versions[%d].view", i)})
		}
	}
	if t.Namespace != farm {
		for i := range t.Status.Earlier {
			out = append(out, toolTree{&t.Status.Earlier[i].View, fmt.Sprintf(".status.earlier[%d].view", i)})
		}
	}
	return out
}

// translateTool renames, in place, each of t's version trees the project
// holds (toolTrees) that the table renames, and returns the [old, new] of
// each.
func translateTool(t *c4v1.Tool, r view.Renames, project, farm string) [][2]string {
	var out [][2]string
	for _, at := range toolTrees(t, project, farm) {
		id, err := c4.Parse(*at.view)
		if err != nil {
			continue
		}
		if cur := r.Current(id); cur != id {
			out = append(out, [2]string{*at.view, cur.String()})
			*at.view = cur.String()
		}
	}
	return out
}

// ProjectCRD is the name of the Project CustomResourceDefinition.
const ProjectCRD = "projects.c4.avalanche.io"

// CheckSchema refuses unless the API server's Project CRD has
// status.renames in the version this binary writes: one without it prunes
// the field, the run's commit point, silently. Read from the CRD itself,
// before anything is written.
func CheckSchema(ctx context.Context, c client.Reader) error {
	crd := &unstructured.Unstructured{}
	crd.SetGroupVersionKind(schema.GroupVersionKind{Group: "apiextensions.k8s.io", Version: "v1", Kind: "CustomResourceDefinition"})
	if err := c.Get(ctx, types.NamespacedName{Name: ProjectCRD}, crd); err != nil {
		return fmt.Errorf("reading the Project CRD %s: %w", ProjectCRD, err)
	}
	versions, _, _ := unstructured.NestedSlice(crd.Object, "spec", "versions")
	for _, v := range versions {
		m, ok := v.(map[string]any)
		if !ok || m["name"] != c4v1.GroupVersion.Version {
			continue
		}
		if _, found, _ := unstructured.NestedMap(m, "schema", "openAPIV3Schema", "properties", "status", "properties", "renames"); found {
			return nil
		}
		return fmt.Errorf("the installed Project CRD %s (%s) has no status.renames, so the API server would drop the table's name: apply this release's CRDs (config/crd) first, then run again", ProjectCRD, c4v1.GroupVersion.Version)
	}
	return fmt.Errorf("the installed Project CRD %s has no version %s", ProjectCRD, c4v1.GroupVersion.Version)
}

// readBack checks the Project's status names the table, as the API server
// keeps it (through a cache, it is waited for).
func readBack(ctx context.Context, c client.Reader, name string, table c4.ID) error {
	deadline := time.Now().Add(10 * time.Second)
	for {
		var p c4v1.Project
		err := c.Get(ctx, types.NamespacedName{Name: name}, &p)
		if err == nil && p.Status.Renames == table.String() {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("project %s: status.renames reads %q after it was written as %s (%v): the API server did not keep it", name, p.Status.Renames, table, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
