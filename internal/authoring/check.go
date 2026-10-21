package authoring

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Avalanche-io/c4"
	"github.com/Avalanche-io/c4-operator/internal/when"
	"github.com/Avalanche-io/c4-operator/schema"
	order_ "github.com/Avalanche-io/c4-operator/work/order"
)

// now is the clock dues are read on.
var now = time.Now

// subCtx is a submission as checking and lowering see it: its jobs by
// key, and each job's tool (a plan's each.tool) resolved on the farm.
type subCtx struct {
	sub     *Submission
	farm    Farm
	project string
	ns      string
	now     time.Time
	loc     *time.Location
	ms      map[string]time.Time
	jobs    map[string]*Job
	tools   map[string]resolved
	errs    []Error
}

type resolved struct {
	tool *Tool
	ref  ToolRef
}

func (c *subCtx) add(e Error) { c.errs = append(c.errs, e) }

// Check checks a parsed submission against the farm: the project, its
// tools and their parameters, the jobs it follows, the dues, the trees,
// and the people it names. The errors are sorted by path. A submission
// Check passes may still be one the farm cannot run yet; Lower says so.
func Check(sub *Submission, farm Farm) []Error {
	c, ok := newSubCtx(sub, farm)
	if ok {
		c.check()
	}
	return sortErrors(c.errs)
}

func newSubCtx(sub *Submission, farm Farm) (*subCtx, bool) {
	c := &subCtx{sub: sub, farm: farm, project: sub.Project, jobs: map[string]*Job{}, tools: map[string]resolved{}, now: now()}
	if c.project == "" {
		c.add(errf("project", "add project: <name>", "the submission names no project"))
		return c, false
	}
	if c.ns = farm.Namespace(c.project); c.ns == "" {
		c.add(errf("project", "c4 farm projects lists the ones you can submit to", "no project %q on this farm", c.project))
		return c, false
	}
	if c.loc = farm.Location(c.project); c.loc == nil {
		c.loc = time.UTC
	}
	c.ms = farm.Milestones(c.ns)
	for i := range sub.Jobs {
		j := &sub.Jobs[i]
		c.jobs[j.Key] = j
	}
	return c, true
}

func (c *subCtx) check() {
	sub := c.sub
	c.checkDue("due", sub.Due)
	c.checkTree("tree", sub.Tree)
	for _, name := range sortedKeys(sub.Read) {
		c.checkTree("read."+name, sub.Read[name])
	}
	c.checkNames()
	c.checkPolicy("policy", sub.Policy)
	c.checkFor()
	c.checkTracker()
	c.resolveTools()
	for i := range sub.Jobs {
		c.checkJob(&sub.Jobs[i])
		c.checkPolicy("jobs."+sub.Jobs[i].Key+".policy", sub.Jobs[i].Policy)
	}
	c.checkCycles()
}

// checkDue refuses a due that is no milestone, time or date.
func (c *subCtx) checkDue(path, s string) {
	if s == "" {
		return
	}
	if _, ok := readDue(s, c.ms, c.now, c.loc); !ok {
		c.add(dueError(path, s, c.project, c.ms))
	}
}

// checkTree refuses a job: tree that names no job in the namespace. A
// folder, a C4 ID and a git: tree are for the front end and the lowering.
func (c *subCtx) checkTree(path, s string) {
	name, ok := strings.CutPrefix(s, "job:")
	if !ok {
		return
	}
	jobs := c.farm.Jobs(c.ns)
	if Latest(jobs, name) != "" {
		return
	}
	fix := "c4 farm jobs lists them"
	if s, ok := closest(name, stems(jobs)); ok {
		fix = fmt.Sprintf("did you mean %q?", s)
	}
	c.add(errf(path, fix, "no job %q in %s", name, c.project))
}

// stems are job names and the names without their versions.
func stems(jobs []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, j := range jobs {
		for _, n := range []string{versionSuffix.ReplaceAllString(j, ""), j} {
			if !seen[n] {
				seen[n] = true
				out = append(out, n)
			}
		}
	}
	return out
}

