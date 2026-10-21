package schema

import (
	_ "embed"
	"fmt"
	"path"
	"slices"
	"strings"
)

//go:embed chain-1.schema.json
var chainSchema []byte

// ChainSchema is the JSON Schema of an accountability chain (chain/1), as
// GET /schema/chain serves it.
func ChainSchema() []byte { return slices.Clone(chainSchema) }

// A Chain is what every signed record and book entry carries to say whom
// its act accrues to: the act, who did it, the party it was for, the
// delegations it ran under, the credentials it spent and the deployments
// that did and recorded it. Accountable is computed when the record is
// made; when it is false, Gap names the first link that failed.
type Chain struct {
	Act         string       `json:"act"`
	By          Actor        `json:"by"`
	For         For          `json:"for"`
	Under       []Delegation `json:"under,omitempty"`
	Using       []Credential `json:"using,omitempty"`
	On          []Deployment `json:"on,omitempty"`
	Accountable bool         `json:"accountable"`
	Gap         string       `json:"gap,omitempty"`
}

// Grades say how strongly a link is evidenced.
const (
	GradeClaimed = "claimed"
	GradeDirect  = "direct"
	GradeRelayed = "relayed"
	GradeSigned  = "signed"
)

// Actor is the chain's by: a person, a unit (with its agent), a deployment
// acting by rule, or a tracker user whose act was relayed.
type Actor struct {
	Person     string `json:"person,omitempty"`
	Unit       string `json:"unit,omitempty"`
	Agent      string `json:"agent,omitempty"`
	Deployment string `json:"deployment,omitempty"`
	Rule       string `json:"rule,omitempty"`
	Tracker    string `json:"tracker,omitempty"`
	User       string `json:"user,omitempty"`
	Grade      string `json:"grade"`
}

// For is the party an act was for, or the subject it was for when that is
// not a party.
type For struct {
	Party   string `json:"party,omitempty"`
	Subject string `json:"subject,omitempty"`
	Grade   string `json:"grade"`
}

// Delegation is one link of under: a grant, a membership, job ownership, a
// policy value and where it came from, or a mapping record.
type Delegation struct {
	Grant      string `json:"grant,omitempty"`
	Membership string `json:"membership,omitempty"`
	Job        string `json:"job,omitempty"`
	Policy     string `json:"policy,omitempty"`
	Record     string `json:"record,omitempty"`
	Kind       string `json:"kind,omitempty"`
	Value      any    `json:"value,omitempty"`
	From       string `json:"from,omitempty"`
	To         string `json:"to,omitempty"`
}

// Credential is one credential spent, and who holds it: a party, or a
// subject that is not a party yet (which leaves the chain unaccountable).
// SignIn is a person's own sign-in spent on a host runner, named by the
// runner's deployment; no lease is made for it.
type Credential struct {
	Lease         string `json:"lease,omitempty"`
	Licence       string `json:"licence,omitempty"`
	Key           string `json:"key,omitempty"`
	SignIn        string `json:"signIn,omitempty"`
	Holder        string `json:"holder,omitempty"`
	HolderSubject string `json:"holderSubject,omitempty"`
}

// Deployment is one deployment that did and recorded the act, with its
// binding and owner.
type Deployment struct {
	Deployment string `json:"deployment"`
	Binding    string `json:"binding,omitempty"`
	Owner      string `json:"owner,omitempty"`
	RunFor     string `json:"runFor,omitempty"`
	HostedBy   string `json:"hostedBy,omitempty"`
}

// ParseChain reads a chain (JSON or YAML) and checks its shape.
func ParseChain(b []byte) (*Chain, []Error) {
	var c Chain
	if errs := parse(b, "chain", &c); errs != nil {
		return nil, errs
	}
	return &c, nil
}

// Names gives the words a chain is read aloud with. A nil Names, or an
// empty answer, falls back to the chain's own names.
type Names interface {
	// Party is a party's display name ("avalanche" is "Avalanche").
	Party(name string) string
	// Record is a record's short name ("g-12" for a grant's ID).
	Record(id string) string
	// Deployment is a deployment's display name ("the queue").
	Deployment(name string) string
	// Subject is a non-party subject in words ("c4-sync itself",
	// "dana@studio.com (no party yet)"). People never read a raw
	// service-account name.
	Subject(subject string) string
}

