package server

import (
	"bytes"
	"context"
	"math"
	"net/http"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Avalanche-io/c4"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/hud/api"
	c4view "github.com/Avalanche-io/c4-operator/internal/view"
	"github.com/Avalanche-io/c4-operator/work/estimate"
	"github.com/Avalanche-io/c4-operator/work/order"
)

// job finds a job the viewer may see; nil answers 404 (not 403, so a
// name in another project does not leak).
func (s *Server) job(w http.ResponseWriter, r *http.Request) (*model, *jobRec, *api.Viewer) {
	v := s.withViewer(w, r)
	if v == nil {
		return nil, nil, nil
	}
	m := s.current()
	if m == nil {
		fail(w, http.StatusServiceUnavailable, "the HUD is still reading the farm")
		return nil, nil, nil
	}
	rec := m.byKey[r.PathValue("ns")+"/"+r.PathValue("name")]
	if rec == nil || !v.Sees(rec.project) {
		fail(w, http.StatusNotFound, "no job %s/%s", r.PathValue("ns"), r.PathValue("name"))
		return nil, nil, nil
	}
	return m, rec, v
}

func (s *Server) handleJob(w http.ResponseWriter, r *http.Request) {
	m, rec, _ := s.job(w, r)
	if rec == nil {
		return
	}
	writeJSON(w, http.StatusOK, s.jobDetail(m, rec))
}

// jobDetail is GET /jobs/{ns}/{name}: the summary plus the job view's
// layout, per-unit state, progress and the class summary.
func (s *Server) jobDetail(m *model, rec *jobRec) map[string]any {
	sum := s.jobSummary(m, rec)
	out := map[string]any{}
	b, _ := jsonMap(sum)
	for k, v := range b {
		out[k] = v
	}
	out["expansions"] = []any{}
	out["edges"] = []any{}
	out["riskUnits"] = ""
	if rec.plan == nil {
		out["axis"] = map[string]any{"kind": "segment"}
		out["groups"] = []any{}
		out["unitList"] = []any{}
		out["progress"] = map[string]any{}
		out["classSummary"] = map[string]any{}
		return out
	}
	plan := rec.plan
	n := len(plan.Units)
	axis := map[string]any{"kind": plan.Axis.Kind}
	if plan.Axis.Kind == "frame" {
		axis["start"], axis["step"] = plan.Axis.Start, max(1, plan.Axis.Step)
		axis["batch"] = displayBatch(rec)
	}
	out["axis"] = axis
	groupKey, groupLabel := "units", "units"
	switch rec.job.Spec.Class {
	case "render":
		groupKey = rec.job.Spec.Params["pass"]
		if groupKey == "" {
			groupKey = "beauty"
		}
		groupLabel = groupKey
	case "ci":
		groupKey, groupLabel = "test", "test "+rec.job.Spec.Params["target"]
	case "ml":
		groupKey, groupLabel = "ranks", "ranks"
	}
	out["groups"] = []map[string]any{{"key": groupKey, "label": groupLabel, "units": "0-" + strconv.Itoa(n-1)}}

	states := unitStateMap(&rec.job.Status, n)
	attempts := parseAttempts(rec.job.Status.Attempts)
	running := map[int]*corev1.Pod{}
	for i := range m.pods {
		p := &m.pods[i]
		if p.Namespace != rec.job.Namespace || p.Labels[c4v1.LabelJob] != rec.job.Name {
			continue
		}
		if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue
		}
		u, _ := strconv.Atoi(p.Labels[c4v1.LabelUnit])
		running[u] = p
	}
	held := rec.job.Spec.Nudge.Hold || rec.job.Spec.Nudge.HoldStop
	est := estimatorFor(rec)
	var works []api.UnitWork
	runRem := 0.0
	if n <= 5000 {
		works, runRem = unitWorks(m, rec, states, est)
	} else {
		runRem = runningRemainder(m, rec, est)
	}
	if w := sum.Work; w.Basis == api.BasisEstimate {
		w.RunningRemainingSlotSeconds = math.Round(math.Min(runRem, w.RemainingSlotSeconds))
		out["work"] = w
	}
	list := make([]map[string]any, 0, n)
	progress := map[string]any{}
	for i, u := range plan.Units {
		st := states[i]
		if held && st == "queued" {
			st = "held"
		}
		e := map[string]any{"i": i, "label": u.Label, "state": st, "attempts": max(attempts[i], boolInt(st != "queued" && st != "held" && st != "blocked")),
			"group": groupKey, "pos": u.Pos}
		if works != nil {
			e["work"] = works[i]
		}
		if p := running[i]; p != nil {
			e["node"] = p.Spec.NodeName
			if f, ok := m.metrics.nodes[p.Spec.NodeName].first("c4_task_progress", "namespace", p.Namespace, "pod", p.Name); ok {
				// the unit's own progress line (.c4/progress), read by its node
				progress[strconv.Itoa(i)] = map[string]any{"fraction": round3(math.Min(0.99, f)), "measured": true}
			} else if p.Status.StartTime != nil {
				el := m.at.Sub(p.Status.StartTime.Time).Seconds()
				pred := est.Predict(u.Key, u.Pos).Mean
				progress[strconv.Itoa(i)] = map[string]any{"fraction": round3(math.Min(0.99, el/math.Max(1, pred))), "measured": false}
			}
		}
		list = append(list, e)
	}
	if n <= 5000 {
		out["unitList"] = list
	}
	out["progress"] = progress
	if rec.job.Spec.Class == "render" {
		out["costByFrame"] = costByFrame(rec, est)
	}
	out["classSummary"] = s.classSummary(m, rec)
	return out
}