// checkNames refuses a job whose name, <name>-<key>-vNNN, would be too
// long.
func (c *subCtx) checkNames() {
	path, stem := "name", c.sub.Name
	if stem == "" {
		path, stem = "shot", c.sub.Shot
	}
	if stem != "" && JobStem(stem) == "" {
		c.add(errf(path, "set name: to override", "%s can't name a job: use letters, digits and -", stem))
		return
	}
	jobs := c.farm.Jobs(c.ns)
	for _, j := range c.sub.Jobs {
		if n := NextName(jobs, stem, j.Key); len(n) > MaxJobName {
			c.add(errf("jobs."+j.Key, "shorten the job's key, or set name: to a shorter stem",
				"%q is longer than a job's name may be (%d characters)", n, MaxJobName))
		}
	}
}

// checkPolicy refuses a policy set the project does not have, and a
// narrowed value that is not the project's to narrow or is above the
// project's own: a submission names and narrows policy, never defines or
// widens it.
func (c *subCtx) checkPolicy(path string, p *schema.Policy) {
	fp := c.farm.Policy(c.project)
	if p == nil || fp == nil {
		return
	}
	if p.Set != "" && !has(fp.Sets, p.Set) {
		fix := "c4 farm policy lists them"
		if s, ok := closest(p.Set, fp.Sets); ok {
			fix = fmt.Sprintf("did you mean %q?", s)
		}
		c.add(errf(path+".set", fix, "no policy set %q in %s", p.Set, c.project))
	}
	for _, k := range sortedKeys(p.Narrow) {
		at := path + ".narrow." + k
		v, ok := fp.Values[k]
		switch {
		case !ok:
			fix := "the values it may narrow are " + strings.Join(sortedKeys(fp.Values), ", ")
			if s, ok := closest(k, sortedKeys(fp.Values)); ok {
				fix = fmt.Sprintf("did you mean %q?", s)
			}
			c.add(errf(at, fix, "%s is not a value of %s's policy", k, c.project))
		case !v.Narrowable:
			c.add(errf(at, "leave it out", "%s is %s's to set; a submission may not narrow it", k, c.project))
		default:
			want, have := plain(p.Narrow[k]), plain(v.Value)
			a, aok := policyNumber(want)
			b, bok := policyNumber(have)
			switch {
			case !aok || !bok || aok != bok:
				c.add(errf(at, fmt.Sprintf("write it as %s's is written: %v", c.project, have), "%v is not a value %s takes", want, k))
			case a > b:
				c.add(errf(at, fmt.Sprintf("use %v or less, or leave it out", have), "%s %v is above %s's %v; a submission may only lower it", k, want, c.project, have))
			}
		}
	}
}

// policyNumber is a policy value as a number to compare: a number, or a
// duration (30m, 2h, 2d) in seconds.
func policyNumber(v any) (float64, bool) {
	if n, ok := toFloat(v); ok {
		return n, true
	}
	s, ok := v.(string)
	if !ok {
		return 0, false
	}
	if d, ok := strings.CutSuffix(s, "d"); ok {
		if n, err := strconv.ParseFloat(d, 64); err == nil {
			return n * 86400, true
		}
	}
	d, err := time.ParseDuration(s)
	return d.Seconds(), err == nil
}

func splitTool(s string) (name, version string) {
	name, version, _ = strings.Cut(s, "@")
	return name, version
}

// tool resolves a tool a job names (tool, each.tool, check.tool, a uses
// key), or reports why not at path.
func (c *subCtx) tool(path, s string) (*Tool, ToolRef, bool) {
	name, version := splitTool(s)
	t, ref, err := c.farm.Tool(c.project, name, version)
	if err == nil && t != nil {
		return t, ref, true
	}
	refs := c.farm.Tools(c.project)
	names := toolNames(refs)
	switch {
	case !has(names, name):
		fix := "c4 farm tools lists them"
		if s, ok := closest(name, names); ok {
			fix = fmt.Sprintf("did you mean %q? %s", s, fix)
		}
		c.add(errf(path, fix, "no tool %q in %s", name, c.project))
	case errors.Is(err, ErrNoTool):
		fix := "c4 farm tools " + name + " lists its versions"
		for _, r := range refs {
			if r.Name == name {
				fix = "the current one is " + r.Version + "; " + fix
			}
		}
		c.add(errf(path, fix, "no version %s of %s in %s", version, name, c.project))
	default:
		c.add(errf(path, "try again, or ask a farm admin", "%s cannot be read: %v", s, err))
	}
	return nil, ToolRef{}, false
}

var versioned = regexp.MustCompile(`-v\d+$`)

