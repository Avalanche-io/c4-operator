package server

// Submitting work through the HUD: objects into a project's store, a tree
// of them recorded as a view, and a WorkJob (with the Materializations of
// its input views) created for the signed-in viewer. These are the calls
// `c4 farm submit` makes and MCP's c4_submit_job wraps. Each is checked
// against the viewer's Kubernetes permissions like every control, and the
// bytes go through the manager's own store access, so no store credential
// leaves the cluster.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Avalanche-io/c4"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/hud/api"
	"github.com/Avalanche-io/c4-operator/internal/jobspec"
	"github.com/Avalanche-io/c4-operator/internal/storeurl"
	c4view "github.com/Avalanche-io/c4-operator/internal/view"
)

// DryRunFunc plans a job that does not exist yet and returns what the
// farm expects of it, writing nothing. The manager wires it to the work
// queue; a nil DryRun answers dry runs with 503.
type DryRunFunc func(ctx context.Context, job *c4v1.WorkJob, project string) (api.SubmitEstimate, error)

// ErrNotPlanned is what a DryRunFunc returns (wrapped) when the queue has
// not planned the farm yet; the HUD answers 503 and the client retries.
var ErrNotPlanned = errors.New("the queue has not planned the farm yet")

// Limits on submissions.
const (
	maxListIDs      = 100000
	maxViewFiles    = 1000000
	headWorkers     = 16
	defaultMaxBytes = 64 << 30
)

// project resolves the project (and the namespace jobs go in) of a
// request that names either, and checks the viewer may see it.
func (s *Server) project(m *model, v *api.Viewer, project, ns string) (string, string, error) {
	switch {
	case ns != "":
		p, ok := m.nsProject[ns]
		if !ok || (project != "" && project != p) {
			return "", "", fmt.Errorf("namespace %s belongs to no project you can see", ns)
		}
		project = p
	case project != "":
		for _, p := range m.projects {
			if p.Name == project && len(p.Spec.Namespaces) > 0 {
				ns = p.Spec.Namespaces[0]
			}
		}
		if ns == "" {
			return "", "", fmt.Errorf("no project %s", project)
		}
	default:
		if len(v.Projects) != 1 {
			return "", "", errors.New("name a project: you can see more than one")
		}
		return s.project(m, v, v.Projects[0], "")
	}
	if !v.Sees(project) {
		return "", "", fmt.Errorf("no project %s", project)
	}
	return project, ns, nil
}

// writerStore checks that the viewer may add work to a project (create
// materializations in its namespace) and returns the project's store.
func (s *Server) writerStore(w http.ResponseWriter, r *http.Request) (*api.Viewer, *storeurl.Sharded, string) {
	v := s.withViewer(w, r)
	if v == nil {
		return nil, nil, ""
	}
	m := s.current()
	if m == nil {
		fail(w, http.StatusServiceUnavailable, "the HUD is still reading the farm")
		return nil, nil, ""
	}
	if host, ok := strings.CutPrefix(v.User, HostAccountPrefix); ok {
		// A host runner stores its units' documents and session views as
		// its own account, for exactly the projects whose units it runs.
		project := r.PathValue("project")
		if !hostRuns(m, host, project) {
			fail(w, http.StatusForbidden, "host %s runs no unit of project %s", host, project)
			return nil, nil, ""
		}
		st, err := s.Stores(r.Context(), project)
		if err != nil || st == nil {
			fail(w, http.StatusServiceUnavailable, "the project's store is not available: %v", err)
			return nil, nil, ""
		}
		return v, st, project
	}
	project, ns, err := s.project(m, v, r.PathValue("project"), "")
	if err != nil || s.Stores == nil {
		fail(w, http.StatusNotFound, "no project %s", r.PathValue("project"))
		return nil, nil, ""
	}
	if err := s.authorize(r.Context(), r, v, ns, "create", "materializations", ""); err != nil {
		fail(w, http.StatusForbidden, "%v", err)
		return nil, nil, ""
	}
	st, err := s.Stores(r.Context(), project)
	if err != nil || st == nil {
		fail(w, http.StatusServiceUnavailable, "the project's store is not available: %v", err)
		return nil, nil, ""
	}
	return v, st, project
}

// HostAccountPrefix names host runners' service accounts:
// c4-hosts/<node> (config/rbac/host-runner.yaml).
const HostAccountPrefix = "system:serviceaccount:c4-hosts:"

