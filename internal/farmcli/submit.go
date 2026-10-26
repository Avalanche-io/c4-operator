package farmcli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Avalanche-io/c4"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/hud/api"
	"github.com/Avalanche-io/c4-operator/internal/view"
	"github.com/Avalanche-io/c4-operator/work/forecast"
)

// SubmitArgs are `c4 farm submit`'s arguments.
type SubmitArgs struct {
	Type     string
	Template string // a WorkJob spec file instead of a type
	Asset    string // a file or folder on this machine
	Root     string
	Project  string
	Name     string
	Shot     string
	Stage    string // the middle of the default name; default the tool's
	Deadline string
	After    []string
	Options  Options
	DryRun   bool
	Yes      bool
	// All sends the folder's every file but .git's, .gitignore's too;
	// Terminal says a person is at one, as All needs
	All      bool
	Terminal bool
}

// Out is where a command talks to the person.
type Out struct {
	W     io.Writer
	In    io.Reader // answers to questions; nil means yes
	Clock Clock
}

func (o Out) printf(format string, a ...any) { fmt.Fprintf(o.W, format, a...) }

func (o Out) confirm(q string) bool {
	if o.In == nil {
		return true
	}
	o.printf("%s [Y/n] ", q)
	line, _ := bufio.NewReader(o.In).ReadString('\n')
	a := strings.ToLower(strings.TrimSpace(line))
	return a == "" || a == "y" || a == "yes"
}

// FindRoot is the folder whose tree a job mounts: root if given, else the
// nearest folder above the asset holding .c4project, else the asset's
// folder (or the asset, when it is a folder).
func FindRoot(asset, root string) (string, string, error) {
	abs, err := filepath.Abs(asset)
	if err != nil {
		return "", "", err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", "", err
	}
	dir := abs
	if !info.IsDir() {
		dir = filepath.Dir(abs)
	}
	if root == "" {
		for d := dir; ; d = filepath.Dir(d) {
			if _, err := os.Stat(filepath.Join(d, ".c4project")); err == nil {
				root = d
				break
			}
			if filepath.Dir(d) == d {
				root = dir
				break
			}
		}
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return "", "", err
	}
	rel, err := filepath.Rel(root, abs)
	if err != nil || strings.HasPrefix(rel, "..") {
		return "", "", fmt.Errorf("%s is not inside %s", asset, root)
	}
	return root, filepath.ToSlash(rel), nil
}

// Bytes is a size as people read it.
func Bytes(n int64) string {
	switch {
	case n >= 1<<40:
		return fmt.Sprintf("%.1f TB", float64(n)/(1<<40))
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.0f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}

var orderLines = map[string]string{
	"binary":     "binary: first and last frame, then the middle, then the quarters",
	"sequential": "in order, first frame to last",
	"reverse":    "last frame to first",
}

// missingIDs asks the farm which objects it lacks, in batches.
func missingIDs(ctx context.Context, c *Client, project string, objs []view.Object) (map[c4.ID]bool, error) {
	out := map[c4.ID]bool{}
	const batch = 50000
	for i := 0; i < len(objs); i += batch {
		var req api.MissingRequest
		for _, o := range objs[i:min(i+batch, len(objs))] {
			req.IDs = append(req.IDs, o.ID.String())
		}
		var resp api.MissingResponse
		if err := c.Do(ctx, "POST", "/objects/"+project+"/missing", req, &resp); err != nil {
			return nil, err
		}
		for _, s := range resp.Missing {
			id, err := c4.Parse(s)
			if err != nil {
				return nil, err
			}
			out[id] = true
		}
	}
	return out, nil
}

// upload sends the objects the farm lacks, a few at a time.
func upload(ctx context.Context, c *Client, project, root string, v *view.View, need map[c4.ID]bool) (int, int64, error) {
	paths := map[c4.ID]string{}
	for _, f := range v.Files {
		if _, ok := paths[f.ID]; !ok {
			paths[f.ID] = filepath.Join(root, filepath.FromSlash(f.Path))
		}
	}
	var todo []view.Object
	for _, o := range v.Objects() {
		if need[o.ID] {
			todo = append(todo, o)
		}
	}
	var (
		next  atomic.Int64
		sent  atomic.Int64
		bytes atomic.Int64
		wg    sync.WaitGroup
		mu    sync.Mutex
		first error
	)
	for range min(8, max(len(todo), 1)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := int(next.Add(1) - 1)
				if i >= len(todo) {
					return
				}
				o := todo[i]
				f, err := os.Open(paths[o.ID])
				if err == nil {
					err = c.Put(ctx, "/objects/"+project+"/"+o.ID.String(), f, o.Size)
					f.Close()
				}
				if err != nil {
					mu.Lock()
					first = errors.Join(first, fmt.Errorf("%s: %w", paths[o.ID], err))
					mu.Unlock()
					return
				}
				sent.Add(1)
				bytes.Add(o.Size)
			}
		}()
	}
	wg.Wait()
	return int(sent.Load()), bytes.Load(), first
}

