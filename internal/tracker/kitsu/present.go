package kitsu

import (
	"fmt"
	"strconv"
	"time"

	"github.com/Avalanche-io/c4-operator/internal/tracker"
)

// defaultFlags are the statuses a state takes where the Tracker's
// statusOut.states names none: Kitsu's flags say what a status means, so
// a production's own names need no map. A person needed takes none: no
// flag means it, so it is the cause comment alone unless the Tracker
// names a status.
var defaultFlags = map[string]string{"running": "is_wip", "rework": "is_wip", "in-review": "is_feedback_request"}

// Present shows a link's state in Kitsu, on the Task:
//   - its status, by a comment that moves it there: the Tracker's
//     statusOut.states (short names), else the flagged status above; done
//     never (approval is the supervisor's), and nothing when the Task is
//     in that status already (a publish moved it to review);
//   - the ETA and the job link in the Task's data, when the Tracker names
//     the keys (scope eta_field, link_field), and what the job has spent
//     under scope spend_field;
//   - one comment when a person is needed or the job failed, with the
//     cause, edited in place as the cause changes.
func (a *Adapter) Present(s tracker.LinkState, c tracker.PresentConfig) []tracker.FieldWrite {
	var out []tracker.FieldWrite
	if s.State != "done" && s.State != "done-accepted" {
		short, flag := c.States[s.State], ""
		if short == "" {
			flag = defaultFlags[s.State]
		}
		if value := short + flag; value != "" {
			out = append(out, tracker.FieldWrite{
				Field: "status", Value: value,
				Build: func(prev tracker.Written) tracker.Write {
					// Each move has its own key: running, held, then
					// running again is two comments, and a probe must not
					// take the second for the first.
					return tracker.Write{On: s.Ref, Kind: "status", Key: tracker.WriteKey(s.Link, "status", value+"|"+prev.Ref),
						Fields: map[string]string{"task_status": short, "flag": flag},
						Note:   &tracker.Note{Body: statusBody(s)}}
				},
			})
		}
	}
	if a.etaField != "" || a.linkField != "" {
		eta := ""
		if s.ETA != nil && s.Active {
			eta = s.ETA.UTC().Format(time.RFC3339)
		}
		risk := fmt.Sprint(s.AtRisk)
		out = append(out, tracker.FieldWrite{
			Field: "fields", Value: s.JobURL + "|" + risk, Continuous: eta,
			Build: func(tracker.Written) tracker.Write {
				f := map[string]string{}
				if a.etaField != "" {
					f[a.etaField] = eta
				}
				if a.linkField != "" {
					f[a.linkField] = s.JobURL
				}
				return tracker.Write{On: s.Ref, Kind: "update", Fields: f}
			},
		})
	}
	// Spend goes where the syncer says (SpendOn): the shot's total by
	// default, which this adapter does not write yet, so only a Tracker
	// with spend_on: task gets the task's own.
	if a.spendField != "" && s.SpendOn != nil && *s.SpendOn == s.Ref && s.SpendTotal > 0 {
		// One number in the pools' currency, as Flow writes it; it moves
		// every tick, so it is continuous and written at most every
		// fieldSeconds. Four places, so a short job at a low rate is not
		// written as nothing.
		spend := strconv.FormatFloat(s.SpendTotal, 'f', 4, 64)
		out = append(out, tracker.FieldWrite{
			Field: "spend", Value: "spend", Continuous: spend,
			Build: func(tracker.Written) tracker.Write {
				return tracker.Write{On: s.Ref, Kind: "update", Fields: map[string]string{a.spendField: spend}}
			},
		})
	}
	if s.Why != "" {
		out = append(out, tracker.FieldWrite{
			Field: "note", Value: s.State + "|" + s.Why,
			Build: func(prev tracker.Written) tracker.Write {
				w := tracker.Write{On: s.Ref, Kind: "note", Key: tracker.WriteKey(s.Link, "note", ""),
					Note: &tracker.Note{Body: noteBody(s)}}
				if prev.Ref != "" {
					w.Note.Replace = &tracker.Ref{Type: "Comment", ID: prev.Ref}
				}
				return w
			},
		})
	}
	return out
}

// statusBody is what a status comment says: the state, the ETA and the
// risk in one line, and the job.
func statusBody(s tracker.LinkState) string {
	line := s.Title
	if line == "" {
		line = s.Words
	}
	return fmt.Sprintf("C4: %s\n%s", line, s.JobURL)
}

func noteBody(s tracker.LinkState) string {
	b := fmt.Sprintf("C4: %s.\n\n%s\n\nC4 job: %s", s.Words, s.Why, s.JobURL)
	if s.Deadline != nil {
		b += fmt.Sprintf("\nDeadline: %s (%s)", tracker.When(*s.Deadline, s.Loc), s.DeadlineSource)
	}
	return b
}