// hostRuns reports whether a unit pod of project is bound to node, the
// host runner's own: the only projects a host may write objects for.
func hostRuns(m *model, node, project string) bool {
	if node == "" || project == "" {
		return false
	}
	for i := range m.pods {
		p := &m.pods[i]
		if p.Spec.NodeName == node && m.nsProject[p.Namespace] == project {
			return true
		}
	}
	return false
}

// missing returns the IDs the store lacks, in input order.
func missing(ctx context.Context, st *storeurl.Sharded, ids []c4.ID) ([]c4.ID, error) {
	has := make([]bool, len(ids))
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		first error
		next  = make(chan int)
	)
	for range min(headWorkers, max(len(ids), 1)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				_, err := st.Head(ctx, ids[i])
				switch {
				case err == nil:
					has[i] = true
				case errors.Is(err, storeurl.ErrNotFound):
				default:
					mu.Lock()
					first = errors.Join(first, err)
					mu.Unlock()
				}
			}
		}()
	}
	for i := range ids {
		next <- i
	}
	close(next)
	wg.Wait()
	if first != nil {
		return nil, first
	}
	var out []c4.ID
	for i, id := range ids {
		if !has[i] {
			out = append(out, id)
		}
	}
	return out, nil
}

func parseIDs(in []string) ([]c4.ID, error) {
	seen := map[c4.ID]bool{}
	out := make([]c4.ID, 0, len(in))
	for _, x := range in {
		id, err := c4.Parse(x)
		if err != nil {
			return nil, fmt.Errorf("%q is not a C4 ID", x)
		}
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out, nil
}

func idStrings(ids []c4.ID) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = id.String()
	}
	return out
}

// handleMissing is POST /objects/{project}/missing.
func (s *Server) handleMissing(w http.ResponseWriter, r *http.Request) {
	_, st, _ := s.writerStore(w, r)
	if st == nil {
		return
	}
	var body api.MissingRequest
	if err := decodeBodyUpTo(r, &body, listBody); err != nil {
		failBody(w, err)
		return
	}
	if len(body.IDs) > maxListIDs {
		fail(w, http.StatusRequestEntityTooLarge, "at most %d IDs per request", maxListIDs)
		return
	}
	ids, err := parseIDs(body.IDs)
	if err != nil {
		fail(w, http.StatusBadRequest, "%v", err)
		return
	}
	miss, err := missing(r.Context(), st, ids)
	if err != nil {
		fail(w, http.StatusBadGateway, "store: %v", err)
		return
	}
	writeJSON(w, http.StatusOK, api.MissingResponse{Missing: append([]string{}, idStrings(miss)...)})
}

// handlePutObject is PUT /objects/{project}/{id}: the body is stored if its
// C4 ID is {id}. It is spooled to disk and identified before anything is
// written, so the store never holds bytes under the wrong name.
func (s *Server) handlePutObject(w http.ResponseWriter, r *http.Request) {
	id, err := c4.Parse(r.PathValue("id"))
	if err != nil {
		fail(w, http.StatusBadRequest, "%q is not a C4 ID", r.PathValue("id"))
		return
	}
	_, st, _ := s.writerStore(w, r)
	if st == nil {
		return
	}
	ctx := r.Context()
	if _, err := st.Head(ctx, id); err == nil {
		io.Copy(io.Discard, r.Body)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id.String(), "stored": false})
		return
	}
	limit := s.MaxObjectBytes
	if limit <= 0 {
		limit = defaultMaxBytes
	}
	tmp, err := os.CreateTemp(s.SpoolDir, "c4-upload-")
	if err != nil {
		fail(w, http.StatusInsufficientStorage, "spool: %v", err)
		return
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	n, err := io.Copy(tmp, http.MaxBytesReader(w, r.Body, limit))
	if err != nil {
		fail(w, http.StatusBadRequest, "upload: %v", err)
		return
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		fail(w, http.StatusInternalServerError, "%v", err)
		return
	}
	if got := c4.Identify(tmp); got != id {
		fail(w, http.StatusBadRequest, "the body's C4 ID is %s, not %s", got, id)
		return
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		fail(w, http.StatusInternalServerError, "%v", err)
		return
	}
	if err := st.Put(ctx, id, tmp, n); err != nil {
		fail(w, http.StatusBadGateway, "store: %v", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id.String(), "stored": true, "bytes": n})
}

