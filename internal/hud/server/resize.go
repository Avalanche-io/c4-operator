package server

import (
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Avalanche-io/c4-operator/hud/api"
	"github.com/Avalanche-io/c4-operator/internal/work/queue"
)

// The patterns spec.forecast's fields take (api/v1alpha1 WorkForecast),
// checked before the change is signed.
var (
	forecastHours  = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?$`)
	forecastChance = regexp.MustCompile(`^(0(\.[0-9]+)?|1(\.0+)?)$`)
)

// handleResize changes a forecast job's size (integrations GAPS 9): a
// tracker's sync following a bid, or a person. Only a forecast is
// resized; a real job's work is its plan.
func (s *Server) handleResize(w http.ResponseWriter, r *http.Request) {
	_, rec, v := s.job(w, r)
	if rec == nil {
		return
	}
	var body api.ForecastChange
	if err := decodeBody(r, &body); err != nil {
		fail(w, http.StatusBadRequest, "body: %v", err)
		return
	}
	ctx := r.Context()
	if err := s.authorize(ctx, r, v, rec.job.Namespace, "patch", "workjobs", rec.job.Name); err != nil {
		fail(w, http.StatusForbidden, "%v", err)
		return
	}
	j, err := s.liveJob(ctx, rec.job.Namespace, rec.job.Name)
	if err != nil {
		fail(w, http.StatusNotFound, "%v", err)
		return
	}
	if j.Spec.Forecast == nil {
		fail(w, http.StatusConflict, "%s is a real job: only a forecast is resized", j.Name)
		return
	}
	patch := client.MergeFrom(j.DeepCopy())
	before := queue.SpecDigest(j)
	f := j.Spec.Forecast
	var did []string
	set := func(name string, to *string, field *string, re *regexp.Regexp) bool {
		if to == nil || *to == *field {
			return true
		}
		if *to != "" && !re.MatchString(*to) {
			fail(w, http.StatusBadRequest, "%s %q: want a decimal number like 12 or 0.5", name, *to)
			return false
		}
		*field = *to
		if *to == "" {
			did = append(did, name+" cleared")
		} else {
			did = append(did, name+" "+*to)
		}
		return true
	}
	if !set("slotHours", body.SlotHours, &f.SlotHours, forecastHours) || !set("sd", body.SD, &f.SD, forecastHours) ||
		!set("unitHours", body.UnitHours, &f.UnitHours, forecastHours) || !set("exists", body.Exists, &f.Exists, forecastChance) {
		return
	}
	if body.Start != nil {
		switch {
		case *body.Start == "" && f.Start != nil:
			f.Start = nil
			did = append(did, "start cleared")
		case *body.Start != "":
			t, err := time.Parse(time.RFC3339, *body.Start)
			if err != nil {
				fail(w, http.StatusBadRequest, "start %q: want an RFC 3339 time", *body.Start)
				return
			}
			if f.Start == nil || !f.Start.Equal(&metav1.Time{Time: t}) {
				f.Start = &metav1.Time{Time: t.UTC()}
				did = append(did, "start "+t.UTC().Format(time.RFC3339))
			}
		}
	}
	if len(did) == 0 {
		// Already so: a retried change writes nothing and signs nothing.
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": j.Name + ": forecast unchanged", "job": s.jobSummary(s.current(), rec)})
		return
	}
	msg := fmt.Sprintf("%s: forecast resized, %s", j.Name, strings.Join(did, ", "))
	s.finishJobControl(w, r, v, rec, j, patch, before, "forecast", "forecast", body, "Resized", msg)
}
