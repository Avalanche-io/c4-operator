package server

import (
	"bytes"
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/Avalanche-io/c4"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Avalanche-io/c4-operator/hud/api"
	"github.com/Avalanche-io/c4-operator/internal/provenance"
	"github.com/Avalanche-io/c4-operator/internal/telemetry"
	"github.com/Avalanche-io/c4-operator/internal/telemetry/farmkey"
	"github.com/Avalanche-io/c4-operator/internal/telemetry/ring"
)

// The farm's telemetry controls (DESIGN.md section 6.9): its status, a
// pseudonym resolved on the farm, the key rotated and the tier set. All
// are the farm admin's, as a diagnostic bundle is (mayDiagnose).

// direct says why the request is not a person acting as themselves, or
// "": not through MCP or an agent, not a unit, not a tracker's sync. The
// tier is raised, and a pseudonym resolved, only this way.
func (s *Server) direct(r *http.Request, v *api.Viewer) string {
	switch {
	case v.Unit != nil:
		return "a unit"
	case via(r) == "mcp" || r.Header.Get("X-C4-Agent") != "":
		return "an agent (MCP)"
	case s.isSync(v):
		return "a tracker's sync (a relayed request)"
	}
	return ""
}

// telemetryAdmin answers 403 or 503 and returns false unless the viewer
// may use the telemetry controls and the farm has a ring.
func (s *Server) telemetryAdmin(w http.ResponseWriter, v *api.Viewer) bool {
	if why := mayDiagnose(v, s.singlePersonFlag()); why != "" {
		fail(w, http.StatusForbidden, "%s", "the farm's telemetry takes the farm admin role")
		return false
	}
	if s.Telemetry == nil {
		fail(w, http.StatusServiceUnavailable, "this farm runs no work queue, so it keeps no telemetry")
		return false
	}
	return true
}

