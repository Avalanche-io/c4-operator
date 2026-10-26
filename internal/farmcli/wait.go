package farmcli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/Avalanche-io/c4-operator/hud/api"
)

// A lead waits for its workers as it waits for any background work: it
// runs `c4 farm wait` as a background command, which exits once, when a
// worker ends or stops on a question, printing what the lead acts on, and
// Claude Code tells the lead. The command long-polls the HUD's wait route
// (GET /jobs/{ns}/{name}/wait), so it holds one request at a time and the
// farm is never polled.

// Exit codes of c4 farm wait.
const (
	WaitSucceeded = 0  // every unit settled ended well (succeeded, accepted)
	WaitFailed    = 1  // one ended badly (failed, superseded)
	WaitTimedOut  = 2  // --timeout passed first
	WaitAsks      = 10 // one stopped on a question
)

// Exit is an exit code a command chose, having said what it had to.
type Exit struct{ Code int }

func (e *Exit) Error() string { return fmt.Sprintf("exit %d", e.Code) }

// WaitArgs are `c4 farm wait UNIT... [--any] [--json] [--timeout D]`.
type WaitArgs struct {
	// Units are job/unit or namespace/job/unit; a unit is its name or its
	// index.
	Units   []string
	Any     bool
	JSON    bool
	Timeout time.Duration
	// Within is how long each request holds (default 25s); Retry is how
	// long to wait after the HUD did not answer (default 5s).
	Within, Retry time.Duration
	// Errs is where it says the HUD is not answering (default: nowhere).
	Errs io.Writer
}

// waitJob is one job's units being waited on.
type waitJob struct {
	path  string // /jobs/{ns}/{name}
	units []string
}

// Wait waits until the units named settle (with Any, one of them), or at
// once when one stops on a question, prints each, and returns an *Exit
// with its code. A question ends any wait: the unit that asked waits on
// the lead, who answers with c4_supply and waits again on those still
// open.
func Wait(ctx context.Context, c *Client, o Out, project string, a WaitArgs) error {
	if a.Within <= 0 {
		a.Within = 25 * time.Second
	}
	if a.Retry <= 0 {
		a.Retry = 5 * time.Second
	}
	jobs, order, err := waitJobs(ctx, c, project, a.Units)
	if err != nil {
		return err
	}
	if a.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, a.Timeout)
		defer cancel()
	}
	results := map[string]api.UnitResult{} // by job path and unit ref
	open := map[string]bool{}
	for _, j := range jobs {
		open[j.path] = true
	}
	quiet := false
	for len(open) > 0 {
		answers, err := waitRound(ctx, c, jobs, open, a)
		var apiErr *APIError
		switch {
		case ctx.Err() != nil:
			printWait(o, order, results, a.JSON, false)
			return &Exit{WaitTimedOut}
		case errors.As(err, &apiErr) && apiErr.Status < 500:
			return err // the job or a unit is not there, or not the caller's
		case err != nil:
			if !quiet && a.Errs != nil {
				fmt.Fprintf(a.Errs, "c4 farm wait: the HUD did not answer (%v); still waiting\n", err)
				quiet = true
			}
			select {
			case <-ctx.Done():
			case <-time.After(a.Retry):
			}
			continue
		}
		quiet = false
		settledOne, asked := false, false
		for path, ans := range answers {
			j := jobs[path]
			for k, u := range ans.Units {
				if k < len(j.units) {
					results[path+"\x00"+j.units[k]] = u
				}
				settledOne = settledOne || u.Settled != ""
				asked = asked || u.Settled == "asks"
			}
			if ans.Done {
				delete(open, path)
			}
		}
		if asked && len(open) > 0 {
			// how the others stand, without waiting for them
			now := a
			now.Within, now.Any = time.Millisecond, false
			if more, err := waitRound(ctx, c, jobs, open, now); err == nil {
				for path, ans := range more {
					for k, u := range ans.Units {
						if k < len(jobs[path].units) {
							results[path+"\x00"+jobs[path].units[k]] = u
						}
					}
				}
			}
		}
		if asked || (a.Any && settledOne) {
			break
		}
	}
	printWait(o, order, results, a.JSON, true)
	return &Exit{waitCode(results, a.Any)}
}

// waitJobs groups the units named by their job, resolved once, and keeps
// the order they were named in.
func waitJobs(ctx context.Context, c *Client, project string, refs []string) (map[string]*waitJob, []string, error) {
	jobs := map[string]*waitJob{}
	byRef := map[string]string{}
	var order []string
	for _, ref := range refs {
		i := strings.LastIndex(ref, "/")
		if i <= 0 || i == len(ref)-1 {
			return nil, nil, fmt.Errorf("%q names no unit: write job/unit or namespace/job/unit", ref)
		}
		jobRef, unit := ref[:i], ref[i+1:]
		path, ok := byRef[jobRef]
		if !ok {
			j, err := findJob(ctx, c, project, jobRef)
			if err != nil {
				return nil, nil, err
			}
			path = jobPath(j)
			byRef[jobRef] = path
		}
		if jobs[path] == nil {
			jobs[path] = &waitJob{path: path}
		}
		jobs[path].units = append(jobs[path].units, unit)
		order = append(order, path+"\x00"+unit)
	}
	return jobs, order, nil
}

