package server

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/provenance"
)

// Enrolling a machine is an offer and an acceptance, as a device sign-in
// is. The machine offers, with its own runner's token: the HUD holds a
// short one-time code for it, shown at the machine (c4-agent enroll-offer). A
// person accepts with that code, signed in as themselves: only someone who
// can see the machine's screen has it, which is the proof that they hold
// the machine. The HUD then writes the Node's owners (its runner cannot)
// and signs the act. So no one enrolls a machine they do not hold. And the
// one whose Claude Code sign-in the machine's runner uses (its holder) is
// the only one who enrolls it: another person's jobs spend that sign-in
// only when its holder adds them (for), as the holder's own signed act.

// EnrollOfferFor is how long a machine's offer stands, and
// EnrollWrongCodes how many wrong codes void it.
const (
	EnrollOfferFor   = 10 * time.Minute
	EnrollWrongCodes = 5
)

// enrollCodeLetters are the letters a code is made of: no 0/O or 1/I/L, so
// it reads off a screen without mistakes.
const enrollCodeLetters = "ABCDEFGHJKMNPQRSTUVWXYZ23456789"

type enrollOffer struct {
	code    string
	holder  string // whose Claude Code sign-in the machine's runner uses
	expires time.Time
	record  string // the offer's signed record
	wrong   int    // wrong codes given for it
}

// newEnrollCode is eight letters, as XXXX-XXXX, each uniform over the
// letters (a byte past the last whole multiple of their count is drawn
// again).
func newEnrollCode() (string, error) {
	limit := 256 - 256%len(enrollCodeLetters)
	out := make([]byte, 0, 9)
	b := make([]byte, 1)
	for len(out) < 9 {
		if len(out) == 4 {
			out = append(out, '-')
			continue
		}
		if _, err := rand.Read(b); err != nil {
			return "", err
		}
		if int(b[0]) >= limit {
			continue
		}
		out = append(out, enrollCodeLetters[int(b[0])%len(enrollCodeLetters)])
	}
	return string(out), nil
}

// normalCode reads a code as a person types or pastes it: any case, with or
// without its dash, spaces and other separators ignored.
func normalCode(c string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(c) {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	s := b.String()
	if len(s) == 8 {
		s = s[:4] + "-" + s[4:]
	}
	return s
}

// handleEnrollOffer is POST /nodes/{node}/enroll-offer: a host runner
// offers its machine for enrollment, with its own token, and gets the code
// to show on the machine. A new offer replaces the last.
func (s *Server) handleEnrollOffer(w http.ResponseWriter, r *http.Request) {
	v := s.withViewer(w, r)
	if v == nil {
		return
	}
	node := r.PathValue("node")
	if host, ok := strings.CutPrefix(v.User, HostAccountPrefix); !ok || host != node {
		fail(w, http.StatusForbidden, "only node %s's own host runner offers it", node)
		return
	}
	var body struct {
		Holder string `json:"holder"`
	}
	if err := decodeBody(r, &body); err != nil {
		failBody(w, err)
		return
	}
	if strings.TrimSpace(body.Holder) == "" {
		fail(w, http.StatusBadRequest, "send {holder}: whose Claude Code sign-in this machine's runner uses")
		return
	}
	code, err := newEnrollCode()
	if err != nil {
		fail(w, http.StatusInternalServerError, "%v", err)
		return
	}
	ctx := r.Context()
	pred := controlPredicate{Actor: v.User, Authenticated: s.authenticatedBy(r, v), Via: via(r),
		Authorization: "its own node's token", Action: "enroll-offer", Target: "node:" + node,
		Change: map[string]any{"holder": body.Holder}}
	rec, err := s.signControl(ctx, s.allProjects(), provenance.Subject{Name: pred.Target, Digest: map[string]string{"sha256": sha256JSON(body.Holder)}}, pred)
	if err != nil {
		fail(w, http.StatusInternalServerError, "the offer's record: %v", err)
		return
	}
	expires := time.Now().Add(EnrollOfferFor)
	s.offerMu.Lock()
	if s.offers == nil {
		s.offers = map[string]enrollOffer{}
	}
	s.offers[node] = enrollOffer{code: code, holder: body.Holder, expires: expires, record: rec}
	s.offerMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"node": node, "code": code, "expires": expires.UTC(), "record": rec,
		"message": fmt.Sprintf("to enroll %s as yours, sign in to the farm and give this code within %s: %s", node, EnrollOfferFor, code)})
}