// cleanViewPath accepts a relative, slash-separated file path.
func cleanViewPath(p string) (string, error) {
	if p == "" || strings.HasPrefix(p, "/") || strings.HasSuffix(p, "/") || strings.Contains(p, "\\") || path.Clean(p) != p ||
		p == ".." || strings.HasPrefix(p, "../") {
		return "", fmt.Errorf("%q is not a clean relative file path", p)
	}
	return p, nil
}

// handlePostView is POST /views/{project}: record a tree of stored objects
// as a view. Answers 409 with the missing IDs if any object is not stored.
func (s *Server) handlePostView(w http.ResponseWriter, r *http.Request) {
	_, st, _ := s.writerStore(w, r)
	if st == nil {
		return
	}
	var body api.ViewRequest
	if err := decodeBodyUpTo(r, &body, viewBody); err != nil {
		failBody(w, err)
		return
	}
	if n := len(body.Files) + len(body.Dirs) + len(body.Links); n == 0 || n > maxViewFiles {
		fail(w, http.StatusBadRequest, "a view needs 1 to %d files, empty directories and links", maxViewFiles)
		return
	}
	root := c4view.NewNode("")
	seen := map[string]bool{}
	var ids []string
	var bytes int64
	for _, f := range body.Files {
		p, err := cleanViewPath(f.Path)
		if err != nil {
			fail(w, http.StatusBadRequest, "%v", err)
			return
		}
		if seen[p] {
			fail(w, http.StatusBadRequest, "%s is listed twice", p)
			return
		}
		seen[p] = true
		id, err := c4.Parse(f.ID)
		if err != nil || f.Size < 0 {
			fail(w, http.StatusBadRequest, "%s: a C4 ID and a size are required", p)
			return
		}
		dir, name := c4view.SplitPath(p)
		parent := root.Dir(dir)
		if _, clash := parent.Dirs[name+"/"]; clash {
			fail(w, http.StatusBadRequest, "%s is both a file and a directory", p)
			return
		}
		parent.Files[name] = c4view.FileEntry(name, f.Size, id, f.Exec)
		ids = append(ids, f.ID)
		bytes += f.Size
	}
	for _, l := range body.Links {
		p, err := cleanViewPath(l.Path)
		if err != nil {
			fail(w, http.StatusBadRequest, "%v", err)
			return
		}
		if seen[p] {
			fail(w, http.StatusBadRequest, "%s is listed twice", p)
			return
		}
		seen[p] = true
		if l.Target == "" || strings.ContainsRune(l.Target, 0) {
			fail(w, http.StatusBadRequest, "%s: a link needs a target", p)
			return
		}
		dir, name := c4view.SplitPath(p)
		root.Dir(dir).Files[name] = c4view.SymlinkEntry(name, l.Target)
	}
	for _, d := range body.Dirs {
		p, err := cleanViewPath(strings.TrimSuffix(d, "/"))
		if err != nil {
			fail(w, http.StatusBadRequest, "%v", err)
			return
		}
		root.Dir(p + "/")
	}
	// a file named like a directory that a later path created
	if err := root.Walk("", func(dir string, n *c4view.Node) error {
		for name := range n.Files {
			if _, ok := n.Dirs[name+"/"]; ok {
				return fmt.Errorf("%s%s is both a file and a directory", dir, name)
			}
		}
		return nil
	}); err != nil {
		fail(w, http.StatusBadRequest, "%v", err)
		return
	}
	uniq, _ := parseIDs(ids)
	ctx := r.Context()
	miss, err := missing(ctx, st, uniq)
	if err != nil {
		fail(w, http.StatusBadGateway, "store: %v", err)
		return
	}
	if len(miss) > 0 {
		writeJSON(w, http.StatusConflict, api.ViewResponse{Files: len(body.Files), Bytes: bytes, Missing: idStrings(miss)})
		return
	}
	v := c4view.FromNode(root)
	for _, rec := range v.Records() {
		if _, err := st.PutBytes(ctx, rec.Data); err != nil {
			fail(w, http.StatusBadGateway, "store: %v", err)
			return
		}
	}
	writeJSON(w, http.StatusOK, api.ViewResponse{View: v.ID.String(), Files: len(body.Files), Bytes: bytes})
}