func (c *subCtx) checkJob(j *Job) {
	p := "jobs." + j.Key
	keys := make([]string, len(c.sub.Jobs))
	for i, x := range c.sub.Jobs {
		keys[i] = x.Key
	}
	r, ok := c.tools[j.Key]
	gen, genRef := r.tool, r.ref
	switch {
	case ok && j.Tool != "":
		c.checkWith(p, r.tool, j)
	case ok && j.Each != nil:
		_, errs := resolve(r.tool, j.Each.With, "", "", false)
		c.errs = append(c.errs, prefix(p+".each", errs)...)
	}
	for _, a := range j.After {
		if a.Job == j.Key {
			c.add(errf(p+".after", "remove "+j.Key+" from after", "%s follows itself", j.Key))
			continue
		}
		if c.jobs[a.Job] != nil {
			continue
		}
		fix := "jobs follow jobs by their key in the file: " + strings.Join(keys, ", ")
		if s, ok := closest(versioned.ReplaceAllString(a.Job, ""), keys); ok {
			fix = fmt.Sprintf("did you mean %q?", s)
			if versioned.MatchString(a.Job) {
				fix = "jobs follow jobs by their key in the file; " + fix
			}
		}
		c.add(errf(p+".after", fix, "no job %q in this submission", a.Job))
	}
	c.checkDue(p+".due", j.Due)
	if len(j.Uses) > 0 && gen != nil {
		c.checkUses(p, j, gen, genRef)
	}
	if ch := j.Check; ch != nil {
		if t, _, ok := c.tool(p+".check.tool", ch.Tool); ok {
			if t.Kind != "check" {
				c.add(errf(p+".check.tool", "use a check tool, one whose kind is check", "%s is a %s tool, not a check", t.Name, t.Kind))
			}
			_, errs := resolve(t, ch.With, "", "", true)
			c.errs = append(c.errs, prefix(p+".check", errs)...)
		}
		c.checkWho(p+".check.then", ch.Then)
	}
	if a := j.Approve; a != nil {
		c.checkWho(p+".approve.by", a.By)
	}
	if w := j.Wait; w != nil {
		c.checkWho(p+".wait.for", w.For)
		if w.Until != "" {
			if t, err := when.Parse(w.Until, c.now, c.loc); err != nil || t.IsZero() {
				c.add(errf(p+".wait.until", "write a time: fri 18:00, +3h, 2026-10-24T06:00:00-04:00", "%q is not a time", w.Until))
			}
		}
	}
	if a := j.Answers; a != nil {
		if name, ok := strings.CutPrefix(a.By, "tool:"); ok {
			c.tool(p+".answers.by", name)
		}
		if name, ok := strings.CutPrefix(a.By, "unit:"); ok && r.tool != nil && j.Tool != "" {
			c.checkAnswerer(p, j, r.tool, name)
		}
	}
}

// checkAnswerer refuses an answers: unit that is not a unit of the job, or
// one that cannot answer: a frames or once tool's units, named <key>/<n>
// from 0 (or <n>), each run a procedure, and only a unit that reasons
// answers questions.
func (c *subCtx) checkAnswerer(p string, j *Job, t *Tool, name string) {
	n := c.unitCount(j, t)
	i, err := strconv.Atoi(strings.TrimPrefix(name, j.Key+"/"))
	switch {
	case err != nil || i < 0 || n > 0 && i >= n:
		fix := "name a unit of the job, or leave answers: out"
		if n > 0 {
			fix = fmt.Sprintf("name one of its units, %s/0 to %s/%d, or leave answers: out", j.Key, j.Key, n-1)
		}
		c.add(errf(p+".answers.by", fix, "no unit %q in %s", name, j.Key))
	default:
		c.add(errf(p+".answers.by", "leave answers: out: only a unit that reasons answers questions",
			"%s/%d cannot answer questions: it runs a procedure", j.Key, i))
	}
}

// unitCount is how many units a frames or once job makes, 0 when its
// frames are not known.
func (c *subCtx) unitCount(j *Job, t *Tool) int {
	if t.Kind == "once" {
		return 1
	}
	fs, err := order_.ParseFrames(farmFrames(c.framesOf(j.Key, map[string]bool{})))
	if err != nil {
		return 0
	}
	chunk := j.Chunk
	if chunk == nil && t.Frames != nil {
		chunk = t.Frames.Chunk
	}
	switch {
	case chunk == nil || chunk.N < 1 && !chunk.All:
		return len(fs)
	case chunk.All:
		return 1
	}
	return (len(fs) + chunk.N - 1) / chunk.N
}