// Line is the chain read aloud, as the HUD and c4 farm show it:
//
//	build github-7/3 by agent block, for joshua, under grant g-12, using
//	joshua's sign-in, on laptop-1 (joshua's) and the queue (Avalanche,
//	managed for Studio X)
func (c *Chain) Line(n Names) string {
	party := func(s string) string { return pick(s, n, func(n Names) string { return n.Party(s) }) }
	record := func(id string) string { return pick(short(id), n, func(n Names) string { return n.Record(id) }) }
	dep := func(s string) string { return pick(path.Base(s), n, func(n Names) string { return n.Deployment(s) }) }
	subject := func(s string) string {
		return pick(lastSegment(s)+" (not a party)", n, func(n Names) string { return n.Subject(s) })
	}
	grade := func(g string) string {
		if g == "" || g == GradeDirect {
			return ""
		}
		return " (" + g + ")"
	}

	var b strings.Builder
	b.WriteString(c.Act)
	switch by := c.By; {
	case by.Unit != "":
		b.WriteString(" " + unitName(by.Unit))
		if by.Agent != "" {
			b.WriteString(" by agent " + by.Agent)
		}
		b.WriteString(grade(by.Grade))
	case by.Agent != "":
		b.WriteString(" by agent " + by.Agent + grade(by.Grade))
	case by.Person != "":
		b.WriteString(" by " + party(by.Person) + grade(by.Grade))
	case by.User != "":
		b.WriteString(" by " + by.User + " in " + by.Tracker + grade(by.Grade))
	case by.Deployment != "":
		b.WriteString(" by " + dep(by.Deployment))
		if by.Rule != "" {
			b.WriteString(", by rule " + by.Rule)
		}
		b.WriteString(grade(by.Grade))
	}
	if c.For.Party != "" {
		b.WriteString(", for " + party(c.For.Party) + grade(c.For.Grade))
	} else {
		b.WriteString(", for " + subject(c.For.Subject))
	}
	if len(c.Under) > 0 {
		var us []string
		for _, u := range c.Under {
			switch {
			case u.Grant != "":
				us = append(us, "grant "+record(u.Grant))
			case u.Membership != "":
				us = append(us, "membership "+record(u.Membership))
			case u.Job != "":
				us = append(us, "job "+u.Job)
			case u.Policy != "":
				s := fmt.Sprintf("%s %v", u.Policy, u.Value)
				if u.From != "" {
					s += " from " + u.From
				}
				us = append(us, s)
			case u.Record != "":
				us = append(us, strings.TrimSpace(u.Kind+" "+record(u.Record)))
			}
		}
		b.WriteString(", under " + and(us))
	}
	if len(c.Using) > 0 {
		var us []string
		for _, u := range c.Using {
			what := "sign-in"
			switch {
			case u.Licence != "":
				what = "licence"
			case u.Key != "":
				what = "key"
			}
			on := ""
			if u.SignIn != "" {
				on = " on " + dep(u.SignIn)
			}
			if u.Holder != "" {
				us = append(us, party(u.Holder)+"'s "+what+on)
			} else {
				us = append(us, "a "+what+" held by "+subject(u.HolderSubject)+on)
			}
		}
		b.WriteString(", using " + and(us))
	}
	if len(c.On) > 0 {
		var os []string
		for _, d := range c.On {
			s := dep(d.Deployment)
			switch {
			case d.Owner != "" && d.RunFor != "":
				s += " (" + party(d.Owner) + ", managed for " + party(d.RunFor) + ")"
			case d.Owner != "":
				s += " (" + party(d.Owner) + "'s)"
			default:
				s += " (no owner bound)"
			}
			os = append(os, s)
		}
		b.WriteString(", on " + and(os))
	}
	if !c.Accountable {
		b.WriteString("; unattributed: " + c.Gap)
	}
	return b.String()
}

func pick(fallback string, n Names, f func(Names) string) string {
	if n != nil {
		if s := f(n); s != "" {
			return s
		}
	}
	return fallback
}

// UnitRef is a unit as a chain names it, its job by name and the first six
// characters of its UID, so a job made again under the same name is
// another: show-a/github-7@3f9a1c/3.
func UnitRef(namespace, job, jobUID, unit string) string {
	if len(jobUID) > 6 {
		jobUID = jobUID[:6]
	}
	return namespace + "/" + job + "@" + jobUID + "/" + unit
}

// unitName is a unit read aloud: show-a/github-7@3f9a1c/3 is github-7/3.
func unitName(u string) string {
	if _, rest, ok := strings.Cut(u, "/"); ok {
		u = rest
	}
	job, idx, _ := strings.Cut(u, "/")
	job, _, _ = strings.Cut(job, "@")
	return job + "/" + idx
}

// lastSegment is a subject's last part: system:serviceaccount:c4-system:c4-sync
// is c4-sync.
func lastSegment(s string) string {
	if i := strings.LastIndexAny(s, ":/"); i >= 0 && i < len(s)-1 {
		return s[i+1:]
	}
	return s
}

// short is a record ID's last six characters, when no name is given.
func short(id string) string {
	if len(id) > 6 {
		return "…" + id[len(id)-6:]
	}
	return id
}

func and(items []string) string {
	switch len(items) {
	case 0:
		return ""
	case 1:
		return items[0]
	}
	return strings.Join(items[:len(items)-1], ", ") + " and " + items[len(items)-1]
}