// waitRound asks each job with units still open, at once, and returns
// their answers; with Any, the first that settles a unit ends the round.
func waitRound(ctx context.Context, c *Client, jobs map[string]*waitJob, open map[string]bool, a WaitArgs) (map[string]api.WaitAnswer, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	type got struct {
		path string
		ans  api.WaitAnswer
		err  error
	}
	ch := make(chan got, len(open))
	for path := range open {
		j := jobs[path]
		q := url.Values{"unit": j.units, "within": {a.Within.String()}}
		if a.Any {
			q.Set("any", "true")
		}
		go func() {
			var ans api.WaitAnswer
			err := c.Do(ctx, "GET", j.path+"/wait?"+q.Encode(), nil, &ans)
			ch <- got{path, ans, err}
		}()
	}
	out := map[string]api.WaitAnswer{}
	var first error
	for range open {
		g := <-ch
		if g.err != nil {
			if first == nil && ctx.Err() == nil {
				first = g.err
			}
			continue
		}
		out[g.path] = g.ans
		if g.ans.Done && (a.Any || answerAsks(g.ans)) {
			cancel() // the others are not needed now
		}
	}
	if len(out) == 0 && first != nil {
		return nil, first
	}
	var apiErr *APIError
	if errors.As(first, &apiErr) && apiErr.Status < 500 {
		return nil, first
	}
	return out, nil
}

// answerAsks reports whether one of an answer's units stopped on a
// question.
func answerAsks(ans api.WaitAnswer) bool {
	for _, u := range ans.Units {
		if u.Settled == "asks" {
			return true
		}
	}
	return false
}

// waitCode is the exit code for what settled.
func waitCode(results map[string]api.UnitResult, anyOne bool) int {
	code := WaitSucceeded
	for _, u := range results {
		switch {
		case u.Settled == "asks":
			return WaitAsks
		case u.Settled == "ended" && u.State != "succeeded" && u.State != "accepted":
			code = WaitFailed
		case u.Settled == "" && !anyOne:
			code = max(code, WaitTimedOut)
		}
	}
	return code
}

// printWait prints each unit named, settled ones with what the lead acts
// on, as text or as JSON.
func printWait(o Out, order []string, results map[string]api.UnitResult, asJSON, done bool) {
	units := make([]api.UnitResult, 0, len(order))
	for _, k := range order {
		if u, ok := results[k]; ok {
			units = append(units, u)
		}
	}
	if asJSON {
		b, _ := json.MarshalIndent(api.WaitAnswer{Done: done, Units: units}, "", "  ")
		o.printf("%s\n", b)
		return
	}
	if !done {
		o.printf("still waiting:\n")
	}
	for _, u := range units {
		name := u.Name
		if name == "" {
			name = fmt.Sprint(u.Unit)
		}
		head := fmt.Sprintf("%s/%s  %s", u.Job, name, u.State)
		if u.Label != "" && u.Label != u.Name {
			head += "  " + u.Label
		}
		if u.DurationSeconds > 0 {
			head += "  " + (time.Duration(u.DurationSeconds) * time.Second).String()
		}
		o.printf("%s\n", head)
		if q := u.Question; q != nil {
			o.printf("  question  %s\n", q.Text)
			if q.Why != "" {
				o.printf("  why       %s\n", q.Why)
			}
			o.printf("  answer    with c4_supply (job %s, unit %s)\n", u.Job, name)
		}
		if s := u.Summary; s != nil {
			text := s.Text
			if text == "" {
				text = s.Excerpt
			}
			switch {
			case text != "":
				lines := strings.Split(strings.TrimSpace(text), "\n")
				o.printf("  summary   %s\n", lines[0])
				for _, l := range lines[1:] {
					o.printf("            %s\n", l)
				}
				o.printf("            (the unit's own words)\n")
			case s.Error != "":
				o.printf("  summary   none readable: %s\n", s.Error)
			}
		}
		if b := u.Branch; b != nil {
			commits := fmt.Sprintf("%d commits", b.Commits)
			if b.Commits == 1 {
				commits = "1 commit"
			}
			o.printf("  branch    %s, %s from %s to %s in %s\n", b.Branch, commits, short(b.Base), short(b.Head), b.Repo)
		}
		if e := u.Ended; e != nil && e.Reason != "" && e.Reason != "Completed" {
			o.printf("  ended     %s: %s\n", e.Reason, e.Message)
		}
	}
}

// short is a commit as git shows it.
func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
