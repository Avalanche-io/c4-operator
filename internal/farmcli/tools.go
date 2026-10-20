package farmcli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/Avalanche-io/c4"

	"github.com/Avalanche-io/c4-operator/hud/api"
	"github.com/Avalanche-io/c4-operator/internal/view"
	"github.com/Avalanche-io/c4-operator/schema"
)

// toolParams are a published tool's parameters, as its tool.yaml wrote them.
func toolParams(d *api.ToolDetail) ([]schema.Param, error) {
	if len(d.Params) == 0 {
		return nil, nil
	}
	var ps []schema.Param
	if err := json.Unmarshal(d.Params, &ps); err != nil {
		return nil, fmt.Errorf("the farm's parameters for %s: %w", d.Name, err)
	}
	return ps, nil
}

func printFieldErrors(o Out, errs []api.FieldError) error {
	for _, e := range errs {
		o.printf("  %s: %s\n", e.Path, e.Message)
		if e.Fix != "" {
			o.printf("    %s\n", e.Fix)
		}
	}
	return fmt.Errorf("the farm refused it: %s", plural(len(errs), "problem", "problems"))
}

// ListTools prints the tools a project can submit.
func ListTools(ctx context.Context, c *Client, o Out, project string) error {
	var r api.ToolListResponse
	if err := c.Do(ctx, "GET", "/tools/"+project, nil, &r); err != nil {
		return err
	}
	if len(r.Tools) == 0 {
		o.printf("no tools in %s yet; c4 farm tool publish DIR adds one\n", project)
		return nil
	}
	sort.Slice(r.Tools, func(i, j int) bool { return r.Tools[i].Name < r.Tools[j].Name })
	w := 0
	for _, t := range r.Tools {
		w = max(w, len(t.Name))
	}
	for _, t := range r.Tools {
		scope := ""
		if t.Scope == "farm" {
			scope = " · farm-wide"
		}
		o.printf("%-*s  %-6s  %-4s  %s%s\n", w, t.Name, t.Kind, t.Version, t.Title, scope)
	}
	return nil
}

// LookupTool reads one published tool.
func LookupTool(ctx context.Context, c *Client, project, name, version string) (*api.ToolDetail, error) {
	p := "/tools/" + project + "/" + url.PathEscape(name)
	if version != "" {
		p += "?version=" + url.QueryEscape(version)
	}
	var t api.ToolDetail
	if err := c.Do(ctx, "GET", p, nil, &t); err != nil {
		return nil, err
	}
	return &t, nil
}

// ShowTool prints a published tool and its parameters.
func ShowTool(ctx context.Context, c *Client, o Out, project, name string) error {
	t, err := LookupTool(ctx, c, project, name, "")
	if err != nil {
		return err
	}
	o.printf("%s %s   %s · %s\n", t.Name, t.Version, t.Title, t.Kind)
	if t.Summary != "" {
		o.printf("          %s\n", t.Summary)
	}
	o.printf("tree      %s\n", t.View)
	if t.Image != "" {
		o.printf("image     %s\n", t.Image)
	}
	params, err := toolParams(t)
	if err != nil {
		return err
	}
	for i, p := range params {
		head := "params"
		if i > 0 {
			head = ""
		}
		def := ""
		if p.Default != nil {
			def = fmt.Sprintf(" (default %v)", p.Default)
		}
		o.printf("%-9s --set %s=%s%s\n", head, p.Name, strings.ToLower(p.Type), def)
	}
	return nil
}

// PublishArgs are `c4 farm tool publish`'s.
type PublishArgs struct {
	Dir     string
	Project string
	Farm    bool // to every project (the farm admin role)
	DryRun  bool
	// Wait bounds the wait for the farm's test run.
	Wait time.Duration
	// Poll is how often the test's state is read (default 2s).
	Poll time.Duration
	// TestedIn publishes one of a release's own tools as tested in that
	// release, running no test on the farm (c4 farm up, as the farm admin).
	TestedIn string
}

