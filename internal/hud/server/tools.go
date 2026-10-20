package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

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

// Tools on the farm (authoring slice 1, item 1.3; TOOL.md sections 5 and
// 9). Publishing a folder with a tool.yaml checks it, pins its images by
// digest into tool.lock (so the published tree's ID covers the exact
// image), records a version, and runs the tool's test as a small job.
// The version is offered (the Tool's spec.view moves to it) only once its
// test passes: a tool whose test fails on the farm is not published. A
// project's tools live in its namespace, the farm's in the operator's;
// a farm-wide tool's tree is copied into every project's store, and
// granted where a project reads only granted trees, because a unit mounts
// only what its own project's store holds (DESIGN 6.2).

const (
	toolFile = "tool.yaml"
	lockFile = "tool.lock"
	// LabelToolTest on a test job names the tool it tests, and
	// AnnotationToolTest the version.
	LabelToolTest      = "c4.avalanche.io/tool-test"
	AnnotationToolTest = "c4.avalanche.io/tool-test-version"
)

// releaseRegistry is where schema.ImageRef puts the farm's own images.
const releaseRegistry = "ghcr.io/avalanche-io/"

// resolveImage is a tool's image as the farm names it: c4/<name> (the
// farm's own images) is expanded as schema.ImageRef does, at ImageTag
// unless the short name gives a tag, and from the farm's ImageRegistry
// (none on a farm whose images are loaded onto its nodes); any other
// reference is the tool's own.
func (s *Server) resolveImage(ref string) string {
	out := schema.ImageRef(ref, orElse(s.ImageTag, "latest"))
	if out == ref || !strings.HasPrefix(out, releaseRegistry) {
		return out
	}
	reg := strings.TrimSuffix(s.ImageRegistry, "/")
	if reg+"/" == releaseRegistry {
		return out
	}
	rest := strings.TrimPrefix(out, releaseRegistry)
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
	project, projectNS, err := s.project(m, v, r.PathValue("project"), "")
	ns := projectNS
	if err != nil || s.Stores == nil {
		fail(w, http.StatusNotFound, "no project %s", r.PathValue("project"))
		return
	}
	scope := "project"
	if body.TestedIn != "" && v.Role != "farm-admin" {
		fail(w, http.StatusForbidden, "only the farm admin (c4 farm up) publishes a tool as tested in a release")
		return
	}
	if body.Farm {
		if v.Role != "farm-admin" {
			fail(w, http.StatusForbidden, "publishing a tool to the whole farm takes the farm-admin role")
			return
		}
		ns, scope = s.Namespace, "farm"
	}
	ctx := r.Context()
	if err := s.authorize(ctx, r, v, ns, "create", "tools", ""); err != nil {
		fail(w, http.StatusForbidden, "%v", err)
		return
	}
	st, err := s.Stores(ctx, project)
	if err != nil || st == nil {
		fail(w, http.StatusServiceUnavailable, "the project's store is not available: %v", err)
		return
	}
	id, err := c4.Parse(body.View)
	if err != nil {
		fail(w, http.StatusBadRequest, "view %q is not a C4 ID", body.View)
		return
	}
	tree, err := c4view.Load(ctx, id, c4view.StoreRecords(st), 16)
	if err != nil {
		fail(w, http.StatusNotFound, "no view %s in %s's store: %v", id, project, err)
		return
	}
	refuse := func(e ...api.FieldError) {
		writeJSON(w, http.StatusUnprocessableEntity, api.ToolPublishResponse{Errors: e})
	}
	doc, ok, err := readFile(ctx, st, tree, toolFile, 1<<20)
	switch {
	case err != nil:
		fail(w, http.StatusBadGateway, "reading %s: %v", toolFile, err)
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
	// The images, pinned.
	lock := schema.Lock{C4: "lock/1", Images: map[string]string{}}
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
	lockJSON, _ := json.MarshalIndent(lock, "", "  ")
	lockJSON = append(lockJSON, '\n')
	published, err := withFile(ctx, st, tree, lockFile, lockJSON, !body.DryRun)
	if err != nil {
		fail(w, http.StatusBadGateway, "store: %v", err)
		return
	}
	out := api.ToolPublishResponse{OK: true, Tool: tool.Name, View: published.ID.String(),
		Image: lock.Images[tool.Runs.Image], Scope: scope, Lock: lockJSON, DryRun: body.DryRun}
	if body.DryRun {
		writeJSON(w, http.StatusOK, out)
		return
	}
	if body.Farm {
		if err := s.spreadTool(ctx, m, st, published); err != nil {
			fail(w, http.StatusBadGateway, "copying the tool into the projects' stores: %v", err)
			return
		}
	}

	// The version, recorded before its test runs.
	var obj c4v1.Tool
	key := types.NamespacedName{Namespace: ns, Name: tool.Name}
	if err := s.Client.Get(ctx, key, &obj); apierrors.IsNotFound(err) {
		obj = c4v1.Tool{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: tool.Name,
			Labels: map[string]string{"app.kubernetes.io/managed-by": "c4-hud"}}}
		if err := s.Client.Create(ctx, &obj); err != nil {
			fail(w, http.StatusConflict, "tool %s: %v", tool.Name, err)
			return
		}
	} else if err != nil {
		fail(w, http.StatusBadGateway, "tool %s: %v", tool.Name, err)
		return
	}
	n := 0
	for _, x := range obj.Status.Versions {
		n = max(n, versionNumber(x.Version))
	}
	version := fmt.Sprintf("v%d", n+1)
	out.Version = version
	ver := c4v1.ToolVersion{Version: version, View: published.ID.String(), Image: out.Image, Title: tool.Title,
		Summary: tool.Summary, Kind: tool.Kind, By: actorOf(r, v), At: metav1.Now(), Test: c4v1.ToolTest{State: c4v1.ToolTestOK}}
	if body.TestedIn != "" {
		// A release's own tool, published by c4 farm up from the folders
		// the release embeds: its release's tests ran it, the farm runs
		// none.
		ver.Test.TestedIn = body.TestedIn
	} else if tool.Test != nil {
		// The test: the tool's test tree, its own view, as the job's tree.
		testTree, err := subtree(ctx, st, published, tool.Test.Tree)
		if err != nil {
			refuse(api.FieldError{Path: "test.tree", Message: err.Error(), Fix: "put the test's files in that folder of the tool"})
			return
		}
		ref := authoring.ToolRef{Name: tool.Name, Version: version, View: published.ID.String(), Image: out.Image, Scope: scope}
		root := c4v1.DefaultProjectRoot
		for _, p := range m.projects {
			if p.Name == project {
				root = p.Spec.TreeRoot()
			}
		}
		req, err := authoring.LowerTest(tool, ref, testTree.ID.String(), project, projectNS, root)
		if err != nil {
			refuse(api.FieldError{Path: "test", Message: err.Error(), Fix: "the test lowers as a submission's job would; fix it as the message says"})
			return
		}
		req.Project = project
		resp, code, err := s.createJob(r, v, m, *req, createOpts{
			Labels:      map[string]string{LabelToolTest: tool.Name},
			Annotations: map[string]string{AnnotationToolTest: version}})
		if err != nil {
			fail(w, code, "the test job: %v", err)
			return
		}
		ver.Test = c4v1.ToolTest{Job: resp.Namespace + "/" + resp.Name, State: c4v1.ToolTestRunning}
	}
	if err := s.addVersion(ctx, key, ver); err != nil {
		fail(w, http.StatusBadGateway, "tool %s: %v", tool.Name, err)
		return
	}
	out.Test = &api.ToolTestState{Job: ver.Test.Job, State: ver.Test.State, TestedIn: ver.Test.TestedIn}
	note := fmt.Sprintf("%s %s published (%s), image %s", tool.Name, version, published.ID, out.Image)
	switch {
	case ver.Test.Job != "":
		note += ", testing in " + ver.Test.Job
	case ver.Test.TestedIn != "":
		note += ", tested in release " + ver.Test.TestedIn
	}
	if via(r) == "mcp" {
		note += "; via MCP"
	}
	s.record(ctx, &obj, "Tool", v.User, "Published", note, bookNote{actor: actorOf(r, v), kind: "tool-publish"})
	writeJSON(w, http.StatusCreated, out)
}

