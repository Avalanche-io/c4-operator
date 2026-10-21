package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Avalanche-io/c4"
	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/hud/api"
	"github.com/Avalanche-io/c4-operator/internal/authoring"
	"github.com/Avalanche-io/c4-operator/internal/imageref"
	"github.com/Avalanche-io/c4-operator/internal/storeurl"
	c4view "github.com/Avalanche-io/c4-operator/internal/view"
	"github.com/Avalanche-io/c4-operator/schema"
)

// Tools on the farm (DESIGN.md 10.6, Tools; the format is
// schema/README.md). Publishing a folder with a tool.yaml checks it, pins
// its images by digest into tool.lock (so the published tree's ID covers
// the exact image), records a version, and runs the tool's test as a small
// job. The version is offered (the Tool's spec moves to it) only once its
// test passes: a tool whose test fails on the farm is not published. A
// person's tool with no test is offered at once, marked untested; an
// agent's must have one. A project's tools live in its namespace, the
// farm's in the operator's. Once offered, a farm-wide tool's tree is
// copied into every project's store, and granted where a project reads
// only granted trees, because a unit mounts only what its own project's
// store holds (DESIGN 6.2).

const (
	toolFile = "tool.yaml"
	lockFile = "tool.lock"
	// LabelToolTest on a test job names the tool it tests, and
	// AnnotationToolTest the version.
	LabelToolTest      = "c4.avalanche.io/tool-test"
	AnnotationToolTest = "c4.avalanche.io/tool-test-version"
)

// resolveImage is a tool's image as the farm names it: c4/<name> (the
// farm's own images) is expanded as schema.ImageRef does, at ImageTag
// unless the short name gives a tag, and from the farm's ImageRegistry
// (none on a farm whose images are loaded onto its nodes); any other
// reference is the tool's own.
func (s *Server) resolveImage(ref string) string {
	out := schema.ImageRef(ref, orElse(s.ImageTag, "latest"))
	if out == ref || !strings.HasPrefix(out, schema.ImageRegistry) {
		return out
	}
	reg := strings.TrimSuffix(s.ImageRegistry, "/")
	if reg+"/" == schema.ImageRegistry {
		return out
	}
	rest := strings.TrimPrefix(out, schema.ImageRegistry)
	if reg == "" {
		return rest
	}
	return reg + "/" + rest
}

// pinImage pins ref by digest (tool.lock holds digests only): a digest a
// node reports for the image, else its registry's. An image loaded onto
// the nodes by tag, with no registry, has no digest to pin: the farm's
// registry, or a reference by digest, is what makes it publishable.
func (s *Server) pinImage(ctx context.Context, m *model, ref string) (string, error) {
	resolved := s.resolveImage(ref)
	if d, ok := imageref.NodeDigest(m.nodes, resolved); ok {
		return d, nil
	}
	z := s.Images
	if z == nil {
		plain := map[string]bool{}
		for _, h := range s.PlainRegistries {
			plain[h] = true
		}
		z = &imageref.Resolver{PlainHTTP: func(host string) bool { return plain[host] || imageref.Loopback(host) }}
	}
	if resolved != ref {
		p, err := z.Pin(ctx, resolved)
		if err != nil {
			return "", fmt.Errorf("as %s: %w", resolved, err)
		}
		return p, nil
	}
	return z.Pin(ctx, resolved)
}

// readFile is a file of a view, from a store.
func readFile(ctx context.Context, st *storeurl.Sharded, v *c4view.View, p string, limit int64) ([]byte, bool, error) {
	for _, f := range v.Files {
		if f.Path != p {
			continue
		}
		r, err := st.Open(f.ID)
		if err != nil {
			return nil, true, err
		}
		defer r.Close()
		b, err := io.ReadAll(io.LimitReader(r, limit+1))
		if err != nil {
			return nil, true, err
		}
		if int64(len(b)) > limit {
			return nil, true, fmt.Errorf("%s is over %d bytes", p, limit)
		}
		return b, true, nil
	}
	return nil, false, nil
}

// storeView writes a view's records into a store.
func storeView(ctx context.Context, st *storeurl.Sharded, v *c4view.View) error {
	for _, rec := range v.Records() {
		if _, err := st.PutBytes(ctx, rec.Data); err != nil {
			return err
		}
	}
	return nil
}

