package storeurl

import (
	"bufio"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// Credentials carries S3 keys. Empty for stores that need none. Temporary
// credentials also carry a session token and the time they expire.
type Credentials struct {
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
	Expires         time.Time // zero: they don't expire
}

func (c Credentials) ok() bool { return c.AccessKeyID != "" && c.SecretAccessKey != "" }

// CredentialSource gives the credentials to sign the next request with.
type CredentialSource interface {
	Retrieve(ctx context.Context) (Credentials, error)
}

type staticSource Credentials

func (s staticSource) Retrieve(context.Context) (Credentials, error) { return Credentials(s), nil }

// Static is a source that always gives c: keys from a Secret, or from a
// person's environment.
func Static(c Credentials) CredentialSource { return staticSource(c) }

// Ambient is the standard AWS credential chain, as the AWS CLI and SDKs
// read it, for a process running as itself (a CLI, a job on an instance):
//
//  1. the environment: AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY and, for
//     temporary keys, AWS_SESSION_TOKEN;
//  2. a web identity (EKS pod roles, IRSA): AWS_WEB_IDENTITY_TOKEN_FILE and
//     AWS_ROLE_ARN, exchanged with STS for temporary keys;
//  3. the instance's role, from the EC2 metadata service (IMDSv2), unless
//     AWS_EC2_METADATA_DISABLED=true.
//
// Temporary keys are renewed before they expire. Ambient never applies by
// default: a component that serves many projects must not lend its own
// identity to a project that named no credentials, so callers choose it.
func Ambient() CredentialSource { return &Chain{} }

// Chain is Ambient's implementation; its fields are for tests.
type Chain struct {
	Getenv       func(string) string // default os.Getenv
	Client       *http.Client        // for STS; default a 30 s timeout
	IMDSClient   *http.Client        // default a 2 s timeout: off EC2 it fails fast
	STSEndpoint  string              // default from AWS_ENDPOINT_URL_STS or the region
	IMDSEndpoint string              // default AWS_EC2_METADATA_SERVICE_ENDPOINT or 169.254.169.254
	Now          func() time.Time

	mu        sync.Mutex
	cur       Credentials
	inflight  chan struct{}
	lastTry   time.Time
	sessionNo int
}

// Renew this long before temporary keys expire. STS and the instance
// role hand out keys valid for an hour or more, and renew them in their
// last 5 to 15 minutes.
const renewBefore = 5 * time.Minute

// After a failed renewal, keys that still hold are used without asking
// again for this long, so a metadata outage doesn't stall every request.
const retryRenewal = 10 * time.Second

func (c *Chain) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Chain) getenv(k string) string {
	if c.Getenv != nil {
		return c.Getenv(k)
	}
	return os.Getenv(k)
}

// Retrieve gives the current keys, renewing them first when they are
// close to expiry. One renewal runs at a time; meanwhile other callers
// keep the keys that still hold, and wait only when none do.
func (c *Chain) Retrieve(ctx context.Context) (Credentials, error) {
	for {
		c.mu.Lock()
		now := c.now()
		valid := c.cur.ok() && (c.cur.Expires.IsZero() || now.Before(c.cur.Expires))
		fresh := valid && (c.cur.Expires.IsZero() || now.Before(c.cur.Expires.Add(-renewBefore)))
		if fresh || (valid && (c.inflight != nil || now.Sub(c.lastTry) < retryRenewal)) {
			cur := c.cur
			c.mu.Unlock()
			return cur, nil
		}
		if ch := c.inflight; ch != nil {
			c.mu.Unlock()
			select {
			case <-ch:
				continue
			case <-ctx.Done():
				return Credentials{}, ctx.Err()
			}
		}
		ch := make(chan struct{})
		c.inflight = ch
		c.lastTry = now
		c.mu.Unlock()

		// The renewal serves every waiter, so it doesn't end with the
		// context of whichever request started it.
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		got, err := c.resolve(rctx)
		cancel()

		c.mu.Lock()
		c.inflight = nil
		close(ch)
		if err == nil {
			c.cur = got
		}
		cur := c.cur
		valid = cur.ok() && (cur.Expires.IsZero() || c.now().Before(cur.Expires))
		c.mu.Unlock()
		if err != nil && !valid {
			return Credentials{}, err
		}
		return cur, nil
	}
}

func (c *Chain) resolve(ctx context.Context) (Credentials, error) {
	id, secret := c.getenv("AWS_ACCESS_KEY_ID"), c.getenv("AWS_SECRET_ACCESS_KEY")
	switch {
	case id != "" && secret != "":
		return Credentials{AccessKeyID: id, SecretAccessKey: secret, SessionToken: c.getenv("AWS_SESSION_TOKEN")}, nil
	case id != "" || secret != "":
		return Credentials{}, errors.New("AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY must be set together")
	}
	if file, role := c.getenv("AWS_WEB_IDENTITY_TOKEN_FILE"), c.getenv("AWS_ROLE_ARN"); file != "" || role != "" {
		if file == "" || role == "" {
			return Credentials{}, errors.New("a web identity needs both AWS_WEB_IDENTITY_TOKEN_FILE and AWS_ROLE_ARN")
		}
		return c.webIdentity(ctx, file, role)
	}
	if strings.EqualFold(c.getenv("AWS_EC2_METADATA_DISABLED"), "true") {
		return Credentials{}, errors.New("no AWS credentials: none in the environment, no web identity, and the instance metadata service is disabled")
	}
	creds, err := c.instanceRole(ctx)
	if err != nil {
		return Credentials{}, fmt.Errorf("no AWS credentials: none in the environment, no web identity, and no instance role (%w)", err)
	}
	return creds, nil
}

