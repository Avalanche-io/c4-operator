package server

// The trackers in a person's projects (integrations GAPS 17), so they have
// one to choose when they let a tracker submit work for them: what each
// connects to and turns into jobs, in words, whether it is connected, how
// many people's grants it holds, and the viewer's own.

import (
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/hud/api"
)

// trackerStale is how many resync periods may pass without one before a
// Tracker that last read Ready is no longer called connected: its sync has
// stopped.
const trackerStale = 3

func (s *Server) handleTrackers(w http.ResponseWriter, r *http.Request) {
	v := s.withViewer(w, r)
	if v == nil {
		return
	}
	m := s.current()
	if m == nil {
		fail(w, http.StatusServiceUnavailable, "the HUD is still reading the farm")
		return
	}
	ctx := r.Context()
	out := api.TrackerList{Trackers: []api.Tracker{}}
	var list c4v1.TrackerList
	if err := s.Client.List(ctx, &list); err != nil {
		if noGrants(err) {
			writeJSON(w, http.StatusOK, out)
			return
		}
		fail(w, http.StatusInternalServerError, "%v", err)
		return
	}
	var grants c4v1.SubmitGrantList
	if err := s.Client.List(ctx, &grants); err != nil && !noGrants(err) {
		fail(w, http.StatusInternalServerError, "%v", err)
		return
	}
	now := time.Now()
	type key struct{ ns, name string }
	held := map[key]int{}
	mine := map[key]*api.Grant{}
	for i := range grants.Items {
		g := &grants.Items[i]
		k := key{g.Namespace, g.Spec.Tracker}
		if g.Lapsed(now) == "" {
			held[k]++
		}
		if g.Spec.Person == v.User {
			gv := grantView(g, m.nsProject[g.Namespace], now)
			mine[k] = &gv
		}
	}
	for i := range list.Items {
		t := &list.Items[i]
		project, ok := m.nsProject[t.Namespace]
		if !ok || !v.Sees(project) {
			continue
		}
		k := key{t.Namespace, t.Name}
		tv := api.Tracker{Namespace: t.Namespace, Name: t.Name, Project: project, Kind: t.Spec.Kind,
			Site: trackerSite(&t.Spec), Syncs: trackerSyncs(&t.Spec), Granted: held[k], Mine: mine[k]}
		tv.Ready, tv.Why = trackerReady(t, now)
		out.Trackers = append(out.Trackers, tv)
	}
	slices.SortFunc(out.Trackers, func(a, b api.Tracker) int {
		return strings.Compare(a.Project+"\x00"+a.Name, b.Project+"\x00"+b.Name)
	})
	writeJSON(w, http.StatusOK, out)
}

// trackerReady reads c4-sync's Ready condition as words: connected, or
// why not.
func trackerReady(t *c4v1.Tracker, now time.Time) (bool, string) {
	c := meta.FindStatusCondition(t.Status.Conditions, "Ready")
	switch {
	case c == nil:
		return false, "it has not connected yet"
	case c.Status != metav1.ConditionTrue && c.Reason == "NoAdapter":
		return false, fmt.Sprintf("this farm has no connector for %s", trackerKindName(t.Spec.Kind))
	case c.Status != metav1.ConditionTrue:
		if c.Message == "" {
			return false, "it could not reach " + trackerSite(&t.Spec)
		}
		return false, "it could not reach " + trackerSite(&t.Spec) + ": " + c.Message
	}
	every := time.Duration(t.Spec.Events.ResyncSeconds) * time.Second
	if every <= 0 {
		every = 900 * time.Second
	}
	if last := t.Status.LastResync; last != nil && now.Sub(last.Time) > trackerStale*every {
		return false, "it has not synced since " + last.UTC().Format(time.RFC3339)
	}
	return true, ""
}

var trackerKinds = map[string]string{
	"github": "GitHub", "gitlab": "GitLab", "flow": "Flow Production Tracking", "ftrack": "ftrack",
	"kitsu": "Kitsu", "ayon": "AYON", "jira": "Jira", "linear": "Linear", "mlflow": "MLflow",
}