// handleViewFiles is GET /views/{project}/{id}/files[?path=prefix]: the
// files of a view, for `c4 farm get`. Bytes come from GET /objects.
func (s *Server) handleViewFiles(w http.ResponseWriter, r *http.Request) {
	v := s.withViewer(w, r)
	if v == nil {
		return
	}
	project := r.PathValue("project")
	id, err := c4.Parse(r.PathValue("id"))
	if err != nil || !v.Sees(project) || s.Stores == nil {
		fail(w, http.StatusNotFound, "no such view")
		return
	}
	st, err := s.Stores(r.Context(), project)
	if err != nil || st == nil {
		fail(w, http.StatusNotFound, "no such view")
		return
	}
	view, err := c4view.Load(r.Context(), id, c4view.StoreRecords(st), 16)
	if err != nil {
		fail(w, http.StatusNotFound, "no such view: %v", err)
		return
	}
	prefix := strings.TrimPrefix(r.URL.Query().Get("path"), "/")
	limit := maxViewFiles
	if l, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && l > 0 && l < limit {
		limit = l
	}
	out := api.ViewListing{View: id.String(), Files: []api.ViewFile{}}
	under := func(p string) bool {
		p = strings.TrimSuffix(p, "/")
		return prefix == "" || p == strings.TrimSuffix(prefix, "/") || strings.HasPrefix(p, strings.TrimSuffix(prefix, "/")+"/")
	}
	full := func() bool {
		if len(out.Files)+len(out.Dirs)+len(out.Links) < limit {
			return false
		}
		out.Truncated = true
		return true
	}
	for _, f := range view.Files {
		if !under(f.Path) {
			continue
		}
		if full() {
			break
		}
		out.Files = append(out.Files, api.ViewFile{Path: f.Path, ID: f.ID.String(), Size: f.Size, Exec: f.Exec})
	}
	for _, l := range view.Symlinks {
		if !under(l.Path) || full() {
			continue
		}
		out.Links = append(out.Links, api.ViewLink{Path: l.Path, Target: l.Target})
	}
	for _, d := range view.EmptyDirs() {
		if !under(d) || full() {
			continue
		}
		out.Dirs = append(out.Dirs, d)
	}
	writeJSON(w, http.StatusOK, out)
}

var versionSuffix = regexp.MustCompile(`-v(\d+)$`)

// nextName is <shot>-<stage>-vNNN, one version past the latest job of
// that shot and stage in the namespace.
func nextName(m *model, ns, shot, stage string) string {
	base := strings.Trim(strings.ToLower(shot+"-"+stage), "-")
	if base == "" {
		base = "job"
	}
	top := 0
	for _, rec := range m.jobs {
		if rec.job.Namespace != ns || !strings.HasPrefix(rec.job.Name, base+"-v") {
			continue
		}
		if mm := versionSuffix.FindStringSubmatch(rec.job.Name); mm != nil && rec.job.Name == base+mm[0] {
			if n, _ := strconv.Atoi(mm[1]); n > top {
				top = n
			}
		}
	}
	return fmt.Sprintf("%s-v%03d", base, top+1)
}

// inputMaterialization names the Materialization of an input view: the
// same view always gets the same name in a namespace.
func inputMaterialization(view string) string {
	sum := sha256.Sum256([]byte(view))
	return "view-" + hex.EncodeToString(sum[:])[:16]
}