// recordView records a tree whose objects the farm has as a view, and
// checks the farm names it as this machine does.
func recordView(ctx context.Context, c *Client, project string, v *view.View) error {
	files := make([]api.ViewFile, 0, len(v.Files))
	for _, f := range v.Files {
		files = append(files, api.ViewFile{Path: f.Path, ID: f.ID.String(), Size: f.Size, Exec: f.Exec})
	}
	req := api.ViewRequest{Files: files, Dirs: v.EmptyDirs()}
	for _, l := range v.Symlinks {
		req.Links = append(req.Links, api.ViewLink{Path: l.Path, Target: l.Target})
	}
	var vr api.ViewResponse
	if err := c.Do(ctx, "POST", "/views/"+project, req, &vr); err != nil {
		return fmt.Errorf("recording the tree: %w", err)
	}
	if vr.View != v.ID.String() {
		return fmt.Errorf("the farm named the tree %s, this machine %s", vr.View, v.ID)
	}
	return nil
}

// Submit plans a job from a type and an asset, shows the plan, and on
// confirmation uploads what the farm lacks and submits it.
func Submit(ctx context.Context, c *Client, o Out, a SubmitArgs) (*api.SubmitResponse, error) {
	if err := allowAll(a.All, a.Terminal); err != nil {
		return nil, err
	}
	// a tool published to the project runs as a submission; the farm's
	// prebuilt types below are the fallback for a farm without tools
	if a.Template == "" {
		t, err := LookupTool(ctx, c, a.Project, a.Type, "")
		switch {
		case err == nil:
			// a farm with tools but no submissions yet runs the types below
			if _, err := SubmitTool(ctx, c, o, t, a); !notFound(err) {
				return nil, err
			}
		case !notFound(err):
			return nil, err
		default:
			o.notOffered(ctx, c, a.Project, a.Type)
		}
	}
	var spec c4v1.WorkJobSpec
	var t JobType
	switch {
	case a.Template != "":
		data, err := os.ReadFile(a.Template)
		if err != nil {
			return nil, err
		}
		if err := readSpec(data, &spec); err != nil {
			return nil, fmt.Errorf("%s: %w", a.Template, err)
		}
		t = JobType{Name: a.Type, Stage: a.Type, Mount: "/show"}
		if len(spec.Inputs) > 0 && spec.Inputs[0].MountPath != "" {
			t.Mount = spec.Inputs[0].MountPath
		}
	default:
		var err error
		if t, err = LookupType(a.Type); err != nil {
			return nil, err
		}
	}
	if a.Asset == "" {
		return nil, fmt.Errorf("give the asset the job reads: a file or folder on this machine")
	}
	root, rel, err := FindRoot(a.Asset, a.Root)
	if err != nil {
		return nil, err
	}
	if err := agentMaySend(root); err != nil {
		return nil, err
	}
	opt := a.Options
	opt.Asset, opt.Shot = rel, a.Shot
	if info, err := os.Stat(a.Asset); err == nil && info.IsDir() {
		opt.AssetIsDir = true
	}
	if a.Template == "" {
		if spec, err = t.Spec(opt); err != nil {
			return nil, err
		}
	}
	shot := a.Shot
	if shot == "" && rel != "." {
		shot = strings.TrimSuffix(filepath.Base(rel), filepath.Ext(rel))
	}
	deadline, err := o.Clock.ParseWhen(a.Deadline)
	if err != nil {
		return nil, err
	}
	if !deadline.IsZero() {
		spec.Deadline = &metav1.Time{Time: deadline.UTC()}
		spec.DeadlineMode = "Pinned"
	}

	// name the tree here, as the farm will
	scan, err := scanUpload(ctx, root, a.All)
	if err != nil {
		return nil, err
	}
	v := scan.v
	tot := v.Totals()
	need, err := missingIDs(ctx, c, a.Project, v.Objects())
	if err != nil {
		return nil, err
	}
	var needBytes int64
	for _, ob := range v.Objects() {
		if need[ob.ID] {
			needBytes += ob.Size
		}
	}
	raw, err := json.Marshal(spec)
	if err != nil {
		return nil, err
	}
	// named as the tool names it: a tool's job takes the tool's name as
	// its stage, and the farm's prebuilt tools share the types' names, so
	// a job submitted while its tool is still testing on a new farm is
	// named as it will be once the test passes
	stage := a.Stage
	if stage == "" {
		stage = t.Name
	}
	req := api.SubmitRequest{Project: a.Project, Name: a.Name, Type: t.Name, Stage: stage, Shot: shot, Spec: raw,
		Inputs: []api.SubmitInput{{View: v.ID.String(), MountPath: t.Mount}}, AIUsage: t.AIUsage, DryRun: true}
	for _, j := range a.After {
		// one upstream job is at /in; several are at /in/<job>
		mp := "/in"
		if len(a.After) > 1 {
			mp = "/in/" + j
		}
		req.Inputs = append(req.Inputs, api.SubmitInput{FromJob: j, MountPath: mp, ReadOnly: true})
	}
	var plan api.SubmitResponse
	planErr := c.Do(ctx, "POST", "/jobs", req, &plan)
	var ae *APIError
	if planErr != nil && !(errors.As(planErr, &ae) && ae.Status == 503) {
		return nil, planErr
	}
	name := plan.Name
	if name == "" {
		name = a.Name
	}
	if name == "" {
		name = "(next version)"
	}
	head := []string{spec.Class}
	if f := spec.Params["frames"]; f != "" && spec.Class == "render" {
		head[0] = t.Name
		head = append(head, plural(frameCount(f), "frame", "frames"))
	}
	if r := spec.Params["renderer"]; r != "" {
		head = append(head, rendererName(r))
	}
	if len(spec.Pools) > 0 {
		head = append(head, "pool "+strings.Join(spec.Pools, ", "))
	}
	o.printf("%s   %s\n\n", name, strings.Join(head, " · "))
	from := ""
	if rel != "." {
		from = " from " + filepath.Base(rel)
	}
	have := int(tot.UniqueObjects) - len(need)
	upl := plural(len(need), "to upload", "to upload")
	if len(need) > 0 {
		upl += " (" + Bytes(needBytes) + ")"
	}
	if have > 0 {
		o.printf("  files     %s found%s%s · %d already on the farm · %s\n", plural(int(tot.Files), "file", "files"), from, leftOut(scan.left), have, upl)
	} else {
		o.printf("  files     %s found%s%s · %s · %s\n", plural(int(tot.Files), "file", "files"), from, leftOut(scan.left), Bytes(tot.Bytes), upl)
	}
	o.printPlan(plan.Estimate, planErr, spec.Class)
	// a repository's own tests name no tool, so they run with no network:
	// say it before a test that fetches fails inside the job
	if spec.Class == "ci" {
		o.printf("  note      %s\n", ciNoNetwork)
	}
	if line, ok := orderLines[spec.Params["order"]]; ok && spec.Class == "render" && frameCount(spec.Params["frames"]) > 2 {
		o.printf("  order     %s\n", line)
	}
	o.printf("\n")
	if a.DryRun {
		return &plan, nil
	}
	if !a.Yes && !o.confirm("Submit?") {
		return nil, errors.New("not submitted")
	}

	sent, sentBytes, err := upload(ctx, c, a.Project, root, v, need)
	if err != nil {
		return nil, err
	}
	if err := recordView(ctx, c, a.Project, v); err != nil {
		return nil, err
	}
	if sent > 0 {
		o.printf("uploaded  %s · %s\n", plural(sent, "file", "files"), Bytes(sentBytes))
	}
	req.DryRun = false
	if req.Name == "" && plan.Name != "" {
		req.Name = plan.Name
	}
	var done api.SubmitResponse
	if err := c.Do(ctx, "POST", "/jobs", req, &done); err != nil {
		return nil, err
	}
	o.printf("queued    %s/%s\n", done.Namespace, done.Name)
	o.printf("HUD       %s\n", c.Web(done.Link, nil))
	return &done, nil
}

