package authoring

import (
	"encoding/json"
	"fmt"
	"path"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/hud/api"
	"github.com/Avalanche-io/c4-operator/internal/work/queue"
	order_ "github.com/Avalanche-io/c4-operator/work/order"
)

// AnnotationSubmission on a WorkJob is the C4 ID of the submission file
// it was lowered from (what was sent), AnnotationSubmissionRecord the ID
// of the signed record of submitting it (which act: the same file sent
// twice is two submissions), and AnnotationNote the submission's note.
const (
	AnnotationSubmission       = "c4.avalanche.io/submission"
	AnnotationSubmissionRecord = "c4.avalanche.io/submission-record"
	AnnotationNote             = "c4.avalanche.io/note"
)

// A Plan is a submission lowered: one POST /jobs request per job, in the
// order written, named and joined to each other.
type Plan struct {
	Project   string
	Namespace string
	Jobs      []PlannedJob
	// Warnings are what the farm does differently from what the file
	// says, until it can.
	Warnings []string
}

// PlannedJob is one job of a plan.
type PlannedJob struct {
	Key     string
	Tool    ToolRef
	Request *api.SubmitRequest
	// After are the names of the jobs it follows.
	After []string
	// Labels and Annotations go on the WorkJob besides the request's.
	Labels, Annotations map[string]string
	// Frames are the frames it runs over (a frames tool) or fills its
	// frame parameter with (a once tool), Units how many units it makes,
	// and Due its due, read.
	Frames Frames
	Units  int
	Due    Due
	// Note is the submission's note:, a line for the job's book.
	Note string
}

// What is the job in a line.
func (j PlannedJob) What() string {
	var s string
	switch {
	case j.Units == 1 && j.Frames != "":
		s = "one unit over " + string(j.Frames)
	case j.Units == 1:
		s = "one unit"
	default:
		s = fmt.Sprintf("%s frames in %d units", string(j.Frames), j.Units)
		if fs, err := order_.ParseFrames(farmFrames(j.Frames)); err == nil {
			s = fmt.Sprintf("%d frames in %d units", len(fs), j.Units)
		}
	}
	if len(j.After) > 0 {
		s += ", after " + strings.Join(j.After, ", ")
	}
	return s
}

// Lower checks a submission (Check) and lowers it to its jobs. The errors
// are Check's when it refuses, else what the farm cannot run yet, else
// what lowering a job found; each list sorted by path.
func Lower(sub *Submission, farm Farm) (*Plan, []Error) {
	c, ok := newSubCtx(sub, farm)
	if !ok {
		return nil, sortErrors(c.errs)
	}
	c.check()
	if len(c.errs) > 0 {
		return nil, sortErrors(c.errs)
	}
	if errs := c.refusals(); len(errs) > 0 {
		return nil, sortErrors(errs)
	}
	return c.lower()
}