// PublishTool uploads a tool's folder (only what the farm lacks), asks the
// farm to publish it, and waits for the farm's run of its test: the tool
// is offered to anyone only once that passes.
func PublishTool(ctx context.Context, c *Client, o Out, a PublishArgs) error {
	t, err := ReadTool(a.Dir)
	if err != nil {
		return err
	}
	v, err := view.ScanDir(a.Dir)
	if err != nil {
		return err
	}
	need, err := missingIDs(ctx, c, a.Project, v.Objects())
	if err != nil {
		return err
	}
	if _, _, err := upload(ctx, c, a.Project, a.Dir, v, need); err != nil {
		return err
	}
	if err := recordView(ctx, c, a.Project, v); err != nil {
		return err
	}
	var r api.ToolPublishResponse
	req := api.ToolPublishRequest{View: v.ID.String(), Farm: a.Farm, DryRun: a.DryRun, TestedIn: a.TestedIn}
	if err := doChecked(ctx, c, "POST", "/tools/"+a.Project, req, &r); err != nil {
		return err
	}
	if len(r.Errors) > 0 {
		return printFieldErrors(o, r.Errors)
	}
	tot := v.Totals()
	o.printf("%s %s   %s · %s, %d to upload · image %s\n", t.Name, r.Version, shortID(r.View), plural(int(tot.Files), "file", "files"),
		len(need), r.Image)
	if a.DryRun || r.Test == nil {
		return nil
	}
	state, err := waitPublish(ctx, c, o, a, t.Name, r.Version, r.Test)
	if err != nil {
		return err
	}
	if state.State != "ok" {
		return fmt.Errorf("the test failed on the farm (%s): %s; the tool is not published", state.Job, state.Reason)
	}
	where := "to " + a.Project
	if a.Farm {
		where = "to every project"
	}
	o.printf("published %s: artists see %q in Submit\n", where, t.Title)
	return nil
}

// waitPublish follows a version's test run on the farm until it ends.
func waitPublish(ctx context.Context, c *Client, o Out, a PublishArgs, name, version string, st *api.ToolTestState) (*api.ToolTestState, error) {
	wait, poll := a.Wait, a.Poll
	if wait <= 0 {
		wait = 15 * time.Minute
	}
	if poll <= 0 {
		poll = 2 * time.Second
	}
	o.printf("test on the farm   %s\n", st.Job)
	end := time.Now().Add(wait)
	for st.State == "running" || st.State == "" {
		if time.Now().After(end) {
			return nil, fmt.Errorf("the farm's test run (%s) did not end in %s; c4 farm tool show %s says when it does", st.Job, wait, name)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(poll):
		}
		t, err := LookupTool(ctx, c, a.Project, name, version)
		if err != nil {
			return nil, err
		}
		if t.Test == nil {
			return nil, errors.New("the farm says nothing of this version's test")
		}
		st = t.Test
	}
	if st.State == "ok" {
		o.printf("                   ok in %ds\n", st.Seconds)
	}
	return st, nil
}

func shortID(id string) string {
	if len(id) < 12 {
		return id
	}
	return id[:4] + "…" + id[len(id)-4:]
}

// SubmitFileArgs are `c4 farm submit FILE`'s.
type SubmitFileArgs struct {
	Path    string
	Project string
	DryRun  bool
	Explain bool
	Yes     bool
}

// SubmitFile sends a submission (c4: submit/1): a local tree is uploaded
// first (only what the farm lacks) and named by its ID; the farm checks it
// and shows the plan; on confirmation it submits.
func SubmitFile(ctx context.Context, c *Client, o Out, a SubmitFileArgs) (*api.SubmissionResponse, error) {
	data, err := os.ReadFile(a.Path)
	if err != nil {
		return nil, err
	}
	// in the order written: the farm creates the jobs in it
	raw, err := schema.ToJSON(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", a.Path, err)
	}
	doc, err := readDoc(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: a submission is a YAML or JSON object", a.Path)
	}
	if v := doc.str("c4"); v != "submit/1" {
		return nil, fmt.Errorf("%s: c4 is %q; a submission says c4: submit/1", a.Path, v)
	}
	return submitDoc(ctx, c, o, doc, filepath.Dir(a.Path), a)
}

// submissionDoc is a submission's top level, its keys in the order
// written and each value as written, so changing one keeps the jobs'
// order.
type submissionDoc struct {
	keys []string
	vals map[string]json.RawMessage
}

func readDoc(raw []byte) (*submissionDoc, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil, errors.New("not an object")
	}
	d := &submissionDoc{vals: map[string]json.RawMessage{}}
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return nil, err
		}
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, err
		}
		d.set(t.(string), v)
	}
	return d, nil
}

func (d *submissionDoc) set(key string, v json.RawMessage) {
	if _, ok := d.vals[key]; !ok {
		d.keys = append(d.keys, key)
	}
	d.vals[key] = v
}

func (d *submissionDoc) setString(key, s string) {
	b, _ := json.Marshal(s)
	d.set(key, b)
}