// notOffered says why a type runs as the farm's built-in job when the
// project has a tool of its name: no version of it is offered yet, since
// its test is running (a new farm's first minutes) or failed.
func (o Out) notOffered(ctx context.Context, c *Client, project, name string) {
	if _, ok := types[name]; !ok {
		return
	}
	t, err := LookupTool(ctx, c, project, name, "v1")
	if err != nil {
		return
	}
	state := ""
	if t.Test != nil {
		state = t.Test.State
	}
	if n := len(t.Versions); n > 0 {
		state = t.Versions[n-1].Test.State
	}
	switch state {
	case c4v1.ToolTestRunning, "":
		o.printf("tool      %s is still testing on the farm, so this runs the built-in %s; the job's name is the same\n", name, name)
	case c4v1.ToolTestFailed:
		o.printf("tool      %s's test failed on the farm, so this runs the built-in %s (c4 farm ls --tool-tests shows the test)\n", name, name)
	}
}

func rendererName(r string) string {
	return map[string]string{"cycles": "Cycles", "karma-xpu": "Karma XPU", "karma-cpu": "Karma CPU", "prman": "RenderMan",
		"arnold": "Arnold", "vray": "V-Ray"}[r]
}

// printPlan prints the estimate and deadline lines of a plan.
// ciNoNetwork is what a ci job's plan says of its network.
const ciNoNetwork = "ci runs a repository's pytest or Go tests with no network: stage dependencies in the folder, or name a tool that declares the network they come from"