// withFile is v with the file p set to data (stored), as a new view.
func withFile(ctx context.Context, st *storeurl.Sharded, v *c4view.View, p string, data []byte, write bool) (*c4view.View, error) {
	root := c4view.NewNode("")
	for _, f := range v.Files {
		if f.Path == p {
			continue
		}
		dir, name := c4view.SplitPath(f.Path)
		root.Dir(dir).Files[name] = c4view.FileEntry(name, f.Size, f.ID, f.Exec)
	}
	for _, l := range v.Symlinks {
		dir, name := c4view.SplitPath(l.Path)
		root.Dir(dir).Files[name] = c4view.SymlinkEntry(name, l.Target)
	}
	for _, d := range v.EmptyDirs() {
		root.Dir(d)
	}
	id := c4.Identify(strings.NewReader(string(data)))
	if write {
		if _, err := st.PutBytes(ctx, data); err != nil {
			return nil, err
		}
	}
	dir, name := c4view.SplitPath(p)
	root.Dir(dir).Files[name] = c4view.FileEntry(name, int64(len(data)), id, false)
	out := c4view.FromNode(root)
	if write {
		if err := storeView(ctx, st, out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// subtree is the folder sub of v as a view of its own, stored.
func subtree(ctx context.Context, st *storeurl.Sharded, v *c4view.View, sub string) (*c4view.View, error) {
	prefix := strings.Trim(path.Clean("/"+sub), "/")
	if prefix != "" {
		prefix += "/"
	}
	root := c4view.NewNode("")
	n := 0
	for _, f := range v.Files {
		if rel, ok := strings.CutPrefix(f.Path, prefix); ok {
			dir, name := c4view.SplitPath(rel)
			root.Dir(dir).Files[name] = c4view.FileEntry(name, f.Size, f.ID, f.Exec)
			n++
		}
	}
	for _, l := range v.Symlinks {
		if rel, ok := strings.CutPrefix(l.Path, prefix); ok {
			dir, name := c4view.SplitPath(rel)
			root.Dir(dir).Files[name] = c4view.SymlinkEntry(name, l.Target)
			n++
		}
	}
	if n == 0 {
		return nil, fmt.Errorf("the tool's tree has nothing under %s", sub)
	}
	out := c4view.FromNode(root)
	return out, storeView(ctx, st, out)
}

// versionNumber is N of v<N>.
func versionNumber(v string) int {
	n, _ := strconv.Atoi(strings.TrimPrefix(v, "v"))
	return n
}

func errs(in []schema.Error) []api.FieldError {
	out := make([]api.FieldError, len(in))
	for i, e := range in {
		out[i] = api.FieldError{Path: e.Path, Message: e.Message, Fix: e.Fix}
	}
	return out
}

// mayPublish is why the viewer may not publish a tool, or "": a person
// with the pipeline TD or farm admin role, or an agent acting for one,
// and to the whole farm the farm admin only. A unit never publishes,
// whatever its farm access (needs.farm: project lets it submit jobs, not
// publish tools). This is the HUD's rule, not RBAC on Tool objects, which
// the HUD writes itself, and development viewers keep to it too.
func mayPublish(v *api.Viewer, farm bool) string {
	switch {
	case v.Unit != nil:
		return "a unit does not publish tools, whatever its farm access; a pipeline TD publishes them"
	case farm && v.Role != "farm-admin":
		return "publishing a tool to the whole farm takes the farm admin role"
	case v.Role != "pipeline-td" && v.Role != "farm-admin":
		return "publishing a tool takes the pipeline TD or farm admin role"
	}
	return ""
}

// agentConfig says whether a Tool is an agent's configuration: one no
// publish made, holding a configuration, network, sidecars or farm
// access. A Tool with neither is a publish that never recorded a version.
func agentConfig(t *c4v1.Tool) bool {
	return len(t.Status.Versions) == 0 && len(t.Status.Earlier) == 0 &&
		(t.Spec.View != "" || len(t.Spec.Egress) > 0 || len(t.Spec.Sidecars) > 0 || t.Spec.C4 != "")
}

// testJobName names a version's test job by its tool, scope and tree (a
// hash of its ID), so
// a project's v1 and the farm's v1 of one tool never collide, under 50
// characters; a tree tested again gets the next free -<n>.
func (s *Server) testJobName(ctx context.Context, ns, tool, scope, view string) string {
	mid := "-test-"
	if scope == "farm" {
		mid = "-farmtest-"
	}
	sum := sha256.Sum256([]byte(view)) // a C4 ID has capitals; a name may not
	base := tool[:min(len(tool), 46-len(mid)-8)] + mid + hex.EncodeToString(sum[:4])
	name := base
	for n := 2; s.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &c4v1.WorkJob{}) == nil; n++ {
		name = fmt.Sprintf("%s-%d", base, n)
	}
	return name
}

// handlePublishTool is POST /tools/{project}.
func (s *Server) handlePublishTool(w http.ResponseWriter, r *http.Request) {
	v := s.withViewer(w, r)
	if v == nil {
		return
	}
	m := s.current()
	if m == nil {
		fail(w, http.StatusServiceUnavailable, "the HUD is still reading the farm")
		return
	}
	var body api.ToolPublishRequest
	if err := decodeBody(r, &body); err != nil {
		failBody(w, err)
		return
	}
	if why := mayPublish(v, body.Farm); why != "" {
		fail(w, http.StatusForbidden, "%s", why)
		return
	}
	project, projectNS, err := s.project(m, v, r.PathValue("project"), "")
	if err != nil || s.Stores == nil {
		fail(w, http.StatusNotFound, "no project %s", r.PathValue("project"))
		return
	}
	ns, scope := projectNS, "project"
	if body.Farm {
		ns, scope = s.Namespace, "farm"
	}
	ctx := r.Context()
	st, err := s.Stores(ctx, project)
	if err != nil || st == nil {
		s.Log.Error(err, "tool publish: store", "project", project)
		fail(w, http.StatusServiceUnavailable, "%s's store cannot be reached just now; try again shortly", project)
		return
	}
	id, err := c4.Parse(body.View)
	if err != nil {
		fail(w, http.StatusBadRequest, "view %q is not a C4 ID", body.View)
		return
	}
	tree, err := c4view.Load(ctx, id, c4view.StoreRecords(st), 16)
	if err != nil {
		fail(w, http.StatusNotFound, "%s is not a tree in %s's store: upload the folder first (c4 farm tool publish does)", id, project)
		return
	}
	refuse := func(e ...api.FieldError) {
		writeJSON(w, http.StatusUnprocessableEntity, api.ToolPublishResponse{Errors: e})
	}
	storeFail := func(err error) {
		s.Log.Error(err, "tool publish: store", "project", project)
		fail(w, http.StatusBadGateway, "%s's store failed while publishing; try again shortly", project)
	}
	doc, ok, err := readFile(ctx, st, tree, toolFile, 1<<20)
	switch {
	case err != nil:
		storeFail(err)
		return
	case !ok:
		refuse(api.FieldError{Path: "", Message: "the folder has no tool.yaml at its top",
			Fix: "publish the folder that holds tool.yaml; c4 farm tool new writes one"})
		return
	}
	tool, perr := schema.ParseTool(doc)
	if len(perr) > 0 {
		refuse(errs(perr)...)
		return
	}
	if cerr := authoring.CheckTool(tool, project, s.farmFor(ctx, m)); len(cerr) > 0 {
		refuse(errs(cerr)...)
		return
	}
	if rerr := authoring.ToolRefusals(tool); len(rerr) > 0 {
		refuse(errs(rerr)...)
		return
	}
	if via(r) == "mcp" && tool.Test == nil {
		refuse(api.FieldError{Path: "test", Message: "a tool an agent publishes must have a test",
			Fix: "add test: with the tree to run it on and what it writes; a person may publish it untested"})
		return
	}
	// A release's own tool is published as the release built it, tool.lock
	// and all, and tested in that release; any other tree is pinned here.
	release, inRelease := s.ReleaseTrees[id.String()]
	var published *c4view.View
	var lockJSON []byte
	var lock *schema.Lock
	if inRelease {
		b, ok, err := readFile(ctx, st, tree, lockFile, 1<<20)
		if err != nil {
			storeFail(err)
			return
		}
		l, lerr := schema.ParseLock(b)
		if !ok || len(lerr) > 0 {
			fail(w, http.StatusInternalServerError, "the release's %s has no readable tool.lock", tool.Name)
			return
		}
		published, lockJSON, lock = tree, b, l
	} else {
		lock = &schema.Lock{C4: "lock/1", Images: map[string]string{}}
		pin := func(at, ref string) bool {
			if ref == "" || lock.Images[ref] != "" {
				return true
			}
			p, err := s.pinImage(ctx, m, ref)
			if err != nil {
				refuse(api.FieldError{Path: at, Message: fmt.Sprintf("%s cannot be pinned to a digest: %v", ref, err),
					Fix: "push the image to a registry the farm reaches (--hud-image-registry), or name it by digest (" + ref + "@sha256:…)"})
				return false
			}
			lock.Images[ref] = p
			return true
		}
		if !pin("runs.image", tool.Runs.Image) {
			return
		}
		for i, sc := range tool.Runs.Sidecars {
			if !pin(fmt.Sprintf("runs.sidecars[%d].image", i), sc.Image) {
				return
			}
		}
		lockJSON, _ = json.MarshalIndent(lock, "", "  ")
		lockJSON = append(lockJSON, '\n')
		if published, err = withFile(ctx, st, tree, lockFile, lockJSON, !body.DryRun); err != nil {
			storeFail(err)
			return
		}
	}
	out := api.ToolPublishResponse{OK: true, Tool: tool.Name, View: published.ID.String(),
		Image: lock.Images[tool.Runs.Image], Scope: scope, Lock: lockJSON, DryRun: body.DryRun}
	if body.DryRun {
		writeJSON(w, http.StatusOK, out)
		return
	}

	// The Tool: one name space with agent configurations. It is made only
	// once nothing is left to refuse, so a refused publish leaves none.
	var obj c4v1.Tool
	key := types.NamespacedName{Namespace: ns, Name: tool.Name}
	err = s.Client.Get(ctx, key, &obj)
	exists := err == nil
	if err != nil && !apierrors.IsNotFound(err) {
		s.Log.Error(err, "tool publish", "tool", key.String())
		fail(w, http.StatusBadGateway, "the farm could not read %s just now; try again shortly", tool.Name)
		return
	}
	if exists && agentConfig(&obj) {
		refuse(api.FieldError{Path: "name", Message: fmt.Sprintf("%s is an agent configuration on this farm; choose another name", tool.Name),
			Fix: "rename the tool in tool.yaml"})
		return
	}

	// The version, and its test.
	ver := c4v1.ToolVersion{View: published.ID.String(), Image: out.Image, Title: tool.Title, Summary: tool.Summary,
		Kind: tool.Kind, Project: project, Release: release, By: actorOf(r, v), At: metav1.Now()}
	var req *api.SubmitRequest
	switch {
	case tool.Test == nil:
		ver.Test = c4v1.ToolTest{State: c4v1.ToolTestUntested}
	default:
		// The test: the tool's test tree, its own view, as the job's tree.
		testTree, err := subtree(ctx, st, published, tool.Test.Tree)
		if err != nil {
			refuse(api.FieldError{Path: "test.tree", Message: err.Error(), Fix: "put the test's files in that folder of the tool"})
			return
		}
		ref := authoring.ToolRef{Name: tool.Name, View: published.ID.String(), Image: out.Image, Scope: scope}
		var lerrs []authoring.Error
		req, lerrs = authoring.LowerTest(tool, ref, testTree.ID.String(), project, projectNS, s.farmFor(ctx, m).Root(project))
		if len(lerrs) > 0 {
			refuse(errs(lerrs)...)
			return
		}
		req.Project, req.Name = project, s.testJobName(ctx, projectNS, tool.Name, scope, ver.View)
		ver.Test = c4v1.ToolTest{Job: projectNS + "/" + req.Name, State: c4v1.ToolTestRunning}
	}
	if !exists {
		obj = c4v1.Tool{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: tool.Name,
			Labels: map[string]string{"app.kubernetes.io/managed-by": "c4-hud"}}}
		if err = s.Client.Create(ctx, &obj); apierrors.IsAlreadyExists(err) {
			err = s.Client.Get(ctx, key, &obj)
		}
		if err != nil {
			s.Log.Error(err, "tool publish", "tool", key.String())
			fail(w, http.StatusBadGateway, "the farm could not record %s just now; try again shortly", tool.Name)
			return
		}
	}
	got, fresh, err := s.addVersion(ctx, key, ver)
	if err != nil {
		if !exists {
			s.Client.Delete(ctx, &obj) // no version, no Tool
		}
		s.Log.Error(err, "tool publish: version", "tool", key.String())
		fail(w, http.StatusBadGateway, "the farm could not record %s's version just now; try again shortly", tool.Name)
		return
	}
	out.Version, out.Release = got.Version, got.Release
	ts := testState(got.Test)
	out.Test = &ts
	if !fresh {
		// The tree is already this version: nothing new, nothing tested.
		writeJSON(w, http.StatusOK, out)
		return
	}
	if req != nil {
		_, code, err := s.createJob(r, v, m, *req, createOpts{
			Labels:      map[string]string{LabelToolTest: tool.Name},
			Annotations: map[string]string{AnnotationToolTest: got.Version}})
		if err != nil {
			test := got.Test
			test.State, test.Reason = c4v1.ToolTestFailed, "the test job could not be made: "+err.Error()
			if e := s.endTest(ctx, m, key, got.Version, test); e != nil {
				s.Log.Error(e, "tool publish: test", "tool", key.String())
			}
			fail(w, code, "%s %s: the test job could not be made: %v", tool.Name, got.Version, err)
			return
		}
	}
	note := fmt.Sprintf("%s %s published (%s), image %s", tool.Name, got.Version, published.ID, out.Image)
	if got.Release != "" {
		note += ", released " + got.Release
	}
	switch got.Test.State {
	case c4v1.ToolTestRunning:
		note += ", testing in " + got.Test.Job
	case c4v1.ToolTestUntested:
		note += ", untested"
	}
	if via(r) == "mcp" {
		note += "; via MCP"
	}
	s.record(ctx, &obj, "Tool", v.User, "Published", note, bookNote{actor: actorOf(r, v), kind: "tool-publish"})
	if got.Test.State != c4v1.ToolTestRunning {
		if err := s.offer(ctx, m, key, got); err != nil {
			s.Log.Error(err, "tool publish: offer", "tool", key.String())
			fail(w, http.StatusBadGateway, "%s %s is recorded but could not be offered just now; publish it again to retry", tool.Name, got.Version)
			return
		}
	}
	writeJSON(w, http.StatusCreated, out)
}

// addVersion records ver as the tool's next version, v<N> numbered in the
// same write so two publishes at once cannot both be v5. A tree that is
// already a version is that version, returned with fresh false; one whose
// test failed is tested again, as the same version (from Earlier, as a new
// one). Versions past MaxToolVersions move to Earlier with how their tests
// ended, failed ones too, so a failed tree stays refused by its ID; past
// MaxEarlierVersions the oldest go, and no longer resolve.
func (s *Server) addVersion(ctx context.Context, key types.NamespacedName, ver c4v1.ToolVersion) (c4v1.ToolVersion, bool, error) {
	for try := 0; ; try++ {
		var obj c4v1.Tool
		if err := s.Client.Get(ctx, key, &obj); err != nil {
			return ver, false, err
		}
		for i, e := range obj.Status.Earlier {
			if e.View != ver.View {
				continue
			}
			if e.State != c4v1.ToolTestFailed {
				return earlierVersion(e), false, nil
			}
			obj.Status.Earlier = slices.Delete(obj.Status.Earlier, i, i+1)
			break
		}
		got, again, n := ver, false, 0
		for i := range obj.Status.Versions {
			x := &obj.Status.Versions[i]
			n = max(n, versionNumber(x.Version))
			if x.View != ver.View {
				continue
			}
			if x.Test.State != c4v1.ToolTestFailed {
				return *x, false, nil
			}
			x.Test, x.By, x.At = ver.Test, ver.By, ver.At
			got, again = *x, true
		}
		if !again {
			got.Version = fmt.Sprintf("v%d", n+1)
			obj.Status.Versions = append(obj.Status.Versions, got)
			if k := len(obj.Status.Versions) - c4v1.MaxToolVersions; k > 0 {
				for _, x := range obj.Status.Versions[:k] {
					obj.Status.Earlier = append(obj.Status.Earlier, c4v1.ToolVersionRef{Version: x.Version, View: x.View,
						Image: x.Image, State: x.Test.State, Release: x.Release})
				}
				obj.Status.Versions = obj.Status.Versions[k:]
				if e := len(obj.Status.Earlier) - c4v1.MaxEarlierVersions; e > 0 {
					obj.Status.Earlier = obj.Status.Earlier[e:]
				}
			}
		}
		err := s.Client.Status().Update(ctx, &obj)
		if err == nil {
			return got, true, nil
		}
		if !apierrors.IsConflict(err) || try > 4 {
			return got, false, err
		}
	}
}

// earlierVersion is an Earlier entry as a version.
func earlierVersion(e c4v1.ToolVersionRef) c4v1.ToolVersion {
	return c4v1.ToolVersion{Version: e.Version, View: e.View, Image: e.Image, Release: e.Release, Test: c4v1.ToolTest{State: e.State}}
}

// toolSpec is the Tool spec a version declares: its tree, what its units
// reach on the network, its sidecars (images as tool.lock pins them) and
// its farm access, and nothing it does not declare, so a version never
// inherits another's or an agent configuration's. The network's licences
// are the licence servers, which go with the pools a job's licences name;
// any other name is a service the farm admin defined, in the operator's
// namespace.
func (s *Server) toolSpec(t *schema.Tool, lock *schema.Lock, view string) c4v1.ToolSpec {
	spec := c4v1.ToolSpec{View: view, C4: t.Needs.Farm}
	for _, n := range t.Needs.Network {
		if n != "licences" {
			spec.Egress = append(spec.Egress, c4v1.ToolEgress{Service: s.Namespace + "/" + n})
		}
	}
	for _, sc := range t.Runs.Sidecars {
		img := sc.Image
		if p := lock.Images[sc.Image]; p != "" {
			img = p
		}
		spec.Sidecars = append(spec.Sidecars, c4v1.ToolSidecar{Name: sc.Name, Image: img, Args: sc.Args, Env: sc.Env, Port: int32(sc.Port)})
	}
	return spec
}

// offer makes a version the one in use: the Tool's spec, every field set
// from the version, and the status fields a listing shows. A farm-wide
// version is then copied and granted into every project.
func (s *Server) offer(ctx context.Context, m *model, key types.NamespacedName, ver c4v1.ToolVersion) error {
	tt, err := s.readPublished(ctx, ver.Project, ver.View)
	if err != nil {
		return err
	}
	var obj c4v1.Tool
	if err := s.Client.Get(ctx, key, &obj); err != nil {
		return err
	}
	if versionNumber(obj.Status.Version) > versionNumber(ver.Version) {
		return nil // a later version is offered already
	}
	if spec := s.toolSpec(tt.tool, tt.lock, ver.View); !equality.Semantic.DeepEqual(obj.Spec, spec) {
		patch := client.MergeFrom(obj.DeepCopy())
		obj.Spec = spec
		if err := s.Client.Patch(ctx, &obj, patch); err != nil {
			return err
		}
	}
	if obj.Status.Version != ver.Version {
		patch := client.MergeFrom(obj.DeepCopy())
		obj.Status.Version, obj.Status.Title, obj.Status.Summary, obj.Status.Kind, obj.Status.Image =
			ver.Version, ver.Title, ver.Summary, ver.Kind, ver.Image
		if err := s.Client.Status().Patch(ctx, &obj, patch); err != nil {
			return err
		}
		note := fmt.Sprintf("%s %s offered (%s)", obj.Name, ver.Version, ver.View)
		if ver.Test.State == c4v1.ToolTestUntested {
			note += ", untested"
		}
		s.record(ctx, &obj, "Tool", "c4-hud", "Offered", note, bookNote{actor: "c4-hud", kind: "tool-offer"})
	}
	if key.Namespace == s.Namespace && m != nil {
		// What is not copied now, the HUD's loop copies (reconcileFarmTools).
		if err := s.spreadTool(ctx, m, ver); err != nil {
			s.Log.Error(err, "farm tool", "tool", obj.Name, "version", ver.Version)
		}
	}
	return nil
}

// spreadTool copies an offered farm-wide version's tree into every
// project's store, and grants it in projects that read only granted
// trees. What is done is remembered, so the HUD's loop can run it for
// projects made later at little cost.
func (s *Server) spreadTool(ctx context.Context, m *model, ver c4v1.ToolVersion) error {
	from, err := s.Stores(ctx, ver.Project)
	if err != nil || from == nil {
		return fmt.Errorf("project %s: no store: %v", ver.Project, err)
	}
	id, err := c4.Parse(ver.View)
	if err != nil {
		return err
	}
	var v *c4view.View
	for _, p := range m.projects {
		if s.spreadDone(p.Name, ver.View) {
			continue
		}
		to, err := s.Stores(ctx, p.Name)
		if err != nil || to == nil {
			return fmt.Errorf("project %s: no store: %v", p.Name, err)
		}
		if to != from {
			if v == nil {
				if v, err = c4view.Load(ctx, id, c4view.StoreRecords(from), 16); err != nil {
					return err
				}
			}
			for _, f := range v.Files {
				if _, err := to.Head(ctx, f.ID); err == nil {
					continue
				}
				r, err := from.Open(f.ID)
				if err != nil {
					return fmt.Errorf("%s: %v", f.Path, err)
				}
				err = to.Put(ctx, f.ID, r, f.Size)
				r.Close()
				if err != nil {
					return fmt.Errorf("project %s: %s: %v", p.Name, f.Path, err)
				}
			}
			if err := storeView(ctx, to, v); err != nil {
				return fmt.Errorf("project %s: %v", p.Name, err)
			}
		}
		if p.Spec.Scope == c4v1.ScopeGrants && !granted(p.Spec.Grants, ver.View) {
			var cur c4v1.Project
			if err := s.Client.Get(ctx, types.NamespacedName{Name: p.Name}, &cur); err != nil {
				return fmt.Errorf("project %s: %v", p.Name, err)
			}
			if !granted(cur.Spec.Grants, ver.View) {
				patch := client.MergeFrom(cur.DeepCopy())
				cur.Spec.Grants = append(cur.Spec.Grants, c4v1.Grant{Tree: ver.View})
				if err := s.Client.Patch(ctx, &cur, patch); err != nil {
					return fmt.Errorf("project %s: granting the tool: %v", p.Name, err)
				}
			}
		}
		s.markSpread(p.Name, ver.View)
	}
	return nil
}

func granted(gs []c4v1.Grant, tree string) bool {
	for _, g := range gs {
		if g.Tree == tree && len(g.Paths) == 0 {
			return true
		}
	}
	return false
}

func (s *Server) spreadDone(project, view string) bool {
	s.toolMu.Lock()
	defer s.toolMu.Unlock()
	return s.spread[project+" "+view]
}

func (s *Server) unmarkSpread(project, view string) {
	s.toolMu.Lock()
	defer s.toolMu.Unlock()
	delete(s.spread, project+" "+view)
}

func (s *Server) markSpread(project, view string) {
	s.toolMu.Lock()
	defer s.toolMu.Unlock()
	if s.spread == nil {
		s.spread = map[string]bool{}
	}
	s.spread[project+" "+view] = true
}

// reconcileFarmTools keeps farm-wide tools where they belong, from the
// HUD's loop: every offered version in every project, the projects made
// since included, and the grants of versions no longer offered taken
// back where no job uses them.
func (s *Server) reconcileFarmTools(ctx context.Context, m *model) {
	if s.Client == nil || s.Stores == nil || m == nil {
		return
	}
	var l c4v1.ToolList
	if err := s.Client.List(ctx, &l, client.InNamespace(s.Namespace)); err != nil {
		return
	}
	offered, old := map[string]bool{}, map[string]bool{}
	for _, t := range l.Items {
		for _, x := range t.Status.Versions {
			if x.Version != t.Status.Version {
				old[x.View] = true
				continue
			}
			offered[x.View] = true
			if err := s.spreadTool(ctx, m, x); err != nil {
				s.Log.V(1).Info("farm tool", "tool", t.Name, "version", x.Version, "error", err.Error())
			}
		}
		for _, e := range t.Status.Earlier {
			old[e.View] = true
		}
	}
	for _, p := range m.projects {
		if p.Spec.Scope != c4v1.ScopeGrants {
			continue
		}
		used := map[string]bool{}
		for _, j := range m.jobs {
			if j.project != p.Name {
				continue
			}
			for _, in := range j.job.Spec.Inputs {
				used[in.Materialization] = true
			}
		}
		drop := func(g c4v1.Grant) bool {
			return old[g.Tree] && !offered[g.Tree] && len(g.Paths) == 0 && !used[inputMaterialization(g.Tree)]
		}
		if !slices.ContainsFunc(p.Spec.Grants, drop) {
			continue
		}
		var cur c4v1.Project
		if err := s.Client.Get(ctx, types.NamespacedName{Name: p.Name}, &cur); err != nil {
			continue
		}
		patch := client.MergeFrom(cur.DeepCopy())
		for _, g := range cur.Spec.Grants {
			if drop(g) {
				s.unmarkSpread(p.Name, g.Tree)
			}
		}
		cur.Spec.Grants = slices.DeleteFunc(cur.Spec.Grants, drop)
		if err := s.Client.Patch(ctx, &cur, patch); err != nil {
			s.Log.V(1).Info("farm tool grants", "project", p.Name, "error", err.Error())
		}
	}
}

// projectTools are the Tools a project may use: its namespace's (scope
// project) and the farm's (scope farm), a project's shadowing a farm one
// of the same name. Only published tools (a version in use) count;
// agent configurations have none.
func (s *Server) projectTools(ctx context.Context, m *model, project string) (map[string]c4v1.Tool, map[string]string, error) {
	ns := ""
	for _, p := range m.projects {
		if p.Name == project && len(p.Spec.Namespaces) > 0 {
			ns = p.Spec.Namespaces[0]
		}
	}
	out, scope := map[string]c4v1.Tool{}, map[string]string{}
	for _, x := range []struct{ ns, scope string }{{s.Namespace, "farm"}, {ns, "project"}} {
		if x.ns == "" {
			continue
		}
		var l c4v1.ToolList
		if err := s.Client.List(ctx, &l, client.InNamespace(x.ns)); err != nil {
			return nil, nil, err
		}
		for _, t := range l.Items {
			if len(t.Status.Versions) == 0 {
				continue
			}
			out[t.Name], scope[t.Name] = t, x.scope
		}
	}
	return out, scope, nil
}

// toolRefs are a project's published tools at the versions in use
// (authoring.Farm's Tools).
func (s *Server) toolRefs(ctx context.Context, m *model, project string) []authoring.ToolRef {
	tools, scope, err := s.projectTools(ctx, m, project)
	if err != nil {
		return nil
	}
	var out []authoring.ToolRef
	for name, t := range tools {
		if t.Status.Version == "" {
			continue
		}
		out = append(out, authoring.ToolRef{Name: name, Version: t.Status.Version, View: t.Spec.View, Image: t.Status.Image, Scope: scope[name]})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// findVersion is a Tool's version v<N>, or the version whose tree is a C4
// ID, from its versions or the earlier ones; nil when it has none.
func findVersion(t *c4v1.Tool, version string) *c4v1.ToolVersion {
	byView := strings.HasPrefix(version, "c4")
	for i := range t.Status.Versions {
		if x := &t.Status.Versions[i]; x.Version == version || (byView && x.View == version) {
			return x
		}
	}
	for _, e := range t.Status.Earlier {
		if e.Version == version || (byView && e.View == version) {
			x := earlierVersion(e)
			return &x
		}
	}
	return nil
}

// offered says whether a version may run: its test passed, or it is a
// person's tool offered untested.
func offered(ver *c4v1.ToolVersion) bool {
	return ver.Test.State == c4v1.ToolTestOK || ver.Test.State == c4v1.ToolTestUntested
}

// toolAt is a published tool at a version, read from its tree in the
// project's store (authoring.Farm's Tool): "" for the one in use, or v<N>
// or a tree's C4 ID for any version offered, the earlier ones too, so a
// job that ran one can run it again. Only a tree that was a version of
// the tool resolves, and only one that passed its test or was offered
// untested: a tree with the tool's name that was never published, or
// whose test failed, is refused.
func (s *Server) toolAt(ctx context.Context, m *model, project, name, version string) (*schema.Tool, authoring.ToolRef, error) {
	tools, scope, err := s.projectTools(ctx, m, project)
	if err != nil {
		return nil, authoring.ToolRef{}, err
	}
	t, ok := tools[name]
	if !ok {
		return nil, authoring.ToolRef{}, fmt.Errorf("%w: %s in %s", authoring.ErrNoTool, name, project)
	}
	if version == "" {
		if t.Status.Version == "" {
			return nil, authoring.ToolRef{}, fmt.Errorf("%w: %s has no version offered in %s yet", authoring.ErrNoTool, name, project)
		}
		version = t.Status.Version
	}
	ver := findVersion(&t, version)
	if ver == nil {
		return nil, authoring.ToolRef{}, fmt.Errorf("%w: %s has no %s in %s", authoring.ErrNoTool, name, version, project)
	}
	if !offered(ver) {
		return nil, authoring.ToolRef{}, fmt.Errorf("%w: %s %s is not offered (its test is %s)", authoring.ErrNoTool, name, ver.Version, ver.Test.State)
	}
	ref := authoring.ToolRef{Name: name, Version: ver.Version, View: ver.View, Image: ver.Image, Scope: scope[name]}
	tt, err := s.readPublished(ctx, project, ver.View)
	if err != nil {
		return nil, ref, err
	}
	return tt.tool, ref, nil
}

// toolTree is a published tree as read: its tool.yaml, parsed and as
// written, and its tool.lock.
type toolTree struct {
	tool *schema.Tool
	doc  []byte
	lock *schema.Lock
}

// readPublished reads a published tree from a project's store.
func (s *Server) readPublished(ctx context.Context, project, view string) (*toolTree, error) {
	if s.Stores == nil {
		return nil, errors.New("the farm has no stores")
	}
	st, err := s.Stores(ctx, project)
	if err != nil || st == nil {
		return nil, fmt.Errorf("%s's store cannot be reached just now", project)
	}
	id, err := c4.Parse(view)
	if err != nil {
		return nil, fmt.Errorf("%q is not a C4 ID", view)
	}
	tree, err := c4view.Load(ctx, id, c4view.StoreRecords(st), 16)
	if err != nil {
		return nil, fmt.Errorf("%s's store does not hold the tree %s", project, view)
	}
	doc, ok, err := readFile(ctx, st, tree, toolFile, 1<<20)
	if err != nil || !ok {
		return nil, fmt.Errorf("the tree %s has no readable %s", view, toolFile)
	}
	tool, perr := schema.ParseTool(doc)
	if len(perr) > 0 {
		return nil, fmt.Errorf("the tree %s: %s", view, perr[0].Message)
	}
	out := &toolTree{tool: tool, doc: doc, lock: &schema.Lock{}}
	if b, ok, err := readFile(ctx, st, tree, lockFile, 1<<20); err == nil && ok {
		if l, lerr := schema.ParseLock(b); len(lerr) == 0 {
			out.lock = l
		}
	}
	return out, nil
}

// readTool is the tool.yaml of a published tree in a project's store.
func (s *Server) readTool(ctx context.Context, project, view string) (*schema.Tool, error) {
	tt, err := s.readPublished(ctx, project, view)
	if err != nil {
		return nil, err
	}
	return tt.tool, nil
}

// handleTools is GET /tools/{project}.
func (s *Server) handleTools(w http.ResponseWriter, r *http.Request) {
	v := s.withViewer(w, r)
	if v == nil {
		return
	}
	m := s.current()
	project := r.PathValue("project")
	if m == nil || !v.Sees(project) {
		fail(w, http.StatusNotFound, "no project %s", project)
		return
	}
	tools, scope, err := s.projectTools(r.Context(), m, project)
	if err != nil {
		s.Log.Error(err, "tools", "project", project)
		fail(w, http.StatusBadGateway, "the farm's tools cannot be read just now; try again shortly")
		return
	}
	out := api.ToolListResponse{Tools: []api.ToolSummary{}}
	for name, t := range tools {
		if t.Status.Version == "" {
			continue // nothing offered yet
		}
		out.Tools = append(out.Tools, summary(t, scope[name]))
	}
	sort.Slice(out.Tools, func(i, j int) bool { return out.Tools[i].Name < out.Tools[j].Name })
	writeJSON(w, http.StatusOK, out)
}

func summary(t c4v1.Tool, scope string) api.ToolSummary {
	out := api.ToolSummary{Name: t.Name, Title: t.Status.Title, Summary: t.Status.Summary, Kind: t.Status.Kind,
		Version: t.Status.Version, View: t.Spec.View, Image: t.Status.Image, Scope: scope}
	if ver := findVersion(&t, t.Status.Version); ver != nil {
		out.Untested = ver.Test.State == c4v1.ToolTestUntested
	}
	return out
}

func testState(t c4v1.ToolTest) api.ToolTestState {
	return api.ToolTestState{Job: t.Job, State: t.State, Seconds: t.Seconds, Reason: t.Reason}
}

// handleTool is GET /tools/{project}/{tool}[?version=v<N>|c4…].
func (s *Server) handleTool(w http.ResponseWriter, r *http.Request) {
	v := s.withViewer(w, r)
	if v == nil {
		return
	}
	m := s.current()
	project, name := r.PathValue("project"), r.PathValue("tool")
	if m == nil || !v.Sees(project) {
		fail(w, http.StatusNotFound, "no project %s", project)
		return
	}
	ctx := r.Context()
	tools, scope, err := s.projectTools(ctx, m, project)
	if err != nil {
		s.Log.Error(err, "tools", "project", project)
		fail(w, http.StatusBadGateway, "the farm's tools cannot be read just now; try again shortly")
		return
	}
	t, ok := tools[name]
	if !ok {
		fail(w, http.StatusNotFound, "no tool %s in %s", name, project)
		return
	}
	version := r.URL.Query().Get("version")
	if version == "" {
		version = t.Status.Version
	}
	ver := findVersion(&t, version)
	if ver == nil {
		fail(w, http.StatusNotFound, "tool %s has no version %s", name, orElse(version, "offered yet"))
		return
	}
	out := api.ToolDetail{ToolSummary: summary(t, scope[name])}
	out.Version, out.View, out.Image = ver.Version, ver.View, ver.Image
	if ver.Title != "" || ver.Kind != "" {
		out.Title, out.Summary, out.Kind = ver.Title, ver.Summary, ver.Kind
	}
	out.Untested = ver.Test.State == c4v1.ToolTestUntested
	ts := testState(ver.Test)
	out.Test = &ts
	for _, x := range t.Status.Versions {
		out.Versions = append(out.Versions, api.ToolVersionInfo{Version: x.Version, View: x.View, Image: x.Image, By: x.By,
			At: x.At.UTC().Format(time.RFC3339), Test: testState(x.Test)})
	}
	for _, e := range t.Status.Earlier {
		x := earlierVersion(e)
		out.Earlier = append(out.Earlier, api.ToolVersionInfo{Version: x.Version, View: x.View, Image: x.Image, Test: testState(x.Test)})
	}
	tt, err := s.readPublished(ctx, project, ver.View)
	if err != nil {
		fail(w, http.StatusBadGateway, "%s %s: %v", name, ver.Version, err)
		return
	}
	tool := tt.tool
	if out.Kind == "" {
		out.Title, out.Summary, out.Kind = tool.Title, tool.Summary, tool.Kind
	}
	out.Tool, _ = schema.ToJSON(tt.doc)
	out.Params, _ = json.Marshal(tool.Params)
	out.ParamSchema = paramSchema(tool.Params)
	if tool.Frames != nil {
		out.Frames, _ = json.Marshal(tool.Frames)
	}
	if b, _ := json.Marshal(tool.Needs); string(b) != "{}" {
		out.Needs = b
	}
	writeJSON(w, http.StatusOK, out)
}

// rangeExpr is an OpenJD RANGE_EXPR as a pattern: frames and ranges with
// an optional step, comma-separated (1-100, 1001-1100:5,1200).
const rangeExpr = `^\s*-?[0-9]+(\s*-\s*-?[0-9]+(\s*:\s*-?[0-9]+)?)?(\s*,\s*-?[0-9]+(\s*-\s*-?[0-9]+(\s*:\s*-?[0-9]+)?)?)*\s*$`

// paramSchema is the JSON Schema of a submission's `with` for a tool's
// params, in Open Job Description's types. A param is required when it
// has no default (a from fills a form, not a submission); x-show and
// x-from carry the tool's show and from for a form to draw.
func paramSchema(params []schema.Param) map[string]any {
	props := map[string]any{}
	var required []string
	for i := range params {
		p := &params[i]
		s := map[string]any{}
		switch p.Type {
		case "INT":
			s["type"] = "integer"
		case "FLOAT":
			s["type"] = "number"
		case "BOOL":
			s["type"] = "boolean"
		case "RANGE_EXPR":
			s["type"], s["pattern"] = "string", rangeExpr
		case "LIST[STRING]", "LIST[PATH]":
			s["type"], s["items"] = "array", map[string]any{"type": "string"}
		case "LIST[INT]":
			s["type"], s["items"] = "array", map[string]any{"type": "integer"}
		case "LIST[FLOAT]":
			s["type"], s["items"] = "array", map[string]any{"type": "number"}
		default: // STRING, PATH
			s["type"] = "string"
		}
		if p.Description != "" {
			s["description"] = p.Description
		}
		if p.Default != nil {
			s["default"] = p.Default
		}
		if len(p.AllowedValues) > 0 {
			s["enum"] = p.AllowedValues
		}
		if p.MinValue != nil {
			s["minimum"] = *p.MinValue
		}
		if p.MaxValue != nil {
			s["maximum"] = *p.MaxValue
		}
		if p.MinLength != nil {
			s["minLength"] = *p.MinLength
		}
		if p.MaxLength != nil {
			s["maxLength"] = *p.MaxLength
		}
		if p.Show != "" {
			s["x-show"] = p.Show
		}
		if p.From != "" {
			s["x-from"] = p.From
		}
		props[p.Name] = s
		// A parameter filled from upstream (from) is given by the job it
		// follows, not by whoever submits.
		if p.Required() && p.From == "" {
			required = append(required, p.Name)
		}
	}
	out := map[string]any{"$schema": "https://json-schema.org/draft/2020-12/schema", "type": "object",
		"properties": props, "additionalProperties": false}
	if len(required) > 0 {
		out["required"] = required
	}
	return out
}

// handleSchema is GET /schema/{format}: the submit/1, tool/1 and lock/1
// JSON Schemas.
func (s *Server) handleSchema(w http.ResponseWriter, r *http.Request) {
	var b []byte
	switch r.PathValue("format") {
	case "submit":
		b = schema.SubmitSchema()
	case "tool":
		b = schema.ToolSchema()
	case "lock":
		b = schema.LockSchema()
	default:
		fail(w, http.StatusNotFound, "no schema %s: there are submit, tool and lock", r.PathValue("format"))
		return
	}
	w.Header().Set("Content-Type", "application/schema+json")
	w.Write(b)
}

// settleToolTests ends the tests of published versions whose jobs have
// finished: a test passes when its job succeeded and every path its
// expect lists is in the job's output, and the version is then offered.
// Run from the HUD's loop; every replica may, and an update is safe to
// repeat.
func (s *Server) settleToolTests(ctx context.Context, m *model) {
	if s.Client == nil || m == nil {
		return
	}
	var l c4v1.ToolList
	if err := s.Client.List(ctx, &l); err != nil {
		return
	}
	for _, t := range l.Items {
		for _, ver := range t.Status.Versions {
			if ver.Test.State != c4v1.ToolTestRunning || ver.Test.Job == "" {
				continue
			}
			test, done := s.testOutcome(ctx, m, t, ver)
			if !done {
				continue
			}
			if err := s.endTest(ctx, m, types.NamespacedName{Namespace: t.Namespace, Name: t.Name}, ver.Version, test); err != nil {
				s.Log.Error(err, "tool test", "tool", t.Namespace+"/"+t.Name, "version", ver.Version)
			}
		}
	}
}

// testOutcome is how a version's test job went, once it has finished.
func (s *Server) testOutcome(ctx context.Context, m *model, t c4v1.Tool, ver c4v1.ToolVersion) (c4v1.ToolTest, bool) {
	test := ver.Test
	ns, name, _ := strings.Cut(test.Job, "/")
	var job c4v1.WorkJob
	if err := s.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &job); err != nil {
		if apierrors.IsNotFound(err) {
			test.State, test.Reason = c4v1.ToolTestFailed, "the test job was deleted before it finished"
			return test, true
		}
		return test, false
	}
	end := time.Now()
	if job.Status.Finished != nil {
		end = job.Status.Finished.Time
	}
	test.Seconds = int64(end.Sub(job.CreationTimestamp.Time).Seconds())
	switch job.Status.Phase {
	case "Succeeded":
	case "Failed":
		test.State, test.Reason = c4v1.ToolTestFailed, "the test job failed"
		if c := failedReason(&job); c != "" {
			test.Reason += ": " + c
		}
		return test, true
	default:
		return ver.Test, false
	}
	project := orElse(ver.Project, m.nsProject[ns])
	tool, err := s.readTool(ctx, project, ver.View)
	if err != nil {
		test.State, test.Reason = c4v1.ToolTestFailed, err.Error()
		return test, true
	}
	if tool.Test == nil || len(tool.Test.Expect) == 0 {
		test.State = c4v1.ToolTestOK
		return test, true
	}
	if job.Status.Output == nil || job.Status.Output.View == "" {
		test.State, test.Reason = c4v1.ToolTestFailed, "the test job succeeded without an output"
		return test, true
	}
	st, err := s.Stores(ctx, m.nsProject[ns])
	if err != nil || st == nil {
		return ver.Test, false
	}
	oid, err := c4.Parse(job.Status.Output.View)
	if err != nil {
		test.State, test.Reason = c4v1.ToolTestFailed, "the test job's output is not a view"
		return test, true
	}
	out, err := c4view.Load(ctx, oid, c4view.StoreRecords(st), 16)
	if err != nil {
		return ver.Test, false
	}
	have := map[string]bool{}
	for _, f := range out.Files {
		have[f.Path] = true
	}
	var missing []string
	for _, p := range tool.Test.Expect {
		if !have[strings.TrimPrefix(path.Clean("/"+p), "/")] {
			missing = append(missing, p)
		}
	}
	if len(missing) > 0 {
		test.State, test.Reason = c4v1.ToolTestFailed, "the test did not write "+strings.Join(missing, ", ")
		return test, true
	}
	test.State = c4v1.ToolTestOK
	return test, true
}

// failedReason is what a failed job's conditions say.
func failedReason(j *c4v1.WorkJob) string {
	for _, c := range j.Status.Conditions {
		if c.Status == metav1.ConditionTrue && c.Message != "" && (c.Type == "NeedsPerson" || c.Type == "Failed") {
			return c.Message
		}
	}
	return ""
}

// endTest records a version's test result in its status and the book,
// and offers the version when it passed.
func (s *Server) endTest(ctx context.Context, m *model, key types.NamespacedName, version string, test c4v1.ToolTest) error {
	var ver c4v1.ToolVersion
	var obj c4v1.Tool
	for try := 0; ; try++ {
		if err := s.Client.Get(ctx, key, &obj); err != nil {
			return err
		}
		found := false
		for i := range obj.Status.Versions {
			if obj.Status.Versions[i].Version == version && obj.Status.Versions[i].Test.State == c4v1.ToolTestRunning {
				obj.Status.Versions[i].Test = test
				ver, found = obj.Status.Versions[i], true
			}
		}
		if !found {
			return nil // another replica ended it
		}
		err := s.Client.Status().Update(ctx, &obj)
		if err == nil {
			break
		}
		if !apierrors.IsConflict(err) || try > 4 {
			return err
		}
	}
	note := fmt.Sprintf("%s %s passed its test (%s)", key.Name, version, test.Job)
	reason := "TestPassed"
	if test.State != c4v1.ToolTestOK {
		note = fmt.Sprintf("%s %s failed its test (%s): %s", key.Name, version, orElse(test.Job, "no job"), test.Reason)
		reason = "TestFailed"
	}
	s.record(ctx, &obj, "Tool", "c4-hud", reason, note, bookNote{actor: "c4-hud", kind: "tool-test"})
	if test.State != c4v1.ToolTestOK {
		return nil
	}
	return s.offer(ctx, m, key, ver)
}

// orElse is s, or d when s is empty.
func orElse(s, d string) string {
	if s == "" {
		return d
	}
	return s
}