// str is a top-level string, "" when it is none.
func (d *submissionDoc) str(key string) string {
	var s string
	json.Unmarshal(d.vals[key], &s)
	return s
}

func (d *submissionDoc) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, k := range d.keys {
		if i > 0 {
			b.WriteByte(',')
		}
		kb, _ := json.Marshal(k)
		b.Write(kb)
		b.WriteByte(':')
		b.Write(d.vals[k])
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// hostOS is this machine's operating system as a source root names it.
func hostOS() string {
	if runtime.GOOS == "darwin" {
		return "macos"
	}
	return runtime.GOOS
}

// submitDoc sends a submission document: a folder tree (relative to base)
// is uploaded and named by its ID, the farm plans it, and on confirmation
// it is submitted.
func submitDoc(ctx context.Context, c *Client, o Out, doc *submissionDoc, base string, a SubmitFileArgs) (*api.SubmissionResponse, error) {
	project := doc.str("project")
	if project == "" {
		project = a.Project
		doc.setString("project", project)
	}
	if project == "" {
		return nil, errors.New("the submission names no project, and neither does --project")
	}
	var roots []api.SourceRoot
	if tree := doc.str("tree"); localTree(tree) {
		dir := tree
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(base, dir)
		}
		id, sent, err := putTree(ctx, c, project, dir)
		if err != nil {
			return nil, fmt.Errorf("tree %s: %w", tree, err)
		}
		if sent > 0 {
			o.printf("uploaded  %s from %s\n", plural(sent, "file", "files"), tree)
		}
		doc.setString("tree", id)
		// the paths the tree's scenes hold were written under it
		if abs, err := filepath.Abs(dir); err == nil {
			roots = []api.SourceRoot{{OS: hostOS(), Path: filepath.ToSlash(abs)}}
		}
	}
	body, err := doc.MarshalJSON()
	if err != nil {
		return nil, err
	}
	var plan api.SubmissionResponse
	req := api.SubmissionRequest{Submission: body, DryRun: true, Explain: a.Explain, SourceRoots: roots}
	if err := doChecked(ctx, c, "POST", "/submissions", req, &plan); err != nil {
		return nil, err
	}
	if len(plan.Errors) > 0 {
		return nil, printFieldErrors(o, plan.Errors)
	}
	if a.Explain && plan.Explain != "" {
		o.printf("%s", strings.TrimRight(plan.Explain, "\n")+"\n")
	} else {
		o.printJobs(plan.Jobs)
	}
	for _, w := range plan.Warnings {
		o.printf("note      %s\n", w)
	}
	if a.DryRun || a.Explain {
		return &plan, nil
	}
	if !a.Yes && !o.confirm("Submit?") {
		return nil, errors.New("not submitted")
	}
	var done api.SubmissionResponse
	req.DryRun, req.Explain = false, false
	if err := doChecked(ctx, c, "POST", "/submissions", req, &done); err != nil {
		return nil, err
	}
	if len(done.Errors) > 0 {
		return nil, printFieldErrors(o, done.Errors)
	}
	for _, j := range done.Jobs {
		o.printf("queued    %s/%s\n", j.Namespace, j.Name)
	}
	return &done, nil
}

// printJobs is the plan of a submission's jobs: each job's line, then its
// estimate.
func (o Out) printJobs(jobs []api.SubmissionJob) {
	w := 0
	for _, j := range jobs {
		w = max(w, len(j.Name))
	}
	for _, j := range jobs {
		o.printf("%-*s   %s\n", w, j.Name, j.What)
		if j.Estimate != nil {
			o.printPlan(j.Estimate, nil)
		}
	}
	o.printf("\n")
}

// localTree says a submission's tree is a folder on this machine, not a C4
// ID, a job's output or a git commit.
func localTree(s string) bool {
	if s == "" || strings.HasPrefix(s, "job:") || strings.HasPrefix(s, "git:") {
		return false
	}
	if _, err := c4.Parse(s); err == nil {
		return false
	}
	return true
}

// putTree uploads a folder's objects the farm lacks, records it as a view,
// and returns its ID and how many files were sent.
func putTree(ctx context.Context, c *Client, project, dir string) (string, int, error) {
	v, err := view.ScanDir(dir)
	if err != nil {
		return "", 0, err
	}
	need, err := missingIDs(ctx, c, project, v.Objects())
	if err != nil {
		return "", 0, err
	}
	sent, _, err := upload(ctx, c, project, dir, v, need)
	if err != nil {
		return "", 0, err
	}
	if err := recordView(ctx, c, project, v); err != nil {
		return "", 0, err
	}
	return v.ID.String(), sent, nil
}

