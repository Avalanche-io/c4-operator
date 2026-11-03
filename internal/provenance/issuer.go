package provenance

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// IdentifyFunc maps a bearer token to the identity it may sign as, and how
// long a certificate for it may last. The manager validates the token
// with TokenReview (audience c4-signer): a node daemon's token names its
// node, any other service account's names the account.
type IdentifyFunc func(ctx context.Context, token string) (identity string, ttl time.Duration, err error)

// Issuer serves certificates: a signer sends its public key and its own
// token, and gets back a short-lived certificate binding the key to the
// identity the token proves. This is the shape of Sigstore's Fulcio,
// self-hosted and without a public log, so it works offline inside a
// studio's security boundary.
type Issuer struct {
	CA       *CA
	Identify IdentifyFunc
	// Bind, when set, finds the deployment binding in force for the
	// identity: its leaf names it and ends by its end (IssueBound).
	Bind BindFunc
	// Relays, when set, lists the Trackers that name the identity as their
	// sync (<namespace>/<tracker>): its leaf names each (RelayURN).
	Relays func(ctx context.Context, identity string) ([]string, error)
	Now    func() time.Time
}

// TooMany is an Identify refusal for asking too often, answered 429 with
// Retry-After: Wait from now.
type TooMany struct {
	Who  string
	Wait time.Duration
}

func (e TooMany) Error() string {
	return fmt.Sprintf("%s asks for certificates too often: ask again in %s", e.Who, e.Wait.Round(time.Second))
}

type certRequest struct {
	PublicKey []byte `json:"publicKey"`
}

type certResponse struct {
	Identity string   `json:"identity"`
	Chain    [][]byte `json:"chain"` // DER, leaf first
}

// ServeHTTP handles POST /v1/certificate.
func (i *Issuer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != "/v1/certificate" {
		http.Error(w, "POST /v1/certificate", http.StatusNotFound)
		return
	}
	tok := bearer(r)
	if tok == "" {
		http.Error(w, "a bearer token (audience c4-signer) is required", http.StatusUnauthorized)
		return
	}
	identity, ttl, err := i.Identify(r.Context(), tok)
	var many TooMany
	switch {
	case errors.As(err, &many):
		w.Header().Set("Retry-After", strconv.Itoa(int(many.Wait/time.Second)+1))
		http.Error(w, err.Error(), http.StatusTooManyRequests)
		return
	case err != nil:
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	var req certRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<12)).Decode(&req); err != nil || len(req.PublicKey) != ed25519.PublicKeySize {
		http.Error(w, "send {publicKey}: a 32-byte Ed25519 public key", http.StatusBadRequest)
		return
	}
	now := time.Now()
	if i.Now != nil {
		now = i.Now()
	}
	var bound Bound
	if i.Bind != nil {
		// Bindings that can't be read certify it unbound, as an identity
		// no binding covers: its records are labelled, never attributed
		// to a binding that may have ended (label mode, before K8).
		if bound, err = i.Bind(r.Context(), identity); err != nil {
			bound = Bound{}
		}
	}
	var syncs []string
	if i.Relays != nil {
		if syncs, err = i.Relays(r.Context(), identity); err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
	}
	cert, err := i.CA.IssueBoundRelay(ed25519.PublicKey(req.PublicKey), identity, bound, syncs, ttl, now)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(certResponse{Identity: identity, Chain: [][]byte{cert.Raw, i.CA.Cert.Raw}})
}

func bearer(r *http.Request) string {
	const p = "Bearer "
	h := r.Header.Get("Authorization")
	if len(h) > len(p) && h[:len(p)] == p {
		return h[len(p):]
	}
	return ""
}

// RemoteIssuer asks an issuer at url for certificates, presenting the
// token token() returns (re-read each time: projected tokens rotate).
func RemoteIssuer(url string, token func() (string, error), hc *http.Client) IssueFunc {
	if hc == nil {
		hc = &http.Client{Timeout: 15 * time.Second}
	}
	return func(ctx context.Context, pub ed25519.PublicKey) ([]*x509.Certificate, error) {
		tok, err := token()
		if err != nil {
			return nil, err
		}
		body, _ := json.Marshal(certRequest{PublicKey: pub})
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url+"/v1/certificate", bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Content-Type", "application/json")
		resp, err := hc.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
			return nil, fmt.Errorf("issuer: %s: %s", resp.Status, bytes.TrimSpace(msg))
		}
		var out certResponse
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			return nil, err
		}
		var chain []*x509.Certificate
		for _, der := range out.Chain {
			c, err := x509.ParseCertificate(der)
			if err != nil {
				return nil, err
			}
			chain = append(chain, c)
		}
		if len(chain) == 0 {
			return nil, errors.New("issuer returned no certificate")
		}
		return chain, nil
	}
}

// LocalIssuer issues from a CA in this process, for the components that
// run beside it (the controller, the queue, the HUD).
func LocalIssuer(ca *CA, identity string, ttl time.Duration) IssueFunc {
	return BoundLocalIssuer(ca, identity, ttl, nil)
}

// BoundLocalIssuer is LocalIssuer under the deployment binding bind finds
// for identity, when bind is set (IssueBound).
func BoundLocalIssuer(ca *CA, identity string, ttl time.Duration, bind BindFunc) IssueFunc {
	return func(ctx context.Context, pub ed25519.PublicKey) ([]*x509.Certificate, error) {
		var b Bound
		if bind != nil {
			// unreadable bindings certify it unbound, as the workload
			// issuer does; it re-certifies once they read (RenewWhen)
			if got, err := bind(ctx, identity); err == nil {
				b = got
			}
		}
		c, err := ca.IssueBound(pub, identity, b, ttl, time.Now())
		if err != nil {
			return nil, err
		}
		return []*x509.Certificate{c, ca.Cert}, nil
	}
}
