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
	"sort"
	"strconv"
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
	// NoWait returns once the farm has the version and its test is
	// running, without waiting for the test's end.
	NoWait bool
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
	req := api.ToolPublishRequest{View: v.ID.String(), Farm: a.Farm, DryRun: a.DryRun}
	if err := doChecked(ctx, c, "POST", "/tools/"+a.Project, req, &r); err != nil {
		return err
	}
	if len(r.Errors) > 0 {
		for i, e := range r.Errors {
			// a local farm's own registry lacks the image: this machine
			// has it, and image load puts it there
			if m := localRegistryLacks.FindStringSubmatch(e.Message); m != nil {
				r.Errors[i].Fix = "load it into the local farm, then publish again: c4 farm image load " + m[1]
			}
		}
		return printFieldErrors(o, r.Errors)
	}
	tot := v.Totals()
	o.printf("%s %s   %s · %s, %d to upload · image %s\n", t.Name, r.Version, shortID(r.View), plural(int(tot.Files), "file", "files"),
		len(need), r.Image)
	if a.DryRun {
		return nil
	}
	where := "to " + a.Project
	if a.Farm {
		where = "to every project"
	}
	if r.Test == nil || r.Test.State == "untested" {
		// a person's tool with no test is offered at once, as it is
		o.printf("published %s, untested: c4 farm submit %s runs it; add test: so the farm checks each version before offering it\n", where, t.Name)
		return nil
	}
	if a.NoWait && (r.Test.State == "running" || r.Test.State == "") {
		o.printf("test on the farm   %s (running; c4 farm tool show %s says how it ends)\n", r.Test.Job, t.Name)
		return nil
	}
	state, err := waitPublish(ctx, c, o, a, t.Name, r.Version, r.Test)
	if err != nil {
		return err
	}
	switch state.State {
	case "ok":
		o.printf("published %s: c4 farm submit %s runs it, and c4 farm tools lists it as %q\n", where, t.Name, t.Title)
		return nil
	case "untested":
		o.printf("published %s, untested: c4 farm submit %s runs it\n", where, t.Name)
		return nil
	}
	return fmt.Errorf("the test failed on the farm (%s): %s; the tool is not published", state.Job, state.Reason)
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
		if st.Seconds > 0 && st.Seconds < 7*24*3600 {
			o.printf("                   ok in %s\n", Span(time.Duration(st.Seconds)*time.Second))
		} else {
			// a test job with no start the farm could time
			o.printf("                   ok\n")
		}
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
	// a folder here is named by its ID, as the farm will name it; the farm
	// plans with the ID alone, so nothing is sent until the plan is accepted
	var tree *folderTree
	if t := doc.str("tree"); localTree(t) {
		dir := t
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(base, dir)
		}
		var err error
		if tree, err = scanTree(ctx, c, project, dir); err != nil {
			return nil, fmt.Errorf("tree %s: %w", t, err)
		}
		doc.setString("tree", tree.v.ID.String())
		// the paths the tree's scenes hold were written under it, unless
		// the file says where they were written
		if abs, err := filepath.Abs(dir); err == nil && doc.str("root") == "" {
			doc.setString("root", filepath.ToSlash(abs))
		}
	}
	body, err := doc.MarshalJSON()
	if err != nil {
		return nil, err
	}
	var plan api.SubmissionResponse
	req := api.SubmissionRequest{Submission: body, DryRun: true, Explain: a.Explain}
	if err := doChecked(ctx, c, "POST", "/submissions", req, &plan); err != nil {
		return nil, err
	}
	if len(plan.Errors) > 0 {
		return nil, printFieldErrors(o, plan.Errors)
	}
	if a.Explain && plan.Explain != "" {
		o.printf("%s", strings.TrimRight(plan.Explain, "\n")+"\n")
	} else {
		o.printJobs(plan.Jobs, tree)
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
	if tree != nil {
		sent, sentBytes, err := upload(ctx, c, project, tree.dir, tree.v, tree.need)
		if err != nil {
			return nil, err
		}
		if err := recordView(ctx, c, project, tree.v); err != nil {
			return nil, err
		}
		if sent > 0 {
			o.printf("uploaded  %s · %s\n", plural(sent, "file", "files"), Bytes(sentBytes))
		}
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
	if n := len(done.Jobs); n > 0 && done.Jobs[n-1].Link != "" {
		o.printf("HUD       %s\n", c.Web(done.Jobs[n-1].Link, nil))
	}
	return &done, nil
}

// folderTree is a submission's tree from a folder on this machine: its
// view, named here as the farm names it, and the objects the farm lacks.
type folderTree struct {
	dir       string
	v         *view.View
	need      map[c4.ID]bool
	needBytes int64
}

func scanTree(ctx context.Context, c *Client, project, dir string) (*folderTree, error) {
	v, err := view.ScanDir(dir)
	if err != nil {
		return nil, err
	}
	need, err := missingIDs(ctx, c, project, v.Objects())
	if err != nil {
		return nil, err
	}
	t := &folderTree{dir: dir, v: v, need: need}
	for _, ob := range v.Objects() {
		if need[ob.ID] {
			t.needBytes += ob.Size
		}
	}
	return t, nil
}

// printJobs is the plan of a submission's jobs: each job's line and its
// estimate, and what of a folder tree is to be sent.
func (o Out) printJobs(jobs []api.SubmissionJob, tree *folderTree) {
	w := 0
	for _, j := range jobs {
		w = max(w, len(j.Name))
	}
	for i, j := range jobs {
		o.printf("%-*s   %s\n", w, j.Name, j.What)
		if i == 0 && tree != nil {
			o.printFiles(tree)
		}
		switch {
		case j.Estimate != nil:
			o.printPlan(j.Estimate, nil)
		case j.EstimateError != "":
			o.printf("  estimate  none yet: %s\n", j.EstimateError)
		}
	}
	o.printf("\n")
}

// printFiles is the files line of a plan: what the folder holds, and what
// of it the farm lacks.
func (o Out) printFiles(t *folderTree) {
	tot := t.v.Totals()
	upl := plural(len(t.need), "to upload", "to upload")
	if len(t.need) > 0 {
		upl += " (" + Bytes(t.needBytes) + ")"
	}
	if have := int(tot.UniqueObjects) - len(t.need); have > 0 {
		o.printf("  files     %s found · %d already on the farm · %s\n", plural(int(tot.Files), "file", "files"), have, upl)
		return
	}
	o.printf("  files     %s found · %s · %s\n", plural(int(tot.Files), "file", "files"), Bytes(tot.Bytes), upl)
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
	types := map[string]string{}
	asset := ""
	for _, p := range params {
		known[p.Name] = true
		types[p.Name] = p.Type
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
		val, err := setValue(types[k], v)
		if err != nil {
			return nil, fmt.Errorf("--set %s=%s: %w", k, v, err)
		}
		with[k] = val
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
	switch {
	case a.Options.Frames != "":
		job["frames"] = a.Options.Frames
	case pattern != "" && t.Kind == "frames":
		// the frames the sequence holds on disk, unless --frames says
		if fs := framesOnDisk(target, pattern); len(fs) > 0 {
			job["frames"] = frameList(fs)
			o.printf("frames    %s, from the %s %s matches (--frames to choose)\n", frameList(fs), plural(len(fs), "file", "files"), pattern)
		}
	}
	if len(a.Options.Pools) > 0 {
		job["pools"] = a.Options.Pools
	}
	shot := a.Shot
	if shot == "" && rel != "." {
		shot = shotOf(root, rel)
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

// shotOf is the shot an asset belongs to: the first name along the tree's
// root and the asset's path that reads as a shot code (sm0140/geo,
// sm0140/sm0140.usda and, in a root named sm0140, plates/x.$F4.exr are all
// sm0140), else the asset's own name.
func shotOf(root, rel string) string {
	names := []string{filepath.Base(root)}
	names = append(names, strings.Split(filepath.ToSlash(rel), "/")...)
	for _, n := range names {
		n, _, _ = strings.Cut(n, ".")
		if shotCode.MatchString(n) {
			return n
		}
	}
	last, _, _ := strings.Cut(names[len(names)-1], ".")
	return last
}

// shotCode is how shots are named: letters, then three or more digits,
// then perhaps a suffix (sm0140, sh0420, sm0220_fx).
var shotCode = regexp.MustCompile(`^[A-Za-z]{1,4}[0-9]{3,}([_-][A-Za-z0-9]+)?$`)

// localRegistryLacks is the farm refusing an image its local registry
// (c4 farm up's) does not hold, and the image it names.
var localRegistryLacks = regexp.MustCompile(`the registry c4-local-registry:[0-9]+ has no (\S+)`)

// frameToken is where a sequence path puts the frame number: $F4, <F4>,
// %04d or #, the forms husk and Houdini accept.
var frameToken = regexp.MustCompile(`\$F\d|<F\d>|%0\dd|#`)

// notFound is the farm answering 404: no such tool, or a farm without tools.
func notFound(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Status == http.StatusNotFound
}

// setValue is a --set value as its parameter's type has it: the farm takes
// an INT, FLOAT or BOOL parameter as a number or a boolean, not a string.
func setValue(typ, v string) (any, error) {
	switch typ {
	case "INT":
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return nil, errors.New("give a whole number")
		}
		return n, nil
	case "FLOAT":
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return nil, errors.New("give a number")
		}
		return f, nil
	case "BOOL":
		b, err := strconv.ParseBool(v)
		if err != nil {
			return nil, errors.New("give true or false")
		}
		return b, nil
	}
	return v, nil
}

// framesOnDisk are the frame numbers of the files in dir that a sequence
// pattern (sm0140.$F4.exr, sm0140.####.exr, sm0140.%04d.exr) matches, in
// order.
func framesOnDisk(dir, pattern string) []int {
	loc := frameToken.FindStringIndex(pattern)
	if loc == nil {
		return nil
	}
	re, err := regexp.Compile("^" + regexp.QuoteMeta(pattern[:loc[0]]) + "([0-9]+)" + regexp.QuoteMeta(strings.TrimLeft(pattern[loc[1]:], "#")) + "$")
	if err != nil {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	seen := map[int]bool{}
	var out []int
	for _, e := range entries {
		if m := re.FindStringSubmatch(e.Name()); m != nil {
			if n, err := strconv.Atoi(m[1]); err == nil && !seen[n] {
				seen[n] = true
				out = append(out, n)
			}
		}
	}
	sort.Ints(out)
	return out
}

// frameList is frames as a frame list: runs of consecutive frames as
// ranges, the rest as single frames (1001-1004,1010).
func frameList(fs []int) string {
	var parts []string
	for i := 0; i < len(fs); {
		j := i
		for j+1 < len(fs) && fs[j+1] == fs[j]+1 {
			j++
		}
		if j > i {
			parts = append(parts, fmt.Sprintf("%d-%d", fs[i], fs[j]))
		} else {
			parts = append(parts, strconv.Itoa(fs[i]))
		}
		i = j + 1
	}
	return strings.Join(parts, ",")
}