// doChecked is Do for calls the farm may answer with field errors (422):
// those come back in out, for the caller to print, not as an error.
func doChecked(ctx context.Context, c *Client, method, path string, in, out any) error {
	err := c.Do(ctx, method, path, in, out, http.StatusUnprocessableEntity)
	var ae *APIError
	if errors.As(err, &ae) && ae.Status == http.StatusUnprocessableEntity {
		return nil
	}
	return err
}

// SubmitTool is `c4 farm submit TOOL ASSET` over a published tool: a
// submission of one job, whose tree is the asset's folder (as FindRoot
// finds it) and whose first input path parameter is the asset.
func SubmitTool(ctx context.Context, c *Client, o Out, t *api.ToolDetail, a SubmitArgs) (*api.SubmissionResponse, error) {
	switch {
	case a.Name != "":
		return nil, errors.New("--name: a tool's job is named <shot>-<stage>-vNNN; set --shot or --stage instead")
	case len(a.After) > 0:
		return nil, errors.New("--after: write a submission file, whose jobs follow each other with after:")
	case a.Options.Image != "":
		return nil, errors.New("--image: a tool runs the image it was published with; publish a new version to change it")
	}
	if a.Asset == "" {
		return nil, fmt.Errorf("give the asset %s reads: a file or folder on this machine", t.Name)
	}
	// a frame sequence (sm0140.$F4.exr) is no file: its folder is found,
	// and the pattern is the parameter
	target, pattern := a.Asset, ""
	if _, err := os.Stat(a.Asset); err != nil && frameToken.MatchString(filepath.Base(a.Asset)) {
		target, pattern = filepath.Dir(a.Asset), filepath.Base(a.Asset)
	}
	root, rel, err := FindRoot(target, a.Root)
	if err != nil {
		return nil, err
	}
	if pattern != "" {
		rel = path.Join(filepath.ToSlash(rel), pattern)
	}
	params, err := toolParams(t)
	if err != nil {
		return nil, err
	}
	with := map[string]any{}
	known := map[string]bool{}
	asset := ""
	for _, p := range params {
		known[p.Name] = true
		if asset == "" && p.Type == "PATH" && p.DataFlow != "OUT" {
			asset = p.Name
		}
	}
	if asset == "" {
		return nil, fmt.Errorf("%s takes no path, so it has nothing to do with %s", t.Name, a.Asset)
	}
	with[asset] = filepath.ToSlash(rel)
	for k, v := range a.Options.Set {
		if !known[k] {
			return nil, fmt.Errorf("%s has no parameter %q (c4 farm tool show %s lists them)", t.Name, k, t.Name)
		}
		with[k] = v
	}
	if r := a.Options.Renderer; r != "" {
		if !known["renderer"] {
			return nil, fmt.Errorf("--renderer: %s takes no renderer", t.Name)
		}
		with["renderer"] = strings.ToLower(r)
	}
	key := a.Stage
	if key == "" {
		key = t.Name
	}
	job := map[string]any{"tool": t.Name, "with": with}
	if a.Options.Frames != "" {
		job["frames"] = a.Options.Frames
	}
	if len(a.Options.Pools) > 0 {
		job["pools"] = a.Options.Pools
	}
	shot := a.Shot
	if shot == "" && rel != "." {
		shot = strings.TrimSuffix(filepath.Base(rel), filepath.Ext(rel))
		shot, _, _ = strings.Cut(shot, ".") // sm0140.$F4.exr names the shot sm0140
	}
	doc := map[string]any{"c4": "submit/1", "project": a.Project, "tree": root, "jobs": map[string]any{key: job}}
	if shot != "" {
		doc["shot"] = shot
	}
	if a.Deadline != "" {
		doc["due"] = a.Deadline
	}
	raw, _ := json.Marshal(doc) // one job: no order to keep
	d, err := readDoc(raw)
	if err != nil {
		return nil, err
	}
	return submitDoc(ctx, c, o, d, "", SubmitFileArgs{Project: a.Project, DryRun: a.DryRun, Yes: a.Yes})
}

// frameToken is where a sequence path puts the frame number: $F4, <F4>,
// %04d or #, the forms husk and Houdini accept.
var frameToken = regexp.MustCompile(`\$F\d|<F\d>|%0\dd|#`)

// notFound is the farm answering 404: no such tool, or a farm without tools.
func notFound(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Status == http.StatusNotFound
}
