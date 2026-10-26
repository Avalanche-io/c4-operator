package flow

import (
	"fmt"
	"strings"
	"time"

	"github.com/Avalanche-io/c4-operator/internal/tracker"
)

// Present shows a link's state in Flow, on the Task:
//   - its status, from the Tracker's statusOut.states map (site codes
//     differ, so nothing is assumed); a state the map leaves out, and done
//     always, write nothing;
//   - the ETA and the job link in the site's custom fields, when the
//     Tracker names them (scope eta_field, link_field);
//   - one Note when a person is needed or the job failed, with the cause,
//     edited in place as the cause changes: one note per cause, never one
//     per frame.
//
// The Version's status is set once, to pending review, when it is
// published; after that it is the supervisor's.
func (a *Adapter) Present(s tracker.LinkState, c tracker.PresentConfig) []tracker.FieldWrite {
	var out []tracker.FieldWrite
	if code := c.States[s.State]; code != "" && s.State != "done" && s.State != "done-accepted" {
		out = append(out, tracker.FieldWrite{
			Field: "status", Value: code,
			Build: func(tracker.Written) tracker.Write {
				return tracker.Write{On: s.Ref, Kind: "update", Fields: map[string]string{"sg_status_list": code}}
			},
		})
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
	if a.spendField != "" && s.Spend > 0 {
		// One number in the pools' currency, so a currency field sums in
		// the site's reports; it moves every tick, so it is continuous and
		// written at most every fieldSeconds.
		spend := fmt.Sprintf("%.2f", s.Spend)
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
					Fields: map[string]string{"subject": "C4: " + strings.ToLower(s.Words)},
					Note:   &tracker.Note{Body: noteBody(s)}}
				if prev.Ref != "" {
					w.Note.Replace = &tracker.Ref{Type: "Note", ID: prev.Ref}
				}
				return w
			},
		})
	}
	return out
}

func noteBody(s tracker.LinkState) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s.\n\n%s\n\nC4 job: %s", s.Words, s.Why, s.JobURL)
	if s.Deadline != nil {
		fmt.Fprintf(&b, "\nDeadline: %s (%s)", tracker.When(*s.Deadline, s.Loc), s.DeadlineSource)
	}
	return b.String()
}
