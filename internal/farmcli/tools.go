package farmcli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/Avalanche-io/c4"

	"github.com/Avalanche-io/c4-operator/hud/api"
	"github.com/Avalanche-io/c4-operator/internal/view"
)

// PublishedTool is a tool as the farm lists it (GET /tools/{project}).
type PublishedTool struct {
	Name    string `json:"name"`
	Title   string `json:"title"`
	Summary string `json:"summary"`
	Kind    string `json:"kind"`
	Version string `json:"version"`
	View    string `json:"view"`
	Scope   string `json:"scope"` // project or farm
	Image   string `json:"image"`
	// Params and Test are given for one tool (GET /tools/{project}/{tool}).
	Params []ToolParam `json:"params,omitempty"`
	Test   *ToolTest   `json:"test,omitempty"`
	// Publish is the version's publishing: its test job and its state.
	Publish *PublishState `json:"publish,omitempty"`
}

// PublishState is a version's test run on the farm.
type PublishState struct {
	Job     string `json:"job"`
	State   string `json:"state"` // running, ok or failed
	Seconds int    `json:"seconds,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

// FieldError is a problem with a field of a file, and how to fix it.
type FieldError struct {
	Path    string `json:"path"`
	Message string `json:"message"`
	Fix     string `json:"fix,omitempty"`
}

func printFieldErrors(o Out, errs []FieldError) error {
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
	var r struct {
		Tools []PublishedTool `json:"tools"`
	}
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
func LookupTool(ctx context.Context, c *Client, project, name, version string) (*PublishedTool, error) {
	p := "/tools/" + project + "/" + url.PathEscape(name)
	if version != "" {
		p += "?version=" + url.QueryEscape(version)
	}
	var t PublishedTool
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
	for i, p := range t.Params {
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
	var r struct {
		OK      bool          `json:"ok"`
		Errors  []FieldError  `json:"errors,omitempty"`
		Tool    string        `json:"tool"`
		Version string        `json:"version"`
		View    string        `json:"view"`
		Image   string        `json:"image"`
		Test    *PublishState `json:"test,omitempty"`
	}
	req := map[string]any{"view": v.ID.String(), "farm": a.Farm, "dryRun": a.DryRun}
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
func waitPublish(ctx context.Context, c *Client, o Out, a PublishArgs, name, version string, st *PublishState) (*PublishState, error) {
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
		if t.Publish == nil {
			return nil, errors.New("the farm says nothing of this version's test")
		}
		st = t.Publish
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

// submissionJob is a job of a submission as the farm planned it.
type submissionJob struct {
	Key       string              `json:"key"`
	Namespace string              `json:"namespace"`
	Name      string              `json:"name"`
	Tool      string              `json:"tool"`
	Version   string              `json:"version"`
	What      string              `json:"what"`
	Estimate  *api.SubmitEstimate `json:"estimate,omitempty"`
}

type submissionResponse struct {
	OK         bool            `json:"ok"`
	Errors     []FieldError    `json:"errors,omitempty"`
	Submission string          `json:"submission"`
	Jobs       []submissionJob `json:"jobs"`
	Explain    string          `json:"explain,omitempty"`
}

// SubmitFile sends a submission (c4: submit/1): a local tree is uploaded
// first (only what the farm lacks) and named by its ID; the farm checks it
// and shows the plan; on confirmation it submits.
func SubmitFile(ctx context.Context, c *Client, o Out, a SubmitFileArgs) (*submissionResponse, error) {
	data, err := os.ReadFile(a.Path)
	if err != nil {
		return nil, err
	}
	raw, err := yaml.YAMLToJSON(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", a.Path, err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil || doc == nil {
		return nil, fmt.Errorf("%s: a submission is a YAML or JSON object", a.Path)
	}
	if doc["c4"] != "submit/1" {
		return nil, fmt.Errorf("%s: c4 is %v; a submission says c4: submit/1", a.Path, doc["c4"])
	}
	project, _ := doc["project"].(string)
	if project == "" {
		project = a.Project
		doc["project"] = project
	}
	if project == "" {
		return nil, errors.New("the submission names no project, and neither does --project")
	}
	if tree, ok := doc["tree"].(string); ok && localTree(tree) {
		dir := tree
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(filepath.Dir(a.Path), dir)
		}
		id, sent, err := putTree(ctx, c, project, dir)
		if err != nil {
			return nil, fmt.Errorf("tree %s: %w", tree, err)
		}
		if sent > 0 {
			o.printf("uploaded  %s from %s\n", plural(sent, "file", "files"), tree)
		}
		doc["tree"] = id
	}
	var plan submissionResponse
	if err := doChecked(ctx, c, "POST", "/submissions", map[string]any{"submission": doc, "dryRun": true, "explain": a.Explain}, &plan); err != nil {
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
	if a.DryRun || a.Explain {
		return &plan, nil
	}
	if !a.Yes && !o.confirm("Submit?") {
		return nil, errors.New("not submitted")
	}
	var done submissionResponse
	if err := doChecked(ctx, c, "POST", "/submissions", map[string]any{"submission": doc}, &done); err != nil {
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
func (o Out) printJobs(jobs []submissionJob) {
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