// estimatorFor rebuilds a job's estimator from its measured units, as the
// queue does each tick.
func estimatorFor(rec *jobRec) *estimate.Estimator {
	e := estimate.New(rec.plan.Estimate)
	if rec.plan.Prior != nil && len(rec.plan.Units) > 0 {
		e.SetPrior(rec.plan.Units[0].Key, *rec.plan.Prior)
	}
	for i, ms := range rec.job.Status.UnitMillis {
		if ms > 0 && i < len(rec.plan.Units) {
			u := rec.plan.Units[i]
			e.Observe(u.Key, u.Pos, float64(ms)/1000)
		}
	}
	return e
}

// costByFrame is the cost curve: measured seconds where a frame rendered,
// else the estimate with its band.
func costByFrame(rec *jobRec, e *estimate.Estimator) []map[string]any {
	plan := rec.plan
	out := make([]map[string]any, 0, len(plan.Units))
	for i, u := range plan.Units {
		m := map[string]any{"frame": plan.Axis.Start + u.Pos}
		if i < len(rec.job.Status.UnitMillis) && rec.job.Status.UnitMillis[i] > 0 {
			m["seconds"] = math.Round(float64(rec.job.Status.UnitMillis[i]) / 1000)
		} else {
			p := e.Predict(u.Key, u.Pos)
			m["estimate"] = math.Round(p.Mean)
			m["low"] = math.Round(math.Max(0, p.Mean-p.SD()))
			m["high"] = math.Round(p.Mean + p.SD())
		}
		out = append(out, m)
	}
	return out
}

