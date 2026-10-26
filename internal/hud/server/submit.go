package server

// Submitting work through the HUD: objects into a project's store, a tree
// of them recorded as a view, and a WorkJob (with the Materializations of
// its input views) created for the signed-in viewer. These are the calls
// `c4 farm submit` makes; MCP submits through POST /submissions. Each is checked
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
	"net/url"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Avalanche-io/c4"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/hud/api"
	"github.com/Avalanche-io/c4-operator/internal/authoring"
	"github.com/Avalanche-io/c4-operator/internal/jobspec"
	"github.com/Avalanche-io/c4-operator/internal/provenance"
	"github.com/Avalanche-io/c4-operator/internal/storeurl"
	c4view "github.com/Avalanche-io/c4-operator/internal/view"
	"github.com/Avalanche-io/c4-operator/internal/work/class"
	"github.com/Avalanche-io/c4-operator/internal/work/queue"
	"github.com/Avalanche-io/c4-operator/schema"
)

// DryRunFunc plans a job that does not exist yet and returns what the
// farm expects of it, writing nothing. The manager wires it to the work
// queue; a nil DryRun answers dry runs with 503.
type DryRunFunc func(ctx context.Context, job *c4v1.WorkJob, project string) (api.SubmitEstimate, error)

// DryRunAllFunc plans drafts together, each after the ones it depends
// on; views are the view IDs of their input Materializations by name, and
// tables the unit tables of agent work not stored yet, by C4 ID.
type DryRunAllFunc func(ctx context.Context, jobs []*c4v1.WorkJob, project string, views map[string]string, tables map[string][]byte) ([]api.SubmitEstimate, error)

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

// nextName is <shot>-<stage>-vNNN, one version past the latest job of
// that shot and stage in the namespace (authoring.NextName).
func nextName(m *model, ns, shot, stage string) string {
	var names []string
	for _, rec := range m.jobs {
		if rec.job.Namespace == ns {
			names = append(names, rec.job.Name)
		}
	}
	return authoring.NextName(names, shot, stage)
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
	// Read strictly, as every document is: a field the request or the
	// spec does not have is an error naming it, never dropped.
	raw, err := io.ReadAll(http.MaxBytesReader(nil, r.Body, jobBody))
	if err != nil {
		failBody(w, err)
		return
	}
	var body api.SubmitRequest
	if err := class.Strict(raw, &body); err != nil {
		fail(w, http.StatusBadRequest, "body: %v", err)
		return
	}
	out, code, err := s.createJob(r, v, m, body, createOpts{})
	var gr *grantRefused
	if errors.As(err, &gr) {
		// Say which refusal it is, for the sync to tell the person.
		writeJSON(w, code, api.SubmitRefused{Error: err.Error(), Reason: gr.reason})
		return
	}
	if errors.As(err, new(leadOnlyError)) {
		writeJSON(w, code, map[string]string{"error": leadOnlyMessage, "fix": leadOnlyFix})
		return
	}
	if err != nil {
		fail(w, code, "%s", err.Error())
		return
	}
	writeJSON(w, code, out)
}

// createOpts are what a caller inside the HUD gives a job that a POST
// /jobs request does not carry: a submission's lowering (authoring) adds
// its annotations (the submission, the derivation class) and labels (the
// milestone).
type createOpts struct {
	Annotations, Labels map[string]string
	// CheckOnly checks the request as a create would, and stops before the
	// estimate: a submission checks every job before it creates any.
	CheckOnly bool
	// EstimateOptional makes a dry run without an estimate a plan that
	// says why (EstimateError) rather than a refusal: a submission's later
	// jobs follow jobs that do not exist yet.
	EstimateOptional bool
	// Draft, when set, receives the WorkJob as it would be created (with
	// CheckOnly: as checked), for planning a submission's jobs together.
	Draft *c4v1.WorkJob
	// Book is a line added to the job's book entry (a submission's ID and
	// note).
	Book string
	// Hold creates the job held (spec.nudge.hold), for a submission that
	// releases its jobs together once all are made; such a job replaces
	// no forecast, since the submission may yet be rolled back.
	Hold bool
	// Made, when set, receives the WorkJob as created (with its UID).
	Made *c4v1.WorkJob
	// Lowered says the HUD's own lowering of a tool (a submission's job,
	// a tool's test) wrote the request, so its spec.tool is the farm's.
	// Without it a request naming spec.tool is refused.
	Lowered bool
}