// refusals are what the farm cannot run yet, in its words: it refuses the
// submission rather than run something other than what the file says.
func (c *subCtx) refusals() []Error {
	var errs []Error
	sub := c.sub
	if strings.HasPrefix(sub.Tree, "git:") {
		errs = append(errs, errf("tree", "ask a farm admin to import the repository, and submit its tree by its C4 ID", "git trees cannot be submitted on this farm yet"))
	}
	for _, name := range sortedKeys(sub.Read) {
		if strings.HasPrefix(sub.Read[name], "git:") {
			errs = append(errs, errf("read."+name, "ask a farm admin to import the repository, and read its tree by its C4 ID", "git trees cannot be read on this farm yet"))
		}
	}
	if sub.Policy != nil {
		errs = append(errs, errf("policy", "remove policy: for now", "policies cannot be applied on this farm yet"))
	}
	for _, j := range sub.Jobs {
		p := "jobs." + j.Key
		if j.Policy != nil {
			errs = append(errs, errf(p+".policy", "remove policy: for now", "policies cannot be applied on this farm yet"))
		}
		if j.Plan != "" {
			errs = append(errs, errf(p+".plan", "submit the plan's blocks as jobs for now", "this farm cannot run plans yet"))
		}
		if r, ok := c.tools[j.Key]; ok && j.Tool != "" {
			errs = append(errs, prefix(p, ToolRefusals(r.tool))...)
		}
		if len(j.After) > 1 {
			errs = append(errs, errf(p+".after", "follow one job, and have that one follow the other", "a job that follows two jobs cannot run on this farm yet"))
		}
		for _, f := range []struct {
			field, what string
			set         bool
		}{
			{"check", "checks cannot run inside a job", j.Check != nil},
			{"approve", "approvals cannot be part of a job", j.Approve != nil},
			{"wait", "waits cannot be part of a job", j.Wait != nil},
			{"uses", "a generator's children cannot run", len(j.Uses) > 0},
			{"over", "units over more than frames cannot run", len(j.Over) > 0},
			{"version", "a given version cannot be set", j.Version != 0},
		} {
			if f.set {
				errs = append(errs, errf(p+"."+f.field, "remove "+f.field+": for now", "%s on this farm yet", f.what))
			}
		}
		if a := j.Answers; a != nil {
			// Only a unit of the job answers its questions for now.
			switch {
			case a.By == "lead":
				errs = append(errs, errf(p+".answers.by", "name a unit of the job: unit:<name>", "a lead answering a job's questions cannot run on this farm yet"))
			case strings.HasPrefix(a.By, "tool:"):
				errs = append(errs, errf(p+".answers.by", "name a unit of the job: unit:<name>", "a tool answering a job's questions cannot run on this farm yet"))
			case len(a.With) > 0:
				errs = append(errs, errf(p+".answers.with", "remove answers.with: for now", "settings for an answering unit cannot be applied on this farm yet"))
			}
		}
		switch pv := j.Preview; {
		case pv == "" || pv == "fml" || pv == "none":
		default:
			errs = append(errs, errf(p+".preview", "use preview: fml or none for now",
				"this farm orders only fml and none so far; preview %q cannot run on this farm yet", pv))
		}
	}
	return errs
}

