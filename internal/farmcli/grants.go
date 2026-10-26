package farmcli

import (
	"context"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/Avalanche-io/c4-operator/hud/api"
)

// Grant lets a tracker submit work for you (internal/hud/API.md, Grants): jobs
// it makes from your issues or tasks are yours, so what is yours (a lease
// of your own sign-in) serves them. Granting again renews it.
func Grant(ctx context.Context, c *Client, o Out, project string, req api.GrantRequest) error {
	if req.Project == "" && req.Namespace == "" {
		req.Project = project
	}
	// no project named: the tracker's own, when only one project has it
	if req.Project == "" && req.Namespace == "" {
		p, err := trackerProject(ctx, c, req.Tracker)
		if err != nil {
			return err
		}
		req.Project = p
	}
	var r api.GrantResponse
	if err := c.Do(ctx, "POST", "/grants", req, &r); err != nil {
		return err
	}
	// the farm writes the end in UTC; say it as every other time is said,
	// local with UTC beside it
	msg := r.Message
	if u := r.Grant.Until; !u.IsZero() {
		msg = strings.ReplaceAll(msg, u.UTC().Format(time.RFC3339), o.Clock.AtUTC(u))
	}
	o.printf("%s\n", msg)
	o.printf("  revoke with: c4 farm revoke %s/%s\n", r.Grant.Namespace, r.Grant.Name)
	return nil
}

// Grants lists your grants, or with tracker (namespace/name) who has
// granted that tracker.
func Grants(ctx context.Context, c *Client, o Out, tracker string) error {
	p := "/grants"
	if tracker != "" {
		p += "?" + url.Values{"tracker": {tracker}}.Encode()
	}
	var l api.GrantList
	if err := c.Do(ctx, "GET", p, nil, &l); err != nil {
		return err
	}
	if len(l.Grants) == 0 {
		o.printf("no grants\n")
		return nil
	}
	for _, g := range l.Grants {
		state := "until " + o.Clock.At(g.Until)
		if !g.Active {
			state = g.Why
		}
		var scope []string
		if len(g.Pools) > 0 {
			scope = append(scope, "pools "+strings.Join(g.Pools, ","))
		}
		if len(g.Classes) > 0 {
			scope = append(scope, "classes "+strings.Join(g.Classes, ","))
		}
		o.printf("%-32s %-10s → tracker %-12s %s", g.Namespace+"/"+g.Name, g.Person, g.Tracker, state)
		if len(scope) > 0 {
			o.printf(" (%s)", strings.Join(scope, "; "))
		}
		o.printf("\n")
	}
	return nil
}

// Revoke takes a grant back: yours, or any in a project you administer.
func Revoke(ctx context.Context, c *Client, o Out, ref string) error {
	ns, name, ok := strings.Cut(ref, "/")
	if !ok || ns == "" || name == "" {
		return fmt.Errorf("name the grant as namespace/name (c4 farm grants lists them)")
	}
	var r api.GrantResponse
	if err := c.Do(ctx, "DELETE", "/grants/"+url.PathEscape(ns)+"/"+url.PathEscape(name), nil, &r); err != nil {
		return err
	}
	o.printf("%s\n", r.Message)
	return nil
}

// trackerProject is the project a tracker named name belongs to, when only
// one has a tracker of that name; "" when none does, for the farm to say
// what is wrong.
func trackerProject(ctx context.Context, c *Client, name string) (string, error) {
	var l api.TrackerList
	if err := c.Do(ctx, "GET", "/trackers", nil, &l); err != nil {
		return "", nil
	}
	var in []string
	for _, t := range l.Trackers {
		if t.Name == name || t.Namespace+"/"+t.Name == name {
			in = append(in, t.Project)
		}
	}
	slices.Sort(in)
	in = slices.Compact(in)
	switch len(in) {
	case 0:
		return "", nil
	case 1:
		return in[0], nil
	}
	return "", fmt.Errorf("tracker %s is in more than one project (%s): name one with --project", name, strings.Join(in, ", "))
}