func (s *Server) classSummary(m *model, rec *jobRec) map[string]any {
	j := &rec.job
	switch j.Spec.Class {
	case "render":
		pass := j.Spec.Params["pass"]
		if pass == "" {
			pass = "beauty"
		}
		return map[string]any{"passes": []string{pass}, "fps": 24}
	case "ml":
		steps, _ := strconv.Atoi(j.Spec.Params["steps"])
		ranks, _ := strconv.Atoi(j.Spec.Params["ranks"])
		var ckpts, yields []map[string]any
		for _, y := range j.Status.Yields {
			ckpts = append(ckpts, map[string]any{"view": y.View, "step": 0, "at": y.At.Time.UTC(), "bytesNew": 0, "bytesDeduped": 0})
			yields = append(yields, map[string]any{"at": y.At.Time.UTC(), "reason": y.Reason})
		}
		if ckpts == nil {
			ckpts = []map[string]any{}
		}
		if yields == nil {
			yields = []map[string]any{}
		}
		data := ""
		for _, in := range j.Spec.Inputs {
			data = in.Materialization
		}
		useful := 0.0
		for _, ms := range j.Status.UnitMillis {
			useful += float64(ms) / 3.6e6 * float64(max(1, ranks))
		}
		return map[string]any{"wrapped": false, "ranks": ranks, "checkpoints": ckpts, "yields": yields,
			"usefulGpuHours": round2(useful), "lostGpuHours": 0, "datasetView": data, "codeView": "",
			"step": 0, "totalSteps": steps}
	case "ci":
		var pipelines []map[string]any
		for i := range m.jobs {
			r := m.jobs[i]
			if r.job.Spec.Class != "ci" || r.job.Namespace != j.Namespace {
				continue
			}
			st := "waiting"
			switch r.job.Status.Phase {
			case "Running":
				st = "running"
			case "Succeeded":
				st = "passed"
			case "Failed":
				st = "failed"
			case "Blocked":
				st = "blocked"
			}
			p := map[string]any{"job": r.key, "stage": "test", "state": st, "queueWaitSeconds": 0, "runSeconds": 0,
				"checkoutSeconds": 0, "cacheOutcome": "warm"}
			if r.job.Status.Started != nil {
				p["queueWaitSeconds"] = math.Max(0, r.job.Status.Started.Sub(r.job.CreationTimestamp.Time).Seconds())
				end := m.at
				if r.job.Status.Finished != nil {
					end = r.job.Status.Finished.Time
					p["timeToGreenSeconds"] = end.Sub(r.job.CreationTimestamp.Time).Seconds()
				}
				p["runSeconds"] = end.Sub(r.job.Status.Started.Time).Seconds()
			}
			pipelines = append(pipelines, p)
		}
		change := map[string]any{"id": j.Spec.Params["change"], "title": j.Annotations["c4.avalanche.io/title"],
			"author": j.Annotations["c4.avalanche.io/owner"], "pipelines": pipelines}
		return map[string]any{"changes": []any{change}, "ttg": map[string]any{"p50": 0, "p95": 0, "targetP50": 300, "targetP95": 600, "samples": []any{}},
			"flaky": []any{}, "mergeQueue": map[string]any{"depth": 0, "p50WaitSeconds": 0, "p95WaitSeconds": 0, "ejectRate": 0,
				"mergesPerHour": 0, "entries": []any{}}, "runnerPools": []any{}}
	}
	return map[string]any{}
}

func unitStateMap(st *c4v1.WorkJobStatus, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = "queued"
	}
	for _, name := range []string{"blocked", "retrying", "running", "failed", "succeeded"} {
		idx, _ := order.DecodeSet(st.UnitStates[name])
		for _, i := range idx {
			if i >= 0 && i < n {
				out[i] = name
			}
		}
	}
	return out
}

