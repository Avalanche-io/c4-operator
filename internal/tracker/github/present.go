package github

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/Avalanche-io/c4-operator/internal/tracker"
)

// Present shows a link's state on GitHub: one comment on the issue,
// edited in place; once there is a pull request, one check run per head
// commit, updated in place, and optionally a commit status. Work states
// only: done completes the check run as success; the approval that may
// follow is GitHub's own review.
func (a *Adapter) Present(s tracker.LinkState, c tracker.PresentConfig) []tracker.FieldWrite {
	key := stateKey(s)
	out := []tracker.FieldWrite{{
		Field: "comment", Value: key, Continuous: eta(s),
		Build: func(prev tracker.Written) tracker.Write {
			w := tracker.Write{On: s.Ref, Kind: "comment", Note: &tracker.Note{Body: commentBody(s)},
				Key: tracker.WriteKey(s.Link, "comment", "")}
			if prev.Ref != "" {
				w.Note.Replace = &tracker.Ref{Type: "Comment", ID: prev.Ref}
			}
			return w
		},
	}}
	sha := s.ReviewHead
	if s.Review == nil || sha == "" {
		return out
	}
	name := c.CheckName
	if name == "" {
		name = "C4"
	}
	status, conclusion := checkState(s)
	fields := map[string]string{
		"name": name, "head_sha": sha, "status": status, "conclusion": conclusion,
		"title": s.Title, "summary": checkSummary(s),
		"details_url": s.JobURL, "external_id": s.Job, "actions": actions(s.Wait),
	}
	checkKey := key + "|" + sha
	out = append(out, tracker.FieldWrite{
		Field: "check", Value: checkKey, Continuous: eta(s),
		Build: func(prev tracker.Written) tracker.Write {
			f := map[string]string{}
			for k, v := range fields {
				f[k] = v
			}
			// A completed run is not reopened: a new run takes over, as a
			// re-run does. A new head commit gets a new run too.
			if prev.Ref != "" && strings.HasSuffix(valueOf(prev.Value), "|"+sha) && !completed(prev.Value) {
				f["id"] = prev.Ref
			}
			return tracker.Write{On: tracker.Ref{Type: "Commit", ID: sha}, Kind: "check", Fields: f,
				Key: tracker.WriteKey(s.Link, "check", checkKey)}
		},
	})
	if !c.CommitStatus {
		return out
	}
	state := "pending"
	switch s.State {
	case "done", "done-accepted":
		state = "success"
	case "needs-person", "failed":
		state = "failure"
	}
	out = append(out, tracker.FieldWrite{
		Field: "status", Value: key + "|" + sha,
		Build: func(tracker.Written) tracker.Write {
			return tracker.Write{On: tracker.Ref{Type: "Commit", ID: sha}, Kind: "status", Fields: map[string]string{
				"sha": sha, "state": state, "context": "c4/" + jobName(s.Job), "description": s.Words, "target_url": s.JobURL}}
		},
	})
	return out
}

// stateKey is what changes a status write: the state, why, the wait and
// the risk. The ETA is left out; it is written on its own clock.
func stateKey(s tracker.LinkState) string {
	w := ""
	if s.Wait != nil {
		w = s.Wait.Server + ":" + strings.Join(s.Wait.Decisions(), ",")
	}
	return fmt.Sprintf("%s|%s|%s|%t", s.State, s.Why, w, s.AtRisk)
}

func eta(s tracker.LinkState) string {
	if s.ETA == nil || !s.Active {
		return ""
	}
	return s.ETA.UTC().Format("2006-01-02T15:04:05Z")
}

func valueOf(v string) string {
	s, _, _ := strings.Cut(v, "#")
	return s
}

func completed(v string) bool {
	switch strings.SplitN(valueOf(v), "|", 2)[0] {
	case "done", "done-accepted", "failed", "needs-person":
		return true
	}
	return false
}

func jobName(job string) string {
	_, n, ok := strings.Cut(job, "/")
	if !ok {
		return job
	}
	return n
}

