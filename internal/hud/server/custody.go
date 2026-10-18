package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/types"

	"github.com/Avalanche-io/c4"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/hud/api"
	"github.com/Avalanche-io/c4-operator/internal/provenance"
	"github.com/Avalanche-io/c4-operator/internal/work/queue"
)

// Control records (DESIGN.md section 9.2): every action a person or agent
// takes through the HUD or MCP is signed by the HUD, which authenticated
// and authorized it, before the write that carries it out. The record
// names the actor, how they signed in, the client (the HUD or an MCP
// agent), the authorization check, and the change. The write adds the
// record's ID to the object's control-records annotation, so the object
// and its record change together.

// maxControlRecords bounds the IDs an object's annotation keeps.
const maxControlRecords = 16

// controlPredicate is the HUD's control record.
type controlPredicate struct {
	Actor         string `json:"actor"`
	Authenticated string `json:"authenticated"`
	Via           string `json:"via"`
	Authorization string `json:"authorization"`
	Action        string `json:"action"`
	Target        string `json:"target"`
	Change        any    `json:"change"`
}

// via names the client: the MCP server says so in X-C4-Client; anything
// else is the web app. The header is the client's claim; the actor is not.
func via(r *http.Request) string {
	if strings.EqualFold(r.Header.Get("X-C4-Client"), "mcp") {
		return "mcp"
	}
	return "hud"
}

func (s *Server) authenticatedBy(r *http.Request) string {
	switch {
	case s.DevViewers != nil:
		return "development viewer (?as=), not authenticated"
	case s.TrustProxy && r.Header.Get("X-Forwarded-User") != "":
		return "SSO proxy (X-Forwarded-User)"
	}
	return "bearer token (TokenReview)"
}

func (s *Server) authorization(verb, resource, ns, name string) string {
	if s.DevViewers != nil {
		return "development mode: not checked"
	}
	target := name
	if ns != "" {
		target = ns + "/" + name
	}
	return fmt.Sprintf("SubjectAccessReview: %s %s %s allowed", verb, resource, target)
}

// signControl signs a control record and stores it in each project's
// store. It returns "" when the HUD has no signer.
func (s *Server) signControl(ctx context.Context, projects []string, subject provenance.Subject, pred controlPredicate) (string, error) {
	if s.Signer == nil || s.Stores == nil || len(projects) == 0 {
		return "", nil
	}
	rec, err := provenance.SignStatement(ctx, s.Signer, provenance.PredicateControl, []provenance.Subject{subject}, pred, time.Now())
	if err != nil {
		return "", err
	}
	id := c4.Identify(bytes.NewReader(rec))
	for _, p := range projects {
		st, err := s.Stores(ctx, p)
		if err != nil || st == nil {
			return "", fmt.Errorf("project %s store: %v", p, err)
		}
		if err := st.Put(ctx, id, bytes.NewReader(rec), int64(len(rec))); err != nil {
			return "", err
		}
	}
	return id.String(), nil
}

// controlJob signs the record of an action on a job and adds it to the
// job's annotation. j is the job as it will be written; before is its
// spec digest before the change.
func (s *Server) controlJob(ctx context.Context, r *http.Request, v *api.Viewer, rec *jobRec, j *c4v1.WorkJob, before, action string, change any) (string, error) {
	pred := controlPredicate{Actor: v.User, Authenticated: s.authenticatedBy(r), Via: via(r),
		Authorization: s.authorization("patch", "workjobs", j.Namespace, j.Name),
		Action:        action, Target: "workjob:" + j.Namespace + "/" + j.Name,
		Change: map[string]any{"request": change, "specBefore": before, "specAfter": queue.SpecDigest(j)}}
	subject := provenance.Subject{Name: pred.Target, Digest: map[string]string{"sha256": before}}
	id, err := s.signControl(ctx, []string{rec.project}, subject, pred)
	if err != nil || id == "" {
		return id, err
	}
	if j.Annotations == nil {
		j.Annotations = map[string]string{}
	}
	j.Annotations[c4v1.AnnotationControlRecords] = appendRecord(j.Annotations[c4v1.AnnotationControlRecords], id)
	return id, nil
}

func appendRecord(list, id string) string {
	ids := strings.Split(list, ",")
	ids = slices.DeleteFunc(ids, func(s string) bool { return s == "" })
	ids = append(ids, id)
	return strings.Join(ids[max(0, len(ids)-maxControlRecords):], ",")
}

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12] + "..."
	}
	return id
}

