package class

import (
	"fmt"
	"slices"
	"strconv"

	corev1 "k8s.io/api/core/v1"
)

// Rounds: a bounded loop over a subgraph (DESIGN.md 10.6, Rounds). A
// generator that declares rounds: {max, until?} runs its loop as rounds
// of siblings, not as nesting, so maxExpandDepth limits nesting only and a
// fifty-round loop is one level deep:
//
//   - Round k's units are named r<k>/<name>: a document refers to its own
//     units by the names it gave them, and to earlier rounds' by their full
//     names.
//   - A round's document that says next: true adds the next round's
//     generator after the round's units, a sibling of the first, named
//     r<k+1>/<base>. With until set, next is the default: the review unit
//     until names (r<k>/<until>) decides instead, and its verdict's summary
//     word pass ends the loop before the next round's generator runs.
//   - At max, no next round is added.
//
// Each round's generator carries C4_ROUND (its round) and C4_ROUND_MAX.

// MaxRounds bounds a loop.
const MaxRounds = 1000

// Rounds is a round generator's loop.
type Rounds struct {
	// Max bounds the loop; Until names the review unit of each round whose
	// pass ends it.
	Max   int
	Until string
	// Round is this generator's round, from 1; First is the index of the
	// loop's first generator, and Base its name.
	Round int
	First int
	Base  string
}

// DocRounds is a document unit's rounds.
type DocRounds struct {
	Max   int    `json:"max"`
	Until string `json:"until,omitempty"`
}

// RoundName is the name of unit name in round k.
func RoundName(k int, name string) string { return fmt.Sprintf("r%d/%s", k, name) }

// UntilUnit is the name of the review that decides whether round k's loop
// goes on.
func (r *Rounds) UntilUnit(k int) string {
	if r.Until == "" {
		return ""
	}
	return RoundName(k, r.Until)
}

// checkRounds checks a generator's declared rounds.
func checkRounds(name string, r *DocRounds, planned bool) error {
	switch {
	case !planned:
		return fmt.Errorf("unit %s has rounds but is not planned (a generator)", name)
	case r.Max < 1 || r.Max > MaxRounds:
		return fmt.Errorf("unit %s: rounds.max %d, want 1 to %d", name, r.Max, MaxRounds)
	}
	return nil
}

// roundEnv sets a round generator's C4_ROUND and C4_ROUND_MAX.
func roundEnv(e []corev1.EnvVar, r *Rounds) []corev1.EnvVar {
	e = slices.DeleteFunc(slices.Clone(e), func(v corev1.EnvVar) bool { return v.Name == "C4_ROUND" || v.Name == "C4_ROUND_MAX" })
	return append(e, env("C4_ROUND", strconv.Itoa(r.Round)), env("C4_ROUND_MAX", strconv.Itoa(r.Max)))
}

// nextRound is the generator of the round after gen's, at index idx,
// running after the round's units and reading what gen read.
func nextRound(gen Unit, idx int, after []int) Unit {
	r := *gen.Rounds
	r.Round++
	n := gen
	n.Index, n.Name, n.Label = idx, RoundName(r.Round, r.Base), RoundName(r.Round, r.Base)
	n.After, n.Inputs = slices.Clone(after), slices.Clone(gen.Inputs)
	for _, in := range n.Inputs {
		if !slices.Contains(n.After, in.From) {
			n.After = append(n.After, in.From)
		}
	}
	slices.Sort(n.After)
	n.Rounds = &r
	n.Env = slices.DeleteFunc(slices.Clone(gen.Env), func(v corev1.EnvVar) bool { return v.Name == "C4_UNIT_NAME" })
	n.Env = roundEnv(append(n.Env, env("C4_UNIT_NAME", n.Name)), &r)
	n.Claims, n.Holds = slices.Clone(gen.Claims), slices.Clone(gen.Holds)
	return n
}
