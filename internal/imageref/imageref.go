// Package imageref pins a container image reference to its digest: what
// publishing a tool writes into tool.lock, so the published tree's C4 ID
// covers the exact image (DESIGN.md 10.6, Published tools). It asks the
// image's registry (the Docker Registry HTTP API v2, with the bearer
// tokens public registries hand out), and can read a digest a node already reports for
// the image (NodeDigest), for a farm that cannot reach the registry.
package imageref

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
)

// Ref is a parsed image reference.
type Ref struct {
	// Domain is the registry as names carry it (docker.io for Docker Hub),
	// Repository the path in it (library/busybox), and Tag or Digest what
	// is asked for. With neither, the tag is latest.
	Domain, Repository, Tag, Digest string
}

var digestRE = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

// Parse reads a reference as Docker and Kubernetes do: a first path part
// with a dot or a colon, or localhost, names the registry; otherwise the
// image is on Docker Hub, under library/ when it has one part.
func Parse(s string) (Ref, error) {
	var r Ref
	if s == "" || strings.ContainsAny(s, " \t\n") {
		return r, fmt.Errorf("%q is not an image reference", s)
	}
	name := s
	if at := strings.Index(name, "@"); at >= 0 {
		r.Digest, name = name[at+1:], name[:at]
		if !digestRE.MatchString(r.Digest) {
			return r, fmt.Errorf("%q: the digest is not sha256:<64 hex>", s)
		}
	}
	if slash := strings.LastIndex(name, "/"); strings.LastIndex(name, ":") > slash {
		c := strings.LastIndex(name, ":")
		r.Tag, name = name[c+1:], name[:c]
	}
	first, rest, ok := strings.Cut(name, "/")
	if ok && (strings.ContainsAny(first, ".:") || first == "localhost") {
		r.Domain, r.Repository = first, rest
	} else {
		r.Domain, r.Repository = "docker.io", name
		if !strings.Contains(name, "/") {
			r.Repository = "library/" + name
		}
	}
	if r.Repository == "" || strings.ToLower(r.Repository) != r.Repository {
		return r, fmt.Errorf("%q: the repository must be lowercase and not empty", s)
	}
	if r.Tag == "" && r.Digest == "" {
		r.Tag = "latest"
	}
	return r, nil
}

// Name is the repository's full name, domain included.
func (r Ref) Name() string { return r.Domain + "/" + r.Repository }

// Pinned is the reference by digest.
func (r Ref) Pinned() string { return r.Name() + "@" + r.Digest }

// Resolver asks registries for digests.
type Resolver struct {
	// Client makes the requests (default: one with a 20 s timeout).
	Client *http.Client
	// PlainHTTP says a registry is reached without TLS; by default only
	// localhost and loopback addresses are (a local farm's registry).
	PlainHTTP func(host string) bool
}

// manifest types a registry may answer with; an index or list is what a
// multi-arch image's tag names, and its digest is what a node pulls by.
var accept = strings.Join([]string{
	"application/vnd.oci.image.index.v1+json",
	"application/vnd.docker.distribution.manifest.list.v2+json",
	"application/vnd.oci.image.manifest.v1+json",
	"application/vnd.docker.distribution.manifest.v2+json",
}, ", ")

// Pin returns ref by digest: as given when it names one, else the digest
// its registry reports for the tag now.
func (z *Resolver) Pin(ctx context.Context, ref string) (string, error) {
	r, err := Parse(ref)
	if err != nil {
		return "", err
	}
	if r.Digest != "" {
		return r.Pinned(), nil
	}
	d, err := z.digest(ctx, r)
	if err != nil {
		return "", fmt.Errorf("%s: %w", ref, err)
	}
	r.Digest = d
	return r.Pinned(), nil
}

func (z *Resolver) client() *http.Client {
	if z.Client != nil {
		return z.Client
	}
	return &http.Client{Timeout: 20 * time.Second}
}

func (z *Resolver) plain(host string) bool {
	if z.PlainHTTP != nil {
		return z.PlainHTTP(host)
	}
	return Loopback(host)
}