// addVersion appends a version to a Tool's status, and offers it at once
// when it has no test to pass.
func (s *Server) addVersion(ctx context.Context, key types.NamespacedName, ver c4v1.ToolVersion) error {
	for try := 0; ; try++ {
		var obj c4v1.Tool
		if err := s.Client.Get(ctx, key, &obj); err != nil {
			return err
		}
		obj.Status.Versions = append(obj.Status.Versions, ver)
		if n := len(obj.Status.Versions); n > c4v1.MaxToolVersions {
			obj.Status.Versions = obj.Status.Versions[n-c4v1.MaxToolVersions:]
		}
		err := s.Client.Status().Update(ctx, &obj)
		if err == nil && ver.Test.State == c4v1.ToolTestOK {
			return s.offer(ctx, key, ver)
		}
		if err == nil || !apierrors.IsConflict(err) || try > 4 {
			return err
		}
	}
}

// offer makes a version the one in use: spec.view, and the status fields
// a listing shows.
func (s *Server) offer(ctx context.Context, key types.NamespacedName, ver c4v1.ToolVersion) error {
	var obj c4v1.Tool
	if err := s.Client.Get(ctx, key, &obj); err != nil {
		return err
	}
	if versionNumber(obj.Status.Version) > versionNumber(ver.Version) {
		return nil // a later version already passed
	}
	if obj.Spec.View != ver.View {
		patch := client.MergeFrom(obj.DeepCopy())
		obj.Spec.View = ver.View
		if err := s.Client.Patch(ctx, &obj, patch); err != nil {
			return err
		}
	}
	patch := client.MergeFrom(obj.DeepCopy())
	obj.Status.Version, obj.Status.Title, obj.Status.Summary, obj.Status.Kind, obj.Status.Image =
		ver.Version, ver.Title, ver.Summary, ver.Kind, ver.Image
	return s.Client.Status().Patch(ctx, &obj, patch)
}