// identityError is a unit's submission refused for who the unit is: its
// own job, its owner, what its tool grants. A submission says it as it is.
type identityError struct{ error }

func (e identityError) Unwrap() error { return e.error }

// unitRefusal is an authorization refusal, an identityError for a unit.
func unitRefusal(v *api.Viewer, err error) error {
	if v != nil && v.Unit != nil {
		return identityError{err}
	}
	return fmt.Errorf("%v", err)
}

// takenError is a create refused because the job's name was taken
// meanwhile: a submission numbers its jobs again rather than report it.
type takenError struct{ error }

// poolsRefuse says which of pools runs only other owners' work, so a job of
// owner's could not run there; "" when each serves owner (a pool with no
// owners runs anyone's).
func poolsRefuse(m *model, owner string, pools []string) string {
	if m == nil {
		return ""
	}
	for _, name := range pools {
		for _, p := range m.pools {
			if p.Name == name && len(p.Spec.Owners) > 0 && !slices.Contains(p.Spec.Owners, owner) {
				return fmt.Sprintf("pool %s runs only the jobs of %s, so a unit of %s's job may not use it", name, strings.Join(p.Spec.Owners, ", "), owner)
			}
		}
	}
	return ""
}

// ownerMay says why a unit of parent, owner's job, may not submit in ns
// for owner, with its status: the API server is asked what owner's own
// submission there would be, with the groups recorded when parent was
// made, never the unit's rights or the HUD's. Where parent recorded no
// groups only what owner is given by name counts, and the refusal says
// so. On a single-person farm its one person (the farm binding's owner,
// else --single-person-farm) always may.
func (s *Server) ownerMay(ctx context.Context, parent *c4v1.WorkJob, owner, ns string) (int, error) {
	if one := s.singlePerson(ctx); one != "" && owner == one {
		return 0, nil
	}
	var groups []string
	raw, known := parent.Annotations[c4v1.AnnotationOwnerGroups]
	if known && json.Unmarshal([]byte(raw), &groups) != nil {
		known, groups = false, nil
	}
	if s.Kube == nil {
		if s.DevViewers != nil {
			return 0, nil // development: no API server to ask
		}
		return http.StatusServiceUnavailable, fmt.Errorf("this HUD cannot ask whether %s may submit in %s", owner, ns)
	}
	ok, err := s.allowed(ctx, owner, groups, ns, "create", "workjobs", "")
	if err != nil {
		return http.StatusServiceUnavailable, fmt.Errorf("could not ask whether %s may submit in %s: %v", owner, ns, err)
	}
	if ok {
		return 0, nil
	}
	why := fmt.Sprintf("%s may not submit in %s, so a unit of %s's job may not either", owner, ns, owner)
	if !known {
		why += fmt.Sprintf(" (%s's groups aren't known for a unit's submission, so only rights given to %s by name count)", owner, owner)
	}
	return http.StatusForbidden, errors.New(why)
}

