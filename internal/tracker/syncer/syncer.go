// Package syncer is c4-sync's loop (proposals/integrations ADAPTER.md
// section 6). It is level-triggered: each pass reads both sides and makes
// only the writes that close the difference, so lost events cost latency,
// never correctness. Events from either side only say where to look first.
//
// One pass over a Tracker:
//  1. intake: tracker entities matching an intake rule become jobs (and a
//     TrackerLink each); a moved due date moves a Tracking deadline;
//  2. for each link, in order of least slack: find what is under review
//     (a pull request), relay new tracker decisions to the job's open
//     wait, write the job's summary state back, and publish its output
//     once it is done.
//
// Every tracker write carries a key; for trackers without idempotency keys
// the adapter probes before writing, and the link's status remembers what
// was written, so a retried pass writes nothing twice.
package syncer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/hud/api"
	"github.com/Avalanche-io/c4-operator/internal/tracker"
	"github.com/Avalanche-io/c4-operator/internal/tracker/farm"
)

// Condition types on Tracker and TrackerLink.
const (
	CondReady       = "Ready"
	CondRateLimited = "RateLimited"
	CondSynced      = "Synced"
	CondDiverged    = "Diverged"
	CondIntake      = "Intake"
)

// Syncer runs passes over Trackers.
type Syncer struct {
	Client client.Client
	Farm   farm.Farm
	// HTTP is the base client for tracker calls; each Tracker's rate
	// bucket wraps it.
	HTTP *http.Client
	Now  func() time.Time
	Log  logr.Logger

	mu    sync.Mutex
	conns map[string]*conn
	seen  *deliveries
	kick  chan struct{}
}

// conn is the live state of one Tracker: its adapter and rate bucket, and
// what events have marked for a look.
type conn struct {
	gen     int64
	adapter tracker.Adapter
	limiter *limiter
	until   time.Time // rate limited until
	dirty   map[string]bool
	pending []tracker.Decision // decisions carried only by deliveries
	resync  time.Time
}

func (s *Syncer) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func key(t *c4v1.Tracker) string { return t.Namespace + "/" + t.Name }

// conn returns the Tracker's connection, rebuilding it when the spec
// changed.
func (s *Syncer) conn(t *c4v1.Tracker) (*conn, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conns == nil {
		s.conns = map[string]*conn{}
	}
	if c := s.conns[key(t)]; c != nil && c.gen == t.Generation {
		return c, nil
	}
	lim := newLimiter(t.Spec.Rate)
	base := s.HTTP
	if base == nil {
		base = &http.Client{Timeout: 30 * time.Second}
	}
	hc := *base
	hc.Transport = &rateTransport{next: base.Transport, lim: lim}
	ns := t.Namespace
	cred := t.Spec.Credential.Secret
	cfg := tracker.Config{
		Endpoint:   t.Spec.Endpoint,
		Scope:      t.Spec.Scope,
		HTTPClient: &hc,
		Now:        s.now,
		Self:       t.Annotations[AnnotationSelf],
		Token: func(ctx context.Context) (string, error) {
			b, err := s.secret(ctx, ns, cred)
			return strings.TrimSpace(string(b)), err
		},
	}
	if ws := t.Spec.WebhookSecret; ws != nil {
		k := *ws
		cfg.WebhookSecret = func(ctx context.Context) ([]byte, error) { return s.secret(ctx, ns, k) }
	}
	a, err := tracker.New(t.Spec.Kind, cfg)
	if err != nil {
		return nil, err
	}
	old := s.conns[key(t)]
	c := &conn{gen: t.Generation, adapter: a, limiter: lim, dirty: map[string]bool{}}
	if old != nil {
		c.dirty, c.pending, c.until = old.dirty, old.pending, old.until
	}
	s.conns[key(t)] = c
	return c, nil
}

// AnnotationSelf on a Tracker is c4-sync's own login in the tracker, so
// deliveries it caused are recognized as echoes.
const AnnotationSelf = "c4.avalanche.io/tracker-self"

// secret reads one Secret key on every use, so a rotated lease takes
// effect at once and no credential is kept in memory between uses.
func (s *Syncer) secret(ctx context.Context, ns string, k c4v1.TrackerSecretKey) ([]byte, error) {
	var sec corev1.Secret
	if err := s.Client.Get(ctx, client.ObjectKey{Namespace: ns, Name: k.Name}, &sec); err != nil {
		return nil, err
	}
	v, ok := sec.Data[k.Key]
	if !ok {
		return nil, fmt.Errorf("secret %s/%s has no key %q", ns, k.Name, k.Key)
	}
	return v, nil
}

// SyncAll runs one pass over every Tracker.
func (s *Syncer) SyncAll(ctx context.Context) error {
	var ts c4v1.TrackerList
	if err := s.Client.List(ctx, &ts); err != nil {
		return err
	}
	var errs []error
	for i := range ts.Items {
		if err := s.Sync(ctx, &ts.Items[i]); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", key(&ts.Items[i]), err))
		}
	}
	return errors.Join(errs...)
}