// Loopback says host (host[:port]) is this machine.
func Loopback(host string) bool {
	h := host
	if x, _, err := net.SplitHostPort(host); err == nil {
		h = x
	}
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

func (z *Resolver) digest(ctx context.Context, r Ref) (string, error) {
	host := r.Domain
	if host == "docker.io" {
		host = "registry-1.docker.io"
	}
	scheme := "https"
	if z.plain(host) {
		scheme = "http"
	}
	u := scheme + "://" + host + "/v2/" + r.Repository + "/manifests/" + r.Tag
	token := ""
	for try := 0; try < 2; try++ {
		for _, method := range []string{http.MethodHead, http.MethodGet} {
			req, err := http.NewRequestWithContext(ctx, method, u, nil)
			if err != nil {
				return "", err
			}
			req.Header.Set("Accept", accept)
			if token != "" {
				req.Header.Set("Authorization", "Bearer "+token)
			}
			resp, err := z.client().Do(req)
			if err != nil {
				return "", fmt.Errorf("the registry %s: %w", host, err)
			}
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
			resp.Body.Close()
			switch {
			case resp.StatusCode == http.StatusUnauthorized && token == "":
				token, err = z.token(ctx, host, resp.Header.Get("WWW-Authenticate"))
				if err != nil {
					return "", fmt.Errorf("the registry %s: %w", host, err)
				}
			case resp.StatusCode == http.StatusOK:
				if d := resp.Header.Get("Docker-Content-Digest"); digestRE.MatchString(d) {
					return d, nil
				}
				if method == http.MethodGet && len(body) > 0 {
					sum := sha256.Sum256(body)
					return "sha256:" + hex.EncodeToString(sum[:]), nil
				}
				continue // a HEAD without the header: ask with GET
			case resp.StatusCode == http.StatusNotFound:
				return "", fmt.Errorf("the registry %s has no %s:%s", host, r.Repository, r.Tag)
			default:
				return "", fmt.Errorf("the registry %s answered %s", host, resp.Status)
			}
			break
		}
		if token == "" {
			break
		}
	}
	return "", errors.New("the registry refused the credentials it issued")
}

// token asks the realm a registry's challenge names for an anonymous pull
// token. The realm comes from the registry's own answer, so a registry a
// publisher runs could name any URL: it is asked only over https (or
// plain http where the registry rules allow that host), and a realm on
// another host than the registry is refused when it is this machine,
// the cluster's network or a metadata service (realmReachable).
func (z *Resolver) token(ctx context.Context, registry, challenge string) (string, error) {
	scheme, params, ok := strings.Cut(challenge, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", fmt.Errorf("it asks for %q credentials, which publishing does not hold", scheme)
	}
	p := map[string]string{}
	for _, kv := range splitParams(params) {
		k, v, _ := strings.Cut(kv, "=")
		p[strings.ToLower(strings.TrimSpace(k))] = strings.Trim(strings.TrimSpace(v), `"`)
	}
	realm, err := url.Parse(p["realm"])
	if err != nil || realm.Scheme == "" {
		return "", fmt.Errorf("its challenge names no token realm")
	}
	if err := z.realmReachable(ctx, registry, realm); err != nil {
		return "", err
	}
	q := realm.Query()
	for _, k := range []string{"service", "scope"} {
		if p[k] != "" {
			q.Set(k, p[k])
		}
	}
	realm.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, realm.String(), nil)
	if err != nil {
		return "", err
	}
	resp, err := z.client().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("its token service answered %s", resp.Status)
	}
	var t struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&t); err != nil {
		return "", fmt.Errorf("its token service: %w", err)
	}
	if t.Token == "" {
		t.Token = t.AccessToken
	}
	if t.Token == "" {
		return "", errors.New("its token service gave no token")
	}
	return t.Token, nil
}

// splitParams splits a challenge's parameters at commas outside quotes.
func splitParams(s string) []string {
	var out []string
	quoted, start := false, 0
	for i, c := range s {
		switch {
		case c == '"':
			quoted = !quoted
		case c == ',' && !quoted:
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}

// NodeDigest is the digest a node reports for ref, from the images it
// holds (node.status.images): an entry that names ref by its tag and also
// by a digest in the same repository. It answers for an image loaded onto
// a farm with no registry it can reach, when the node lists the image's
// repository digest.
func NodeDigest(nodes []corev1.Node, ref string) (string, bool) {
	r, err := Parse(ref)
	if err != nil {
		return "", false
	}
	if r.Digest != "" {
		return r.Pinned(), true
	}
	tagged := r.Name() + ":" + r.Tag
	for _, n := range nodes {
		for _, img := range n.Status.Images {
			has := false
			for _, name := range img.Names {
				if x, err := Parse(name); err == nil && x.Digest == "" && x.Name()+":"+x.Tag == tagged {
					has = true
				}
			}
			if !has {
				continue
			}
			for _, name := range img.Names {
				if x, err := Parse(name); err == nil && x.Digest != "" && x.Name() == r.Name() {
					return x.Pinned(), true
				}
			}
		}
	}
	return "", false
}

// realmReachable says why a token realm is not asked: a scheme other than
// https (or http where the registry rules allow the realm's host), or a
// host other than the registry's own that is, or resolves to, an address
// a registry has no business sending the HUD to: loopback, private,
// link-local (a cloud's metadata service), unspecified or carrier-grade
// NAT. A registry's realm on its own host is the registry's to name.
func (z *Resolver) realmReachable(ctx context.Context, registry string, realm *url.URL) error {
	switch {
	case realm.Scheme == "https":
	case realm.Scheme == "http" && z.plain(realm.Host):
	default:
		return fmt.Errorf("its token realm %s is not https", realm.Redacted())
	}
	if sameHost(realm.Host, registry) {
		return nil
	}
	h := realm.Hostname()
	ips := []net.IP{net.ParseIP(h)}
	if ips[0] == nil {
		addrs, err := net.DefaultResolver.LookupIPAddr(ctx, h)
		if err != nil {
			return fmt.Errorf("its token realm %s: %w", h, err)
		}
		ips = ips[:0]
		for _, a := range addrs {
			ips = append(ips, a.IP)
		}
	}
	for _, ip := range ips {
		if internal(ip) {
			return fmt.Errorf("its token realm %s is on this machine or the cluster's network (%s), not the registry's", h, ip)
		}
	}
	return nil
}

func sameHost(a, b string) bool {
	ha, hb := a, b
	if h, _, err := net.SplitHostPort(a); err == nil {
		ha = h
	}
	if h, _, err := net.SplitHostPort(b); err == nil {
		hb = h
	}
	return strings.EqualFold(ha, hb)
}

var cgnat = &net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}

func internal(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() || ip.IsMulticast() || cgnat.Contains(ip)
}