// resolveTools resolves every job's tool (a plan's each.tool) before any
// job is checked: a job's frames may come from a job written after it.
func (c *subCtx) resolveTools() {
	for _, j := range c.sub.Jobs {
		p := "jobs." + j.Key
		switch {
		case j.Tool != "":
			if t, ref, ok := c.tool(p+".tool", j.Tool); ok {
				c.tools[j.Key] = resolved{t, ref}
			}
		case j.Each != nil:
			if t, ref, ok := c.tool(p+".each.tool", j.Each.Tool); ok {
				c.tools[j.Key] = resolved{t, ref}
			}
		}
	}
}

// checkFor refuses a for: that is no person or party the farm knows,
// when it keeps a list of them.
func (c *subCtx) checkFor() {
	who := c.sub.For
	people, parties := c.farm.People(), c.farm.Parties()
	if who == "" || people == nil && parties == nil || has(people, who) || has(parties, who) {
		return
	}
	known := append(append([]string(nil), people...), parties...)
	fix := "for: names a person or party the work is for: " + strings.Join(sortedStrings(known), ", ")
	if s, ok := closest(who, known); ok {
		fix = fmt.Sprintf("did you mean %q?", s)
	}
	c.add(errf("for", fix, "no person or party %q on this farm", who))
}

// checkTracker refuses a tracker: whose tracker the project has not
// configured, when the farm can say which it has.
func (c *subCtx) checkTracker() {
	ref := c.sub.Tracker
	trackers := c.farm.Trackers(c.project)
	if ref == "" || trackers == nil {
		return
	}
	name, _, _ := strings.Cut(ref, "/")
	if has(trackers, name) {
		return
	}
	fix := "leave tracker: out, or ask a farm admin to connect the tracker"
	if len(trackers) > 0 {
		fix = "the project's trackers are " + strings.Join(sortedStrings(trackers), ", ")
	}
	if s, ok := closest(name, trackers); ok {
		fix = fmt.Sprintf("did you mean %q?", s)
	}
	c.add(errf("tracker", fix, "no tracker %q in %s", name, c.project))
}

// checkWith checks a job's with against its tool, and its frames and size.
func (c *subCtx) checkWith(p string, t *Tool, j *Job) {
	if e := onceFrames(t, j.Frames); e != nil {
		c.add(prefix(p, []Error{*e})[0])
	}
	frames := c.framesOf(j.Key, map[string]bool{})
	if t.Kind == "frames" && frames == "" {
		shot := c.sub.Shot
		why := shot + " has no cut on the farm"
		if shot == "" {
			why = "the submission names no shot"
		}
		c.add(errf(p+".frames", "add frames: (as 1001-1240)", "%s needs frames: %s and %s follows no job with frames", j.Key, why, j.Key))
	}
	_, errs := resolve(t, j.With, "", frames, true)
	c.errs = append(c.errs, prefix(p, errs)...)
	if j.Size != "" {
		if _, ok := t.Needs.Sizes[j.Size]; !ok {
			fix := t.Name + " has no sizes"
			if names := sortedKeys(t.Needs.Sizes); len(names) > 0 {
				fix = "its sizes are " + strings.Join(names, ", ")
			}
			c.add(errf(p+".size", fix, "%s has no size %q", t.Name, j.Size))
		}
	}
}

// framesOf is a job's frames: its own, else those of the one frames job
// it follows, else the shot's cut, else "". A once job's frame parameter
// takes the same default.
func (c *subCtx) framesOf(key string, seen map[string]bool) Frames {
	j := c.jobs[key]
	if j == nil {
		return ""
	}
	if j.Frames != "" {
		return j.Frames
	}
	if !seen[key] && len(j.After) == 1 {
		seen[key] = true
		if up := c.jobs[j.After[0].Job]; up != nil {
			if r, ok := c.tools[up.Key]; ok && r.tool.Kind == "frames" {
				if f := c.framesOf(up.Key, seen); f != "" {
					return f
				}
			}
		}
	}
	return Frames(c.farm.Cut(c.project, c.sub.Shot))
}

