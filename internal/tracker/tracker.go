// Package tracker is what c4-sync's loop knows about any tracker
// (docs/c4-sync.md): one interface of eleven methods, and the few normalized types it speaks. The loop never switches
// on an adapter's kind; everything tracker-specific is in the adapter and
// in the Tracker object's configuration.
package tracker

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/Avalanche-io/c4-operator/internal/effect"
)

// Ref names one entity in one tracker.
type Ref struct {
	Type string // Issue, PullRequest, CheckRun, Comment, Review...
	ID   string
	URL  string
}

func (r Ref) String() string { return r.Type + "/" + r.ID }

// Item is one entity, normalized just enough for the loop.
type Item struct {
	Ref       Ref
	Parent    *Ref // the repository, the shot, the experiment
	Title     string
	Body      string
	Due       *Due
	Status    Status
	Assignees []Person
	Labels    []string
	Fields    map[string]string // adapter fields by native name (a PR's head SHA)
	UpdatedAt time.Time
	Version   string // etag, revision or updatedAt: for conditional writes
	// Name is the short handle people use for the entity (sh040-lighting,
	// 7, eng-12), which names the job made from it; "" names it by its ID.
	// NameUnique says no other entity in the Tracker's scope has it (an
	// issue number), so the job's name needs nothing added to tell it
	// apart.
	Name       string
	NameUnique bool
	// Job is a C4 job the entity already names as its own
	// (<namespace>/<name>): one a client submitted itself, such as
	// mlflow run --backend c4. Intake adopts it instead of submitting one.
	Job string
}

// Due is a due date as the tracker holds it.
type Due struct {
	At       time.Time
	DateOnly bool // the tracker stores a date and no time of day
}

// Status is a native status with the category the loop reasons about.
type Status struct {
	Native   string // "open", "In Progress", "CHANGES_REQUESTED"
	Category string // planned, ready, active, review, rework, done, omitted
}

// Person is a tracker user.
type Person struct {
	ID    string // the tracker's stable id or login
	Name  string
	Email string
}

// Decision is a reviewer's judgment read from the tracker.
type Decision struct {
	On       Ref // what was judged: the PR, the version, the run
	Source   Ref // the review, status change, action or score carrying it
	By       Person
	Native   string // the tracker's own word: APPROVED, CHANGES_REQUESTED, rework
	Body     string // the review text and its inline comments
	At       time.Time
	Evidence []byte // the raw entity or delivery, kept for the record
}

// Output is what C4 publishes for a finished job.
type Output struct {
	Name    string
	Version int // 1 for the link's first job, 2 for the job that followed it...
	JobURL  string
	View    string // the output view's C4 ID
	Record  string // the job record's C4 ID
	Custody string // the provenance view's URL
	Follows string // the job this version follows; "" for the first
	// Preview is a still of the output for trackers that review one (a
	// Kitsu preview); nil when the farm has none.
	Preview *Still
}

// Still is one image of an output: a frame's preview proxy.
type Still struct {
	Name string // a file name with its extension (sh040.1120.png)
	Data []byte
}

// Describe says what the output is, and nothing about the job's state:
// what is published stays true, while the state is kept current where it
// lives (a check run, a status). "C4 output, version 2, follows work/gh-7".
func (o Output) Describe() string {
	d := fmt.Sprintf("C4 output, version %d", o.Version)
	if o.Follows != "" {
		d += ", follows " + o.Follows
	}
	return d
}

// Note is a comment to add, or to edit in place.
type Note struct {
	Body    string
	Replace *Ref // edit this comment rather than add one
}

// Write is one change the loop wants made, with its idempotency key.
type Write struct {
	On     Ref               // the entity written to, or that a new one hangs off
	Kind   string            // the adapter's write kind: status, check, comment
	Fields map[string]string // native field names
	Note   *Note
	Key    string // stable across retries: the adapter forwards or probes it
}

// Query selects entities for List.
type Query struct {
	Type   string
	Labels []string
	State  string // open, closed, all
	Text   string // a marker the entity's body must contain
	// Fields are equality filters in the tracker's own field names.
	Fields map[string]string
}

// Delivery is one pushed event, verified and parsed.
type Delivery struct {
	ID        string
	Refs      []Ref      // entities to reconcile
	Decisions []Decision // decisions carried only by the delivery itself
	Self      bool       // made by the adapter's own identity: an echo
}

