package linear

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/Avalanche-io/c4-operator/internal/tracker"
)

// Present shows a link's state on the Linear issue:
//   - its workflow state: the Tracker's statusOut.states names one (a
//     team's names are its own), else the team's first started state while
//     the work runs or is reworked; nothing else is moved, and never to a
//     completed or canceled state, which are the reviewer's;
//   - the job's status card: one attachment at the job's HUD URL, its
//     title the state in words and its subtitle the state, the ETA and the
//     risk, updated in place (Linear keys attachments by URL);
//   - one comment when a person is needed or the job failed, with the
//     cause, edited in place as the cause changes.
func (a *Adapter) Present(s tracker.LinkState, c tracker.PresentConfig) []tracker.FieldWrite {
	var out []tracker.FieldWrite
	if f, v := stateWrite(s.State, c.States); f != nil {
		out = append(out, tracker.FieldWrite{
			Field: "status", Value: v,
			Build: func(tracker.Written) tracker.Write {
				return tracker.Write{On: s.Ref, Kind: "status", Fields: f}
			},
		})
	}
	if s.JobURL != "" {
		out = append(out, tracker.FieldWrite{
			Field: "card", Value: stateKey(s), Continuous: eta(s),
			Build: func(tracker.Written) tracker.Write {
				md := map[string]any{"job": s.Job, "state": s.State, "atRisk": s.AtRisk}
				if s.ETA != nil && s.Active {
					md["eta"] = s.ETA.UTC().Format(time.RFC3339)
				}
				if s.Deadline != nil {
					md["deadline"], md["deadlineSource"] = s.Deadline.UTC().Format(time.RFC3339), s.DeadlineSource
				}
				b, _ := json.Marshal(md)
				return tracker.Write{On: s.Ref, Kind: "attachment", Fields: map[string]string{
					"url": s.JobURL, "title": "C4: " + s.Words, "subtitle": s.Title, "metadata": string(b)}}
			},
		})
	}
	if s.Why != "" {
		out = append(out, tracker.FieldWrite{
			Field: "comment", Value: s.State + "|" + s.Why,
			Build: func(prev tracker.Written) tracker.Write {
				w := tracker.Write{On: s.Ref, Kind: "comment", Key: tracker.WriteKey(s.Link, "comment", ""),
					Note: &tracker.Note{Body: commentBody(s)}}
				if prev.Ref != "" {
					w.Note.Replace = &tracker.Ref{Type: "Comment", ID: prev.Ref}
				}
				return w
			},
		})
	}
	return out
}

// stateWrite is the status write for a summary state, and its value: a
// state the Tracker names, else the team's first started state while the
// work runs or is reworked. done is never written.
func stateWrite(state string, names map[string]string) (map[string]string, string) {
	if state == "done" || state == "done-accepted" {
		return nil, ""
	}
	if n := names[state]; n != "" {
		return map[string]string{"state": n}, "name:" + n
	}
	if state == "running" || state == "rework" {
		return map[string]string{"stateType": "started"}, "type:started"
	}
	return nil, ""
}

// stateKey is what changes the card: the state, why, the wait and the
// risk. The ETA is left out; it is written on its own clock.
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

func commentBody(s tracker.LinkState) string {
	var b strings.Builder
	fmt.Fprintf(&b, "**%s.**\n\n%s\n\n[C4 job](%s)", s.Words, s.Why, s.JobURL)
	if s.Deadline != nil {
		fmt.Fprintf(&b, " · deadline %s (%s)", s.Deadline.UTC().Format("Mon 2 Jan 15:04 UTC"), s.DeadlineSource)
	}
	return b.String()
}