// handleNodeEnroll is POST /nodes/{node}/enroll {code}: a person accepts a
// machine's offer, with the code shown at it. They become one of its owners;
// the act is signed (control/v1, enroll, by and for the person, naming the
// offer), and the HUD writes the Node's owners. When the machine's runner
// uses someone else's sign-in, the answer says so.
func (s *Server) handleNodeEnroll(w http.ResponseWriter, r *http.Request) {
	v := s.withViewer(w, r)
	if v == nil {
		return
	}
	if v.Unit != nil || strings.HasPrefix(v.User, "system:") {
		fail(w, http.StatusForbidden, "only a person enrolls a machine, with their own sign-in; %s is not one", v.User)
		return
	}
	node := r.PathValue("node")
	var body struct {
		Code string `json:"code"`
		// For adds someone else as an owner: only the machine's holder may,
		// since their jobs will spend the holder's sign-in.
		For string `json:"for"`
	}
	if err := decodeBody(r, &body); err != nil {
		failBody(w, err)
		return
	}
	s.offerMu.Lock()
	offer, ok := s.offers[node]
	valid := ok && time.Now().Before(offer.expires) &&
		subtle.ConstantTimeCompare([]byte(normalCode(body.Code)), []byte(offer.code)) == 1
	switch {
	case valid && v.User != offer.holder:
		// the holder's to agree to: the offer stands for them
	case valid:
		delete(s.offers, node) // one use
	case ok:
		if offer.wrong++; offer.wrong >= EnrollWrongCodes {
			delete(s.offers, node) // guessing ends the offer
		} else {
			s.offers[node] = offer
		}
	}
	s.offerMu.Unlock()
	if !valid {
		fail(w, http.StatusForbidden, "no standing offer from %s with that code: on it, run c4-agent enroll-offer for a new one", node)
		return
	}
	if v.User != offer.holder {
		fail(w, http.StatusForbidden, "%s's sign-in is %s's; only %s can let others' jobs run on it: %s enrolls you with c4 farm enroll %s --for %s",
			node, offer.holder, offer.holder, offer.holder, node, v.User)
		return
	}
	who := v.User
	if f := strings.TrimSpace(body.For); f != "" {
		who = f
	}
	ctx := r.Context()
	var n corev1.Node
	if err := s.Client.Get(ctx, types.NamespacedName{Name: node}, &n); err != nil {
		fail(w, http.StatusNotFound, "no machine %s: %v", node, err)
		return
	}
	if !slices.ContainsFunc(n.Spec.Taints, func(t corev1.Taint) bool { return t.Key == hostTaint }) {
		fail(w, http.StatusForbidden, "%s is not a host runner's machine", node)
		return
	}
	owners := nodeOwnersOf(&n)
	if !slices.Contains(owners, who) {
		owners = append(owners, who)
		slices.Sort(owners)
		patch := client.MergeFrom(n.DeepCopy())
		if n.Annotations == nil {
			n.Annotations = map[string]string{}
		}
		list, _ := json.Marshal(owners)
		n.Annotations[c4v1.AnnotationNodeOwners] = string(list)
		if err := s.Client.Patch(ctx, &n, patch); err != nil {
			fail(w, http.StatusConflict, "%v", err)
			return
		}
	}
	pred := controlPredicate{Actor: v.User, Authenticated: s.authenticatedBy(r, v), Via: via(r), Agent: claimedAgent(r),
		Authorization: "the code " + node + " showed, from its offer " + offer.record,
		Action:        "enroll", Target: "node:" + node, Change: map[string]any{"for": who, "owners": owners, "holder": offer.holder, "offer": offer.record}}
	rec, err := s.signControl(ctx, v.Projects, provenance.Subject{Name: pred.Target, Digest: map[string]string{"sha256": sha256JSON(owners)}}, pred)
	if err != nil {
		fail(w, http.StatusInternalServerError, "the enrollment's record: %v", err)
		return
	}
	msg := fmt.Sprintf("%s enrolled as %s's machine", node, who)
	if who != v.User {
		msg = fmt.Sprintf("%s enrolled for %s by its holder %s: %s's jobs spend %s's sign-in", node, who, v.User, who, v.User)
	}
	out := map[string]any{"node": node, "owners": owners, "record": rec, "message": msg}
	s.record(ctx, &n, "Node", v.User, "Enrolled", msg, bookNote{actor: actorOf(r, v), kind: "enroll"})
	writeJSON(w, http.StatusOK, out)
}

// allProjects are every project's names, for a record that is no one
// project's (a machine's offer).
func (s *Server) allProjects() []string {
	m := s.current()
	if m == nil {
		return nil
	}
	out := make([]string, 0, len(m.projects))
	for _, p := range m.projects {
		out = append(out, p.Name)
	}
	return out
}