// Sync runs one pass over one Tracker and records the outcome in its
// conditions.
func (s *Syncer) Sync(ctx context.Context, t *c4v1.Tracker) error {
	c, err := s.conn(t)
	if err != nil {
		s.setTrackerCond(ctx, t, CondReady, metav1.ConditionFalse, "NoAdapter", err.Error())
		return err
	}
	if now := s.now(); now.Before(c.until) {
		s.setTrackerCond(ctx, t, CondRateLimited, metav1.ConditionTrue, "RateLimited",
			"waiting until "+c.until.UTC().Format(time.RFC3339))
		return nil
	}
	err = s.pass(ctx, t, c)
	var rl *tracker.RateLimited
	if errors.As(err, &rl) {
		s.mu.Lock()
		c.until = rl.Until
		s.mu.Unlock()
		s.setTrackerCond(ctx, t, CondRateLimited, metav1.ConditionTrue, "RateLimited", rl.Error())
		return nil
	}
	s.setTrackerCond(ctx, t, CondRateLimited, metav1.ConditionFalse, "WithinLimits", "")
	if err != nil {
		s.setTrackerCond(ctx, t, CondReady, metav1.ConditionFalse, "SyncFailed", err.Error())
		return err
	}
	s.setTrackerCond(ctx, t, CondReady, metav1.ConditionTrue, "Synced", "")
	return nil
}