// Capabilities says what the tracker offers, so the loop adapts without
// knowing which tracker it is.
type Capabilities struct {
	EventLog          bool
	Webhooks          bool
	IdempotencyKeys   bool
	EditableComments  bool
	CustomFields      bool
	DateOnlyDue       bool
	ConditionalWrites bool
	// ReviewHeads: what is under review has head commits, read again when
	// it changes (a pull request), and status goes on the head.
	ReviewHeads bool
	// PublishForReview: Publish makes the entity people review (a Flow
	// Version), which becomes the link's review.
	PublishForReview bool
	// PublishPreview: Publish uploads the output's still (Output.Preview),
	// which the loop fetches from the farm only for such adapters.
	PublishPreview bool
	// Mentions: the tracker can tell a person by name, where they work
	// (write kind mention: an @-mention, a Note addressed to them).
	Mentions bool
}

// Adapter is one tracker family.
type Adapter interface {
	Kind() string
	Capabilities() Capabilities

	// Reading.
	Get(ctx context.Context, ref Ref) (Item, error)
	List(ctx context.Context, q Query, page string) (items []Item, next string, err error)
	Changes(ctx context.Context, cursor string) (refs []Ref, next string, err error)
	Decisions(ctx context.Context, on Ref, since time.Time) ([]Decision, error)

	// Receive verifies and parses one pushed delivery. Adapters whose
	// tracker does not push return ErrUnsupported.
	Receive(r *http.Request) (Delivery, error)

	// Writing. Every call carries its key; Probe answers "did this
	// already happen?" for trackers that take no idempotency key. Guard
	// says how a write is kept from happening twice (internal/effect):
	// the loop journals each write and, after an interrupted one, sends
	// it again only as its guard allows. Publish keeps itself once.
	Apply(ctx context.Context, w Write) (Ref, error)
	Publish(ctx context.Context, on Ref, out Output, key string) (Ref, error)
	Probe(ctx context.Context, w Write) (done bool, ref Ref, err error)
	Guard(w Write) effect.Guard

	// People.
	Person(ctx context.Context, id string) (Person, error)
}

// ErrUnsupported is what an adapter returns for a method its tracker has
// no counterpart for.
var ErrUnsupported = errors.New("tracker: not supported by this tracker")

// ErrNotFound is an entity the tracker does not have, or will not show.
var ErrNotFound = errors.New("tracker: not found")

// ErrRefused is a request the tracker answered and declined (a 4xx other
// than a rate limit): nothing was done.
var ErrRefused = errors.New("tracker: refused")

// Definite says err means a write did not happen: the tracker answered
// and refused it, has no such entity, cannot take it, or asked to wait.
// Any other error (a timeout, a 5xx) may have followed the write.
func Definite(err error) bool {
	var rl *RateLimited
	return errors.Is(err, ErrRefused) || errors.Is(err, ErrNotFound) || errors.Is(err, ErrUnsupported) || errors.As(err, &rl)
}

// ErrBadSignature is a delivery whose signature does not verify.
var ErrBadSignature = errors.New("tracker: delivery signature does not verify")

// RateLimited is a refusal for rate; Until is when to try again.
type RateLimited struct {
	Until time.Time
	Msg   string
}

func (e *RateLimited) Error() string {
	return fmt.Sprintf("tracker: rate limited until %s: %s", e.Until.UTC().Format(time.RFC3339), e.Msg)
}

// Config is what an adapter is built from: the Tracker object's spec,
// resolved.
type Config struct {
	Endpoint string
	Scope    map[string]string
	// Token returns the current credential; it is called for every
	// request, so a rotated lease takes effect at once.
	Token func(ctx context.Context) (string, error)
	// WebhookSecret returns the delivery-signing secret, if any.
	WebhookSecret func(ctx context.Context) ([]byte, error)
	// Self is the adapter's own identity in the tracker, for echoes.
	Self       string
	HTTPClient *http.Client
	Now        func() time.Time
}

// Factory builds an adapter.
type Factory func(Config) (Adapter, error)

var (
	regMu    sync.Mutex
	registry = map[string]Factory{}
)

// Register makes an adapter available by kind.
func Register(kind string, f Factory) {
	regMu.Lock()
	defer regMu.Unlock()
	registry[kind] = f
}

// New builds the adapter for kind.
func New(kind string, c Config) (Adapter, error) {
	regMu.Lock()
	f, ok := registry[kind]
	regMu.Unlock()
	if !ok {
		return nil, fmt.Errorf("tracker: no adapter for kind %q (have %v)", kind, Kinds())
	}
	return f(c)
}

// Kinds lists the registered adapters.
func Kinds() []string {
	regMu.Lock()
	defer regMu.Unlock()
	out := make([]string, 0, len(registry))
	for k := range registry {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