// lower makes each job's request. Names are numbered for the whole
// submission first, so a job's inputs and after can name the jobs it
// follows before they exist.
func (c *subCtx) lower() (*Plan, []Error) {
	sub := c.sub
	stem := sub.Name
	if stem == "" {
		stem = sub.Shot
	}
	existing := c.farm.Jobs(c.ns)
	names := map[string]string{}
	for _, j := range sub.Jobs {
		names[j.Key] = NextName(existing, stem, j.Key)
	}
	root := c.farm.Root(c.project)
	if root == "" {
		root = c4v1.DefaultProjectRoot
	}
	var env []corev1.EnvVar
	if r := sub.Root; r != "" {
		// the submitter's root, from which the tool runner maps the paths
		// the scenes hold to the tree
		b, _ := json.Marshal([]sourceRoot{{OS: rootOS(r), Path: r}})
		env = append(env, corev1.EnvVar{Name: "C4_SOURCE_ROOTS", Value: string(b)})
	}
	plan := &Plan{Project: c.project, Namespace: c.ns}
	var errs []Error
	for i := range sub.Jobs {
		j := &sub.Jobs[i]
		p := "jobs." + j.Key
		r := c.tools[j.Key]
		frames := c.framesOf(j.Key, map[string]bool{})
		tj := toolJob{tool: r.tool, ref: r.ref, project: c.project, ns: c.ns, root: root, shot: sub.Shot,
			name: names[j.Key], stage: j.Key, frames: frames, with: j.With, size: j.Size, needs: j.Needs,
			pools: j.Pools, retries: j.Retries, chunk: j.Chunk, order: j.Order, env: env}
		if a := j.Answers; a != nil {
			tj.answerer, _ = strings.CutPrefix(a.By, "unit:")
		}
		if r.tool.Kind == "once" {
			tj.frames = ""
			if frameParam(r.tool) != nil {
				tj.frames = frames
			}
		}
		if j.MaxParallel != nil {
			tj.maxPar = *j.MaxParallel
		}
		if j.Preview == "fml" {
			// first, middle and last first: binary order covers them
			tj.order = "binary"
		}
		if m := j.Measure; m != nil {
			tj.measure = &c4v1.WorkMeasureSpec{Name: m.Name, Required: m.Required}
			if m.Of > 0 {
				of := int64(m.Of)
				tj.measure.Of = &of
			}
		}
		pj := PlannedJob{Key: j.Key, Tool: r.ref, Labels: map[string]string{}, Annotations: map[string]string{}, Frames: tj.frames}
		// The tree: the job it follows, else the submission's tree.
		switch {
		case len(j.After) == 1:
			up := names[j.After[0].Job]
			tj.inputs = append(tj.inputs, api.SubmitInput{FromJob: up, MountPath: root})
			pj.After = []string{up}
			if how := j.After[0].How; how != "all" {
				upFrames := c.framesOf(j.After[0].Job, map[string]bool{})
				if how != "" || frames != "" && upFrames != "" {
					plan.Warnings = append(plan.Warnings, fmt.Sprintf("%s follows %s %s; for now it waits for the whole of %s",
						j.Key, j.After[0].Job, map[string]string{"": "frame by frame", "frames": "frame by frame", "previous": "a frame behind"}[how], up))
				}
			}
		case sub.Tree != "":
			in, after := c.treeInput(sub.Tree, root, false, existing)
			tj.inputs = append(tj.inputs, in)
			pj.After = append(pj.After, after...)
		}
		for _, name := range sortedKeys(sub.Read) {
			in, after := c.treeInput(sub.Read[name], path.Join(ReadRoot, name), true, existing)
			tj.inputs = append(tj.inputs, in)
			pj.After = append(pj.After, after...)
		}
		req, jerrs := lowerTool(tj)
		if len(jerrs) > 0 {
			errs = append(errs, prefix(p, jerrs)...)
			continue
		}
		req.TrackerRef, req.For, req.Follows = sub.Tracker, sub.For, j.Follows
		if req.Follows == "" {
			// the previous version of the same stem and key, if there is one
			req.Follows = Latest(existing, strings.TrimSuffix(versionSuffix.ReplaceAllString(names[j.Key], ""), "-"))
		}
		pj.Note = sub.Note
		due := j.Due
		if due == "" {
			due = sub.Due
		}
		pj.Due, _ = readDue(due, c.ms, c.now, c.loc)
		switch d := pj.Due; {
		case d.Milestone != "":
			pj.Labels[c4v1.LabelMilestone] = d.Milestone
		case !d.At.IsZero():
			req.Due = d.At.Format(time.RFC3339)
		case d.Date != "":
			req.Due = d.Date
		}
		if r.tool.Reuse == "exact" {
			pj.Annotations[queue.AnnotationDerivationClass] = "exact"
		}
		pj.Request = req
		pj.Units = units(req)
		plan.Jobs = append(plan.Jobs, pj)
	}
	if len(errs) > 0 {
		return nil, sortErrors(errs)
	}
	return plan, nil
}

// sourceRoot is one of C4_SOURCE_ROOTS: a root on the submitter's
// machine and its operating system.
type sourceRoot struct {
	OS   string `json:"os"`
	Path string `json:"path"`
}

// rootOS is the operating system a root's form says: a drive letter is
// windows, /Users and /Volumes macos, any other absolute path linux.
func rootOS(r string) string {
	switch {
	case len(r) > 2 && r[1] == ':':
		return "windows"
	case strings.HasPrefix(r, "/Users/") || strings.HasPrefix(r, "/Volumes/"):
		return "macos"
	}
	return "linux"
}

// treeInput is a tree mounted at mount: a C4 ID as its view, job:<name>
// as the latest such job's output, which the job then follows. Anything
// else (a folder) is left as written, for the front end to upload first.
func (c *subCtx) treeInput(s, mount string, readOnly bool, existing []string) (api.SubmitInput, []string) {
	if name, ok := strings.CutPrefix(s, "job:"); ok {
		n := Latest(existing, name)
		return api.SubmitInput{FromJob: n, MountPath: mount, ReadOnly: readOnly}, []string{n}
	}
	return api.SubmitInput{View: s, MountPath: mount, ReadOnly: readOnly}, nil
}

// units is how many units a lowered render job makes.
func units(req *api.SubmitRequest) int {
	var spec c4v1.WorkJobSpec
	if json.Unmarshal(req.Spec, &spec) != nil {
		return 0
	}
	fs, err := order_.ParseFrames(spec.Params["frames"])
	if err != nil {
		return 0
	}
	chunk := 1
	fmt.Sscan(spec.Params["chunk"], &chunk)
	if chunk < 1 {
		chunk = 1
	}
	return (len(fs) + chunk - 1) / chunk
}