func sha256JSON(v any) string {
	b, _ := json.Marshal(v)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// --- chain of custody

// link is one record in a chain of custody.
type link struct {
	ID       int        `json:"id"`
	Parent   int        `json:"parent"`
	Kind     string     `json:"kind"`
	Record   string     `json:"record,omitempty"`
	Signer   string     `json:"signer,omitempty"`
	SignedAt *time.Time `json:"signedAt,omitempty"`
	Verified bool       `json:"verified"`
	// Policy is empty when the signer is who the farm's policy wants for
	// this kind of record; otherwise it says why not.
	Policy  string `json:"policy,omitempty"`
	About   string `json:"about"`
	Problem string `json:"problem,omitempty"`
}

// custody is a chain of custody: who vouched for each step from a file
// back to where its inputs came from.
type custody struct {
	Job      string   `json:"job"`
	File     *fileRef `json:"file,omitempty"`
	Verified bool     `json:"verified"`
	// Complete is false when a step has no record (an input with no
	// record of where it came from, or a job that ended before signing).
	Complete bool     `json:"complete"`
	Signers  []string `json:"signers"`
	Links    []*link  `json:"links"`
	Gaps     []string `json:"gaps"`

	s     *Server
	m     *model
	ctx   context.Context
	seen  map[string]bool
	depth int
}

const custodyDepth = 4

func (c *custody) add(parent int, kind, about string) *link {
	l := &link{ID: len(c.Links), Parent: parent, Kind: kind, About: about}
	c.Links = append(c.Links, l)
	return l
}

func (c *custody) gap(l *link, msg string) {
	l.Problem = msg
	c.Gaps = append(c.Gaps, msg)
}

// check verifies a record and applies the signer policy; want is the
// predicate type the step needs.
func (c *custody) check(l *link, project, recordID, want string) *provenance.Verified {
	l.Record = recordID
	v, err := c.s.verify(c.ctx, project, recordID)
	if err != nil {
		l.Problem = err.Error()
		return nil
	}
	at := v.SignedAt
	l.Verified, l.Signer, l.SignedAt = true, v.Identity, &at
	if !slices.Contains(c.Signers, v.Identity) {
		c.Signers = append(c.Signers, v.Identity)
	}
	if want != "" && v.PredicateType != want {
		l.Verified = false
		l.Problem = fmt.Sprintf("a %s record where a %s record belongs", v.PredicateType, want)
		return nil
	}
	if err := provenance.CheckSigner(v); err != nil {
		l.Policy = err.Error()
	}
	return v
}

func predicate[T any](v *provenance.Verified) T {
	var out T
	json.Unmarshal(v.Predicate, &out)
	return out
}

// materialization follows an input: the controller's resolution record,
// then the record of where its source came from.
func (c *custody) materialization(parent int, project, ns, name, wantView string) {
	l := c.add(parent, "resolution", fmt.Sprintf("Materialization %s/%s", ns, name))
	var mat c4v1.Materialization
	if err := c.s.Client.Get(c.ctx, types.NamespacedName{Namespace: ns, Name: name}, &mat); err != nil {
		c.gap(l, fmt.Sprintf("Materialization %s/%s: %v", ns, name, err))
		return
	}
	if mat.Status.Record == "" {
		c.gap(l, fmt.Sprintf("Materialization %s/%s has no resolution record (it resolved with signing off)", ns, name))
		return
	}
	v := c.check(l, project, mat.Status.Record, provenance.PredicateResolution)
	if v == nil {
		return
	}
	res := predicate[struct {
		View         string `json:"view"`
		Source       string `json:"source"`
		Files        int64  `json:"files"`
		SourceRecord *struct {
			Record string `json:"record"`
		} `json:"sourceRecord"`
	}](v)
	l.About = fmt.Sprintf("Materialization %s/%s resolved to view %s (%d files) from %s, every object present", ns, name, shortID(res.View), res.Files, shortID(res.Source))
	if wantView != "" && res.View != wantView {
		l.Verified = false
		l.Problem = fmt.Sprintf("the record resolves to %s, but the task mounted %s", shortID(res.View), shortID(wantView))
		return
	}
	if res.SourceRecord != nil && res.SourceRecord.Record != "" {
		c.source(l.ID, project, res.SourceRecord.Record, res.Source)
		return
	}
	// No source record: the source may still be a job's output.
	for _, r := range c.m.jobs {
		if o := r.job.Status.Output; o != nil && o.View == res.Source {
			c.job(l.ID, r, true)
			return
		}
	}
	g := c.add(l.ID, "source", "view "+shortID(res.Source))
	c.gap(g, fmt.Sprintf("no record says where view %s came from (ingest it with a signer, or name its record on the Materialization)", shortID(res.Source)))
}

// source follows a source record: an import or ingest (a leaf), or the
// record of the job that produced the source.
func (c *custody) source(parent int, project, recordID, source string) {
	l := c.add(parent, "source", "the source's record")
	v := c.check(l, project, recordID, "")
	if v == nil {
		return
	}
	if !v.HasSubject(source) {
		l.Verified = false
		l.Problem = fmt.Sprintf("the record does not name the source %s", shortID(source))
		return
	}
	switch v.PredicateType {
	case provenance.PredicateIngest, provenance.PredicateImport:
		p := predicate[map[string]any](v)
		l.Kind = map[string]string{provenance.PredicateIngest: "ingest", provenance.PredicateImport: "import"}[v.PredicateType]
		l.About = fmt.Sprintf("%s of %v as view %s", l.Kind, firstOf(p, "source", "repository", "dir"), shortID(source))
		if c := firstOf(p, "commit"); c != nil {
			l.About += fmt.Sprintf(" (commit %v)", c)
		}
	case provenance.PredicateJob:
		p := predicate[struct {
			Job string `json:"job"`
		}](v)
		l.Kind, l.About = "job", fmt.Sprintf("%s produced view %s", p.Job, shortID(source))
		if r := c.m.byKey[p.Job]; r != nil {
			c.jobDetails(l, r, v, true)
		}
	default:
		l.Problem = fmt.Sprintf("a %s record cannot vouch for a source", v.PredicateType)
		l.Verified = false
	}
}

func firstOf(m map[string]any, keys ...string) any {
	for _, k := range keys {
		if v, ok := m[k]; ok && v != "" {
			return v
		}
	}
	return nil
}

// job follows a job's own record (signed by the queue when it ended).
func (c *custody) job(parent int, r *jobRec, upstream bool) {
	l := c.add(parent, "job", r.key)
	o := r.job.Status.Output
	if o == nil || o.Record == "" {
		if r.job.Status.Finished == nil {
			l.About = r.key + " has not ended; the queue signs its record when it does"
			return
		}
		c.gap(l, r.key+" ended without a job record (it ended with signing off)")
		return
	}
	v := c.check(l, r.project, o.Record, provenance.PredicateJob)
	if v == nil {
		return
	}
	if v.Subject("output") != o.View {
		l.Verified = false
		l.Problem = fmt.Sprintf("the record seals output %s, the job reports %s", shortID(v.Subject("output")), shortID(o.View))
		return
	}
	c.jobDetails(l, r, v, upstream)
}

// jobDetails adds what a job record vouches for: its decisions and
// controls and, for an upstream job, its task records and inputs.
func (c *custody) jobDetails(l *link, r *jobRec, v *provenance.Verified, upstream bool) {
	if c.seen[r.key] {
		return
	}
	c.seen[r.key] = true
	p := predicate[struct {
		Phase     string `json:"phase"`
		Decisions []struct {
			Unit   int    `json:"unit"`
			Kind   string `json:"kind"`
			Reason string `json:"reason"`
			For    string `json:"for"`
			Record string `json:"record"`
		} `json:"decisions"`
		Controls []string `json:"controls"`
	}](v)
	l.About = fmt.Sprintf("%s ended %s with output %s", r.key, p.Phase, shortID(v.Subject("output")))
	for _, d := range p.Decisions {
		dl := c.add(l.ID, "decision", fmt.Sprintf("unit %d: %s (%s) for %s", d.Unit, d.Kind, d.Reason, d.For))
		if d.Record == "" {
			c.gap(dl, "a decision without its record")
			continue
		}
		c.check(dl, r.project, d.Record, provenance.PredicateDecision)
	}
	for _, id := range p.Controls {
		c.control(l.ID, r.project, id)
	}
	if !upstream || c.depth >= custodyDepth {
		return
	}
	c.tasks(l.ID, r)
	c.depth++
	defer func() { c.depth-- }()
	for _, in := range r.job.Spec.Inputs {
		c.materialization(l.ID, r.project, r.job.Namespace, queue.InputMaterialization(r.job.Name, in), "")
	}
}

func (c *custody) control(parent int, project, id string) {
	l := c.add(parent, "control", "an action through the HUD")
	v := c.check(l, project, id, provenance.PredicateControl)
	if v == nil {
		return
	}
	p := predicate[controlPredicate](v)
	l.About = fmt.Sprintf("%s: %s by %s via %s (%s)", p.Target, p.Action, p.Actor, p.Via, p.Authorization)
}

// maxTaskChecks bounds the task records checked per upstream job.
const maxTaskChecks = 64

// tasks checks an upstream job's task records (up to maxTaskChecks) and
// summarizes who signed them.
func (c *custody) tasks(parent int, r *jobRec) {
	l := c.add(parent, "tasks", r.key+": task records")
	ix, err := c.s.index(c.ctx, r)
	if err != nil || ix == nil {
		c.gap(l, r.key+" has no provenance index")
		return
	}
	units := make([]int, 0, len(ix.Units))
	for u := range ix.Units {
		units = append(units, u)
	}
	slices.Sort(units)
	nodes := map[string]int{}
	checked, bad := 0, 0
	for _, u := range units[:min(len(units), maxTaskChecks)] {
		v, err := c.s.verify(c.ctx, r.project, ix.Units[u])
		checked++
		if err != nil || v.PredicateType != provenance.PredicateTask || provenance.CheckSigner(v) != nil {
			bad++
			continue
		}
		nodes[v.Identity]++
		if !slices.Contains(c.Signers, v.Identity) {
			c.Signers = append(c.Signers, v.Identity)
		}
	}
	var by []string
	for id, n := range nodes {
		by = append(by, fmt.Sprintf("%s (%d)", strings.TrimPrefix(id, provenance.IdentityBase), n))
	}
	slices.Sort(by)
	l.Verified = bad == 0 && checked > 0
	l.About = fmt.Sprintf("%d of %d units' task records checked, %d verified with the node that ran them as signer: %s",
		checked, len(units), checked-bad, strings.Join(by, ", "))
	if bad > 0 {
		l.Problem = fmt.Sprintf("%d task records failed verification or the signer policy", bad)
	}
}

// handleCustody is GET /custody/{ns}/{name}?path=P or ?unit=N: the chain
// of custody of a file the job wrote: the task record (signed by the node
// that ran it), the resolution of each input (signed by the controller),
// where each input came from (an import or ingest signed by a workload,
// or an upstream job's record signed by the queue, with its decisions,
// its controls and its own inputs), and the job's own record. Every link
// is verified against the farm's roots and the signer policy.
func (s *Server) handleCustody(w http.ResponseWriter, r *http.Request) {
	m, rec, _ := s.job(w, r)
	if rec == nil {
		return
	}
	ctx := r.Context()
	ix, err := s.index(ctx, rec)
	if err != nil {
		fail(w, http.StatusServiceUnavailable, "provenance index: %v", err)
		return
	}
	if ix == nil {
		fail(w, http.StatusNotFound, "%s has no task records yet: none of its units has finished with signing on", rec.key)
		return
	}
	c := &custody{Job: rec.key, Signers: []string{}, Gaps: []string{}, s: s, m: m, ctx: ctx, seen: map[string]bool{}}
	path, recordID := r.URL.Query().Get("path"), ""
	if path != "" {
		recordID = ix.Records[path]
	} else if u, err := strconv.Atoi(r.URL.Query().Get("unit")); err == nil {
		recordID = ix.Units[u]
	} else {
		fail(w, http.StatusBadRequest, "give path= (a file the job wrote) or unit= (a unit index)")
		return
	}
	if recordID == "" {
		fail(w, http.StatusNotFound, "%s has no task record for that file or unit", rec.key)
		return
	}
	t := c.add(-1, "task", "the task that wrote it")
	if v := c.check(t, rec.project, recordID, provenance.PredicateTask); v != nil {
		st, _ := taskStatement(v)
		p := st.Predicate
		t.About = fmt.Sprintf("unit %s, attempt %s, of %s ran on %s", p.Invocation.Unit, p.Invocation.Attempt, rec.key, p.Runtime.Node)
		if path != "" {
			id := v.Subject(path)
			if id == "" {
				t.Verified = false
				t.Problem = "the record does not name " + path
			} else {
				c.File = &fileRef{Path: path, ID: id}
				t.About += " and wrote " + path
			}
		}
		c.seen[rec.key] = true
		for _, in := range p.Inputs {
			c.materialization(t.ID, rec.project, p.Invocation.Namespace, in.Materialization, in.View)
		}
		delete(c.seen, rec.key)
	}
	c.job(-1, rec, false)
	// Verified: every record present checked out and was signed by whom
	// the policy wants. Complete: no step lacked a record.
	c.Verified, c.Complete = true, len(c.Gaps) == 0
	for _, l := range c.Links {
		badRecord := l.Record != "" && (!l.Verified || l.Policy != "")
		badTasks := l.Kind == "tasks" && !l.Verified && l.Problem != "" && !slices.Contains(c.Gaps, l.Problem)
		if badRecord || badTasks {
			c.Verified = false
		}
	}
	writeJSON(w, http.StatusOK, c)
}
