// Package syncer is c4-sync's loop (docs/c4-sync.md). It is
// level-triggered: each pass reads both sides and makes
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
	"slices"
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
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/hud/api"
	"github.com/Avalanche-io/c4-operator/internal/effect"
	"github.com/Avalanche-io/c4-operator/internal/tracker"
	"github.com/Avalanche-io/c4-operator/internal/tracker/farm"
	"github.com/Avalanche-io/c4-operator/internal/work/class"
)

// Condition types on Tracker and TrackerLink.
const (
	CondReady       = "Ready"
	CondRateLimited = "RateLimited"
	CondSynced      = "Synced"
	CondDiverged    = "Diverged"
	CondIntake      = "Intake"
	// CondUnconfirmed on a TrackerLink: a write to the tracker was
	// interrupted, and nothing can tell whether it happened; it is held
	// for a person (internal/hud/API.md, Held tracker writes).
	CondUnconfirmed = "Unconfirmed"
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
	gen       int64
	adapter   tracker.Adapter
	presenter tracker.Presenter
	limiter   *limiter
	until     time.Time // rate limited until
	dirty     map[string]bool
	pending   []tracker.Decision // decisions carried only by deliveries
	resync    time.Time
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
	p, ok := a.(tracker.Presenter)
	if !ok {
		return nil, fmt.Errorf("adapter %s does not present states", t.Spec.Kind)
	}
	old := s.conns[key(t)]
	c := &conn{gen: t.Generation, adapter: a, presenter: p, limiter: lim, dirty: map[string]bool{}}
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

	seen, err := s.intake(ctx, t, c, links)
	if err != nil {
		return err
	}
	if links, err = s.links(ctx, t); err != nil {
		return err
	}
	// An entity that changed but no longer matches its intake rule (its
	// status moved on) still has its due date followed. A forecast's
	// entity that no rule matches (an omitted shot), or that is gone, is
	// work no longer expected: its forecast is withdrawn, and its link
	// goes with it.
	s.mu.Lock()
	changed := map[string]bool{}
	for k := range c.dirty {
		changed[k] = true
	}
	s.mu.Unlock()
	kept := links[:0]
	for _, l := range links {
		ref := tracker.Ref{Type: l.Spec.Ref.Type, ID: l.Spec.Ref.ID}
		if seen[l.Name] || !(changed[ref.String()] || resync) || int(l.Spec.Intake) >= len(t.Spec.Intake) {
			kept = append(kept, l)
			continue
		}
		it, err := c.adapter.Get(ctx, ref)
		gone := errors.Is(err, tracker.ErrNotFound)
		if err != nil && !gone {
			return err
		}
		rule := t.Spec.Intake[l.Spec.Intake]
		if rule.As == "forecast" {
			why := ref.String() + " no longer matches any of the tracker's intake rules"
			if gone {
				why = ref.String() + " is gone from the tracker"
			}
			if err := s.withdraw(ctx, l, why); err != nil {
				return err
			}
			continue
		}
		kept = append(kept, l)
		if gone {
			continue
		}
		if err := s.followDue(ctx, l, rule, it); err != nil {
			return err
		}
	}
	links = kept
	if err := s.findReviews(ctx, t, c, links); err != nil {
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
	// What changed in the farm since this Tracker last read its log
	// (internal/hud/API.md, GET /book?since), however long it was away.
	feed, farmCursor, err := s.readFeed(ctx, t)
	if err != nil {
		return err
	}
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
	for _, l := range links {
		if err := s.mirrorDecided(ctx, t, c, l, feed); err != nil {
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
		st.FarmCursor = farmCursor
		st.Links = int32(len(links))
		if resync {
			now := metav1.NewTime(s.now())
			st.LastResync = &now
		}
	})
}

// feedPages bounds the log a pass reads; the rest waits for the next.
const feedPages = 10

// readFeed reads the farm's transitions after the Tracker's cursor. A new
// Tracker starts at the log's head (it acts on what happens from now); a
// cursor the log no longer reaches goes on from where it begins.
func (s *Syncer) readFeed(ctx context.Context, t *c4v1.Tracker) ([]api.Transition, int64, error) {
	cursor := t.Status.FarmCursor
	var out []api.Transition
	for page := 0; page < feedPages; page++ {
		res, err := s.Farm.Book(ctx, cursor, 500)
		var gone *farm.ErrGone
		if errors.As(err, &gone) {
			s.Log.Info("the farm's log no longer reaches this Tracker's cursor; going on from its start", "tracker", key(t), "oldest", gone.Oldest)
			cursor = gone.Oldest - 1
			continue
		}
		if err != nil {
			return nil, cursor, err
		}
		if t.Status.FarmCursor == 0 && cursor == 0 {
			return nil, res.Head, nil
		}
		out = append(out, res.Entries...)
		cursor = res.Next
		if !res.More {
			break
		}
	}
	return out, cursor, nil
}

// mirrorDecided tells the tracker of decisions made in C4 itself on a
// linked job's waits (in the HUD, by MCP), which it would otherwise never
// see: a wait decided is no longer open, however long c4-sync was away.
// Decisions the tracker relayed are its own already. Once each.
func (s *Syncer) mirrorDecided(ctx context.Context, t *c4v1.Tracker, c *conn, l *c4v1.TrackerLink, feed []api.Transition) error {
	if !c.adapter.Capabilities().EditableComments || l.Spec.Target.WorkJob == "" {
		return nil
	}
	jobs := append([]string{l.Spec.Target.WorkJob}, l.Status.Versions...)
	on := tracker.Ref{Type: l.Spec.Ref.Type, ID: l.Spec.Ref.ID, URL: l.Spec.Ref.URL}
	for _, tr := range feed {
		if tr.Kind != "wait-decided" || !slices.Contains(jobs, strings.TrimPrefix(tr.Job, l.Namespace+"/")) ||
			strings.Contains(tr.Actor, " via tracker "+t.Name+" ") {
			continue
		}
		field := "decided/" + tracker.WriteKey(tr.Key, "", "")[:12]
		unit := "a wait"
		if tr.Unit != nil {
			unit = fmt.Sprintf("unit %d", *tr.Unit)
		}
		body := fmt.Sprintf("C4: %s of %s was decided %s by %s, in C4, at %s.", unit, tr.Job, tr.Do, or(tr.Actor, "someone"),
			tr.At.UTC().Format("Mon 2 Jan 15:04 UTC"))
		if tr.Record != "" {
			body += " Record " + tr.Record + "."
		}
		key := tracker.WriteKey(l.Namespace+"/"+l.Name, field, "")
		if err := s.write(ctx, c, l, field, "told", "", 0, s.now(), func(tracker.Written) tracker.Write {
			return tracker.Write{On: on, Kind: "comment", Fields: map[string]string{"subject": "C4: decided in C4"}, Note: &tracker.Note{Body: body}, Key: key}
		}); err != nil {
			return err
		}
	}
	return nil
}

func or(s, d string) string {
	if s == "" {
		return d
	}
	return s
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

func JobName(tr string, r tracker.Ref) string { return jobDNS(tr + "-" + r.ID) }

// jobDNS is dns within a WorkJob name's 50 characters (its unit pods are
// named after it).
func jobDNS(s string) string {
	s = dns(s)
	if len(s) > 50 {
		h := sha256.Sum256([]byte(s))
		s = strings.TrimRight(s[:39], "-") + "-" + hex.EncodeToString(h[:])[:10]
	}
	return s
}

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

// intake makes jobs and links for matching entities, and returns the
// links whose entity it saw.
func (s *Syncer) intake(ctx context.Context, t *c4v1.Tracker, c *conn, links []*c4v1.TrackerLink) (map[string]bool, error) {
	seen := map[string]bool{}
	byName := map[string]*c4v1.TrackerLink{}
	for _, l := range links {
		byName[l.Name] = l
	}
	for i, rule := range t.Spec.Intake {
		page := ""
		for {
			items, next, err := c.adapter.List(ctx, tracker.Query{Type: rule.Match.Type, Labels: rule.Match.Labels,
				State: rule.Match.State, Fields: rule.Match.Fields}, page)
			if err != nil {
				return nil, err
			}
			for _, it := range items {
				name := LinkName(t.Name, it.Ref)
				seen[name] = true
				if err := s.intakeItem(ctx, t, i, rule, it, byName[name]); err != nil {
					return nil, err
				}
			}
			if next == "" {
				break
			}
			page = next
		}
	}
	return seen, nil
}

// Deadline turns a tracker due date into a deadline. A date with no time
// of day falls at the rule's local time on that date, else at the
// project's due time (conv, Project spec.work.dueTime; 18:00 UTC when
// nil), as the farm reads a date sent to it.
func Deadline(d *tracker.Due, rule c4v1.TrackerDeadline, conv *c4v1.DueTime) (*time.Time, string, error) {
	if d == nil {
		return nil, "no due date", nil
	}
	if !d.DateOnly && rule.DateOnlyAt == "" {
		t := d.At.UTC()
		return &t, "", nil
	}
	if rule.DateOnlyAt != "" {
		conv = &c4v1.DueTime{At: rule.DateOnlyAt, TimeZone: rule.TimeZone}
	}
	at, err := conv.DueOn(d.At.UTC().Format("2006-01-02"))
	if err != nil {
		return nil, "", fmt.Errorf("intake deadline: %w", err)
	}
	at = at.UTC()
	return &at, "", nil
}

// deadline is Deadline under the due-time convention of the project that
// owns ns.
func (s *Syncer) deadline(ctx context.Context, ns string, d *tracker.Due, rule c4v1.TrackerDeadline) (*time.Time, string, error) {
	var conv *c4v1.DueTime
	if d != nil && d.DateOnly && rule.DateOnlyAt == "" {
		var ps c4v1.ProjectList
		if err := s.Client.List(ctx, &ps); err != nil {
			return nil, "", fmt.Errorf("reading the project's due time: %w", err)
		}
		for _, p := range ps.Items {
			if slices.Contains(p.Spec.Namespaces, ns) && p.Spec.Work != nil {
				conv = p.Spec.Work.DueTime
			}
		}
	}
	return Deadline(d, rule, conv)
}

func (s *Syncer) intakeItem(ctx context.Context, t *c4v1.Tracker, idx int, rule c4v1.TrackerIntake, it tracker.Item, l *c4v1.TrackerLink) error {
	due, why, err := s.deadline(ctx, t.Namespace, it.Due, rule.Deadline)
	if err != nil {
		return err
	}
	switch rule.As {
	case "milestone":
		if due == nil {
			return nil
		}
		return s.ensureMilestone(ctx, t, idx, rule, it, *due)
	case "job", "forecast":
	default:
		return fmt.Errorf("intake %d: as %q", idx, rule.As)
	}
	if rule.Job == nil {
		return fmt.Errorf("intake %d: as %s needs a job spec", idx, rule.As)
	}
	jr := farm.JobRef{Namespace: t.Namespace, Name: JobName(t.Name, it.Ref)}
	if rule.As == "forecast" {
		jr.Name = jobDNS(jr.Name + "-forecast")
	}
	replaces := ""
	if l != nil && rule.As == "job" && l.Spec.Intake != int32(idx) && int(l.Spec.Intake) < len(t.Spec.Intake) &&
		t.Spec.Intake[l.Spec.Intake].As == "forecast" {
		// The entity was a forecast and is ready now: the real job
		// replaces it, and the link moves to it.
		replaces = l.Spec.Target.WorkJob
	}
	if l != nil && rule.As == "forecast" && int(l.Spec.Intake) < len(t.Spec.Intake) && t.Spec.Intake[l.Spec.Intake].As == "job" {
		return s.followDue(ctx, l, t.Spec.Intake[l.Spec.Intake], it) // already real
	}
	if l != nil && rule.As == "forecast" {
		if err := s.followBid(ctx, l, rule, it); err != nil {
			return err
		}
	}
	if l == nil || replaces != "" {
		spec := *rule.Job.DeepCopy()
		spec.Deadline, spec.DeadlineMode = nil, ""
		if rule.As == "forecast" {
			spec.Forecast = forecastOf(rule, it)
		}
		if due != nil {
			spec.Deadline = &metav1.Time{Time: *due}
			spec.DeadlineMode = "Tracking"
		}
		var got farm.JobRef
		if it.Job != "" {
			// The entity names a job a client already submitted: adopt it.
			ns, name, ok := strings.Cut(it.Job, "/")
			if !ok || ns != t.Namespace || name == "" {
				s.setTrackerCond(ctx, t, CondIntake, metav1.ConditionFalse, "ForeignJob",
					fmt.Sprintf("%s names job %q, which is not in this Tracker's namespace %s", it.Ref, it.Job, t.Namespace))
				return nil
			}
			if _, err := s.Farm.Job(ctx, farm.JobRef{Namespace: ns, Name: name}); err != nil {
				return fmt.Errorf("adopting %s for %s: %w", it.Job, it.Ref, err)
			}
			got = farm.JobRef{Namespace: ns, Name: name}
		} else {
			person, why := forPerson(t, rule, it)
			if why != "" {
				s.setTrackerCond(ctx, t, CondIntake, metav1.ConditionFalse, "NoPerson", why)
				return nil
			}
			var err error
			got, err = s.Farm.Submit(ctx, farm.JobRequest{Namespace: jr.Namespace, Name: jr.Name, Spec: spec,
				TrackerRef: t.Name + "/" + it.Ref.String(), For: person, Replaces: replaces})
			if refused(err) {
				s.setTrackerCond(ctx, t, CondIntake, metav1.ConditionFalse, "NoGrant", noGrant(person, err))
				return nil
			}
			if err != nil {
				return fmt.Errorf("submitting %s for %s: %w", jr, it.Ref, err)
			}
			if person != "" {
				s.clearTrackerCond(ctx, t, CondIntake, "NoGrant", "Granted", person+" lets this tracker submit work for them")
			}
		}
		if replaces != "" {
			before := l.DeepCopy()
			l.Spec.Target.WorkJob, l.Spec.Intake = got.Name, int32(idx)
			return s.Client.Patch(ctx, l, client.MergeFrom(before))
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
	return s.followDue(ctx, l, rule, it)
}

// forecastOf sizes a forecast job from its rule: the entity's own
// estimate times PerUnit, else the rule's slot-hours, else (empty) the
// class's plan of the job's params.
func forecastOf(rule c4v1.TrackerIntake, it tracker.Item) *c4v1.WorkForecast {
	f := &c4v1.WorkForecast{}
	r := rule.Forecast
	if r == nil {
		return f
	}
	f.SlotHours, f.UnitHours = r.SlotHours, r.UnitHours
	if r.Field != "" {
		v, err1 := strconv.ParseFloat(it.Fields[r.Field], 64)
		per, err2 := strconv.ParseFloat(r.PerUnit, 64)
		if err1 == nil && err2 == nil && v > 0 && per > 0 {
			f.SlotHours = strconv.FormatFloat(v*per, 'f', -1, 64)
		}
	}
	return f
}

// followBid resizes a forecast job when what sizes it (the entity's bid,
// or the rule) changed since it was made: the slot-hours and the unit
// hours, the two the rule sets; anything else on the forecast is left as
// it is.
func (s *Syncer) followBid(ctx context.Context, l *c4v1.TrackerLink, rule c4v1.TrackerIntake, it tracker.Item) error {
	if l.Spec.Target.WorkJob == "" {
		return nil
	}
	jr := farm.JobRef{Namespace: l.Namespace, Name: l.Spec.Target.WorkJob}
	j, err := s.Farm.Job(ctx, jr)
	if err != nil || j == nil || j.Forecast == nil {
		return nil // gone, or real already: nothing to size
	}
	want := forecastOf(rule, it)
	var c api.ForecastChange
	if !sameHours(want.SlotHours, j.Forecast.SlotHours) {
		c.SlotHours = &want.SlotHours
	}
	if !sameHours(want.UnitHours, j.Forecast.UnitHours) {
		c.UnitHours = &want.UnitHours
	}
	if c.SlotHours == nil && c.UnitHours == nil {
		return nil
	}
	if err := s.Farm.Resize(ctx, jr, c); err != nil {
		return fmt.Errorf("resizing %s for %s: %w", jr, it.Ref, err)
	}
	return nil
}

// withdraw deletes a link's forecast job, signed and booked by the HUD,
// then the link: if the entity matches a rule again, it is a new forecast.
func (s *Syncer) withdraw(ctx context.Context, l *c4v1.TrackerLink, why string) error {
	if l.Spec.Target.WorkJob != "" {
		err := s.Farm.Withdraw(ctx, farm.JobRef{Namespace: l.Namespace, Name: l.Spec.Target.WorkJob}, why)
		if err != nil && !errors.Is(err, farm.ErrNotFound) {
			return fmt.Errorf("withdrawing %s/%s: %w", l.Namespace, l.Spec.Target.WorkJob, err)
		}
	}
	if err := s.Client.Delete(ctx, l); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// sameHours compares two decimal strings as numbers ("30" is "30.0").
func sameHours(a, b string) bool {
	if a == b {
		return true
	}
	x, err1 := strconv.ParseFloat(a, 64)
	y, err2 := strconv.ParseFloat(b, 64)
	return err1 == nil && err2 == nil && x == y
}

// forPerson is whom an intake rule's job is for: "" for c4-sync's own
// work, else a C4 person; why says why no person could be named.
func forPerson(t *c4v1.Tracker, rule c4v1.TrackerIntake, it tracker.Item) (person, why string) {
	switch f := rule.For; {
	case f == nil:
		return "", ""
	case f.Person != "":
		return f.Person, ""
	case f.Assignee:
		for _, a := range it.Assignees {
			if p := t.Spec.People[a.ID]; p != "" {
				return p, ""
			}
		}
		return "", fmt.Sprintf("%s has no assignee that people maps to a C4 person, so there is no one to submit it for", it.Ref)
	}
	return "", "intake for names neither a person nor the assignee"
}

// refused is the HUD declining to submit for a person: no grant (403),
// or a HUD that does not take grants (501).
func refused(err error) bool {
	var ae *farm.APIError
	return errors.As(err, &ae) && (ae.Status == http.StatusForbidden || ae.Status == http.StatusNotImplemented)
}

func noGrant(person string, err error) string {
	return fmt.Sprintf("%s has not let this tracker submit work for them; they can grant it on the Tracker's page in the HUD (%v)", person, err)
}

// ensureMilestone keeps the entity's Milestone: its due date, covering
// the jobs whose labels match the entity's fields (the rule's covers).
// The Tracker owns it, so it goes when the Tracker does.
func (s *Syncer) ensureMilestone(ctx context.Context, t *c4v1.Tracker, idx int, rule c4v1.TrackerIntake, it tracker.Item, due time.Time) error {
	if len(rule.Covers) == 0 {
		s.setTrackerCond(ctx, t, CondIntake, metav1.ConditionFalse, "MilestoneCoversNothing",
			fmt.Sprintf("intake %d makes milestones but names no covers (job label: entity field)", idx))
		return nil
	}
	sel := map[string]string{}
	for label, field := range rule.Covers {
		v := it.Fields[field]
		if v == "" {
			s.setTrackerCond(ctx, t, CondIntake, metav1.ConditionFalse, "MilestoneCoversNothing",
				fmt.Sprintf("%s has no %s for the milestone's %s", it.Ref, field, label))
			return nil
		}
		sel[label] = v
	}
	m := &c4v1.Milestone{ObjectMeta: metav1.ObjectMeta{Namespace: t.Namespace, Name: LinkName(t.Name, it.Ref)}}
	_, err := controllerutil.CreateOrPatch(ctx, s.Client, m, func() error {
		if m.Labels == nil {
			m.Labels = map[string]string{}
		}
		m.Labels[c4v1.LabelTracker] = t.Name
		m.Spec.Due = metav1.NewTime(due)
		m.Spec.Selector = metav1.LabelSelector{MatchLabels: sel}
		m.Spec.Source = &c4v1.MilestoneSource{Tracker: t.Name, Ref: it.Ref.String()}
		return controllerutil.SetControllerReference(t, m, s.Client.Scheme())
	})
	return err
}

// followDue moves a link's job deadline when the tracker's due date moved,
// while the job still follows tracking. A person who pinned another time
// in the HUD wins; the link says so instead.
func (s *Syncer) followDue(ctx context.Context, l *c4v1.TrackerLink, rule c4v1.TrackerIntake, it tracker.Item) error {
	due, _, err := s.deadline(ctx, l.Namespace, it.Due, rule.Deadline)
	if err != nil || due == nil || l.Spec.Target.WorkJob == "" {
		return err
	}
	jr := farm.JobRef{Namespace: l.Namespace, Name: l.Spec.Target.WorkJob}
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

// --- what is under review ---

// findReviews asks the adapter, when it finds reviews (GitHub's pull
// requests), what is under review for links that have nothing yet.
func (s *Syncer) findReviews(ctx context.Context, t *c4v1.Tracker, c *conn, links []*c4v1.TrackerLink) error {
	f, ok := c.adapter.(tracker.ReviewFinder)
	if !ok {
		return nil
	}
	var need []tracker.LinkState
	byLink := map[string]*c4v1.TrackerLink{}
	for _, l := range links {
		if l.Status.Review == nil && l.Spec.Target.WorkJob != "" {
			ls := s.baseState(l)
			need = append(need, ls)
			byLink[ls.Link] = l
		}
	}
	if len(need) == 0 {
		return nil
	}
	found, err := f.FindReviews(ctx, need)
	if err != nil {
		return err
	}
	for k, r := range found {
		ref := c4v1.TrackerRef{Type: r.Type, ID: r.ID, URL: r.URL}
		if err := s.patchLinkStatus(ctx, byLink[k], func(st *c4v1.TrackerLinkStatus) { st.Review = &ref }); err != nil {
			return err
		}
	}
	return nil
}

// baseState is what a link says about itself, before its job is read.
func (s *Syncer) baseState(l *c4v1.TrackerLink) tracker.LinkState {
	jr := farm.JobRef{Namespace: l.Namespace, Name: l.Spec.Target.WorkJob}
	ls := tracker.LinkState{
		Link:       l.Namespace + "/" + l.Name,
		Ref:        tracker.Ref{Type: l.Spec.Ref.Type, ID: l.Spec.Ref.ID, URL: l.Spec.Ref.URL},
		ReviewHead: l.Status.ReviewHead,
		Job:        jr.String(),
		JobURL:     s.Farm.JobURL(jr),
	}
	if r := l.Status.Review; r != nil {
		ls.Review = &tracker.Ref{Type: r.Type, ID: r.ID, URL: r.URL}
	}
	return ls
}

// linkState is the link and its job's summary, as an adapter presents it.
func (s *Syncer) linkState(l *c4v1.TrackerLink, sum Summary) tracker.LinkState {
	ls := s.baseState(l)
	ls.State, ls.Words, ls.Title, ls.Why = sum.State, sum.Words(), sum.Title(), sum.Why
	ls.Active, ls.ETA, ls.AtRisk, ls.Deadline = sum.Active(), sum.ETA, sum.AtRisk(), sum.Deadline
	ls.DeadlineSource = deadlineSource(sum)
	if w := sum.Wait; w != nil {
		ls.Wait = &tracker.WaitState{Server: w.WaitsFor, Choices: w.Choices, Escalated: w.Escalated}
	}
	return ls
}

func deadlineSource(s Summary) string {
	switch {
	case s.Mode == "Tracking":
		return "from the tracker"
	case s.Source == "suggested":
		return "suggested by the farm"
	}
	return "set in C4"
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
	var rule c4v1.TrackerIntake
	if int(l.Spec.Intake) < len(t.Spec.Intake) {
		rule = t.Spec.Intake[l.Spec.Intake]
	}
	if rule.As == "forecast" {
		// A forecast never runs: nothing to tell the tracker but its date,
		// which intake follows (DESIGN.md 10.12, forecast jobs).
		s.setLinkCond(ctx, l, CondSynced, metav1.ConditionTrue, "Forecast", "a forecast job; the tracker hears from it once real work replaces it")
		return nil
	}
	review := ""
	switch {
	case rule.Review != nil:
		review = rule.Review.Wait
	case rule.Job != nil && rule.Job.Review != nil:
		// The job's own review (spec.review) is the one the tracker serves.
		review = class.ReviewName
	}
	jr := farm.JobRef{Namespace: l.Namespace, Name: l.Spec.Target.WorkJob}
	// A rework relayed for this job whose next version was not made (a
	// pass that failed between relaying and moving the link) is finished
	// now: submitting and storing are idempotent.
	if rule.Rework == "nextVersion" {
		if d := lastDecision(l); d != nil && d.Do == "rework" && d.Job == jr.Name {
			return s.nextVersion(ctx, t, l, j, rule)
		}
	}
	sum := Summarize(j, review, reworking(l))

	// Decisions first: a decision relayed now changes what is written.
	if sum.Wait != nil {
		do, err := s.relay(ctx, t, c, l, j, jr, sum.Wait)
		if err != nil {
			return err
		}
		if do == "rework" && rule.Rework == "nextVersion" {
			return s.nextVersion(ctx, t, l, j, rule)
		}
		if do != "" {
			if j, err = s.Farm.Job(ctx, jr); err != nil {
				return err
			}
			if j == nil {
				return nil
			}
			sum = Summarize(j, review, reworking(l))
		}
	}
	// Publish before presenting: a published review entity (a Flow
	// Version) is what the presentation then points at.
	if err := s.publish(ctx, c, l, jr, sum); err != nil {
		return err
	}
	if err := s.reviewHead(ctx, c, l, fresh); err != nil {
		return err
	}
	ls := s.linkState(l, sum)
	cfg := tracker.PresentConfig{States: t.Spec.StatusOut.States, CheckName: t.Spec.StatusOut.CheckName,
		CommitStatus: t.Spec.StatusOut.CommitStatus}
	now, every := s.now(), seconds(t.Spec.StatusOut.FieldSeconds, 300)
	for _, fw := range c.presenter.Present(ls, cfg) {
		if err := s.write(ctx, c, l, fw.Field, fw.Value, fw.Continuous, every, now, fw.Build); err != nil {
			return err
		}
	}
	if err := s.mirrorAssignments(ctx, c, t, l, j, jr, now); err != nil {
		return err
	}
	if err := s.mirrorCauses(ctx, c, l, j, jr, now); err != nil {
		return err
	}
	if l.Status.State != sum.State {
		if err := s.patchLinkStatus(ctx, l, func(st *c4v1.TrackerLinkStatus) { st.State = sum.State }); err != nil {
			return err
		}
	}
	s.setLinkCond(ctx, l, CondSynced, metav1.ConditionTrue, "Synced", "")
	return nil
}

// mirrorAssignments tells a person, where they work, when failed units
// are assigned to them in C4 (docs/c4-sync.md, Assignments): a mention on the
// entity (an @-mention on the issue, a Note addressed to them on the
// Task), once per assignment. A C4 person the Tracker's people map gives
// no tracker user stays assigned in C4 only, and the link says so.
func (s *Syncer) mirrorAssignments(ctx context.Context, c *conn, t *c4v1.Tracker, l *c4v1.TrackerLink, j *api.Job, jr farm.JobRef, now time.Time) error {
	if !c.adapter.Capabilities().Mentions {
		return nil
	}
	for _, a := range j.Acks {
		if a.Kind != "assign" || a.Assignee == "" {
			continue
		}
		field := "assign/" + tracker.WriteKey(jr.String(), a.Units, a.Assignee+"@"+a.At.UTC().Format(time.RFC3339))[:12]
		user := trackerUser(t, a.Assignee)
		if user == "" {
			s.setLinkCond(ctx, l, "Assigned", metav1.ConditionFalse, "NoTrackerUser",
				fmt.Sprintf("units %s are assigned to %s, who has no tracker user in the Tracker's people; the assignment stays in C4", a.Units, a.Assignee))
			continue
		}
		text := fmt.Sprintf("C4: units %s of %s failed and are assigned to you", a.Units, jr)
		if a.Actor != "" {
			text += " by " + a.Actor
		}
		if a.Note != "" {
			text += ": " + a.Note
		}
		text += ". " + s.Farm.JobURL(jr)
		on := tracker.Ref{Type: l.Spec.Ref.Type, ID: l.Spec.Ref.ID, URL: l.Spec.Ref.URL}
		key := tracker.WriteKey(l.Namespace+"/"+l.Name, field, "")
		if err := s.write(ctx, c, l, field, "told "+user, "", 0, now, func(tracker.Written) tracker.Write {
			return tracker.Write{On: on, Kind: "mention", Fields: map[string]string{"user": user, "subject": "C4: units assigned to you"},
				Note: &tracker.Note{Body: text}, Key: key}
		}); err != nil {
			return err
		}
	}
	return nil
}

// mirrorCauses keeps one comment per cause a person must act on (units
// failed for good, escalated, or held for information; DESIGN.md 10.14,
// Causes) on the entity, edited in place as the cause grows: a tracker reads
// one item per cause, not one per failure.
func (s *Syncer) mirrorCauses(ctx context.Context, c *conn, l *c4v1.TrackerLink, j *api.Job, jr farm.JobRef, now time.Time) error {
	if !c.adapter.Capabilities().EditableComments {
		return nil
	}
	on := tracker.Ref{Type: l.Spec.Ref.Type, ID: l.Spec.Ref.ID, URL: l.Spec.Ref.URL}
	for _, cs := range j.Causes {
		if !slices.ContainsFunc(cs.Did, func(d string) bool { return d == "failed" || d == "escalated" || d == "held" }) {
			continue
		}
		field := "cause/" + tracker.WriteKey(jr.String(), cs.Key, "")[:12]
		body := causeComment(cs, jr, s.Farm.JobURL(jr))
		key := tracker.WriteKey(l.Namespace+"/"+l.Name, field, "")
		if err := s.write(ctx, c, l, field, body, "", 0, now, func(prev tracker.Written) tracker.Write {
			n := &tracker.Note{Body: body}
			if prev.Ref != "" {
				n.Replace = &tracker.Ref{Type: "Comment", ID: prev.Ref}
			}
			return tracker.Write{On: on, Kind: "comment", Fields: map[string]string{"subject": "C4: " + cs.Title}, Note: n, Key: key}
		}); err != nil {
			return err
		}
	}
	return nil
}

// causeComment is a cause as a tracker comment.
func causeComment(c api.Cause, jr farm.JobRef, url string) string {
	attempts := "attempt"
	if c.Count != 1 {
		attempts += "s"
	}
	b := fmt.Sprintf("C4 cause on %s: %s. %d %s, units %s", jr, c.Title, c.Count, attempts, c.Units)
	if len(c.Nodes) > 0 {
		b += ", on " + strings.Join(c.Nodes, ", ")
	}
	b += "."
	if len(c.Did) > 0 {
		b += " The farm " + strings.Join(c.Did, ", then ") + "."
	}
	if c.To != "" {
		b += " Escalated to " + c.To + "."
	}
	return b + " " + url
}

// trackerUser is the tracker user the Tracker's people map makes person,
// or "" when none does.
func trackerUser(t *c4v1.Tracker, person string) string {
	users := make([]string, 0, len(t.Spec.People))
	for u, p := range t.Spec.People {
		if p == person {
			users = append(users, u)
		}
	}
	sort.Strings(users)
	if len(users) == 0 {
		return ""
	}
	return users[0]
}

// reviewHead reads the head of what is under review again when it may
// have changed, for adapters whose reviews have heads.
func (s *Syncer) reviewHead(ctx context.Context, c *conn, l *c4v1.TrackerLink, fresh bool) error {
	if l.Status.Review == nil || !c.adapter.Capabilities().ReviewHeads || (!fresh && l.Status.ReviewHead != "") {
		return nil
	}
	it, err := c.adapter.Get(ctx, tracker.Ref{Type: l.Status.Review.Type, ID: l.Status.Review.ID})
	if err != nil {
		return err
	}
	head := it.Fields["head"]
	if head == l.Status.ReviewHead {
		return nil
	}
	return s.patchLinkStatus(ctx, l, func(st *c4v1.TrackerLinkStatus) { st.ReviewHead = head })
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

// DefaultVerdicts maps GitHub's decisions to C4's, for a decision the
// Tracker's verdictsIn does not name (docs/c4-sync.md), by adapters with no
// defaults of their own (tracker.Verdicter). A COMMENTED review decides
// nothing.
var DefaultVerdicts = map[string]string{
	"APPROVED":          "approve",
	"CHANGES_REQUESTED": "rework",
	"action:approve":    "approve",
	"action:rework":     "rework",
	"action:accept":     "accept",
}

// verdictFor is the C4 decision for a tracker's native word: the Tracker's
// verdictsIn, else the adapter's own default (tracker.Verdicter), else, for
// adapters without one, DefaultVerdicts: those are GitHub's words.
func verdictFor(t *c4v1.Tracker, a tracker.Adapter, native string) string {
	if v, ok := t.Spec.VerdictsIn[native]; ok {
		return v
	}
	if vr, ok := a.(tracker.Verdicter); ok {
		return vr.Verdict(native)
	}
	return DefaultVerdicts[native]
}

// relay sends the newest decision not yet relayed to the open wait. Older
// unrelayed decisions are superseded by it: a reviewer who requested
// changes and then approved has approved.
func (s *Syncer) relay(ctx context.Context, t *c4v1.Tracker, c *conn, l *c4v1.TrackerLink, j *api.Job, jr farm.JobRef, w *api.Wait) (string, error) {
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
			return "", err
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
		if done[d.Source.String()] || verdictFor(t, c.adapter, d.Native) == "" || d.At.Before(w.Since.Add(-time.Second)) {
			continue
		}
		if pick == nil || d.At.After(pick.At) {
			pick = d
		}
	}
	if pick == nil {
		return "", nil
	}
	rec := c4v1.TrackerDecision{Source: pick.Source.String(), By: pick.By.ID, Unit: unitRef(w), Job: jr.Name,
		At: metav1.NewTime(pick.At), Note: clip(pick.Body, 4096)}
	do := verdictFor(t, c.adapter, pick.Native)
	if w.Escalated && do == "approve" {
		do = "accept" // an escalated unit is accepted, not approved
	}
	rec.Do = do
	person, reason := s.authorize(t, w, pick.By)
	if reason != "" {
		rec.Error = reason
		return "", s.recordDecision(ctx, l, rec)
	}
	if !allowed(w, do) {
		rec.Error = fmt.Sprintf("the wait does not take %q (choices: %s); add it to the wait's choices or map the tracker's decision to one", do, strings.Join(w.Choices, ", "))
		return "", s.recordDecision(ctx, l, rec)
	}
	err := s.Farm.Decide(ctx, jr, farm.Relay{
		Project: j.Project, Unit: rec.Unit, Do: do, Note: pick.Body, Reply: w.Reply,
		Tracker: t.Name, Source: pick.Source.URL + " (" + pick.Source.String() + ")",
		By: "@" + pick.By.ID, Person: person, Evidence: pick.Evidence,
	})
	if errors.Is(err, farm.ErrNotOpen) {
		rec.Error = err.Error()
		return "", s.recordDecision(ctx, l, rec)
	}
	if err != nil {
		return "", err
	}
	return do, s.recordDecision(ctx, l, rec)
}

func unitRef(w *api.Wait) string {
	if w.Name != "" {
		return w.Name
	}
	return strconv.Itoa(w.Unit)
}

// allowed is the queue's rule (internal/work/queue/wait.go): a wait with
// choices takes one of them or fail; one without takes anything.
func allowed(w *api.Wait, do string) bool {
	if w.Escalated {
		return do == "accept" || do == "rework" || do == "fail"
	}
	if do == "fail" || len(w.Choices) == 0 {
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

// write makes one field's write when its value changed, or when only its
// continuous part changed and every has passed. The write goes through
// the effect journal (internal/effect; docs/c4-sync.md, "What it writes,
// and what it never writes"), which is the
// link's status.written: the intent is kept before it is sent, so a write
// interrupted by a crash or a lost answer is probed, sent again, or held
// for a person, as the adapter's guard for it allows, and never made twice
// on a guess. The previous write's fingerprint and the tracker entity it
// made are kept there too.
func (s *Syncer) write(ctx context.Context, c *conn, l *c4v1.TrackerLink, field, value, cont string, every time.Duration, now time.Time,
	build func(prev tracker.Written) tracker.Write) error {
	prev := l.Status.Written[field]
	full := value + "#" + cont
	switch {
	case prev.Value == full && prev.State == "":
		return nil
	case prev.State == "" && stateOf(prev.Value) == value && (every == 0 || now.Sub(prev.At.Time) < every):
		return nil // only the ETA moved, and not long ago
	}
	w := build(tracker.Written{Value: prev.Value, Ref: prev.Ref})
	_, err := effect.Do(ctx, &linkJournal{s: s, l: l, field: field, was: prev}, effect.Call{
		Key: field, Intent: full, Guard: c.adapter.Guard(w), Definite: tracker.Definite,
		Send: func(ctx context.Context) (string, error) {
			ref, err := c.adapter.Apply(ctx, w)
			if err != nil {
				return "", err
			}
			if ref.ID == "" {
				return prev.Ref, nil
			}
			return ref.ID, nil
		},
		Probe: func(ctx context.Context) (bool, string, error) {
			done, ref, err := c.adapter.Probe(ctx, w)
			return done, ref.ID, err
		},
	}, now)
	var held *effect.HeldError
	if errors.As(err, &held) {
		s.setLinkCond(ctx, l, CondUnconfirmed, metav1.ConditionTrue, "Held",
			fmt.Sprintf("the %s write was interrupted, and %s cannot tell whether it happened: a person confirms it", field, c.adapter.Kind()))
		return nil
	}
	if err == nil {
		if cond := meta.FindStatusCondition(l.Status.Conditions, CondUnconfirmed); cond != nil && cond.Status == metav1.ConditionTrue && !anyHeld(l) {
			s.setLinkCond(ctx, l, CondUnconfirmed, metav1.ConditionFalse, "Confirmed", "")
		}
	}
	return err
}

func anyHeld(l *c4v1.TrackerLink) bool {
	for _, w := range l.Status.Written {
		if w.State == string(effect.Unconfirmed) {
			return true
		}
	}
	return false
}

// linkJournal is the effect journal for one field of a link: its
// status.written entry. was is the entry as the write began; Drop puts a
// written one back (a refused write leaves the last one standing), or
// keeps only its entity.
type linkJournal struct {
	s     *Syncer
	l     *c4v1.TrackerLink
	field string
	was   c4v1.TrackerWritten
}

func (j *linkJournal) Load(_ context.Context, key string) (effect.Entry, bool, error) {
	w, ok := j.l.Status.Written[key]
	st := effect.Done
	if w.State != "" {
		st = effect.State(w.State)
	}
	return effect.Entry{Key: key, Intent: w.Value, State: st, Ref: w.Ref, At: w.At.Time}, ok, nil
}

func (j *linkJournal) Save(ctx context.Context, e effect.Entry) error {
	w := c4v1.TrackerWritten{Value: e.Intent, Ref: e.Ref, At: metav1.NewTime(e.At)}
	if e.State != effect.Done {
		w.State = string(e.State)
	}
	return j.put(ctx, e.Key, w)
}

func (j *linkJournal) Drop(ctx context.Context, key string) error {
	w := j.was
	if w.State != "" {
		w = c4v1.TrackerWritten{Ref: w.Ref, At: w.At}
	}
	return j.put(ctx, key, w)
}

func (j *linkJournal) put(ctx context.Context, key string, w c4v1.TrackerWritten) error {
	return j.s.patchLinkStatus(ctx, j.l, func(st *c4v1.TrackerLinkStatus) {
		if st.Written == nil {
			st.Written = map[string]c4v1.TrackerWritten{}
		}
		st.Written[key] = w
	})
}

func stateOf(v string) string {
	s, _, _ := strings.Cut(v, "#")
	return s
}

func (s *Syncer) remember(ctx context.Context, l *c4v1.TrackerLink, field, value, ref string, now time.Time) error {
	return s.patchLinkStatus(ctx, l, func(st *c4v1.TrackerLinkStatus) {
		if st.Written == nil {
			st.Written = map[string]c4v1.TrackerWritten{}
		}
		st.Written[field] = c4v1.TrackerWritten{Value: value, Ref: ref, At: metav1.NewTime(now)}
	})
}

// --- outputs out ---

// publish puts the job's output in the tracker once there is one to look
// at: when its review wait opens, or when it is done. Each output view is
// published once. Where publishing makes the entity people review (a Flow
// Version), that becomes the link's review.
func (s *Syncer) publish(ctx context.Context, c *conn, l *c4v1.TrackerLink, jr farm.JobRef, sum Summary) error {
	if sum.Output == "" || (sum.State != StateInReview && sum.State != StateDone && sum.State != StateDoneAccepted) {
		return nil
	}
	if prev := l.Status.Written["publish"]; prev.Value == sum.Output {
		return nil
	}
	on := tracker.Ref{Type: l.Spec.Ref.Type, ID: l.Spec.Ref.ID, URL: l.Spec.Ref.URL}
	forReview := c.adapter.Capabilities().PublishForReview
	if r := l.Status.Review; r != nil && !forReview {
		on = tracker.Ref{Type: r.Type, ID: r.ID}
	}
	url := s.Farm.JobURL(jr)
	out := tracker.Output{Name: jr.String(), Version: len(l.Status.Versions) + 1, JobURL: url, View: sum.Output,
		Custody: strings.TrimSuffix(url, "/") + "/provenance"}
	if n := len(l.Status.Versions); n > 0 {
		out.Follows = farm.JobRef{Namespace: l.Namespace, Name: l.Status.Versions[n-1]}.String()
	}
	// A still, where the farm has one: best effort, the output publishes
	// without it.
	if name, data, err := s.Farm.Preview(ctx, jr); err == nil && len(data) > 0 {
		out.Preview = &tracker.Still{Name: name, Data: data}
	} else if err != nil && !errors.Is(err, farm.ErrNotFound) {
		s.Log.Error(err, "reading the output's preview; publishing without it", "job", jr.String())
	}
	ref, err := c.adapter.Publish(ctx, on, out, tracker.WriteKey(l.Namespace+"/"+l.Name, "publish", sum.Output))
	if err != nil {
		return err
	}
	if err := s.remember(ctx, l, "publish", sum.Output, ref.ID, s.now()); err != nil {
		return err
	}
	if forReview {
		r := c4v1.TrackerRef{Type: ref.Type, ID: ref.ID, URL: ref.URL}
		return s.patchLinkStatus(ctx, l, func(st *c4v1.TrackerLinkStatus) { st.Review, st.ReviewHead = &r, "" })
	}
	return nil
}

// nextVersion answers a rework the media way, as Flow's dailies do: a
// new job follows the one reviewed, with the reviewer's note as an input
// view, and the link moves to it. The tracker counts versions, so the next
// output publishes as the next version.
func (s *Syncer) nextVersion(ctx context.Context, t *c4v1.Tracker, l *c4v1.TrackerLink, j *api.Job, rule c4v1.TrackerIntake) error {
	if rule.Job == nil {
		return fmt.Errorf("intake %d: nextVersion needs a job spec", l.Spec.Intake)
	}
	prev := l.Spec.Target.WorkJob
	n := len(l.Status.Versions) + 2
	base := JobName(t.Name, tracker.Ref{ID: l.Spec.Ref.ID})
	name := dns(fmt.Sprintf("%s-v%03d", base, n))
	note := lastDecisionNote(l)
	var inputs []api.SubmitInput
	if note != "" {
		view, err := s.Farm.StoreView(ctx, j.Project, map[string][]byte{"note.md": []byte(note)})
		if err != nil {
			return fmt.Errorf("storing the note for %s: %w", name, err)
		}
		inputs = append(inputs, api.SubmitInput{View: view, MountPath: NoteMount, ReadOnly: true})
	}
	spec := *rule.Job.DeepCopy()
	spec.Deadline, spec.DeadlineMode = nil, ""
	if d := j.Objective.Deadline; d != nil {
		spec.Deadline = &metav1.Time{Time: *d}
		spec.DeadlineMode = j.Objective.Mode
	}
	person := ""
	if rule.For != nil {
		person = j.Owner // the next version is for whom the last one was
	}
	_, err := s.Farm.Submit(ctx, farm.JobRequest{Namespace: l.Namespace, Name: name, Spec: spec, Inputs: inputs,
		Follows: prev, TrackerRef: t.Name + "/" + l.Spec.Ref.Type + "/" + l.Spec.Ref.ID, For: person})
	if refused(err) {
		s.setLinkCond(ctx, l, "Version", metav1.ConditionFalse, "NoGrant", noGrant(person, err))
		return nil
	}
	if err != nil {
		return fmt.Errorf("submitting %s after %s: %w", name, prev, err)
	}
	before := l.DeepCopy()
	l.Spec.Target.WorkJob = name
	if err := s.Client.Patch(ctx, l, client.MergeFrom(before)); err != nil {
		return err
	}
	return s.patchLinkStatus(ctx, l, func(st *c4v1.TrackerLinkStatus) {
		st.Versions = append(st.Versions, prev)
		st.Review, st.ReviewHead, st.State = nil, "", StateQueued
	})
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	r := []rune(s[:n])
	return string(r[:len(r)-1])
}

// NoteMount is where a next version reads the note it answers.
const NoteMount = "/c4/notes"

// lastDecisionNote is the text of the decision just relayed.
func lastDecisionNote(l *c4v1.TrackerLink) string {
	if d := lastDecision(l); d != nil {
		return d.Note
	}
	return ""
}

// lastDecision is the last decision relayed (not refused) for the link.
func lastDecision(l *c4v1.TrackerLink) *c4v1.TrackerDecision {
	for i := len(l.Status.Decisions) - 1; i >= 0; i-- {
		if d := &l.Status.Decisions[i]; d.Error == "" {
			return d
		}
	}
	return nil
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

// clearTrackerCond sets the condition true only when it is false for
// reason was: what was wrong is now right.
func (s *Syncer) clearTrackerCond(ctx context.Context, t *c4v1.Tracker, typ, was, reason, msg string) {
	if c := meta.FindStatusCondition(t.Status.Conditions, typ); c != nil && c.Status == metav1.ConditionFalse && c.Reason == was {
		s.setTrackerCond(ctx, t, typ, metav1.ConditionTrue, reason, msg)
	}
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
