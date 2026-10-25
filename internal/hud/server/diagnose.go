package server

import (
	"bytes"
	"net/http"
	"strconv"
	"time"

	"github.com/Avalanche-io/c4-operator/hud/api"
	"github.com/Avalanche-io/c4-operator/internal/telemetry/bundle"
	"github.com/Avalanche-io/c4-operator/internal/telemetry/ring"
)

// mayDiagnose is why the viewer may not take a diagnostic bundle, or "":
// the farm admin, or the single-person farm's installer, who holds every
// role there. A unit never does, whatever its grant. A bundle holds no
// project's names, but it is the whole farm's shape, so it takes the
// role that sees the whole farm.
func mayDiagnose(v *api.Viewer, singlePerson string) string {
	switch {
	case v.Unit != nil:
		return "a unit does not take diagnostic bundles; the farm admin does"
	case v.Role == "farm-admin", singlePerson != "" && v.User == singlePerson:
		return ""
	}
	return "a diagnostic bundle takes the farm admin role"
}

// handleDiagnose writes a diagnostic bundle (DESIGN.md section 6.9): GET
// /api/v1/diagnose[?window=30m] answers a gzipped tar of the farm's
// redacted decisions within window of the latest (all the ring keeps
// without one), named c4-diagnose-<id>.tgz. Nothing is written unless
// the whole bundle is: without the farm key, or if any record fails to
// encode, the answer is an error and no bytes of a bundle.
func (s *Server) handleDiagnose(w http.ResponseWriter, r *http.Request) {
	v := s.withViewer(w, r)
	if v == nil {
		return
	}
	if why := mayDiagnose(v, s.singlePersonFlag()); why != "" {
		fail(w, http.StatusForbidden, "%s", why)
		return
	}
	var window time.Duration
	if q := r.URL.Query().Get("window"); q != "" {
		d, err := time.ParseDuration(q)
		if err != nil || d <= 0 {
			fail(w, http.StatusBadRequest, "window %q: a duration such as 30m or 2h", q)
			return
		}
		window = d
	}
	if s.Telemetry == nil {
		fail(w, http.StatusServiceUnavailable, "this farm runs no work queue, so it has no decisions to put in a bundle")
		return
	}
	snaps, dropped, err := s.Telemetry.Snapshots(r.Context(), window)
	if err != nil {
		fail(w, http.StatusServiceUnavailable, "the farm's telemetry key could not be read, so no bundle was written: %v", err)
		return
	}
	if len(snaps) == 0 {
		fail(w, http.StatusServiceUnavailable, "the queue has not decided anything yet; ask again after its first tick")
		return
	}
	var buf bytes.Buffer
	id, err := bundle.Write(&buf, bundle.Bundle{Snapshots: snaps, Dropped: dropped})
	if err != nil {
		fail(w, http.StatusInternalServerError, "no bundle was written: %v", err)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "application/gzip")
	h.Set("Content-Disposition", `attachment; filename="`+bundle.FileName(id)+`"`)
	h.Set("Content-Length", strconv.Itoa(buf.Len()))
	h.Set("Cache-Control", "no-store")
	h.Set("X-C4-Bundle", id.String())
	if s.Telemetry.Tier() == ring.TierOff {
		h.Set("X-C4-Bundle-Tier", "present")
	} else {
		h.Set("X-C4-Bundle-Tier", "ring")
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buf.Bytes())
}