// handleSubmit is POST /jobs: plan (dryRun) or create a WorkJob for the
// viewer.
func (s *Server) handleSubmit(w http.ResponseWriter, r *http.Request) {
	v := s.withViewer(w, r)
	if v == nil {
		return
	}
	m := s.current()
	if m == nil {
		fail(w, http.StatusServiceUnavailable, "the HUD is still reading the farm")
		return
	}
	var body api.SubmitRequest
	if err := decodeBody(r, &body); err != nil {
		fail(w, http.StatusBadRequest, "body: %v", err)
		return
	}
	if body.For != "" {
		// Work for another person needs the grant they gave the tracker
		// (integrations GAPS 17), which the HUD does not take yet: refused,
		// never submitted as the caller's own instead.
		fail(w, http.StatusNotImplemented, "for %q: submitting for another person needs their grant to the tracker, which this HUD does not take yet", body.For)
		return
	}
	project, ns, err := s.project(m, v, body.Project, body.Namespace)
	if err != nil {
		fail(w, http.StatusNotFound, "%v", err)
		return
	}
	var spec c4v1.WorkJobSpec
	if len(body.Spec) > 0 {
		if err := json.Unmarshal(body.Spec, &spec); err != nil {
			fail(w, http.StatusBadRequest, "spec: %v", err)
			return
		}
	}
	if spec.Class == "" {
		fail(w, http.StatusBadRequest, "spec.class is required: render, ci or ml")
		return
	}
	if len(spec.Template.Spec.Containers) == 0 {
		fail(w, http.StatusBadRequest, "spec.template needs a container: the image each unit runs")
		return
	}
	spec.Template = jobspec.Restrict(spec.Template)
	name := body.Name
	if name == "" {
		stage := body.Stage
		if stage == "" {
			stage = body.Type
		}
		name = nextName(m, ns, body.Shot, stage)
	}
	if errs := validation.IsDNS1123Subdomain(name); len(errs) > 0 {
		fail(w, http.StatusBadRequest, "name %q: %s", name, strings.Join(errs, "; "))
		return
	}
	ctx := r.Context()
	if err := s.authorize(ctx, r, v, ns, "create", "workjobs", ""); err != nil {
		fail(w, http.StatusForbidden, "%v", err)
		return
	}

	// input views become Materializations, one per view
	var mats []*c4v1.Materialization
	for _, in := range body.Inputs {
		if in.MountPath == "" || !path.IsAbs(in.MountPath) || path.Clean(in.MountPath) != in.MountPath {
			fail(w, http.StatusBadRequest, "input mountPath %q must be a clean absolute path", in.MountPath)
			return
		}
		switch {
		case in.View != "" && in.FromJob != "":
			fail(w, http.StatusBadRequest, "an input is a view or a job, not both")
			return
		case in.View != "":
			if _, err := c4.Parse(in.View); err != nil {
				fail(w, http.StatusBadRequest, "input view %q is not a C4 ID", in.View)
				return
			}
			mn := inputMaterialization(in.View)
			mats = append(mats, &c4v1.Materialization{
				ObjectMeta: metav1.ObjectMeta{Name: mn, Namespace: ns, Labels: map[string]string{"app.kubernetes.io/managed-by": "c4-hud"}},
				Spec:       c4v1.MaterializationSpec{Source: c4v1.ManifestSource{ID: in.View}}})
			spec.Inputs = append(spec.Inputs, c4v1.WorkInput{Materialization: mn, MountPath: in.MountPath, ReadOnly: in.ReadOnly})
		case in.FromJob != "":
			spec.Inputs = append(spec.Inputs, c4v1.WorkInput{FromJob: in.FromJob, MountPath: in.MountPath, ReadOnly: in.ReadOnly})
			if !contains(spec.DependsOn, in.FromJob) {
				spec.DependsOn = append(spec.DependsOn, in.FromJob)
			}
		default:
			fail(w, http.StatusBadRequest, "an input needs a view or a fromJob")
			return
		}
	}
	if len(mats) > 0 {
		if err := s.authorize(ctx, r, v, ns, "create", "materializations", ""); err != nil {
			fail(w, http.StatusForbidden, "%v", err)
			return
		}
	}
	if body.Due != "" {
		t, err := s.dueOn(ctx, project, body.Due)
		if err != nil {
			fail(w, http.StatusBadRequest, "due: %v", err)
			return
		}
		spec.Deadline = &metav1.Time{Time: t}
	}
	if body.Follows != "" {
		if errs := validation.IsDNS1123Subdomain(body.Follows); len(errs) > 0 {
			fail(w, http.StatusBadRequest, "follows %q: %s", body.Follows, strings.Join(errs, "; "))
			return
		}
		if _, err := s.liveJob(ctx, ns, body.Follows); err != nil {
			fail(w, http.StatusBadRequest, "follows %q: no such job in %s", body.Follows, ns)
			return
		}
		spec.Follows = body.Follows
	}
	if spec.Deadline == nil && spec.DeadlineMode == "" {
		spec.DeadlineMode = "Suggested"
	}

	job := &c4v1.WorkJob{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns,
			Labels:      map[string]string{},
			Annotations: map[string]string{c4v1.AnnotationOwner: v.User, c4v1.AnnotationSubmittedVia: submittedVia(r)}},
		Spec: spec,
	}
	if body.Shot != "" {
		if errs := validation.IsValidLabelValue(body.Shot); len(errs) > 0 {
			fail(w, http.StatusBadRequest, "shot %q: %s", body.Shot, strings.Join(errs, "; "))
			return
		}
		job.Labels[c4v1.LabelShot] = body.Shot
	}
	if body.TrackerRef != "" {
		tracker, rest, ok := strings.Cut(body.TrackerRef, "/")
		if !ok || !strings.Contains(rest, "/") || strings.HasPrefix(rest, "/") || strings.HasSuffix(rest, "/") {
			fail(w, http.StatusBadRequest, "trackerRef %q: want <tracker>/<type>/<id>", body.TrackerRef)
			return
		}
		if errs := validation.IsValidLabelValue(tracker); len(errs) > 0 || tracker == "" {
			fail(w, http.StatusBadRequest, "trackerRef %q: tracker %q: %s", body.TrackerRef, tracker, strings.Join(errs, "; "))
			return
		}
		job.Annotations[c4v1.AnnotationTrackerRef] = body.TrackerRef
		job.Labels[c4v1.LabelTracker] = tracker
	}
	if body.Type != "" {
		if errs := validation.IsValidLabelValue(body.Type); len(errs) > 0 {
			fail(w, http.StatusBadRequest, "type %q: %s", body.Type, strings.Join(errs, "; "))
			return
		}
		job.Labels[c4v1.LabelJobType] = body.Type
	}
	if body.AIUsage != nil {
		raw, err := json.Marshal(body.AIUsage)
		if err != nil {
			fail(w, http.StatusBadRequest, "aiUsage: %v", err)
			return
		}
		job.Annotations[c4v1.AnnotationAIUsage] = string(raw)
	}

	out := api.SubmitResponse{OK: true, DryRun: body.DryRun, Namespace: ns, Name: name, Project: project,
		Link: "#/job/" + ns + "/" + name}
	for _, mt := range mats {
		out.Materializations = append(out.Materializations, mt.Name)
	}
	est, estErr := s.dryRun(ctx, job, project)
	if est != nil {
		out.Estimate = est
	}
	if body.DryRun {
		if estErr != nil {
			code := http.StatusUnprocessableEntity
			if errors.Is(estErr, ErrNotPlanned) {
				code = http.StatusServiceUnavailable
			}
			fail(w, code, "%v", estErr)
			return
		}
		out.Message = name + ": planned, not submitted"
		writeJSON(w, http.StatusOK, out)
		return
	}
	if estErr != nil {
		out.EstimateError = estErr.Error()
	}

	for _, mt := range mats {
		if err := s.Client.Create(ctx, mt); err != nil && !apierrors.IsAlreadyExists(err) {
			fail(w, http.StatusConflict, "materialization %s: %v", mt.Name, err)
			return
		}
	}
	if err := s.Client.Create(ctx, job); err != nil {
		code := http.StatusConflict
		if !apierrors.IsAlreadyExists(err) {
			code = http.StatusBadRequest
		}
		fail(w, code, "%v", err)
		return
	}
	msg := fmt.Sprintf("%s: submitted, %s, %d input view%s", name, spec.Class, len(mats), plural(len(mats)))
	note := msg
	if via(r) == "mcp" {
		note += "; via MCP"
	}
	s.record(ctx, job, "WorkJob", v.User, "Submitted", note, bookNote{actor: actorOf(r, v), kind: "submit"})
	out.Message = msg
	writeJSON(w, http.StatusCreated, out)
}

