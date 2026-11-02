package provenance

import (
	"slices"
	"strings"
)

// Deployments (DESIGN 3.3): what a deployment binding names after its
// farm, node/laptop-1, is its signer's identity under IdentityBase, byte
// for byte. A binding for one deployment may only narrow what the signer
// policy (CheckSigner) already lets that deployment sign.

// RecordKind is a record's kind as a binding's for names it: its
// predicate type under https://cccc.io/provenance/ (task/v1, control/v1).
func RecordKind(predicateType string) string {
	return strings.TrimPrefix(predicateType, "https://cccc.io/provenance/")
}

// IdentityDeployment is the deployment an identity is, as a binding names
// it after its farm (node/laptop-1): the identity under IdentityBase,
// unchanged, or "" for one that isn't under it.
func IdentityDeployment(identity string) string {
	dep, ok := strings.CutPrefix(identity, IdentityBase)
	if !ok {
		return ""
	}
	return dep
}

// HeldBy is the deployment a record's signer is bound through when its
// leaf names no binding of its own: a host unit's key is held by its host
// runner (RunnerURN), so the unit's records take runner/<node>'s binding
// in force (DESIGN 3.3). ok is false for any other signer, bound through
// its own identity. Every verifier path asks this one rule, so the farm
// book's and the chain of custody's can't differ.
func HeldBy(v *Verified) (identity string, ok bool) {
	if v.Binding != "" || v.Runner == "" {
		return "", false
	}
	return RunnerIdentity(v.Runner), true
}

// deploymentKinds is the signer policy's record kinds for each kind of
// deployment a binding may name alone: what CheckSigner lets it sign.
var deploymentKinds = map[string][]string{
	// a node daemon signs the task records of units it ran
	"node": {RecordKind(PredicateTask)},
	// a host runner signs its units' task records, and its own requests
	"runner": {RecordKind(PredicateTask), RecordKind(PredicateRequest)},
	// a tracker's sync relays the decisions made in the tracker, and signs
	// its requests (K6 certifies it as its Tracker's)
	"sync": {RecordKind(PredicateAct), RecordKind(PredicateRequest)},
	// an importer signs a project's imports and ingests
	"import":               {RecordKind(PredicateImport), RecordKind(PredicateIngest)},
	"component/controller": {RecordKind(PredicateResolution)},
	"component/queue":      {RecordKind(PredicateDecision), RecordKind(PredicateJob), RecordKind(PredicateBook)},
	"component/hud":        {RecordKind(PredicateControl)},
}

// DeploymentKinds is what the signer policy lets a deployment sign, by
// record kind, for a deployment a binding may name alone (node/<name>,
// runner/<name>, sync/<tracker>, import/<project>,
// component/{controller,queue,hud}); nil for anything else.
func DeploymentKinds(deployment string) []string {
	if k, ok := deploymentKinds[deployment]; ok && strings.HasPrefix(deployment, "component/") {
		return slices.Clone(k)
	}
	kind, name, ok := strings.Cut(deployment, "/")
	if !ok || name == "" || kind == "component" {
		return nil
	}
	return slices.Clone(deploymentKinds[kind])
}

// NarrowsOnly reports the kinds of for that the signer policy doesn't let
// deployment sign: none for a binding that only narrows it. A node never
// signs control records, whatever a binding says.
func NarrowsOnly(deployment string, kinds []string) []string {
	allowed := DeploymentKinds(deployment)
	var wider []string
	for _, k := range kinds {
		if !slices.Contains(allowed, k) {
			wider = append(wider, k)
		}
	}
	return wider
}