// spreadTool copies a farm-wide tool's tree into every project's store,
// and grants it in projects that read only granted trees.
func (s *Server) spreadTool(ctx context.Context, m *model, from *storeurl.Sharded, v *c4view.View) error {
	for _, p := range m.projects {
		to, err := s.Stores(ctx, p.Name)
		if err != nil || to == nil {
			return fmt.Errorf("project %s: no store: %v", p.Name, err)
		}
		if to != from {
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
		if p.Spec.Scope != c4v1.ScopeGrants {
			continue
		}
		var cur c4v1.Project
		if err := s.Client.Get(ctx, types.NamespacedName{Name: p.Name}, &cur); err != nil {
			return fmt.Errorf("project %s: %v", p.Name, err)
		}
		granted := false
		for _, g := range cur.Spec.Grants {
			granted = granted || (g.Tree == v.ID.String() && len(g.Paths) == 0)
		}
		if granted {
			continue
		}
		patch := client.MergeFrom(cur.DeepCopy())
		cur.Spec.Grants = append(cur.Spec.Grants, c4v1.Grant{Tree: v.ID.String()})
		if err := s.Client.Patch(ctx, &cur, patch); err != nil {
			return fmt.Errorf("project %s: granting the tool: %v", p.Name, err)
		}
	}
	return nil
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

// toolAt is a published tool at a version ("" for the one in use), read
// from its tree in the project's store (authoring.Farm's Tool).
func (s *Server) toolAt(ctx context.Context, m *model, project, name, version string) (*schema.Tool, authoring.ToolRef, error) {
	tools, scope, err := s.projectTools(ctx, m, project)
	if err != nil {
		return nil, authoring.ToolRef{}, err
	}
	t, ok := tools[name]
	if !ok {
		return nil, authoring.ToolRef{}, fmt.Errorf("%w: %s in %s", authoring.ErrNoTool, name, project)
	}
	var ver *c4v1.ToolVersion
	for i := range t.Status.Versions {
		x := &t.Status.Versions[i]
		if (version == "" && x.Version == t.Status.Version) || x.Version == version {
			ver = x
		}
	}
	if ver == nil || (version == "" && t.Status.Version == "") {
		return nil, authoring.ToolRef{}, fmt.Errorf("%w: %s has no %s in %s", authoring.ErrNoTool, name, orElse(version, "passing version"), project)
	}
	if version != "" && ver.Test.State != c4v1.ToolTestOK {
		return nil, authoring.ToolRef{}, fmt.Errorf("%w: %s %s is not published (its test is %s)", authoring.ErrNoTool, name, version, ver.Test.State)
	}
	ref := authoring.ToolRef{Name: name, Version: ver.Version, View: ver.View, Image: ver.Image, Scope: scope[name]}
	tool, err := s.readTool(ctx, project, ver.View)
	return tool, ref, err
}

// readTool parses the tool.yaml of a published tree in a project's store.
func (s *Server) readTool(ctx context.Context, project, view string) (*schema.Tool, error) {
	if s.Stores == nil {
		return nil, errors.New("no stores")
	}
	st, err := s.Stores(ctx, project)
	if err != nil || st == nil {
		return nil, fmt.Errorf("project %s: no store: %v", project, err)
	}
	id, err := c4.Parse(view)
	if err != nil {
		return nil, err
	}
	tree, err := c4view.Load(ctx, id, c4view.StoreRecords(st), 16)
	if err != nil {
		return nil, err
	}
	doc, ok, err := readFile(ctx, st, tree, toolFile, 1<<20)
	if err != nil || !ok {
		return nil, fmt.Errorf("tree %s: no %s: %v", view, toolFile, err)
	}
	tool, perr := schema.ParseTool(doc)
	if len(perr) > 0 {
		return nil, fmt.Errorf("tree %s: %s", view, perr[0].Message)
	}
	return tool, nil
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
		fail(w, http.StatusBadGateway, "%v", err)
		return
	}
	out := api.ToolListResponse{Tools: []api.ToolSummary{}}
	for name, t := range tools {
		if t.Status.Version == "" {
			continue // nothing passed yet: not offered
		}
		out.Tools = append(out.Tools, summary(t, scope[name]))
	}
	sort.Slice(out.Tools, func(i, j int) bool { return out.Tools[i].Name < out.Tools[j].Name })
	writeJSON(w, http.StatusOK, out)
}

func summary(t c4v1.Tool, scope string) api.ToolSummary {
	return api.ToolSummary{Name: t.Name, Title: t.Status.Title, Summary: t.Status.Summary, Kind: t.Status.Kind,
		Version: t.Status.Version, View: t.Spec.View, Image: t.Status.Image, Scope: scope}
}

func testState(t c4v1.ToolTest) api.ToolTestState {
	return api.ToolTestState{Job: t.Job, State: t.State, Seconds: t.Seconds, Reason: t.Reason, TestedIn: t.TestedIn}
}

// handleTool is GET /tools/{project}/{tool}[?version=].
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
		fail(w, http.StatusBadGateway, "%v", err)
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
	var ver *c4v1.ToolVersion
	for i := range t.Status.Versions {
		if t.Status.Versions[i].Version == version {
			ver = &t.Status.Versions[i]
		}
	}
	if ver == nil {
		fail(w, http.StatusNotFound, "tool %s has no version %s", name, orElse(version, "that passed its test"))
		return
	}
	out := api.ToolDetail{ToolSummary: summary(t, scope[name])}
	out.Version, out.View, out.Image = ver.Version, ver.View, ver.Image
	out.Title, out.Summary, out.Kind = ver.Title, ver.Summary, ver.Kind
	ts := testState(ver.Test)
	out.Test = &ts
	for _, x := range t.Status.Versions {
		out.Versions = append(out.Versions, api.ToolVersionInfo{Version: x.Version, View: x.View, Image: x.Image, By: x.By,
			At: x.At.UTC().Format(time.RFC3339), Test: testState(x.Test)})
	}
	tool, err := s.readTool(ctx, project, ver.View)
	if err != nil {
		fail(w, http.StatusBadGateway, "%v", err)
		return
	}
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

// paramSchema is the JSON Schema of a submission's `with` for a tool's
// params, in Open Job Description's types.
func paramSchema(params []schema.Param) map[string]any {
	props := map[string]any{}
	var required []string
	for _, p := range params {
		s := map[string]any{}
		switch p.Type {
		case "INT":
			s["type"] = "integer"
		case "FLOAT":
			s["type"] = "number"
		case "BOOL":
			s["type"] = "boolean"
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
		props[p.Name] = s
		if p.Default == nil && p.From == "" {
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
			if err := s.endTest(ctx, types.NamespacedName{Namespace: t.Namespace, Name: t.Name}, ver.Version, test); err != nil {
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
	tool, err := s.readTool(ctx, m.nsProject[ns], ver.View)
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

// endTest records a version's test result, and offers the version when
// it passed.
func (s *Server) endTest(ctx context.Context, key types.NamespacedName, version string, test c4v1.ToolTest) error {
	var ver c4v1.ToolVersion
	for try := 0; ; try++ {
		var obj c4v1.Tool
		if err := s.Client.Get(ctx, key, &obj); err != nil {
			return err
		}
		found := false
		for i := range obj.Status.Versions {
			if obj.Status.Versions[i].Version == version && obj.Status.Versions[i].Test.State == c4v1.ToolTestRunning {
				obj.Status.Versions[i].Test, ver, found = test, obj.Status.Versions[i], true
				ver.Test = test
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
	if test.State != c4v1.ToolTestOK {
		return nil
	}
	return s.offer(ctx, key, ver)
}

// orElse is s, or d when s is empty.
func orElse(s, d string) string {
	if s == "" {
		return d
	}
	return s
}