func (s *Server) handleTelemetry(w http.ResponseWriter, r *http.Request) {
	v := s.withViewer(w, r)
	if v == nil || !s.telemetryAdmin(w, v) {
		return
	}
	key, err := s.Telemetry.Key(r.Context())
	if err != nil {
		fail(w, http.StatusServiceUnavailable, "the farm's telemetry key could not be read: %v", err)
		return
	}
	st := s.Telemetry.Stats()
	out := api.Telemetry{Tier: ring.TierName(s.Telemetry.Tier()), KillSwitch: ring.KillSwitch(), Epoch: key.Epoch,
		Ring: api.TelemetryRing{Window: st.Window.String(), MaxBytes: st.MaxBytes, Bytes: st.Bytes, Snapshots: st.Snapshots, Dropped: st.Dropped}}
	if s.Kube != nil {
		if _, rec, _, err := farmkey.LoadTier(r.Context(), s.Kube, s.Namespace); err == nil {
			out.Record = rec
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// handleTelemetryResolve maps a pseudonym back to the name it stands for,
// among the names the farm holds now, under the current key. The name
// goes only to a person signed in as themselves: never through MCP.
func (s *Server) handleTelemetryResolve(w http.ResponseWriter, r *http.Request) {
	v := s.withViewer(w, r)
	if v == nil || !s.telemetryAdmin(w, v) {
		return
	}
	if who := s.direct(r, v); who != "" {
		fail(w, http.StatusForbidden, "a pseudonym is resolved only for a person signed in as themselves, in the HUD or with c4 farm telemetry; not for %s", who)
		return
	}
	var body api.TelemetryResolve
	if err := decodeBody(r, &body); err != nil {
		failBody(w, err)
		return
	}
	p, err := telemetry.ParsePseudonym(body.Pseudonym)
	if err != nil {
		fail(w, http.StatusBadRequest, "%q is not a pseudonym (j.…, p.…, o.…): %v", body.Pseudonym, err)
		return
	}
	key, err := s.Telemetry.Key(r.Context())
	if err != nil {
		fail(w, http.StatusServiceUnavailable, "the farm's telemetry key could not be read: %v", err)
		return
	}
	names := map[telemetry.Kind][]string{}
	if snap, ok := s.Telemetry.Present(); ok {
		names = telemetry.NamesIn(snap)
	}
	if m := s.current(); m != nil {
		for _, j := range m.jobs {
			names[telemetry.KindJob] = append(names[telemetry.KindJob], j.key)
		}
		for _, p := range m.projects {
			names[telemetry.KindProject] = append(names[telemetry.KindProject], p.Name)
		}
	}
	name, ok := telemetry.Resolve(key, p, names[p.Kind])
	if !ok {
		fail(w, http.StatusNotFound, "no %s the farm holds now has pseudonym %s under the current key (epoch %d); one from an earlier epoch, or of a %s since gone, cannot be resolved",
			telemetry.KindName(p.Kind), body.Pseudonym, key.Epoch, telemetry.KindName(p.Kind))
		return
	}
	writeJSON(w, http.StatusOK, api.TelemetryName{Kind: telemetry.KindName(p.Kind), Name: name})
}

// handleTelemetryTier sets the farm's tier, a or b. Raising it takes a
// person acting directly; lowering it is accepted by any route, because
// turning telemetry down can never leak (DESIGN.md section 6.9).
func (s *Server) handleTelemetryTier(w http.ResponseWriter, r *http.Request) {
	v := s.withViewer(w, r)
	if v == nil || !s.telemetryAdmin(w, v) {
		return
	}
	var body api.TelemetryTier
	if err := decodeBody(r, &body); err != nil {
		failBody(w, err)
		return
	}
	to, ok := ring.ParseTier(body.Tier)
	if !ok {
		fail(w, http.StatusBadRequest, "tier %q: a (off) or b (diagnostic bundles); the farm's collector and the weekly report are not built yet", body.Tier)
		return
	}
	from := s.Telemetry.Tier()
	if to == from {
		writeJSON(w, http.StatusOK, api.TelemetryChange{Tier: ring.TierName(to), Message: "the farm's telemetry is already at tier " + ring.TierName(to)})
		return
	}
	custody := "any"
	if to > from {
		if who := s.direct(r, v); who != "" {
			fail(w, http.StatusForbidden, "raising the farm's telemetry takes a person signed in as themselves, in the HUD or with c4 farm telemetry; not %s. Lowering it is accepted from anywhere", who)
			return
		}
		if ring.KillSwitch() {
			fail(w, http.StatusConflict, "C4_TELEMETRY=off is set in the manager's environment, which holds the farm's telemetry at tier a")
			return
		}
		custody = "direct"
	}
	change := map[string]any{"from": ring.TierName(from), "to": ring.TierName(to), "custody": custody}
	id, rec, ok := s.signFarm(w, r, v, "telemetry-tier", "telemetry:tier", change)
	if !ok {
		return
	}
	if s.Kube != nil {
		if err := farmkey.SaveTier(r.Context(), s.Kube, s.Namespace, ring.TierName(to), id, rec); err != nil {
			fail(w, http.StatusInternalServerError, "the tier was not changed: %v", err)
			return
		}
	}
	got := s.Telemetry.SetTier(to)
	msg := "the farm's telemetry is now at tier " + ring.TierName(got) + map[ring.Tier]string{
		ring.TierOff:     " (off): a diagnostic bundle holds the present moment, and the decisions kept are gone",
		ring.TierBundles: " (diagnostic bundles): the farm keeps its recent decisions, redacted, for a bundle; nothing leaves the farm"}[got]
	s.bookTelemetry(r.Context(), r, v, "telemetry-tier", msg, id)
	writeJSON(w, http.StatusOK, api.TelemetryChange{Tier: ring.TierName(got), Record: id, Message: msg})
}

// handleTelemetryRotate rotates the farm key: a new epoch, whose
// pseudonyms are unrelated to the last; the ring's records go with the
// old key.
func (s *Server) handleTelemetryRotate(w http.ResponseWriter, r *http.Request) {
	v := s.withViewer(w, r)
	if v == nil || !s.telemetryAdmin(w, v) {
		return
	}
	if s.Kube == nil {
		fail(w, http.StatusServiceUnavailable, "this HUD has no cluster client to rotate the key with")
		return
	}
	old, err := s.Telemetry.Key(r.Context())
	if err != nil {
		fail(w, http.StatusServiceUnavailable, "the farm's telemetry key could not be read: %v", err)
		return
	}
	id, _, ok := s.signFarm(w, r, v, "telemetry-rotate", "telemetry:key", map[string]any{"fromEpoch": old.Epoch, "toEpoch": old.Epoch + 1})
	if !ok {
		return
	}
	key, err := farmkey.Rotate(r.Context(), s.Kube, s.Namespace)
	if err != nil {
		fail(w, http.StatusInternalServerError, "the key was not rotated: %v", err)
		return
	}
	s.Telemetry.Rekey()
	msg := "the farm's telemetry key is now in epoch " + strconv.FormatUint(uint64(key.Epoch), 10) + ": pseudonyms from now on are unrelated to earlier ones, and the decisions kept under the old key are gone"
	s.bookTelemetry(r.Context(), r, v, "telemetry-rotate", msg, id)
	writeJSON(w, http.StatusOK, api.TelemetryChange{Epoch: key.Epoch, Record: id, Message: msg})
}

// signFarm signs a farm-level control record and keeps it in every
// project's store. It returns the record and its ID; without a signer
// (outside development) it answers 503 and false, since every telemetry
// change is a signed record.
func (s *Server) signFarm(w http.ResponseWriter, r *http.Request, v *api.Viewer, action, target string, change any) (string, []byte, bool) {
	if s.Signer == nil {
		if s.DevViewers == nil {
			fail(w, http.StatusServiceUnavailable, "a telemetry change is a signed record, and this HUD has no signer")
			return "", nil, false
		}
		return "", nil, true
	}
	ctx := r.Context()
	pred := controlPredicate{Actor: v.User, Authenticated: s.authenticatedBy(r, v), Via: via(r), Agent: claimedAgent(r),
		Authorization: "the farm admin role (" + v.Role + ")", Action: action, Target: target, Change: change}
	if s.direct(r, v) == "" {
		pred.Custody = "direct"
	}
	rec, err := provenance.SignStatement(ctx, s.Signer, provenance.PredicateControl,
		[]provenance.Subject{{Name: target, Digest: map[string]string{"sha256": sha256JSON(change)}}}, pred, time.Now())
	if err != nil {
		fail(w, http.StatusServiceUnavailable, "control record: %v", err)
		return "", nil, false
	}
	id := c4.Identify(bytes.NewReader(rec))
	if m := s.current(); m != nil && s.Stores != nil {
		for _, p := range m.projects {
			if st, err := s.Stores(ctx, p.Name); err == nil && st != nil {
				_ = st.Put(ctx, id, bytes.NewReader(rec), int64(len(rec)))
			}
		}
	}
	return id.String(), rec, true
}

// bookTelemetry books a telemetry change on the farm's setting.
func (s *Server) bookTelemetry(ctx context.Context, r *http.Request, v *api.Viewer, kind, msg, record string) {
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: farmkey.Setting, Namespace: s.Namespace}}
	s.record(ctx, cm, "ConfigMap", v.User, "Telemetry", msg, bookNote{actor: actorOf(r, v), kind: kind, record: record})
}
