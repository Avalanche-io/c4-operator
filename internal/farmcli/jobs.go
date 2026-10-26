package farmcli

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/Avalanche-io/c4-operator/hud/api"
)

// Snapshot reads the farm as the user sees it.
func Snapshot(ctx context.Context, c *Client) (*api.Snapshot, error) {
	var s api.Snapshot
	if err := c.Do(ctx, "GET", "/snapshot", nil, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// ResolveProject is the project a command works in: the one given, else
// the farm's default, else the only project the user sees.
func ResolveProject(ctx context.Context, c *Client, given, def string) (string, error) {
	if given != "" {
		return given, nil
	}
	if def != "" {
		return def, nil
	}
	s, err := Snapshot(ctx, c)
	if err != nil {
		return "", err
	}
	if s.Viewer != nil && len(s.Viewer.Projects) == 1 {
		return s.Viewer.Projects[0], nil
	}
	var names []string
	if s.Viewer != nil {
		names = s.Viewer.Projects
	}
	return "", fmt.Errorf("name a project with --project: you see %s", strings.Join(names, ", "))
}

// findJob finds a job by ns/name, or by name in the user's projects.
func findJob(ctx context.Context, c *Client, project, ref string) (*api.Job, error) {
	s, err := Snapshot(ctx, c)
	if err != nil {
		return nil, err
	}
	var hits []*api.Job
	for i := range s.Jobs {
		j := &s.Jobs[i]
		switch {
		case strings.Contains(ref, "/") && j.Namespace+"/"+j.Name == ref:
			return j, nil
		case j.Name == ref && (project == "" || j.Project == project):
			hits = append(hits, j)
		}
	}
	switch len(hits) {
	case 1:
		return hits[0], nil
	case 0:
		return nil, fmt.Errorf("no job %s in your projects (c4 farm ls lists them)", ref)
	}
	return nil, fmt.Errorf("%s is in more than one namespace; give namespace/name", ref)
}

func jobPath(j *api.Job) string {
	return "/jobs/" + url.PathEscape(j.Namespace) + "/" + url.PathEscape(j.Name)
}

// LsArgs filter `c4 farm ls`.
type LsArgs struct {
	Project string
	Mine    bool
	AtRisk  bool
	All     bool // include finished jobs
	// ToolTests includes the farm's tests of its tools, which publishing a
	// tool runs: the farm's work, not the person's
	ToolTests bool
}

// Ls prints the queue in run order.
func Ls(ctx context.Context, c *Client, o Out, a LsArgs) error {
	s, err := Snapshot(ctx, c)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(o.W, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "RUN\tJOB\tDONE\tREADY\tDEADLINE\tSLACK\tOWNER")
	n, finished := 0, 0
	var latest *api.Job
	var whys []string
	for i, j := range s.Jobs {
		if a.Project != "" && j.Project != a.Project {
			continue
		}
		if a.Mine && (s.Viewer == nil || j.Owner != s.Viewer.User) {
			continue
		}
		if j.ToolTest != "" && !a.ToolTests {
			continue
		}
		if !a.All && (j.Phase == "Succeeded" || j.Phase == "Failed") {
			if latest == nil || j.CreatedAt.After(latest.CreatedAt) {
				latest = &s.Jobs[i]
			}
			finished++
			continue
		}
		late := j.Estimate.SlackSeconds < 0 || !j.Estimate.Feasible
		if a.AtRisk && !late {
			continue
		}
		rank := "-"
		if j.Rank > 0 {
			rank = "#" + strconv.Itoa(j.Rank)
		}
		done := fmt.Sprintf("%d of %d", j.Units.Succeeded, j.Units.Total)
		ready, due, slack := "-", "-", "-"
		if !j.Estimate.EtaP50.IsZero() {
			ready = "~" + o.Clock.At(j.Estimate.EtaP50)
		}
		if d := j.Objective.Deadline; d != nil {
			due = o.Clock.At(*d)
			if j.Objective.Source == "suggested" {
				due = "~" + due
			}
			s := time.Duration(j.Estimate.SlackSeconds) * time.Second
			if s < 0 {
				// "late" carries the sign; "~-2h late" would say it twice.
				s = -s
			}
			slack = "~" + Span(s)
			if late {
				slack += " late"
			}
		}
		if why := stuck(&j); why != "" {
			// It cannot run, so it is not ready at any time.
			ready, slack = "stuck", "-"
			whys = append(whys, "stuck "+j.Namespace+"/"+j.Name+": "+why)
		} else if why := heldFailed(&j); why != "" {
			ready, slack = "held", "-"
			whys = append(whys, "held "+j.Namespace+"/"+j.Name+": "+why)
		}
		fmt.Fprintf(tw, "%s\t%s/%s\t%s\t%s\t%s\t%s\t%s\n", rank, j.Namespace, j.Name, done, ready, due, slack, j.Owner)
		n++
	}
	tw.Flush()
	if n == 0 {
		o.printf("no jobs waiting or running\n")
	}
	if latest != nil {
		verb := "finished"
		if latest.Phase == "Failed" {
			verb = "failed"
		}
		o.printf("%s finished (c4 farm ls --all lists them); the newest, %s/%s, %s: c4 farm get %s DIR\n",
			plural(finished, "job", "jobs"), latest.Namespace, latest.Name, verb, latest.Name)
	}
	for _, w := range whys {
		o.printf("%s\n", w)
	}
	return nil
}

// stuck says why a job cannot run at all (its pods are refused, its image
// cannot be pulled, it has no pool or no plan, its quota is used up), or
// "" if it can. Such a job has no ready time worth showing.
func stuck(j *api.Job) string {
	n := j.NeedsPerson
	if n == nil || j.Units.Running > 0 {
		return ""
	}
	switch n.Reason {
	case "stuck", "permission", "quota":
		return n.Message
	}
	return ""
}

// heldFailed says why a job waits on a person with nothing running, its
// units having failed: the farm's message, then what the units said as
// they failed (its causes, the most common first). A job that names no
// tool adds that it had no network only when that may be why: a unit
// failed reaching something, or none said why. "" for every other job.
func heldFailed(j *api.Job) string {
	n := j.NeedsPerson
	if n == nil || n.Reason != "failed" || j.Units.Running > 0 {
		return ""
	}
	var exits []api.Cause
	for _, c := range j.Causes {
		if c.Kind == "exit" {
			exits = append(exits, c)
		}
	}
	sort.SliceStable(exits, func(a, b int) bool { return exits[a].Count > exits[b].Count })
	why, sep, said, reached := n.Message, "; why: ", false, false
	for _, c := range exits[:min(2, len(exits))] {
		why += sep + c.Title
		sep = ", or "
	}
	for _, c := range exits {
		said = said || strings.Contains(c.Title, ": ")
		reached = reached || networkWords.MatchString(c.Title)
	}
	if j.NoNetwork != "" && (reached || !said) {
		why += "; " + j.NoNetwork
	}
	return why
}

// networkWords are what a program says when it could not reach a host.
var networkWords = regexp.MustCompile(`(?i)dial|no such host|resolv|connection (refused|reset|timed out)|network is unreachable|timed? ?out|could not connect|failed to connect|unable to connect|proxy|tls handshake`)

// Watch prints a job's title block whenever it changes, until the job
// finishes or ctx ends.
func Watch(ctx context.Context, c *Client, o Out, project, ref string, every time.Duration) error {
	last := ""
	for {
		j, err := findJob(ctx, c, project, ref)
		if err != nil {
			return err
		}
		line := titleBlock(o.Clock, j)
		if line != last {
			o.printf("%s  %s\n", o.Clock.At(o.Clock.now()), line)
			last = line
		}
		if j.Phase == "Succeeded" || j.Phase == "Failed" {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(every):
		}
	}
}

func titleBlock(cl Clock, j *api.Job) string {
	parts := []string{fmt.Sprintf("%s  done %d of %d", strings.ToLower(j.Phase), j.Units.Succeeded, j.Units.Total)}
	if j.Units.Running > 0 {
		parts = append(parts, fmt.Sprintf("running %d", j.Units.Running))
	}
	if j.Units.Failed > 0 {
		parts = append(parts, fmt.Sprintf("failed %d", j.Units.Failed))
	}
	if why := stuck(j); why != "" {
		parts = append(parts, "stuck: "+why)
	} else if !j.Estimate.EtaP50.IsZero() && j.Phase != "Succeeded" {
		parts = append(parts, "ready ~"+cl.At(j.Estimate.EtaP50)+" · ~"+cl.At(j.Estimate.EtaP90))
	}
	if d := j.Objective.Deadline; d != nil {
		parts = append(parts, "deadline "+cl.At(*d), "slack ~"+Span(time.Duration(j.Estimate.SlackSeconds)*time.Second))
	}
	if j.Output != nil && j.Output.View != "" {
		parts = append(parts, "output "+ShortID(j.Output.View))
	}
	return strings.Join(parts, "  ")
}

// ShortID is a C4 ID as the HUD shows it: "c4oa Zo9X … gi4q".
func ShortID(id string) string {
	if len(id) < 12 {
		return id
	}
	return id[:4] + " " + id[4:8] + " … " + id[len(id)-4:]
}

// Control sends one control to a job (the same call as its HUD button).
func Control(ctx context.Context, c *Client, o Out, project, ref, verb string, body map[string]any) error {
	j, err := findJob(ctx, c, project, ref)
	if err != nil {
		return err
	}
	var r struct {
		Message string `json:"message"`
	}
	if err := c.Do(ctx, "POST", jobPath(j)+"/"+verb, body, &r); err != nil {
		return err
	}
	o.printf("%s\n", r.Message)
	return nil
}

// Deadline moves a job's deadline, read on the local clock.
func Deadline(ctx context.Context, c *Client, o Out, project, ref, when string) error {
	if strings.EqualFold(strings.TrimSpace(when), "suggested") {
		return Control(ctx, c, o, project, ref, "nudge", map[string]any{"deadlineMode": "Suggested"})
	}
	t, err := o.Clock.ParseWhen(when)
	if err != nil {
		return err
	}
	if err := Control(ctx, c, o, project, ref, "nudge", map[string]any{"deadline": t.UTC().Format(time.RFC3339)}); err != nil {
		return err
	}
	o.printf("deadline  %s\n", o.Clock.AtUTC(t))
	return nil
}

// Open is the job view's (or side widget's) address.
func Open(ctx context.Context, c *Client, project, ref string, widget bool) (string, error) {
	j, err := findJob(ctx, c, project, ref)
	if err != nil {
		return "", err
	}
	route := "job/" + j.Namespace + "/" + j.Name
	if widget {
		route = "widget/" + j.Namespace + "/" + j.Name
	}
	return c.Web(route, nil), nil
}