// createJob checks and creates a job from a POST /jobs request for viewer
// v, or plans it (body.DryRun), as handleSubmit and the submissions
// endpoint do. code is the HTTP status, with err the refusal's message.
func (s *Server) createJob(r *http.Request, v *api.Viewer, m *model, body api.SubmitRequest, o createOpts) (api.SubmitResponse, int, error) {
	project, ns, err := s.project(m, v, body.Project, body.Namespace)
	if err != nil {
		return api.SubmitResponse{}, http.StatusNotFound, fmt.Errorf("%v", err)
	}
	var spec c4v1.WorkJobSpec
	if len(body.Spec) > 0 {
		if err := class.Strict(body.Spec, &spec); err != nil {
			return api.SubmitResponse{}, http.StatusBadRequest, fmt.Errorf("spec: %v", err)
		}
	}
	if spec.Tool != nil && !o.Lowered {
		// What a job runs under is the farm's to say, from the tool's
		// published version, never the client's.
		return api.SubmitResponse{}, http.StatusBadRequest, fmt.Errorf("spec.tool is set by the farm when it lowers a tool's submission: submit the tool by name (POST /submissions, c4 farm submit) rather than naming it in spec")
	}
	if spec.Class == "" {
		return api.SubmitResponse{}, http.StatusBadRequest, fmt.Errorf("spec.class is required: render, ci or ml")
	}
	if len(spec.Template.Spec.Containers) == 0 && spec.Forecast == nil {
		// A forecast never runs: it needs no image yet.
		return api.SubmitResponse{}, http.StatusBadRequest, fmt.Errorf("spec.template needs a container: the image each unit runs")
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
		return api.SubmitResponse{}, http.StatusBadRequest, fmt.Errorf("name %q: %s", name, strings.Join(errs, "; "))
	}
	ctx := r.Context()
	if err := s.authorize(ctx, r, v, ns, "create", "workjobs", ""); err != nil {
		return api.SubmitResponse{}, http.StatusForbidden, unitRefusal(v, err)
	}
	if agentSpec(spec) && leadOnly(v, projectObject(m, project)) {
		// a worker may not start workers, by any door
		return api.SubmitResponse{}, http.StatusForbidden, leadOnlyError{}
	}

	// input views become Materializations, one per view
	var mats []*c4v1.Materialization
	for _, in := range body.Inputs {
		if in.MountPath == "" || !path.IsAbs(in.MountPath) || path.Clean(in.MountPath) != in.MountPath {
			return api.SubmitResponse{}, http.StatusBadRequest, fmt.Errorf("input mountPath %q must be a clean absolute path", in.MountPath)
		}
		switch {
		case in.View != "" && in.FromJob != "":
			return api.SubmitResponse{}, http.StatusBadRequest, fmt.Errorf("an input is a view or a job, not both")
		case in.View != "":
			if _, err := c4.Parse(in.View); err != nil {
				return api.SubmitResponse{}, http.StatusBadRequest, fmt.Errorf("input view %q is not a C4 ID", in.View)
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
			return api.SubmitResponse{}, http.StatusBadRequest, fmt.Errorf("an input needs a view or a fromJob")
		}
	}
	if len(mats) > 0 {
		if err := s.authorize(ctx, r, v, ns, "create", "materializations", ""); err != nil {
			return api.SubmitResponse{}, http.StatusForbidden, unitRefusal(v, err)
		}
	}
	if body.Due != "" {
		t, err := s.dueOn(ctx, project, body.Due)
		if err != nil {
			return api.SubmitResponse{}, http.StatusBadRequest, fmt.Errorf("due: %v", err)
		}
		spec.Deadline = &metav1.Time{Time: t}
	}
	if body.Follows != "" {
		if errs := validation.IsDNS1123Subdomain(body.Follows); len(errs) > 0 {
			return api.SubmitResponse{}, http.StatusBadRequest, fmt.Errorf("follows %q: %s", body.Follows, strings.Join(errs, "; "))
		}
		if _, err := s.liveJob(ctx, ns, body.Follows); err != nil {
			return api.SubmitResponse{}, http.StatusBadRequest, fmt.Errorf("follows %q: no such job in %s", body.Follows, ns)
		}
		spec.Follows = body.Follows
	}
	var replaced *c4v1.WorkJob
	if body.Replaces != "" {
		f, err := s.liveJob(ctx, ns, body.Replaces)
		switch {
		case err != nil:
			return api.SubmitResponse{}, http.StatusBadRequest, fmt.Errorf("replaces %q: no such job in %s", body.Replaces, ns)
		case f.Spec.Forecast == nil:
			return api.SubmitResponse{}, http.StatusBadRequest, fmt.Errorf("replaces %q: it is a real job, not a forecast", body.Replaces)
		case spec.Forecast != nil:
			return api.SubmitResponse{}, http.StatusBadRequest, fmt.Errorf("replaces %q: a forecast replaces nothing; a real job replaces a forecast", body.Replaces)
		}
		replaced = f
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
	if replaced != nil {
		job.Annotations[c4v1.AnnotationReplaces] = replaced.Name
	}
	if v.Unit == nil && v.Groups != nil {
		// the signed-in owner's groups, for what a unit of the job
		// submits in their name later
		b, _ := json.Marshal(v.Groups)
		job.Annotations[c4v1.AnnotationOwnerGroups] = string(b)
	}
	if owner := s.ownerFor(ctx, v); owner != v.User && body.For == "" {
		// A role's sign-in on a one-person farm is that person acting in
		// the role: the person owns the job, so their leases and pools
		// serve it, and the role stays as how they acted.
		job.Annotations[c4v1.AnnotationOwner] = owner
		job.Annotations[c4v1.AnnotationSubmittedAs] = devRole(v)
	}
	var own *c4v1.WorkJob
	if u := v.Unit; u != nil {
		// a unit's job is its job's owner's, never the unit's own: the
		// runners of that owner run it, and the owner answers for it
		var parent c4v1.WorkJob
		if err := s.Client.Get(ctx, types.NamespacedName{Namespace: u.Namespace, Name: u.Job}, &parent); err != nil {
			return api.SubmitResponse{}, http.StatusForbidden, identityError{fmt.Errorf("%s: its own job %s/%s cannot be read", v.User, u.Namespace, u.Job)}
		}
		if u.JobUID == "" {
			return api.SubmitResponse{}, http.StatusForbidden, identityError{fmt.Errorf("%s: its pod names no WorkJob as its controller, so its own job cannot be told from another of the name %s/%s", v.User, u.Namespace, u.Job)}
		}
		if string(parent.UID) != u.JobUID {
			return api.SubmitResponse{}, http.StatusForbidden, identityError{fmt.Errorf("%s: the unit's own job %s/%s was replaced by another of the same name", v.User, u.Namespace, u.Job)}
		}
		owner := parent.Annotations[c4v1.AnnotationOwner]
		if owner == "" {
			return api.SubmitResponse{}, http.StatusForbidden, identityError{fmt.Errorf("%s: its own job %s/%s has no owner to submit for", v.User, u.Namespace, u.Job)}
		}
		// The job runs on the owner's runners and in the owner's name, so
		// the owner must be able to make it there themselves, not only the
		// unit: in every namespace, its parent's too.
		if code, err := s.ownerMay(ctx, &parent, owner, ns); err != nil {
			return api.SubmitResponse{}, code, fmt.Errorf("%s: %v", v.User, err)
		}
		// Pools that run only others' work would never run it (the queue
		// keeps to the pools that serve the owner): say so now.
		if why := poolsRefuse(m, owner, spec.Pools); why != "" {
			return api.SubmitResponse{}, http.StatusForbidden, fmt.Errorf("%s: %s", v.User, why)
		}
		if g, ok := parent.Annotations[c4v1.AnnotationOwnerGroups]; ok {
			job.Annotations[c4v1.AnnotationOwnerGroups] = g
		}
		// A job a tracker's sync made for a person, under the person's
		// grant: its children are bound by the same grant, so a unit
		// cannot widen what the person granted.
		if parent.Annotations[c4v1.AnnotationGrant] != "" {
			if ns != parent.Namespace {
				// A grant is a person's to one tracker in one namespace: it
				// holds nowhere else, so a child elsewhere could never run.
				return api.SubmitResponse{}, http.StatusForbidden, fmt.Errorf("%s: a unit of a job made under %s's grant to tracker %s submits in %s; the grant holds only in %s",
					v.User, owner, c4v1.GrantTracker(&parent), ns, parent.Namespace)
			}
			g, err := s.parentGrantCovers(ctx, &parent, owner, &spec)
			if err != nil {
				return api.SubmitResponse{}, http.StatusForbidden, fmt.Errorf("%s: %v", v.User, err)
			}
			// the grant as it is now, not the parent's recorded copy
			job.Annotations[c4v1.AnnotationGrant] = grantRef(g)
			job.Annotations[c4v1.AnnotationGrantTracker] = c4v1.GrantTracker(&parent)
		}
		job.Annotations[c4v1.AnnotationOwner] = owner
		job.Annotations[c4v1.AnnotationSubmittedByUnit] = schema.UnitRef(u.Namespace, u.Job, u.JobUID, u.Unit)
		own = &parent
	}
	if body.Shot != "" {
		if errs := validation.IsValidLabelValue(body.Shot); len(errs) > 0 {
			return api.SubmitResponse{}, http.StatusBadRequest, fmt.Errorf("shot %q: %s", body.Shot, strings.Join(errs, "; "))
		}
		job.Labels[c4v1.LabelShot] = body.Shot
	}
	if body.TrackerRef != "" {
		tracker, rest, ok := strings.Cut(body.TrackerRef, "/")
		if !ok || !strings.Contains(rest, "/") || strings.HasPrefix(rest, "/") || strings.HasSuffix(rest, "/") {
			return api.SubmitResponse{}, http.StatusBadRequest, fmt.Errorf("trackerRef %q: want <tracker>/<type>/<id>", body.TrackerRef)
		}
		if errs := validation.IsValidLabelValue(tracker); len(errs) > 0 || tracker == "" {
			return api.SubmitResponse{}, http.StatusBadRequest, fmt.Errorf("trackerRef %q: tracker %q: %s", body.TrackerRef, tracker, strings.Join(errs, "; "))
		}
		job.Annotations[c4v1.AnnotationTrackerRef] = body.TrackerRef
		job.Labels[c4v1.LabelTracker] = tracker
	}
	if body.TrackerURL != "" {
		if body.TrackerRef == "" {
			return api.SubmitResponse{}, http.StatusBadRequest, errors.New("trackerURL needs the trackerRef it is the address of")
		}
		if !trackerURL(body.TrackerURL) {
			return api.SubmitResponse{}, http.StatusBadRequest, fmt.Errorf("trackerURL %q: want an http(s) URL or a path on this host", body.TrackerURL)
		}
		job.Annotations[c4v1.AnnotationTrackerURL] = body.TrackerURL
	}
	var grant *c4v1.SubmitGrant
	if body.For != "" {
		// Work for another person, by a tracker's sync under the grant the
		// person gave that tracker (internal/hud/API.md, Grants): the
		// person owns it. Refused otherwise, never created as the caller's
		// own instead.
		g, err := s.grantFor(ctx, v, ns, &body, &spec)
		if err != nil {
			code := http.StatusForbidden
			var ge *errGrant
			if errors.As(err, &ge) {
				code = ge.code
			}
			return api.SubmitResponse{}, code, fmt.Errorf("for %s: %w", body.For, err)
		}
		grant = g
		job.Annotations[c4v1.AnnotationOwner] = body.For
		// the person's groups are not known to a tracker's sync
		delete(job.Annotations, c4v1.AnnotationOwnerGroups)
		job.Annotations[c4v1.AnnotationSubmittedVia] = c4v1.SyncDevViewer
		job.Annotations[c4v1.AnnotationGrant] = grantRef(g)
	}
	replaceWhy := ""
	if replaced != nil {
		// Only one who could have made the forecast replaces it.
		why, err := s.mayRetire(ctx, v, replaced, grant)
		if err != nil {
			return api.SubmitResponse{}, http.StatusForbidden, fmt.Errorf("replaces %q: %v", replaced.Name, err)
		}
		replaceWhy = why
	}
	if body.Type != "" {
		if errs := validation.IsValidLabelValue(body.Type); len(errs) > 0 {
			return api.SubmitResponse{}, http.StatusBadRequest, fmt.Errorf("type %q: %s", body.Type, strings.Join(errs, "; "))
		}
		job.Labels[c4v1.LabelJobType] = body.Type
	}
	if body.Stage != "" {
		if errs := validation.IsValidLabelValue(body.Stage); len(errs) > 0 {
			return api.SubmitResponse{}, http.StatusBadRequest, fmt.Errorf("stage %q: %s", body.Stage, strings.Join(errs, "; "))
		}
		job.Labels[c4v1.LabelStage] = body.Stage
	}
	for k, val := range o.Labels {
		job.Labels[k] = val
	}
	for k, val := range o.Annotations {
		job.Annotations[k] = val
	}
	if o.Hold {
		if replaced != nil {
			return api.SubmitResponse{}, http.StatusBadRequest, fmt.Errorf("replaces %q: a submission's job replaces no forecast", replaced.Name)
		}
		if job.Spec.Nudge.Hold {
			job.Annotations[AnnotationSubmissionHoldAsked] = "true"
		}
		job.Spec.Nudge.Hold = true
	}
	if body.AIUsage != nil {
		raw, err := json.Marshal(body.AIUsage)
		if err != nil {
			return api.SubmitResponse{}, http.StatusBadRequest, fmt.Errorf("aiUsage: %v", err)
		}
		job.Annotations[c4v1.AnnotationAIUsage] = string(raw)
	}
	if code, err := s.sealed(ctx, v, &job.Spec, own); err != nil {
		return api.SubmitResponse{}, code, err
	}

	out := api.SubmitResponse{OK: true, DryRun: body.DryRun, Namespace: ns, Name: name, Project: project,
		Link: "#/job/" + ns + "/" + name}
	for _, mt := range mats {
		out.Materializations = append(out.Materializations, mt.Name)
	}
	if o.Draft != nil {
		*o.Draft = *job.DeepCopy()
	}
	if o.CheckOnly {
		return out, http.StatusOK, nil
	}
	est, estErr := s.dryRun(ctx, job, project)
	if est != nil {
		out.Estimate = est
	}
	if body.DryRun {
		if estErr != nil && o.EstimateOptional {
			out.EstimateError = estErr.Error()
		} else if estErr != nil {
			code := http.StatusUnprocessableEntity
			if errors.Is(estErr, ErrNotPlanned) {
				code = http.StatusServiceUnavailable
			}
			return api.SubmitResponse{}, code, fmt.Errorf("%v", estErr)
		}
		out.Message = name + ": planned, not submitted"
		return out, http.StatusOK, nil
	}
	if estErr != nil {
		out.EstimateError = estErr.Error()
	}

	replaceRecord := ""
	if replaced != nil {
		// Signed first: the replacement deletes the forecast.
		pred := controlPredicate{Actor: v.User, Authenticated: s.authenticatedBy(r, v), Via: via(r), Agent: claimedAgent(r),
			Authorization: s.authorization(v, "create", "workjobs", ns, "") + "; " + replaceWhy, Action: "replace",
			Target: "workjob:" + ns + "/" + replaced.Name, Change: map[string]any{"replacedBy": name, "forecast": replaced.Spec.Forecast}}
		id, err := s.signControl(ctx, []string{project}, provenance.Subject{Name: pred.Target,
			Digest: map[string]string{"sha256": queue.SpecDigest(replaced)}}, pred)
		if err != nil {
			return api.SubmitResponse{}, http.StatusServiceUnavailable, fmt.Errorf("control record: %v", err)
		}
		replaceRecord = id
	}
	for _, mt := range mats {
		if err := s.Client.Create(ctx, mt); err != nil && !apierrors.IsAlreadyExists(err) {
			return api.SubmitResponse{}, http.StatusConflict, fmt.Errorf("materialization %s: %w", mt.Name, err)
		}
	}
	if err := s.Client.Create(ctx, job); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return api.SubmitResponse{}, http.StatusConflict, takenError{fmt.Errorf("%w", err)}
		}
		return api.SubmitResponse{}, http.StatusBadRequest, fmt.Errorf("%w", err)
	}
	if o.Made != nil {
		*o.Made = *job.DeepCopy()
	}
	msg := fmt.Sprintf("%s: submitted, %s, %d input view%s", name, spec.Class, len(mats), plural(len(mats)))
	if grant != nil {
		msg += fmt.Sprintf(", for %s under their grant to tracker %s", grant.Spec.Person, grant.Spec.Tracker)
	}
	note := msg
	if o.Book != "" {
		note += "; " + o.Book
	}
	if via(r) == "mcp" {
		note += "; via MCP"
	}
	s.record(ctx, job, "WorkJob", v.User, "Submitted", note, bookNote{actor: actorOf(r, v), kind: "submit"})
	if replaced != nil {
		// The forecast's work is this job now (DESIGN.md 10.12). A delete that
		// fails here is tried again until the forecast is gone
		// (sweepReplaced): the job names it.
		if err := s.retireForecast(ctx, replaced, name, v.User, actorOf(r, v), replaceRecord); err != nil {
			s.Log.Error(err, "delete the replaced forecast; will try again", "job", ns+"/"+replaced.Name)
		}
	}
	out.Message = msg
	return out, http.StatusCreated, nil
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

// trackerURL says whether u is a link the HUD may put on a job: http or
// https with a host, or a path on the HUD's own host. Never a script or
// another scheme, and never protocol-relative (//host).
func trackerURL(u string) bool {
	if len(u) > 2048 || strings.ContainsAny(u, "\x00\r\n\\") {
		return false
	}
	p, err := url.Parse(u)
	if err != nil {
		return false
	}
	switch p.Scheme {
	case "http", "https":
		return p.Host != ""
	case "":
		return p.Host == "" && strings.HasPrefix(u, "/") && !strings.HasPrefix(u, "//")
	}
	return false
}