// ---- web identity (STS AssumeRoleWithWebIdentity) ----

func (c *Chain) stsEndpoint() string {
	if c.STSEndpoint != "" {
		return c.STSEndpoint
	}
	if ep := c.getenv("AWS_ENDPOINT_URL_STS"); ep != "" {
		return ep
	}
	region := c.getenv("AWS_REGION")
	if region == "" {
		region = c.getenv("AWS_DEFAULT_REGION")
	}
	switch {
	case region == "":
		return "https://sts.amazonaws.com"
	case strings.HasPrefix(region, "cn-"):
		return "https://sts." + region + ".amazonaws.com.cn"
	}
	return "https://sts." + region + ".amazonaws.com"
}

func (c *Chain) webIdentity(ctx context.Context, file, role string) (Credentials, error) {
	// The token file is rotated by the kubelet; read it at every renewal.
	tok, err := os.ReadFile(file)
	if err != nil {
		return Credentials{}, fmt.Errorf("web identity token: %w", err)
	}
	session := c.getenv("AWS_ROLE_SESSION_NAME")
	if session == "" {
		c.mu.Lock()
		c.sessionNo++
		session = fmt.Sprintf("c4-%d-%d", c.now().Unix(), c.sessionNo)
		c.mu.Unlock()
	}
	form := url.Values{
		"Action":           {"AssumeRoleWithWebIdentity"},
		"Version":          {"2011-06-15"},
		"RoleArn":          {role},
		"RoleSessionName":  {session},
		"WebIdentityToken": {strings.TrimSpace(string(tok))},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.stsEndpoint(), strings.NewReader(form.Encode()))
	if err != nil {
		return Credentials{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	client := c.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return Credentials{}, fmt.Errorf("sts: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Code    string `xml:"Error>Code"`
			Message string `xml:"Error>Message"`
		}
		xml.Unmarshal(body, &e)
		return Credentials{}, fmt.Errorf("sts AssumeRoleWithWebIdentity: status %d: %s %s", resp.StatusCode, e.Code, e.Message)
	}
	var out struct {
		Creds struct {
			AccessKeyID     string `xml:"AccessKeyId"`
			SecretAccessKey string `xml:"SecretAccessKey"`
			SessionToken    string `xml:"SessionToken"`
			Expiration      string `xml:"Expiration"`
		} `xml:"AssumeRoleWithWebIdentityResult>Credentials"`
	}
	if err := xml.Unmarshal(body, &out); err != nil {
		return Credentials{}, fmt.Errorf("sts: %w", err)
	}
	return temporary(out.Creds.AccessKeyID, out.Creds.SecretAccessKey, out.Creds.SessionToken, out.Creds.Expiration, "sts")
}

// ---- instance role (IMDSv2) ----

func (c *Chain) instanceRole(ctx context.Context) (Credentials, error) {
	base := c.IMDSEndpoint
	if base == "" {
		base = c.getenv("AWS_EC2_METADATA_SERVICE_ENDPOINT")
	}
	if base == "" {
		base = "http://169.254.169.254"
	}
	base = strings.TrimSuffix(base, "/")
	client := c.IMDSClient
	if client == nil {
		client = &http.Client{Timeout: 2 * time.Second}
	}
	call := func(method, path, token string) (string, error) {
		req, err := http.NewRequestWithContext(ctx, method, base+path, nil)
		if err != nil {
			return "", err
		}
		if token == "" {
			req.Header.Set("X-Aws-Ec2-Metadata-Token-Ttl-Seconds", "21600")
		} else {
			req.Header.Set("X-Aws-Ec2-Metadata-Token", token)
		}
		resp, err := client.Do(req)
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if resp.StatusCode != http.StatusOK {
			return "", fmt.Errorf("instance metadata %s: status %d", path, resp.StatusCode)
		}
		return string(body), nil
	}
	token, err := call(http.MethodPut, "/latest/api/token", "")
	if err != nil {
		return Credentials{}, err
	}
	roles, err := call(http.MethodGet, "/latest/meta-data/iam/security-credentials/", token)
	if err != nil {
		return Credentials{}, err
	}
	sc := bufio.NewScanner(strings.NewReader(roles))
	if !sc.Scan() || strings.TrimSpace(sc.Text()) == "" {
		return Credentials{}, errors.New("the instance has no role")
	}
	doc, err := call(http.MethodGet, "/latest/meta-data/iam/security-credentials/"+strings.TrimSpace(sc.Text()), token)
	if err != nil {
		return Credentials{}, err
	}
	var out struct {
		Code            string
		AccessKeyID     string `json:"AccessKeyId"`
		SecretAccessKey string
		Token           string
		Expiration      string
	}
	if err := json.Unmarshal([]byte(doc), &out); err != nil {
		return Credentials{}, fmt.Errorf("instance role: %w", err)
	}
	if out.Code != "" && out.Code != "Success" {
		return Credentials{}, fmt.Errorf("instance role: %s", out.Code)
	}
	return temporary(out.AccessKeyID, out.SecretAccessKey, out.Token, out.Expiration, "instance role")
}

func temporary(id, secret, token, expiration, from string) (Credentials, error) {
	exp, err := time.Parse(time.RFC3339, strings.TrimSpace(expiration))
	if err != nil {
		return Credentials{}, fmt.Errorf("%s: expiration %q: %w", from, expiration, err)
	}
	c := Credentials{AccessKeyID: id, SecretAccessKey: secret, SessionToken: token, Expires: exp}
	if !c.ok() || token == "" {
		return Credentials{}, fmt.Errorf("%s: incomplete credentials", from)
	}
	return c, nil
}