// checkUses checks a generator's children's settings: each key a tool it
// emits, each with against that tool, nothing required (the generator
// gives the rest).
func (c *subCtx) checkUses(p string, j *Job, gen *Tool, ref ToolRef) {
	for _, k := range sortedKeys(j.Uses) {
		path := p + ".uses." + k
		if !has(gen.Emits, k) {
			at := gen.Name + "@" + ref.Version
			if len(gen.Emits) == 0 {
				c.add(errf(path, "remove uses:", "%s emits no work, so it has no children to set", at))
				continue
			}
			fix := fmt.Sprintf("rename the key to one of them, or publish a %s that emits %s", gen.Name, k)
			if s, ok := closestKey(k, gen.Emits); ok {
				fix = fmt.Sprintf("rename the key to %q, or publish a %s that emits %s", s, gen.Name, k)
			}
			c.add(errf(path, fix, "%s does not emit %q; it emits %s", at, k, strings.Join(gen.Emits, ", ")))
			continue
		}
		u := j.Uses[k]
		t, _, ok := c.tool(path, k)
		if !ok {
			continue
		}
		_, errs := resolve(t, u.With, "", "", false)
		c.errs = append(c.errs, prefix(path, errs)...)
		if u.Size != "" {
			if _, ok := t.Needs.Sizes[u.Size]; !ok {
				c.add(errf(path+".size", "its sizes are "+strings.Join(sortedKeys(t.Needs.Sizes), ", "), "%s has no size %q", t.Name, u.Size))
			}
		}
	}
}

// closestKey is closest, else a candidate the key ends or starts with
// (idiom-review for review).
func closestKey(k string, cands []string) (string, bool) {
	if s, ok := closest(k, cands); ok {
		return s, true
	}
	best := ""
	for _, s := range cands {
		if (strings.HasSuffix(k, "-"+s) || strings.HasPrefix(k, s+"-")) && len(s) > len(best) {
			best = s
		}
	}
	return best, best != ""
}

// checkWho refuses a person, role or party the farm does not know, when
// it keeps a list of them.
func (c *subCtx) checkWho(path, who string) {
	kind, name, ok := strings.Cut(who, ":")
	if !ok {
		return
	}
	var list []string
	switch kind {
	case "person":
		list = c.farm.People()
	case "role":
		list = c.farm.Roles()
	case "party":
		list = c.farm.Parties()
	default:
		return
	}
	if list == nil || has(list, name) {
		return
	}
	fix := fmt.Sprintf("the farm's are %s", strings.Join(sortedStrings(list), ", "))
	if s, ok := closest(name, list); ok {
		fix = fmt.Sprintf("did you mean %q?", kind+":"+s)
	}
	c.add(errf(path, fix, "no %s %q on this farm", kind, name))
}

// checkCycles refuses jobs that follow each other, where the cycle
// closes, reading the jobs in the order written.
func (c *subCtx) checkCycles() {
	order := map[string]int{}
	for i, j := range c.sub.Jobs {
		order[j.Key] = i
	}
	edges := map[string][]string{} // a job to those it follows, as read so far
	var path func(from, to string, seen map[string]bool) []string
	path = func(from, to string, seen map[string]bool) []string {
		if from == to {
			return []string{to}
		}
		if seen[from] {
			return nil
		}
		seen[from] = true
		for _, n := range edges[from] {
			if p := path(n, to, seen); p != nil {
				return append([]string{from}, p...)
			}
		}
		return nil
	}
	for _, j := range c.sub.Jobs {
		for _, a := range j.After {
			if c.jobs[a.Job] == nil || a.Job == j.Key {
				continue
			}
			ring := path(a.Job, j.Key, map[string]bool{})
			if ring == nil {
				edges[j.Key] = append(edges[j.Key], a.Job)
				continue
			}
			sort.Slice(ring, func(x, y int) bool { return order[ring[x]] < order[ring[y]] })
			p := "jobs." + j.Key + ".after"
			if len(ring) == 2 {
				c.add(errf(p, "remove one of the two after fields", "%s and %s follow each other, which is a cycle", ring[0], ring[1]))
			} else {
				c.add(errf(p, "remove one of the after fields", "%s follow each other in a ring, which is a cycle", andList(ring)))
			}
		}
	}
}

func andList(ss []string) string {
	if len(ss) < 2 {
		return strings.Join(ss, "")
	}
	return strings.Join(ss[:len(ss)-1], ", ") + " and " + ss[len(ss)-1]
}

// sortErrors sorts errors by path, keeping the order of those at one.
func sortErrors(errs []Error) []Error {
	sort.SliceStable(errs, func(i, j int) bool { return errs[i].Path < errs[j].Path })
	return errs
}

// isID says whether s is a C4 ID.
func isID(s string) bool {
	_, err := c4.Parse(s)
	return err == nil
}