func trackerKindName(k string) string {
	if n, ok := trackerKinds[k]; ok {
		return n
	}
	return k
}

// trackerSite is what a Tracker connects to, as a person names it: for
// GitHub the repository ("github.com/owner/name"); for others the
// product, the site, and the scope's project, team or experiment when it
// is a name rather than a number.
func trackerSite(sp *c4v1.TrackerSpec) string {
	host := ""
	if u, err := url.Parse(sp.Endpoint); err == nil {
		host = u.Hostname()
	}
	if sp.Kind == "github" {
		if host == "" || host == "api.github.com" {
			host = "github.com"
		}
		if repo := sp.Scope["repo"]; repo != "" {
			return host + "/" + repo
		}
		return host
	}
	parts := []string{trackerKindName(sp.Kind)}
	if sp.Kind == "flow" {
		// A hosted Flow site is <studio>.shotgrid.autodesk.com: the
		// studio's name is the site.
		for _, suffix := range []string{".shotgrid.autodesk.com", ".shotgunstudio.com"} {
			if s, ok := strings.CutSuffix(host, suffix); ok {
				host = upperFirst(s)
			}
		}
	}
	if host != "" {
		parts = append(parts, host)
	}
	for _, k := range []string{"project", "team", "experiment"} {
		if v := sp.Scope[k]; v != "" {
			if _, err := strconv.Atoi(v); err != nil {
				parts = append(parts, v)
			}
			break
		}
	}
	return strings.Join(parts, ", ")
}

func upperFirst(s string) string {
	if s == "" {
		return s
	}
	return string(unicode.ToUpper(rune(s[0]))) + s[1:]
}

// trackerSyncs says what each intake rule turns into jobs: "issues
// labeled render", "tasks assigned to a person, where
// step.Step.short_name is LGT", "milestones, as due dates". Field names
// are the tracker's own, as the rule names them.
func trackerSyncs(sp *c4v1.TrackerSpec) []string {
	var out []string
	for _, in := range sp.Intake {
		var b strings.Builder
		switch in.Match.State {
		case "closed":
			b.WriteString("closed ")
		case "all":
			b.WriteString("open and closed ")
		}
		b.WriteString(entityPlural(in.Match.Type))
		if len(in.Match.Labels) > 0 {
			b.WriteString(" labeled " + strings.Join(in.Match.Labels, ", "))
		}
		if f := in.For; f != nil && f.Assignee {
			b.WriteString(" assigned to a person")
		} else if f != nil && f.Person != "" {
			b.WriteString(" for " + f.Person)
		}
		if len(in.Match.Fields) > 0 {
			ks := make([]string, 0, len(in.Match.Fields))
			for k := range in.Match.Fields {
				ks = append(ks, k)
			}
			slices.Sort(ks)
			for i, k := range ks {
				ks[i] = k + " is " + in.Match.Fields[k]
			}
			b.WriteString(", where " + strings.Join(ks, " and "))
		}
		if in.As == "milestone" {
			b.WriteString(", as due dates")
		}
		out = append(out, b.String())
	}
	return out
}

// entityPlural is a tracker entity type as words: PullRequest is "pull
// requests".
func entityPlural(t string) string {
	if t == "" {
		return "items"
	}
	var b strings.Builder
	for i, r := range t {
		if i > 0 && unicode.IsUpper(r) {
			b.WriteByte(' ')
		}
		b.WriteRune(unicode.ToLower(r))
	}
	s := b.String()
	switch {
	case strings.HasSuffix(s, "s"), strings.HasSuffix(s, "x"), strings.HasSuffix(s, "ch"):
		return s + "es"
	case strings.HasSuffix(s, "y") && !strings.HasSuffix(s, "ey"):
		return s[:len(s)-1] + "ies"
	}
	return s + "s"
}