func (s *Server) dryRun(ctx context.Context, job *c4v1.WorkJob, project string) (*api.SubmitEstimate, error) {
	if s.DryRun == nil {
		return nil, fmt.Errorf("%w: estimates are not available on this farm", ErrNotPlanned)
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	est, err := s.DryRun(cctx, job.DeepCopy(), project)
	if err != nil {
		return nil, err
	}
	return &est, nil
}

func submittedVia(r *http.Request) string {
	if via(r) == "mcp" {
		return "mcp"
	}
	return "hud"
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// dueOn reads a deadline sent as an RFC 3339 time, or as a date
// ("2026-10-23") at the time of day the project's due-time convention
// gives (Project spec.work.dueTime; 18:00 UTC without one).
func (s *Server) dueOn(_ context.Context, project, due string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, due); err == nil {
		return t, nil
	}
	if _, err := time.Parse("2006-01-02", due); err != nil {
		return time.Time{}, fmt.Errorf("%q is neither an RFC 3339 time nor a date (YYYY-MM-DD)", due)
	}
	var conv *c4v1.DueTime
	if m := s.current(); m != nil {
		for _, p := range m.projects {
			if p.Name == project && p.Spec.Work != nil {
				conv = p.Spec.Work.DueTime
			}
		}
	}
	return conv.DueOn(due)
}
