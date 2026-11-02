package agent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/Avalanche-io/c4-operator/internal/provenance"
	"github.com/Avalanche-io/c4-operator/schema"
)

// A host runner signs each call it makes to the HUD with its own key
// (request/v1, X-C4-Request; PEOPLE-KEYS 2.4): a key made on this machine,
// certified as runner/<node> by the farm's workload issuer on the
// machine's own token (audience c4-signer), which the HUD serves to host
// runners at its own address (/workload/v1/certificate). The key never
// leaves the runner's memory and is renewed past half its hour. Until
// every client signs (K8) the call also carries its token, so a HUD that
// takes no signed request yet reads it as before.

// keyRetry is how long a runner whose key could not be certified waits
// before asking the issuer again; its calls go with their token alone
// meanwhile.
const keyRetry = time.Minute

// HostKey is an enrolled machine's runner key: certified by the workload
// issuer the HUD at hud serves, on the token in the file signerToken. Nil
// without a HUD. It obtains its certificate when it first signs.
func HostKey(hud, signerToken string) *provenance.Signer {
	if hud == "" {
		return nil
	}
	token := func() (string, error) {
		b, err := os.ReadFile(signerToken)
		if err != nil || strings.TrimSpace(string(b)) == "" {
			return "", fmt.Errorf("no issuer token at %s", signerToken)
		}
		return strings.TrimSpace(string(b)), nil
	}
	return provenance.LazySigner(provenance.RemoteIssuer(strings.TrimRight(hud, "/")+"/workload", token, nil))
}

// SignHUDRequest signs req, whose body is body, as key: its method, its
// target as it goes on the wire and its body.
func SignHUDRequest(ctx context.Context, key *provenance.Signer, req *http.Request, body []byte) error {
	if key == nil {
		return errors.New("no key")
	}
	h, err := provenance.SignRequest(ctx, key, req.Method, req.URL.RequestURI(), body, time.Now())
	if err != nil {
		return err
	}
	req.Header.Set(schema.RequestHeader, h)
	return nil
}

// hudKey is a runner's key state: when it may next ask the issuer after a
// failure, and the last failure it reported.
type hudKey struct {
	mu      sync.Mutex
	after   time.Time
	lastErr string
}

// signHUD signs a call to the HUD as this machine's runner when it has a
// key (Key). A key that cannot be certified is said once, until it can,
// and the call goes with its token alone.
func (r *Runner) signHUD(ctx context.Context, req *http.Request, body []byte) {
	if r.Key == nil {
		return
	}
	k := &r.hudKey
	k.mu.Lock()
	wait := time.Now().Before(k.after)
	k.mu.Unlock()
	if wait {
		return
	}
	err := SignHUDRequest(ctx, r.Key, req, body)
	k.mu.Lock()
	defer k.mu.Unlock()
	switch {
	case err == nil && k.lastErr != "":
		k.lastErr = ""
		r.logf("this machine's key is certified again: its calls to the farm are signed (%s)", r.Key.Identity())
	case err != nil:
		k.after = time.Now().Add(keyRetry)
		if msg := err.Error(); msg != k.lastErr {
			k.lastErr = msg
			r.logf("this machine's key: %v; its calls to the farm go with its token alone until the issuer certifies it", err)
		}
	}
}