func (s *Syncer) pass(ctx context.Context, t *c4v1.Tracker, c *conn) error {
	links, err := s.links(ctx, t)
	if err != nil {
		return err
	}
	// Changes since the cursor mark entities for a closer look.
	refs, next, err := c.adapter.Changes(ctx, t.Status.Cursor)
	if err != nil {
		return err
	}
	s.mu.Lock()
	for _, r := range refs {
		c.dirty[r.String()] = true
	}
	resync := c.resync.IsZero() || s.now().After(c.resync)
	if resync {
		c.resync = s.now().Add(seconds(t.Spec.Events.ResyncSeconds, 900))
	}
	s.mu.Unlock()

	if err := s.intake(ctx, t, c, links); err != nil {
		return err
	}
	if links, err = s.links(ctx, t); err != nil {
		return err
	}
	if err := s.discoverReviews(ctx, t, c, links, resync); err != nil {
		return err
	}
	// Least slack first, so a tight job's decision is never stuck behind
	// idle ones when the tracker throttles.
	jobs := map[string]*api.Job{}
	for _, l := range links {
		if l.Spec.Target.WorkJob == "" {
			continue
		}
		j, err := s.Farm.Job(ctx, farm.JobRef{Namespace: l.Namespace, Name: l.Spec.Target.WorkJob})
		if err != nil && !errors.Is(err, farm.ErrNotFound) {
			return err
		}
		jobs[l.Name] = j
	}
	sort.SliceStable(links, func(a, b int) bool {
		return slack(jobs[links[a].Name]) < slack(jobs[links[b].Name])
	})
	s.mu.Lock()
	dirty := make(map[string]bool, len(c.dirty))
	for k := range c.dirty {
		dirty[k] = true
	}
	s.mu.Unlock()
	var errs []error
	for _, l := range links {
		fresh := resync
		if r := l.Status.Review; r != nil && dirty[r.Type+"/"+r.ID] {
			fresh = true
		}
		if err := s.reconcile(ctx, t, c, l, jobs[l.Name], fresh); err != nil {
			var rl *tracker.RateLimited
			if errors.As(err, &rl) {
				return err
			}
			errs = append(errs, fmt.Errorf("%s: %w", l.Name, err))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return err
	}
	s.mu.Lock()
	c.dirty = map[string]bool{}
	s.mu.Unlock()
	return s.patchTrackerStatus(ctx, t, func(st *c4v1.TrackerStatus) {
		st.Cursor = next
		st.Links = int32(len(links))
		if resync {
			now := metav1.NewTime(s.now())
			st.LastResync = &now
		}
	})
}

func slack(j *api.Job) float64 {
	if j == nil {
		return 1e18
	}
	return j.Estimate.SlackSeconds
}

func seconds(v int32, def int) time.Duration {
	if v <= 0 {
		v = int32(def)
	}
	return time.Duration(v) * time.Second
}

func (s *Syncer) links(ctx context.Context, t *c4v1.Tracker) ([]*c4v1.TrackerLink, error) {
	var ls c4v1.TrackerLinkList
	if err := s.Client.List(ctx, &ls, client.InNamespace(t.Namespace)); err != nil {
		return nil, err
	}
	var out []*c4v1.TrackerLink
	for i := range ls.Items {
		if ls.Items[i].Spec.Tracker == t.Name {
			out = append(out, &ls.Items[i])
		}
	}
	return out, nil
}

// --- intake ---

// LinkName is the TrackerLink for a tracker entity; JobName the job it
// makes. Both are deterministic, so a retried intake finds what an
// earlier one made.
func LinkName(tr string, r tracker.Ref) string { return dns(tr + "-" + r.Type + "-" + r.ID) }

func JobName(tr string, r tracker.Ref) string { return dns(tr + "-" + r.ID) }

var nonDNS = regexp.MustCompile(`[^a-z0-9-]+`)

func dns(s string) string {
	s = nonDNS.ReplaceAllString(strings.ToLower(s), "-")
	s = strings.Trim(s, "-")
	if len(s) > 63 {
		h := sha256.Sum256([]byte(s))
		s = strings.TrimRight(s[:52], "-") + "-" + hex.EncodeToString(h[:])[:10]
	}
	return s
}

func (s *Syncer) intake(ctx context.Context, t *c4v1.Tracker, c *conn, links []*c4v1.TrackerLink) error {
	byName := map[string]*c4v1.TrackerLink{}
	for _, l := range links {
		byName[l.Name] = l
	}
	for i, rule := range t.Spec.Intake {
		page := ""
		for {
			items, next, err := c.adapter.List(ctx, tracker.Query{Type: rule.Match.Type, Labels: rule.Match.Labels, State: rule.Match.State}, page)
			if err != nil {
				return err
			}
			for _, it := range items {
				if err := s.intakeItem(ctx, t, i, rule, it, byName[LinkName(t.Name, it.Ref)]); err != nil {
					return err
				}
			}
			if next == "" {
				break
			}
			page = next
		}
	}
	return nil
}

// Deadline turns a tracker due date into a deadline. A date with no time
// of day becomes the rule's local time on that date; without one it is
// not guessed at, and the job gets the farm's suggestion instead.
func Deadline(d *tracker.Due, rule c4v1.TrackerDeadline) (*time.Time, string, error) {
	if d == nil {
		return nil, "no due date", nil
	}
	if !d.DateOnly && rule.DateOnlyAt == "" {
		t := d.At.UTC()
		return &t, "", nil
	}
	if rule.DateOnlyAt == "" {
		return nil, "the due date has no time of day and the rule names none (dateOnlyAt)", nil
	}
	loc := time.UTC
	if rule.TZ != "" {
		l, err := time.LoadLocation(rule.TZ)
		if err != nil {
			return nil, "", fmt.Errorf("intake tz: %w", err)
		}
		loc = l
	}
	hm, err := time.Parse("15:04", rule.DateOnlyAt)
	if err != nil {
		return nil, "", fmt.Errorf("intake dateOnlyAt %q: want HH:MM", rule.DateOnlyAt)
	}
	y, m, dd := d.At.UTC().Date()
	at := time.Date(y, m, dd, hm.Hour(), hm.Minute(), 0, 0, loc).UTC()
	return &at, "", nil
}

func (s *Syncer) intakeItem(ctx context.Context, t *c4v1.Tracker, idx int, rule c4v1.TrackerIntake, it tracker.Item, l *c4v1.TrackerLink) error {
	due, why, err := Deadline(it.Due, rule.Deadline)
	if err != nil {
		return err
	}
	switch rule.As {
	case "milestone":
		if due == nil {
			return nil
		}
		err := s.Farm.EnsureMilestone(ctx, farm.Milestone{Namespace: t.Namespace, Name: LinkName(t.Name, it.Ref),
			Due: *due, Tracker: t.Name, Ref: it.Ref.String(),
			Selector: map[string]string{c4v1.LabelTracker: t.Name}})
		if errors.Is(err, farm.ErrNotBuilt) {
			s.setTrackerCond(ctx, t, CondIntake, metav1.ConditionFalse, "MilestoneNotBuilt",
				"intake as milestone needs the operator's Milestone (integrations GAPS 1)")
			return nil
		}
		return err
	case "job":
	default:
		return fmt.Errorf("intake %d: as %q", idx, rule.As)
	}
	if rule.Job == nil {
		return fmt.Errorf("intake %d: as job needs a job spec", idx)
	}
	jr := farm.JobRef{Namespace: t.Namespace, Name: JobName(t.Name, it.Ref)}
	if l == nil {
		spec := *rule.Job.DeepCopy()
		spec.Deadline, spec.DeadlineMode = nil, ""
		if due != nil {
			spec.Deadline = &metav1.Time{Time: *due}
			spec.DeadlineMode = "Tracking"
		}
		got, err := s.Farm.Submit(ctx, farm.JobRequest{Namespace: jr.Namespace, Name: jr.Name, Spec: spec,
			Tracker: t.Name, TrackerRef: t.Name + "/" + it.Ref.String()})
		if err != nil {
			return fmt.Errorf("submitting %s for %s: %w", jr, it.Ref, err)
		}
		l = &c4v1.TrackerLink{
			ObjectMeta: metav1.ObjectMeta{Namespace: t.Namespace, Name: LinkName(t.Name, it.Ref),
				Labels: map[string]string{c4v1.LabelTracker: t.Name}},
			Spec: c4v1.TrackerLinkSpec{Tracker: t.Name, Intake: int32(idx),
				Ref:    c4v1.TrackerRef{Type: it.Ref.Type, ID: it.Ref.ID, URL: it.Ref.URL},
				Target: c4v1.TrackerTarget{WorkJob: got.Name}},
		}
		if err := s.Client.Create(ctx, l); err != nil && !apierrors.IsAlreadyExists(err) {
			return err
		}
		if why != "" {
			s.setLinkCond(ctx, l, "Deadline", metav1.ConditionFalse, "NotFromTracker", why)
		}
		return nil
	}
	// The link exists: a moved due date moves the job's deadline, while
	// the job still follows tracking. A person who pinned another time in
	// the HUD wins; the link says so instead.
	if due == nil {
		return nil
	}
	j, err := s.Farm.Job(ctx, jr)
	if err != nil || j == nil {
		return nil
	}
	if j.Objective.Mode != "Tracking" {
		if j.Objective.Deadline == nil || !j.Objective.Deadline.Equal(*due) {
			s.setLinkCond(ctx, l, CondDiverged, metav1.ConditionTrue, "DeadlineNotTracking",
				"the tracker's due date moved to "+due.Format(time.RFC3339)+", but the job's deadline is "+j.Objective.Mode+" in C4")
		}
		return nil
	}
	if j.Objective.Deadline != nil && j.Objective.Deadline.Equal(*due) {
		return nil
	}
	return s.Farm.SetDeadline(ctx, jr, *due)
}

// --- reviews: what is under review for each link ---

// closing matches GitHub-style closing keywords; markerFor is the line a
// change names its job with.
var closing = regexp.MustCompile(`(?i)\b(?:close[sd]?|fix(?:e[sd])?|resolve[sd]?)\s+#(\d+)\b`)

func markerFor(ns, job string) string { return "C4-Job: " + ns + "/" + job }

func (s *Syncer) discoverReviews(ctx context.Context, t *c4v1.Tracker, c *conn, links []*c4v1.TrackerLink, resync bool) error {
	var need []*c4v1.TrackerLink
	for _, l := range links {
		if l.Status.Review == nil && l.Spec.Ref.Type == "Issue" && l.Spec.Target.WorkJob != "" {
			need = append(need, l)
		}
	}
	if len(need) == 0 {
		return nil
	}
	_ = resync // every pass looks while some link has no review: one list call
	page := ""
	var prs []tracker.Item
	for {
		items, next, err := c.adapter.List(ctx, tracker.Query{Type: "PullRequest", State: "open"}, page)
		if errors.Is(err, tracker.ErrUnsupported) {
			return nil
		}
		if err != nil {
			return err
		}
		prs = append(prs, items...)
		if next == "" {
			break
		}
		page = next
	}
	for _, l := range need {
		for _, pr := range prs {
			if !mentions(pr.Body, l) {
				continue
			}
			ref := c4v1.TrackerRef{Type: pr.Ref.Type, ID: pr.Ref.ID, URL: pr.Ref.URL}
			if err := s.patchLinkStatus(ctx, l, func(st *c4v1.TrackerLinkStatus) { st.Review = &ref }); err != nil {
				return err
			}
			break
		}
	}
	return nil
}

func mentions(body string, l *c4v1.TrackerLink) bool {
	if strings.Contains(body, markerFor(l.Namespace, l.Spec.Target.WorkJob)) {
		return true
	}
	for _, m := range closing.FindAllStringSubmatch(body, -1) {
		if m[1] == l.Spec.Ref.ID {
			return true
		}
	}
	return false
}

// --- one link ---

// reconcile closes the difference for one link. fresh says the entity
// under review may have changed (a reported change, or a resync).
func (s *Syncer) reconcile(ctx context.Context, t *c4v1.Tracker, c *conn, l *c4v1.TrackerLink, j *api.Job, fresh bool) error {
	if l.Spec.Target.WorkJob == "" {
		return nil
	}
	if j == nil {
		s.setLinkCond(ctx, l, CondSynced, metav1.ConditionFalse, "JobGone", "the job "+l.Spec.Target.WorkJob+" is not in the farm")
		return nil
	}
	jr := farm.JobRef{Namespace: l.Namespace, Name: l.Spec.Target.WorkJob}
	review := ""
	if int(l.Spec.Intake) < len(t.Spec.Intake) {
		if r := t.Spec.Intake[l.Spec.Intake].Review; r != nil {
			review = r.Wait
		}
	}
	sum := Summarize(j, review, reworking(l))

	// Decisions first: a decision relayed now changes what is written.
	if sum.Wait != nil {
		relayed, err := s.relay(ctx, t, c, l, j, jr, sum.Wait)
		if err != nil {
			return err
		}
		if relayed {
			if j, err = s.Farm.Job(ctx, jr); err != nil {
				return err
			}
			if j == nil {
				return nil
			}
			sum = Summarize(j, review, reworking(l))
		}
	}
	if err := s.statusOut(ctx, t, c, l, jr, sum, fresh); err != nil {
		return err
	}
	if err := s.publish(ctx, t, c, l, j, jr, sum); err != nil {
		return err
	}
	s.setLinkCond(ctx, l, CondSynced, metav1.ConditionTrue, "Synced", "")
	return nil
}

// reworking: the last decision relayed for this link was a rework.
func reworking(l *c4v1.TrackerLink) bool {
	for i := len(l.Status.Decisions) - 1; i >= 0; i-- {
		d := l.Status.Decisions[i]
		if d.Error == "" {
			return d.Do == "rework"
		}
	}
	return false
}

// --- decisions in ---

// DefaultVerdicts maps GitHub's decisions to C4's (MAPPING.md section 6).
// A COMMENTED review decides nothing.
var DefaultVerdicts = map[string]string{
	"APPROVED":          "approve",
	"CHANGES_REQUESTED": "rework",
	"action:approve":    "approve",
	"action:rework":     "rework",
	"action:accept":     "accept",
}

func verdictFor(t *c4v1.Tracker, native string) string {
	if v, ok := t.Spec.VerdictsIn[native]; ok {
		return v
	}
	return DefaultVerdicts[native]
}

// relay sends the newest decision not yet relayed to the open wait. Older
// unrelayed decisions are superseded by it: a reviewer who requested
// changes and then approved has approved.
func (s *Syncer) relay(ctx context.Context, t *c4v1.Tracker, c *conn, l *c4v1.TrackerLink, j *api.Job, jr farm.JobRef, w *api.Wait) (bool, error) {
	var cands []tracker.Decision
	if l.Status.Review != nil {
		// Reviews already relayed (or refused) are not read again.
		since := w.Since.Add(-time.Second)
		for _, d := range l.Status.Decisions {
			if d.At.After(since) {
				since = d.At.Time
			}
		}
		ds, err := c.adapter.Decisions(ctx, tracker.Ref{Type: l.Status.Review.Type, ID: l.Status.Review.ID, URL: l.Status.Review.URL}, since)
		if err != nil && !errors.Is(err, tracker.ErrUnsupported) {
			return false, err
		}
		cands = append(cands, ds...)
	}
	s.mu.Lock()
	checkRef := l.Status.Written["check"].Ref
	var keep []tracker.Decision
	for _, d := range c.pending {
		if d.On.Type == "CheckRun" && d.On.ID == checkRef && checkRef != "" {
			cands = append(cands, d)
		} else {
			keep = append(keep, d)
		}
	}
	c.pending = keep
	s.mu.Unlock()

	done := map[string]bool{}
	for _, d := range l.Status.Decisions {
		done[d.Source] = true
	}
	var pick *tracker.Decision
	for i := range cands {
		d := &cands[i]
		if done[d.Source.String()] || verdictFor(t, d.Native) == "" || d.At.Before(w.Since.Add(-time.Second)) {
			continue
		}
		if pick == nil || d.At.After(pick.At) {
			pick = d
		}
	}
	if pick == nil {
		return false, nil
	}
	rec := c4v1.TrackerDecision{Source: pick.Source.String(), By: pick.By.ID, Unit: unitRef(w), At: metav1.NewTime(pick.At)}
	do := verdictFor(t, pick.Native)
	if w.Escalated && do == "approve" {
		do = "accept" // an escalated unit is accepted, not approved
	}
	rec.Do = do
	person, reason := s.authorize(t, w, pick.By)
	if reason != "" {
		rec.Error = reason
		return false, s.recordDecision(ctx, l, rec)
	}
	if !allowed(w, do) {
		rec.Error = fmt.Sprintf("the wait does not take %q (choices: %s)", do, strings.Join(append([]string{"approve"}, w.Choices...), ", "))
		return false, s.recordDecision(ctx, l, rec)
	}
	err := s.Farm.Decide(ctx, jr, farm.Relay{
		Project: j.Project, Unit: rec.Unit, Do: do, Note: pick.Body,
		Tracker: t.Name, Source: pick.Source.URL + " (" + pick.Source.String() + ")",
		By: "@" + pick.By.ID, Person: person, Evidence: pick.Evidence,
	})
	if errors.Is(err, farm.ErrNotOpen) {
		rec.Error = err.Error()
		return false, s.recordDecision(ctx, l, rec)
	}
	if err != nil {
		return false, err
	}
	return true, s.recordDecision(ctx, l, rec)
}

func unitRef(w *api.Wait) string {
	if w.Name != "" {
		return w.Name
	}
	return strconv.Itoa(w.Unit)
}

func allowed(w *api.Wait, do string) bool {
	if w.Escalated {
		return do == "accept" || do == "rework" || do == "fail"
	}
	if do == "fail" || len(w.Choices) == 0 {
		return true
	}
	if do == "approve" {
		return true
	}
	for _, c := range w.Choices {
		if c == do {
			return true
		}
	}
	return false
}

// authorize checks that the tracker user may serve the wait: a person's
// wait only by that person, as the Tracker's people map names them; a
// party's wait by anyone mapped. Roles never come from the tracker.
func (s *Syncer) authorize(t *c4v1.Tracker, w *api.Wait, by tracker.Person) (string, string) {
	person, mapped := t.Spec.People[by.ID]
	switch {
	case strings.HasPrefix(w.WaitsFor, "person:"):
		want := strings.TrimPrefix(w.WaitsFor, "person:")
		if !mapped {
			return "", fmt.Sprintf("@%s is not mapped to a C4 person (spec.people); the wait is for %s", by.ID, want)
		}
		if person != want {
			return person, fmt.Sprintf("@%s is %s in C4; the wait is for %s", by.ID, person, want)
		}
		return person, ""
	case strings.HasPrefix(w.WaitsFor, "party:"):
		if !mapped {
			return "", fmt.Sprintf("@%s is not mapped to a C4 person (spec.people)", by.ID)
		}
		return person, ""
	}
	return "", "the wait is for " + w.WaitsFor + ", which a tracker cannot serve"
}

func (s *Syncer) recordDecision(ctx context.Context, l *c4v1.TrackerLink, d c4v1.TrackerDecision) error {
	return s.patchLinkStatus(ctx, l, func(st *c4v1.TrackerLinkStatus) {
		st.Decisions = append(st.Decisions, d)
		if n := len(st.Decisions); n > maxDecisions {
			st.Decisions = st.Decisions[n-maxDecisions:]
		}
	})
}

const maxDecisions = 50

// --- status out ---

func (s *Syncer) statusOut(ctx context.Context, t *c4v1.Tracker, c *conn, l *c4v1.TrackerLink, jr farm.JobRef, sum Summary, fresh bool) error {
	now := s.now()
	every := seconds(t.Spec.StatusOut.FieldSeconds, 300)
	link := s.Farm.JobURL(jr)
	// The issue's one comment, edited in place.
	body := commentBody(jr, link, sum, l)
	if err := s.write(ctx, c, l, "comment", sum.Key(), etaKey(sum), every, now, func(prev c4v1.TrackerWritten) tracker.Write {
		w := tracker.Write{On: tracker.Ref{Type: l.Spec.Ref.Type, ID: l.Spec.Ref.ID}, Kind: "comment",
			Note: &tracker.Note{Body: body}, Key: writeKey(l, "comment", "")}
		if prev.Ref != "" {
			w.Note.Replace = &tracker.Ref{Type: "Comment", ID: prev.Ref}
		}
		return w
	}); err != nil {
		return err
	}
	if l.Status.Review == nil {
		return nil
	}
	sha := l.Status.ReviewHead
	if fresh || sha == "" {
		pr, err := c.adapter.Get(ctx, tracker.Ref{Type: l.Status.Review.Type, ID: l.Status.Review.ID})
		if err != nil {
			return err
		}
		if head := pr.Fields["head_sha"]; head != sha {
			sha = head
			if err := s.patchLinkStatus(ctx, l, func(st *c4v1.TrackerLinkStatus) { st.ReviewHead = head }); err != nil {
				return err
			}
		}
	}
	if sha == "" {
		return nil
	}
	name := t.Spec.StatusOut.CheckName
	if name == "" {
		name = "C4"
	}
	status, conclusion := checkState(sum)
	fields := map[string]string{
		"name": name, "head_sha": sha, "status": status, "conclusion": conclusion,
		"title": sum.Title(), "summary": checkSummary(jr, link, sum),
		"details_url": link, "external_id": jr.String(),
	}
	if sum.Wait != nil {
		fields["actions"] = actions(sum.Wait)
	} else {
		fields["actions"] = ""
	}
	checkKey := sum.Key() + "|" + sha
	if err := s.write(ctx, c, l, "check", checkKey, etaKey(sum), every, now, func(prev c4v1.TrackerWritten) tracker.Write {
		f := map[string]string{}
		for k, v := range fields {
			f[k] = v
		}
		// A run that is completed is not reopened: a new run takes over,
		// as a re-run does. A new head commit gets a new run too.
		if prev.Ref != "" && strings.HasSuffix(stateOf(prev.Value), "|"+sha) && !completed(prev.Value) {
			f["id"] = prev.Ref
		}
		return tracker.Write{On: tracker.Ref{Type: "Commit", ID: sha}, Kind: "check", Fields: f,
			Key: writeKey(l, "check", checkKey)}
	}); err != nil {
		return err
	}
	if !t.Spec.StatusOut.CommitStatus {
		return nil
	}
	state := map[bool]string{true: "pending", false: ""}[sum.Active()]
	switch sum.State {
	case StateDone, StateDoneAccepted:
		state = "success"
	case StateNeedsPerson, StateFailed:
		state = "failure"
	}
	ctxName := "c4/" + jr.Name
	desc := sum.Words()
	return s.write(ctx, c, l, "status", sum.Key()+"|"+sha, "", 0, now, func(c4v1.TrackerWritten) tracker.Write {
		return tracker.Write{On: tracker.Ref{Type: "Commit", ID: sha}, Kind: "status",
			Fields: map[string]string{"sha": sha, "state": state, "context": ctxName, "description": desc, "target_url": link}}
	})
}

// write makes one field's write when its value changed, or when only its
// continuous part changed and every has passed. The previous write's
// fingerprint and the tracker entity it made are kept in the link.
func (s *Syncer) write(ctx context.Context, c *conn, l *c4v1.TrackerLink, field, value, cont string, every time.Duration, now time.Time,
	build func(prev c4v1.TrackerWritten) tracker.Write) error {
	prev := l.Status.Written[field]
	full := value + "#" + cont
	switch {
	case prev.Value == full:
		return nil
	case stateOf(prev.Value) == value && (every == 0 || now.Sub(prev.At.Time) < every):
		return nil // only the ETA moved, and not long ago
	}
	w := build(prev)
	if w.Key != "" {
		done, ref, err := c.adapter.Probe(ctx, w)
		if err != nil {
			return err
		}
		if done {
			return s.remember(ctx, l, field, full, ref.ID, now)
		}
	}
	ref, err := c.adapter.Apply(ctx, w)
	if err != nil {
		return err
	}
	id := ref.ID
	if id == "" {
		id = prev.Ref
	}
	return s.remember(ctx, l, field, full, id, now)
}

func stateOf(v string) string {
	s, _, _ := strings.Cut(v, "#")
	return s
}

func completed(v string) bool {
	st := strings.SplitN(stateOf(v), "|", 2)[0]
	return st == StateDone || st == StateDoneAccepted || st == StateFailed || st == StateNeedsPerson
}

func (s *Syncer) remember(ctx context.Context, l *c4v1.TrackerLink, field, value, ref string, now time.Time) error {
	return s.patchLinkStatus(ctx, l, func(st *c4v1.TrackerLinkStatus) {
		if st.Written == nil {
			st.Written = map[string]c4v1.TrackerWritten{}
		}
		st.Written[field] = c4v1.TrackerWritten{Value: value, Ref: ref, At: metav1.NewTime(now)}
		if field == "comment" {
			st.State = strings.SplitN(stateOf(value), "|", 2)[0]
		}
	})
}

// writeKey is the idempotency key of a write: the link, the field and
// what it writes. A crash between a write and remembering it is found by
// the adapter's probe under the same key.
func writeKey(l *c4v1.TrackerLink, field, value string) string {
	h := sha256.Sum256([]byte(l.Namespace + "/" + l.Name + "/" + field + "/" + value))
	return hex.EncodeToString(h[:12])
}

func etaKey(s Summary) string {
	if s.ETA == nil || !s.Active() {
		return ""
	}
	return s.ETA.UTC().Format(time.RFC3339)
}

// checkState maps a summary state to a check run's status and conclusion:
// work states only. done is the farm's work finished; the approval that
// may follow is GitHub's own review.
func checkState(s Summary) (status, conclusion string) {
	switch s.State {
	case StateQueued, StateHeld:
		return "queued", ""
	case StateRunning, StateRework, StateInReview:
		return "in_progress", ""
	case StateNeedsPerson:
		return "completed", "action_required"
	case StateFailed:
		return "completed", "failure"
	case StateDone, StateDoneAccepted:
		return "completed", "success"
	}
	return "queued", ""
}

// actions are the check run's buttons for an open wait: GitHub allows
// three.
func actions(w *api.Wait) string {
	if w.Escalated {
		return "accept=Accept;rework=Rework"
	}
	parts := []string{"approve=Approve"}
	for _, c := range w.Choices {
		if c != "approve" && len(parts) < 3 {
			parts = append(parts, c+"="+strings.ToUpper(c[:1])+c[1:])
		}
	}
	return strings.Join(parts, ";")
}

func checkSummary(jr farm.JobRef, link string, s Summary) string {
	var b strings.Builder
	fmt.Fprintf(&b, "C4 job [%s](%s): %s.", jr, link, s.Words())
	if s.Deadline != nil {
		fmt.Fprintf(&b, "\n\nDeadline %s (%s).", s.Deadline.UTC().Format("Mon 2 Jan 15:04 UTC"), deadlineSource(s))
	}
	if s.ETA != nil && s.Active() {
		fmt.Fprintf(&b, " Expected by %s.", s.ETA.UTC().Format("Mon 2 Jan 15:04 UTC"))
	}
	if s.Why != "" {
		fmt.Fprintf(&b, "\n\n%s", s.Why)
	}
	if s.Wait != nil {
		b.WriteString("\n\nReview this pull request (approve, or request changes) to decide; C4 reads the review.")
	}
	return b.String()
}

func deadlineSource(s Summary) string {
	if s.Mode == "Tracking" {
		return "from the tracker"
	}
	if s.Source == "suggested" {
		return "suggested by the farm"
	}
	return "set in C4"
}

func commentBody(jr farm.JobRef, link string, s Summary, l *c4v1.TrackerLink) string {
	var b strings.Builder
	fmt.Fprintf(&b, "**C4:** %s\n\n", s.Title())
	fmt.Fprintf(&b, "Job [%s](%s)", jr, link)
	if s.Deadline != nil {
		fmt.Fprintf(&b, ", deadline %s (%s)", s.Deadline.UTC().Format("Mon 2 Jan 15:04 UTC"), deadlineSource(s))
	}
	b.WriteString(".")
	if s.Why != "" {
		fmt.Fprintf(&b, "\n\n%s", s.Why)
	}
	if l.Status.Review == nil && l.Spec.Ref.Type == "Issue" {
		fmt.Fprintf(&b, "\n\nA pull request for this work names it with `%s` or a closing keyword for #%s.", markerFor(jr.Namespace, jr.Name), l.Spec.Ref.ID)
	}
	return b.String()
}

// --- outputs out ---

func (s *Syncer) publish(ctx context.Context, t *c4v1.Tracker, c *conn, l *c4v1.TrackerLink, j *api.Job, jr farm.JobRef, sum Summary) error {
	if sum.State != StateDone && sum.State != StateDoneAccepted || sum.Output == "" {
		return nil
	}
	if prev := l.Status.Written["publish"]; prev.Value == sum.Output {
		return nil
	}
	on := tracker.Ref{Type: l.Spec.Ref.Type, ID: l.Spec.Ref.ID}
	if r := l.Status.Review; r != nil {
		on = tracker.Ref{Type: r.Type, ID: r.ID}
	}
	out := tracker.Output{Name: jr.String(), View: sum.Output,
		Custody: strings.TrimSuffix(s.Farm.JobURL(jr), "/") + "/provenance",
		Summary: sum.Words() + "."}
	ref, err := c.adapter.Publish(ctx, on, out, writeKey(l, "publish", sum.Output))
	if err != nil {
		return err
	}
	return s.remember(ctx, l, "publish", sum.Output, ref.ID, s.now())
}

// --- status writes to the API ---

func (s *Syncer) patchLinkStatus(ctx context.Context, l *c4v1.TrackerLink, f func(*c4v1.TrackerLinkStatus)) error {
	before := l.DeepCopy()
	f(&l.Status)
	if err := s.Client.Status().Patch(ctx, l, client.MergeFrom(before)); err != nil {
		return fmt.Errorf("recording on %s: %w", l.Name, err)
	}
	return nil
}

func (s *Syncer) setLinkCond(ctx context.Context, l *c4v1.TrackerLink, typ string, st metav1.ConditionStatus, reason, msg string) {
	if c := meta.FindStatusCondition(l.Status.Conditions, typ); c != nil && c.Status == st && c.Reason == reason && c.Message == msg {
		return
	}
	if err := s.patchLinkStatus(ctx, l, func(ls *c4v1.TrackerLinkStatus) {
		meta.SetStatusCondition(&ls.Conditions, metav1.Condition{Type: typ, Status: st, Reason: reason, Message: msg})
	}); err != nil {
		s.Log.Error(err, "link condition", "link", l.Name)
	}
}

func (s *Syncer) patchTrackerStatus(ctx context.Context, t *c4v1.Tracker, f func(*c4v1.TrackerStatus)) error {
	before := t.DeepCopy()
	f(&t.Status)
	return s.Client.Status().Patch(ctx, t, client.MergeFrom(before))
}

func (s *Syncer) setTrackerCond(ctx context.Context, t *c4v1.Tracker, typ string, st metav1.ConditionStatus, reason, msg string) {
	if c := meta.FindStatusCondition(t.Status.Conditions, typ); c != nil && c.Status == st && c.Reason == reason && c.Message == msg {
		return
	}
	if err := s.patchTrackerStatus(ctx, t, func(ts *c4v1.TrackerStatus) {
		meta.SetStatusCondition(&ts.Conditions, metav1.Condition{Type: typ, Status: st, Reason: reason, Message: msg})
	}); err != nil {
		s.Log.Error(err, "tracker condition", "tracker", key(t))
	}
}
