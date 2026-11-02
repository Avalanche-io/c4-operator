package provenance

import (
	"crypto/sha256"
	"crypto/x509"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/Avalanche-io/c4-operator/schema"
)

// Personal keys (schema/README.md, act/v1 and device/v1): every act is
// signed by its actor's own key, and the HUD's control record cites it.
const (
	// PredicateAct: one act as its actor signed it: a person's device key,
	// an agent's or a unit's own key, or a tracker's relay naming the
	// person. Signed by the identity in its signer field.
	PredicateAct = schema.ActPredicateType
	// PredicateDevice: one of a person's device keys, as the people's
	// issuer enrolled it. Signed by the issuer.
	PredicateDevice = schema.DevicePredicateType
	// PredicateRequest: one HTTP request, signed by the client that sends
	// it (a person's CLI, an agent, a unit or a host runner). It proves who
	// is calling and nothing more: it is never an act, and CheckSigner
	// judges it as its own kind.
	PredicateRequest = schema.RequestPredicateType
)

// AgentIdentity is an agent's identity outside a job, acting under a
// person's grant; its certificate names the grant.
func AgentIdentity(agent string) string { return IdentityBase + "agent/" + url.PathEscape(agent) }

// UnitIdentity is a unit's identity for one attempt: the job by name and
// UID, so a job made again under the name is another.
func UnitIdentity(namespace, job, unit, jobUID string, attempt int) string {
	return fmt.Sprintf("%sunit/%s/%s/%s@%s/%d", IdentityBase, namespace, job, unit, jobUID, attempt)
}

// UnitOf is a unit's identity read back: ok is false for any other, or a
// malformed one (an attempt that is not a positive number).
type UnitOf struct {
	Namespace, Job, Unit, JobUID string
	Attempt                      int
}

// ParseUnitIdentity reads unit/<ns>/<job>/<unit>@<job UID>/<attempt>.
func ParseUnitIdentity(id string) (UnitOf, bool) {
	rest, ok := strings.CutPrefix(id, IdentityBase+"unit/")
	if !ok {
		return UnitOf{}, false
	}
	parts := strings.Split(rest, "/")
	if len(parts) != 4 {
		return UnitOf{}, false
	}
	unit, uid, ok := strings.Cut(parts[2], "@")
	attempt, err := strconv.Atoi(parts[3])
	if !ok || err != nil || attempt < 1 || parts[0] == "" || parts[1] == "" || unit == "" || uid == "" {
		return UnitOf{}, false
	}
	return UnitOf{Namespace: parts[0], Job: parts[1], Unit: unit, JobUID: uid, Attempt: attempt}, true
}

// A leaf certificate names a person's device, or the grant an agent or CI
// job acts under, in a further URI SAN beside its identity.
const (
	DeviceURN = "urn:c4:device:"
	GrantURN  = "urn:c4:grant:"
)

// LeafURIs are the URI SANs of a leaf: its identity, then its device or
// grant, when it has one.
func LeafURIs(identity, device, grant string) ([]*url.URL, error) {
	var out []*url.URL
	for _, s := range []string{identity, prefixed(DeviceURN, device), prefixed(GrantURN, grant)} {
		if s == "" {
			continue
		}
		u, err := url.Parse(s)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, nil
}

func prefixed(prefix, id string) string {
	if id == "" {
		return ""
	}
	return prefix + id
}

// CertDevice is the device ID a leaf names, or "".
func CertDevice(c *x509.Certificate) string { return certURN(c, DeviceURN) }

// CertGrant is the grant ID a leaf names, or "".
func CertGrant(c *x509.Certificate) string { return certURN(c, GrantURN) }

func certURN(c *x509.Certificate, prefix string) string {
	for _, u := range c.URIs {
		if id, ok := strings.CutPrefix(u.String(), prefix); ok {
			return id
		}
	}
	return ""
}

// CertDeviceID is the device ID of a leaf's own key: the C4 ID of its DER
// SubjectPublicKeyInfo. A person's leaf must name it (CertDevice).
func CertDeviceID(c *x509.Certificate) string { return schema.DeviceID(c.RawSubjectPublicKeyInfo) }

// ActSubject is what an act on an object names: the object by kind, name
// and UID, never a bare name, which comes back when the object is made
// again, with the SHA-256 of its spec as the farm served it
// (workjob:show-a/sh040-light-v007@<uid>). A job's digest is
// queue.SpecDigest, the same the HUD serves beside the job.
func ActSubject(kind, namespace, name, uid, specDigest string) Subject {
	return Subject{Name: kind + ":" + namespace + "/" + name + "@" + uid, Digest: map[string]string{"sha256": specDigest}}
}

// PayloadHash is the key the farm takes each act once by: the SHA-256 of
// the DSSE payload as signed, never the envelope's C4 ID, which a
// re-encoded envelope or signature would change.
func PayloadHash(payload []byte) string {
	h := sha256.Sum256(payload)
	return fmt.Sprintf("%x", h[:])
}
