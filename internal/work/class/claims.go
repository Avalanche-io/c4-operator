package class

import (
	"fmt"
	"path"
	"strings"
)

// Claims (PRIMITIVES Q8): what a unit will write, a path or a named
// resource, with a mode. The queue never runs two units of a job whose
// claims conflict: Ra's P-21 rule, that lanes sharing a file run in
// sequence, as a scheduling fact rather than a lead's chore.
//
// A claim on a path covers everything under it: "src/net/" and
// "src/net/conn.go" overlap, "src/net" and "src/netx" do not. A claim
// whose name has a scheme ("db:test", "device:phone-3") is a named
// resource and overlaps only the same name. Two shared claims never
// conflict; an exclusive claim conflicts with any claim it overlaps.
//
// A unit takes its claims when it starts and holds them until it ends,
// and waits for nothing while it holds them (its edges were met before it
// started), so claims cannot deadlock and need no cycle check.

// Claim modes.
const (
	ClaimExclusive = "exclusive"
	ClaimShared    = "shared"
)

// Claim is one thing a unit claims.
type Claim struct {
	On   string
	Mode string
}

// ParseClaim checks a claim from a document, defaulting its mode.
func ParseClaim(on, mode string) (Claim, error) {
	on = strings.TrimSpace(on)
	if on == "" {
		return Claim{}, fmt.Errorf("a claim on nothing")
	}
	switch mode {
	case "":
		mode = ClaimExclusive
	case ClaimExclusive, ClaimShared:
	default:
		return Claim{}, fmt.Errorf("claim on %s: mode %q, want exclusive or shared", on, mode)
	}
	return Claim{On: normalize(on), Mode: mode}, nil
}

func normalize(on string) string {
	if isResource(on) {
		return on
	}
	dir := strings.HasSuffix(on, "/")
	p := path.Clean(strings.TrimPrefix(on, "./"))
	if p == "." {
		return "/"
	}
	p = strings.TrimPrefix(p, "/")
	if dir {
		p += "/"
	}
	return p
}

func isResource(on string) bool {
	i := strings.Index(on, ":")
	return i > 0 && !strings.Contains(on[:i], "/")
}

// Overlaps reports whether two claims name overlapping things.
func (c Claim) Overlaps(o Claim) bool {
	if isResource(c.On) || isResource(o.On) {
		return c.On == o.On
	}
	a, b := strings.TrimSuffix(c.On, "/"), strings.TrimSuffix(o.On, "/")
	if a == "" || b == "" || c.On == "/" || o.On == "/" {
		return true // the whole tree
	}
	return a == b || strings.HasPrefix(b, a+"/") || strings.HasPrefix(a, b+"/")
}

// Conflicts reports whether two claims cannot be held at once.
func (c Claim) Conflicts(o Claim) bool {
	if c.Mode == ClaimShared && o.Mode == ClaimShared {
		return false
	}
	return c.Overlaps(o)
}

// ConflictsWith returns the first of a's claims that conflicts with one
// of b's, and that one.
func ConflictsWith(a, b []Claim) (Claim, Claim, bool) {
	for _, x := range a {
		for _, y := range b {
			if x.Conflicts(y) {
				return x, y, true
			}
		}
	}
	return Claim{}, Claim{}, false
}