func parseAttempts(s string) map[int]int {
	out := map[int]int{}
	for _, pair := range strings.Split(s, ",") {
		a, b, ok := strings.Cut(pair, ":")
		if !ok {
			continue
		}
		i, e1 := strconv.Atoi(a)
		n, e2 := strconv.Atoi(b)
		if e1 == nil && e2 == nil {
			out[i] = n
		}
	}
	return out
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// handleUnit is GET /jobs/{ns}/{name}/units/{i}: the unit's current pod,
// its log tail and its events.
func (s *Server) handleUnit(w http.ResponseWriter, r *http.Request) {
	m, rec, _ := s.job(w, r)
	if rec == nil {
		return
	}
	i, err := strconv.Atoi(r.PathValue("i"))
	if err != nil || rec.plan == nil || i < 0 || i >= len(rec.plan.Units) {
		fail(w, http.StatusNotFound, "no unit %s", r.PathValue("i"))
		return
	}
	u := rec.plan.Units[i]
	states := unitStateMap(&rec.job.Status, len(rec.plan.Units))
	est := estimatorFor(rec)
	out := map[string]any{"i": i, "label": u.Label, "state": states[i], "attempts": []map[string]any{},
		"estimateSeconds": math.Round(est.Predict(u.Key, u.Pos).Mean), "logTail": []string{}, "events": []map[string]any{}}
	if i < len(rec.job.Status.UnitMillis) && rec.job.Status.UnitMillis[i] > 0 {
		out["durationSeconds"] = math.Round(float64(rec.job.Status.UnitMillis[i]) / 1000)
	}
	if ix, err := s.index(r.Context(), rec); err == nil && ix != nil && ix.Units[i] != "" {
		if t, err := s.trace(r.Context(), m, rec, ix.Units[i], ""); err == nil {
			p := map[string]any{"record": t.Record, "verified": t.Verified, "signer": t.Signer, "problem": t.Problem}
			if st := t.Statement; st != nil {
				p["node"] = st.Predicate.Runtime.Node
				p["images"] = st.Predicate.Runtime.Images
				p["inputs"] = st.Predicate.Inputs
				p["files"] = st.Predicate.Output.FilesListed
				p["attempt"] = st.Predicate.Invocation.Attempt
				if len(st.Predicate.AI) > 0 {
					p["ai"] = st.Predicate.AI
				}
			}
			out["provenance"] = p
		}
	}
	var pod *corev1.Pod
	for k := range m.pods {
		p := &m.pods[k]
		if p.Namespace == rec.job.Namespace && p.Labels[c4v1.LabelJob] == rec.job.Name && p.Labels[c4v1.LabelUnit] == strconv.Itoa(i) {
			pod = p
		}
	}
	if pod != nil {
		att := map[string]any{"n": pod.Annotations[c4v1.AnnotationAttempt], "node": pod.Spec.NodeName}
		if pod.Status.StartTime != nil {
			att["start"] = pod.Status.StartTime.Time.UTC()
			out["durationSeconds"] = math.Round(m.at.Sub(pod.Status.StartTime.Time).Seconds())
		}
		out["attempts"] = []map[string]any{att}
		if cs := pod.Annotations[c4v1.AnnotationOutputChangeset]; cs != "" {
			out["outputChangeset"] = cs
		}
		if s.Kube != nil {
			ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
			defer cancel()
			tail := int64(20)
			if b, err := s.Kube.CoreV1().Pods(pod.Namespace).GetLogs(pod.Name, &corev1.PodLogOptions{TailLines: &tail}).DoRaw(ctx); err == nil {
				out["logTail"] = strings.Split(strings.TrimRight(string(b), "\n"), "\n")
			}
			if evs, err := s.Kube.CoreV1().Events(pod.Namespace).List(ctx, metav1.ListOptions{FieldSelector: "involvedObject.name=" + pod.Name}); err == nil {
				var list []map[string]any
				for _, e := range evs.Items {
					at := e.LastTimestamp.Time
					if at.IsZero() {
						at = e.EventTime.Time
					}
					list = append(list, map[string]any{"type": e.Type, "reason": e.Reason, "message": e.Message, "at": at.UTC()})
				}
				sort.Slice(list, func(a, b int) bool { return list[a]["at"].(time.Time).Before(list[b]["at"].(time.Time)) })
				if list != nil {
					out["events"] = list
				}
			}
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// previewName matches a render's preview proxies: preview/NNNN.png.
var previewName = regexp.MustCompile(`(^|/)preview/(\d+)\.png$`)

// handlePreview is GET /jobs/{ns}/{name}/preview: one entry per frame,
// with the URL of its preview proxy where the job's output view has one.
func (s *Server) handlePreview(w http.ResponseWriter, r *http.Request) {
	_, rec, _ := s.job(w, r)
	if rec == nil {
		return
	}
	out := map[string]any{"fps": 24, "frames": []map[string]any{}}
	if rec.plan == nil || rec.plan.Axis.Kind != "frame" {
		writeJSON(w, http.StatusOK, out)
		return
	}
	urls := map[int]string{}
	if o := rec.job.Status.Output; o != nil && o.View != "" && s.Stores != nil {
		if found, err := s.previews(r.Context(), rec.project, o.View); err == nil {
			urls = found
		} else {
			s.Log.Error(err, "previews", "job", rec.key)
		}
	}
	var frames []map[string]any
	for _, u := range rec.plan.Units {
		f := rec.plan.Axis.Start + u.Pos
		fr := map[string]any{"frame": f}
		if id, ok := urls[f]; ok {
			fr["url"] = "/api/v1/objects/" + rec.project + "/" + id
		}
		frames = append(frames, fr)
	}
	out["frames"] = frames
	writeJSON(w, http.StatusOK, out)
}

// previews indexes the preview proxies of an output view by frame. The
// index is cached by view ID: views never change.
func (s *Server) previews(ctx context.Context, project, viewID string) (map[int]string, error) {
	s.mu.RLock()
	if hit, ok := s.previewCache[viewID]; ok {
		s.mu.RUnlock()
		return hit, nil
	}
	s.mu.RUnlock()
	st, err := s.Stores(ctx, project)
	if err != nil || st == nil {
		return nil, err
	}
	id, err := c4.Parse(viewID)
	if err != nil {
		return nil, err
	}
	v, err := c4view.Load(ctx, id, c4view.StoreRecords(st), 16)
	if err != nil {
		return nil, err
	}
	out := map[int]string{}
	for _, f := range v.Files {
		mm := previewName.FindStringSubmatch(f.Path)
		if mm == nil {
			continue
		}
		n, _ := strconv.Atoi(mm[2])
		out[n] = f.ID.String()
	}
	s.mu.Lock()
	if s.previewCache == nil {
		s.previewCache = map[string]map[int]string{}
	}
	s.previewCache[viewID] = out
	s.mu.Unlock()
	return out, nil
}

// handleObject is GET /objects/{project}/{id}: an object's bytes, for
// viewers of the project.
func (s *Server) handleObject(w http.ResponseWriter, r *http.Request) {
	v := s.withViewer(w, r)
	if v == nil {
		return
	}
	project := r.PathValue("project")
	id, err := c4.Parse(r.PathValue("id"))
	if err != nil || !v.Sees(project) || s.Stores == nil {
		fail(w, http.StatusNotFound, "no such object")
		return
	}
	st, err := s.Stores(r.Context(), project)
	if err != nil || st == nil {
		fail(w, http.StatusNotFound, "no such object")
		return
	}
	data, err := c4view.ReadObject(st, id)
	if err != nil {
		fail(w, http.StatusNotFound, "no such object")
		return
	}
	ct := http.DetectContentType(data)
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	http.ServeContent(w, r, path.Base(id.String()), time.Time{}, bytes.NewReader(data))
}

// handleGraph is GET /graph: jobs and their dependencies.
func (s *Server) handleGraph(w http.ResponseWriter, r *http.Request) {
	v := s.withViewer(w, r)
	if v == nil {
		return
	}
	m := s.current()
	if m == nil {
		fail(w, http.StatusServiceUnavailable, "the HUD is still reading the farm")
		return
	}
	nodes := []map[string]any{}
	edges := []map[string]any{}
	for _, rec := range m.jobs {
		if !v.Sees(rec.project) {
			continue
		}
		j := &rec.job
		n := map[string]any{"id": rec.key, "class": j.Spec.Class, "driver": j.Spec.Class, "phase": j.Status.Phase, "critical": false}
		if shot := j.Labels[LabelShot]; shot != "" {
			n["shot"] = shot
		}
		if e := j.Status.Estimate; e != nil {
			if e.ETA90 != nil {
				n["etaP90"] = e.ETA90.Time.UTC()
			}
			n["slackSeconds"] = e.SlackSeconds
		}
		if j.Status.Deadline != nil {
			n["deadline"] = j.Status.Deadline.Time.UTC()
		}
		nodes = append(nodes, n)
		for _, d := range j.Spec.DependsOn {
			e := map[string]any{"from": j.Namespace + "/" + d, "to": rec.key}
			if up := m.byKey[j.Namespace+"/"+d]; up != nil && up.job.Status.Output != nil {
				e["view"] = up.job.Status.Output.View
			}
			edges = append(edges, e)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"nodes": nodes, "edges": edges})
}

// handleDiff is GET /views/{project}/diff?from=ID&to=ID[&limit=N]: the
// files that differ between two views of a project, read from records
// only (no checkout). It is the query a CI system asks to choose targets.
func (s *Server) handleDiff(w http.ResponseWriter, r *http.Request) {
	v := s.withViewer(w, r)
	if v == nil {
		return
	}
	project := r.PathValue("project")
	if !v.Sees(project) || s.Stores == nil {
		fail(w, http.StatusNotFound, "no project %s", project)
		return
	}
	from, err1 := c4.Parse(r.URL.Query().Get("from"))
	to, err2 := c4.Parse(r.URL.Query().Get("to"))
	if err1 != nil || err2 != nil {
		fail(w, http.StatusBadRequest, "from and to must be view IDs (90 characters starting c4)")
		return
	}
	limit := 1000
	if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && n > 0 && n <= 100000 {
		limit = n
	}
	st, err := s.Stores(r.Context(), project)
	if err != nil || st == nil {
		fail(w, http.StatusServiceUnavailable, "project %s has no reachable store", project)
		return
	}
	start := time.Now()
	changes, truncated, err := c4view.ChangedFiles(r.Context(), from, to, c4view.StoreRecords(st), limit)
	if err != nil {
		fail(w, http.StatusNotFound, "%v", err)
		return
	}
	counts := map[string]int{"added": 0, "modified": 0, "removed": 0}
	var bytesAdded int64
	for _, c := range changes {
		counts[c.Op]++
		if c.Op != "removed" {
			bytesAdded += c.Size
		}
	}
	if changes == nil {
		changes = []c4view.FileChange{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"from": from.String(), "to": to.String(), "changes": changes,
		"counts": counts, "bytesAddedOrModified": bytesAdded, "truncated": truncated,
		"seconds": round3(time.Since(start).Seconds())})
}