// An estimate that rests on the class default (NoHistory) says so in
// place of a finish and a slack: a guess shows neither a ready time nor a
// miss. class names the units ("frames" for a render).
func (o Out) printPlan(e *api.SubmitEstimate, planErr error, class string) {
	if e == nil {
		msg := "the farm has not planned yet; it estimates the job once it is queued"
		if planErr == nil {
			msg = "none"
		}
		o.printf("  estimate  %s\n", msg)
		return
	}
	if e.NoHistory {
		o.printf("  estimate  %s\n", forecast.NoHistory(class))
		if e.Deadline != nil {
			o.printf("  deadline  %s\n", o.deadlineWords(e))
		}
		return
	}
	if e.ETA50 != nil {
		line := "ready ~" + o.Clock.At(*e.ETA50)
		if e.ETA90 != nil {
			line += "  (p90 ~" + o.Clock.At(*e.ETA90) + ")"
		}
		o.printf("  estimate  %s\n", line)
	}
	if e.Deadline != nil {
		d := o.deadlineWords(e)
		slack := time.Duration(e.SlackSeconds) * time.Second
		switch {
		case !e.Feasible || e.SlackSeconds < 0:
			late := "at current capacity"
			if e.SlackSeconds < 0 {
				late = "by ~" + Span(-slack) + " " + late
			}
			o.printf("  deadline  %s · will miss %s\n", d, late)
		case e.AtRisk:
			o.printf("  deadline  %s · slack ~%s, at risk\n", d, Span(slack))
		default:
			o.printf("  deadline  %s · slack ~%s\n", d, Span(slack))
		}
	}
	if e.Message != "" {
		o.printf("  note      %s\n", e.Message)
	}
}

// deadlineWords is a plan's deadline and where it came from.
func (o Out) deadlineWords(e *api.SubmitEstimate) string {
	d := o.Clock.AtUTC(*e.Deadline)
	switch e.DeadlineSource {
	case "suggested":
		d = "~" + d + ", suggested by the farm"
	case "class":
		d += ", the class's turnaround"
	}
	return d
}

// WaitStore waits until the project's store answers through the HUD (a
// missing-objects check of one object), or until timeout: the HUD can
// answer before the store behind it does.
func WaitStore(ctx context.Context, c *Client, project string, timeout time.Duration) error {
	probe := []view.Object{{ID: c4.Identify(strings.NewReader("c4 farm: is the store answering?"))}}
	deadline := time.Now().Add(timeout)
	for {
		_, err := missingIDs(ctx, c, project, probe)
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the store did not answer in %s", Span(timeout))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}
