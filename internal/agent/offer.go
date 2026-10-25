package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
)

// A machine is enrolled by its offer and its holder's acceptance (the
// HUD's POST /nodes/<node>/enroll-offer and /enroll): its runner offers it
// with its own token and shows the code the HUD gives, and the holder,
// signed in as themselves, accepts with that code. Until someone has, the
// runner offers again before each code runs out, and the farm places no
// one's work on it.

// OfferEvery is how often an unowned machine's runner makes a new offer:
// a little within the HUD's ten minutes, so a code always stands.
const OfferEvery = 9 * time.Minute

// EnrollOffer is the HUD's answer to an offer.
type EnrollOffer struct {
	Node    string    `json:"node"`
	Code    string    `json:"code"`
	Expires time.Time `json:"expires"`
	Record  string    `json:"record,omitempty"`
	Message string    `json:"message"`
}

// OfferMachine posts a machine's enrollment offer to the HUD, as its
// runner (token: its HUD token), naming the holder whose sign-in it uses.
func OfferMachine(ctx context.Context, hc *http.Client, hud, token, node, holder string) (EnrollOffer, error) {
	body, _ := json.Marshal(map[string]string{"holder": holder})
	u := strings.TrimRight(hud, "/") + "/api/v1/nodes/" + url.PathEscape(node) + "/enroll-offer"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return EnrollOffer{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return EnrollOffer{}, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		return EnrollOffer{}, fmt.Errorf("the farm refused this machine's offer: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	var o EnrollOffer
	if err := json.Unmarshal(b, &o); err != nil {
		return EnrollOffer{}, err
	}
	return o, nil
}

// nodeOwned reports whether a Node names any owner (c4v1.AnnotationNodeOwners,
// a JSON list).
func nodeOwned(n *corev1.Node) bool {
	var owners []string
	return json.Unmarshal([]byte(n.Annotations[c4v1.AnnotationNodeOwners]), &owners) == nil && len(owners) > 0
}

// offerTick offers the machine for enrollment while no one owns it, before
// each code runs out, and says the code; once someone has enrolled it, it
// says so once and offers no more.
func (r *Runner) offerTick(ctx context.Context) {
	if !r.Offer || r.HUD == "" || r.offerDone || (!r.offerDue.IsZero() && r.now().Before(r.offerDue)) {
		return
	}
	var n corev1.Node
	if err := r.Client.Get(ctx, types.NamespacedName{Name: r.Node}, &n); err != nil {
		return // the heartbeat makes it; the next round offers
	}
	if nodeOwned(&n) {
		r.offerDone = true
		r.logf("this machine is enrolled: the farm places its owners' work here")
		return
	}
	token := ""
	if r.HUDToken != nil {
		token = r.HUDToken()
	}
	o, err := OfferMachine(ctx, r.HTTPClient, r.HUD, token, r.Node, r.holder())
	if err != nil {
		r.offerDue = r.now().Add(time.Minute)
		r.logf("offering this machine: %v", err)
		return
	}
	r.offerDue = r.now().Add(OfferEvery)
	r.logf("this machine is not enrolled yet: %s, or run c4 farm enroll %s %s", o.Message, r.Node, o.Code)
}