// checkState maps a summary state to a check run's status and conclusion.
func checkState(s tracker.LinkState) (status, conclusion string) {
	switch s.State {
	case "running", "rework", "in-review":
		return "in_progress", ""
	case "needs-person":
		return "completed", "action_required"
	case "failed":
		return "completed", "failure"
	case "done", "done-accepted":
		return "completed", "success"
	}
	return "queued", "" // queued, held
}

// actions are the check run's buttons for an open wait (three at most).
func actions(w *tracker.WaitState) string {
	var parts []string
	for _, d := range w.Decisions() {
		if len(parts) < 3 {
			parts = append(parts, d+"="+strings.ToUpper(d[:1])+d[1:])
		}
	}
	return strings.Join(parts, ";")
}

func checkSummary(s tracker.LinkState) string {
	var b strings.Builder
	fmt.Fprintf(&b, "C4 job [%s](%s): %s.", s.Job, s.JobURL, s.Words)
	if s.Deadline != nil {
		fmt.Fprintf(&b, "\n\nDeadline %s (%s).", s.Deadline.UTC().Format("Mon 2 Jan 15:04 UTC"), s.DeadlineSource)
	}
	if s.ETA != nil && s.Active {
		fmt.Fprintf(&b, " Expected by %s.", s.ETA.UTC().Format("Mon 2 Jan 15:04 UTC"))
	}
	if s.Why != "" {
		fmt.Fprintf(&b, "\n\n%s", s.Why)
	}
	if s.Wait != nil {
		b.WriteString("\n\nReview this pull request (approve, or request changes) to decide; C4 reads the review.")
	}
	return b.String()
}

func commentBody(s tracker.LinkState) string {
	var b strings.Builder
	fmt.Fprintf(&b, "**C4:** %s\n\n", s.Title)
	fmt.Fprintf(&b, "Job [%s](%s)", s.Job, s.JobURL)
	if s.Deadline != nil {
		fmt.Fprintf(&b, ", deadline %s (%s)", s.Deadline.UTC().Format("Mon 2 Jan 15:04 UTC"), s.DeadlineSource)
	}
	b.WriteString(".")
	if s.Why != "" {
		fmt.Fprintf(&b, "\n\n%s", s.Why)
	}
	if s.Review == nil && s.Ref.Type == "Issue" {
		fmt.Fprintf(&b, "\n\nA pull request for this work names it with `%s` or a closing keyword for #%s.", Marker(s.Job), s.Ref.ID)
	}
	return b.String()
}

// Marker is the line a pull request names its job with.
func Marker(job string) string { return "C4-Job: " + job }

// closing matches GitHub's closing keywords.
var closing = regexp.MustCompile(`(?i)\b(?:close[sd]?|fix(?:e[sd])?|resolve[sd]?)\s+#(\d+)\b`)

// FindReviews finds, for links on issues with no pull request yet, an open
// pull request that names the job (Marker) or closes the issue. One list
// of open pull requests serves every link.
func (a *Adapter) FindReviews(ctx context.Context, links []tracker.LinkState) (map[string]tracker.Ref, error) {
	var need []tracker.LinkState
	for _, l := range links {
		if l.Review == nil && l.Ref.Type == "Issue" {
			need = append(need, l)
		}
	}
	if len(need) == 0 {
		return nil, nil
	}
	var prs []tracker.Item
	page := ""
	for {
		items, next, err := a.List(ctx, tracker.Query{Type: "PullRequest", State: "open"}, page)
		if err != nil {
			if errors.Is(err, tracker.ErrUnsupported) {
				return nil, nil
			}
			return nil, err
		}
		prs = append(prs, items...)
		if next == "" {
			break
		}
		page = next
	}
	out := map[string]tracker.Ref{}
	for _, l := range need {
		for _, pr := range prs {
			if mentions(pr.Body, l) {
				out[l.Link] = pr.Ref
				break
			}
		}
	}
	return out, nil
}

func mentions(body string, l tracker.LinkState) bool {
	if strings.Contains(body, Marker(l.Job)) {
		return true
	}
	for _, m := range closing.FindAllStringSubmatch(body, -1) {
		if m[1] == l.Ref.ID {
			return true
		}
	}
	return false
}
