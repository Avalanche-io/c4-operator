// Package process keeps the farm's record of computations: what a unit of
// work was (its container images, command and parameters, and the C4 IDs
// of everything it read), what it cost, and what it wrote.
//
// With provenance, inputs plus a process give repeatable outputs when the
// process is deterministic. So the farm has three ways to get an output to
// a site: move it; recompute it from inputs the site holds (or that are
// cheaper to move than the output); or reuse it, because that exact
// output already exists. This package answers the questions the planner
// needs for the last two:
//
//   - Recompute: what recomputing an output costs, from past runs of the
//     same computation, or of the same process on other inputs.
//   - Determinism: whether a process may be trusted to repeat. It is
//     earned only from re-runs of the same computation whose output IDs
//     matched, never assumed; one mismatch marks the process
//     nondeterministic for good.
//   - Reuse: an existing output of the same computation, offered only for
//     a deterministic process.
//
// It also gives the estimator an exact-rerun prior: a unit that has run
// before with the same process and inputs is predicted from what it took.
//
// One primitive carries all of it: a derivation, process ID + input IDs
// -> output ID, with what it cost. A process's class (Exact, Validated,
// Volatile, in the vocabulary of C4's derivation-record design) is not
// stored; it is computed from evidence: what the job declared, whether
// its pods were sealed, and how many re-runs of the same derivation wrote
// the same output or another. Platform, for GPU work, is part of the
// process ID.
//
// Experimental: the record format and the class rules will change with
// C4's derivation-record design, which this is helping to shape
// (proposals/eta-deadlines/DERIVATION-LESSONS.md).
//
// The package is pure. The queue records every succeeded unit
// (Book.Record); the scheduler asks the Book.
package process

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"
)

// Container is one container of a unit, as the process key sees it.
type Container struct {
	// Digest is the image digest the container ran (sha256:...). A
	// container without one is not identifiable: an image tag can change
	// under the same name.
	Digest  string
	Command []string
	Args    []string
	// Env is the container's environment. Variables that name the run
	// rather than the work (RunEnv) are dropped from the key.
	Env        map[string]string
	WorkingDir string
}

// RunEnv are environment variables that differ between runs of the same
// computation: which job, which attempt, which pod. They are left out of a
// process key.
var RunEnv = map[string]bool{
	"C4_JOB": true, "C4_UNIT": true, "C4_ATTEMPT": true, "C4_PODINFO": true,
	"C4_RESUME_VIEW": true, "C4_OUTPUT": true, "HOSTNAME": true,
}

// ProcessID is the identity of a process: the containers' image digests,
// commands, arguments, environment (less RunEnv) and working directories,
// and the platform. Platform is empty for CPU work; for GPU pools it names
// the GPU model, driver version and compute stack, because GPU results are
// reproducible on the same platform at best (Ra's finding: GPU kernels are
// validated, not exact, across platforms), so only runs on one platform
// are comparable. It reports false if a container has no image digest.
func ProcessID(cs []Container, platform string) (string, bool) {
	return programID(cs, platform, nil)
}

// ProgramID is the identity of the program a process runs: ProcessID
// without the environment variables in perUnit, the parameters that tell
// one unit of a job from another (which frames, which shard). Identity is
// per process, for reuse; trust is per program, because a renderer that
// repeats on frame 1001 is the same renderer on frame 1002. A process's
// class and its evidence accrue to its program.
func ProgramID(cs []Container, platform string, perUnit map[string]bool) (string, bool) {
	return programID(cs, platform, perUnit)
}

func programID(cs []Container, platform string, drop map[string]bool) (string, bool) {
	type canon struct {
		Digest  string      `json:"d"`
		Command []string    `json:"c,omitempty"`
		Args    []string    `json:"a,omitempty"`
		Env     [][2]string `json:"e,omitempty"`
		Dir     string      `json:"w,omitempty"`
	}
	out := make([]canon, 0, len(cs))
	for _, c := range cs {
		if !strings.HasPrefix(c.Digest, "sha256:") {
			return "", false
		}
		x := canon{Digest: c.Digest, Command: c.Command, Args: c.Args, Dir: c.WorkingDir}
		for k, v := range c.Env {
			if !RunEnv[k] && !drop[k] {
				x.Env = append(x.Env, [2]string{k, v})
			}
		}
		sort.Slice(x.Env, func(i, j int) bool { return x.Env[i][0] < x.Env[j][0] })
		out = append(out, x)
	}
	if len(out) == 0 {
		return "", false
	}
	return digest(struct {
		C []canon `json:"c"`
		P string  `json:"p,omitempty"`
	}{out, platform}), true
}

// Input is one input a unit read: where it was mounted and the C4 ID of
// the view mounted there.
type Input struct {
	Mount string `json:"mount"`
	View  string `json:"view"`
}

// InputsID is the identity of a set of inputs, independent of order. It
// reports false if any input lacks a view ID.
func InputsID(in []Input) (string, bool) {
	s := append([]Input(nil), in...)
	for _, x := range s {
		if x.View == "" {
			return "", false
		}
	}
	sort.Slice(s, func(i, j int) bool { return s[i].Mount < s[j].Mount })
	return digest(s), true
}

// Key identifies a computation: a process and the inputs it read.
type Key struct {
	Process string `json:"process"`
	Inputs  string `json:"inputs"`
}

func digest(v any) string {
	b, _ := json.Marshal(v)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
