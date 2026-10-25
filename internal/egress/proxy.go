// Package egress is the farm's egress proxy (DESIGN 10.6, tools).
// A unit whose tool allows hosts reaches them only through it: the
// unit's NetworkPolicy allows the proxy and DNS, the queue points the
// unit's HTTPS_PROXY and HTTP_PROXY at it, and the proxy lets a request
// through only to a host the unit's tool lists. The proxy knows the
// unit by its pod's address, so every request it passes or refuses is
// logged against a job and a unit. It works on any CNI.
package egress

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-logr/logr"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
)

// Unit is the unit behind a client address.
type Unit struct {
	Namespace, Job, Unit, Pod, Tool string
	// JobTool is set for a tool job's unit: its tool is its job's.
	JobTool bool
}

// Adviser says what would let a refused unit reach a host, for the
// proxy's refusal to name the line to add.
type Adviser interface {
	Advise(ctx context.Context, u Unit, host string, port int) string
}

// Ranger knows the cluster's own address blocks (its pods', its
// services', its nodes'), which a host rule never reaches either.
type Ranger interface {
	Ranges(ctx context.Context) []*net.IPNet
}

// Resolver says which unit is at an address, and what its tool allows.
type Resolver interface {
	Rules(ctx context.Context, ip string) (Unit, []c4v1.ToolEgress, error)
}

// Proxy is an HTTP forward proxy: CONNECT tunnels and absolute-form
// requests, each to a host the client unit's tool allows.
type Proxy struct {
	Resolve Resolver
	Log     logr.Logger
	// Dial reaches upstream hosts (default net.Dialer), at the address
	// the proxy checked.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
	// Lookup resolves a host's addresses (default the system resolver).
	Lookup func(ctx context.Context, host string) ([]net.IP, error)
}

// inside are the addresses a host rule never reaches: what a name someone
// else's DNS controls could point at to reach the cluster, the node or a
// cloud's metadata service (private, loopback, link-local, carrier-grade
// NAT, reserved, and the IPv6 forms that carry an IPv4 address). A
// resolver that knows the cluster's own blocks (Ranger) adds them. A unit
// reaches one only through a cidr rule that covers it on that port.
var inside = func() []*net.IPNet {
	var out []*net.IPNet
	for _, c := range []string{
		"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16", "172.16.0.0/12",
		"192.0.0.0/24", "192.168.0.0/16", "198.18.0.0/15", "224.0.0.0/4", "240.0.0.0/4",
		"::/128", "::1/128", "fc00::/7", "fe80::/10", "ff00::/8",
		// IPv6 forms that carry an IPv4 address: NAT64 (well-known and
		// local-use), IPv4-compatible, 6to4 and Teredo
		"64:ff9b::/96", "64:ff9b:1::/48", "::/96", "2002::/16", "2001::/32",
	} {
		_, n, _ := net.ParseCIDR(c)
		out = append(out, n)
	}
	return out
}()

// reachable is the first of host's addresses the unit may reach on port:
// one outside the inside ranges and the cluster's own, or inside them only
// where one of its cidr rules covers it on that port. The proxy dials
// that address, not the name again, so the name cannot point elsewhere in
// between.
func (p *Proxy) reachable(ctx context.Context, host string, port int, rules []c4v1.ToolEgress) (net.IP, error) {
	var cluster []*net.IPNet
	if rg, ok := p.Resolve.(Ranger); ok {
		cluster = rg.Ranges(ctx)
	}
	ips := []net.IP{net.ParseIP(host)}
	if ips[0] == nil {
		look := p.Lookup
		if look == nil {
			look = func(ctx context.Context, host string) ([]net.IP, error) {
				return net.DefaultResolver.LookupIP(ctx, "ip", host)
			}
		}
		var err error
		if ips, err = look(ctx, host); err != nil {
			return nil, &unresolved{host: host, err: err}
		}
	}
	var refused []string
	for _, ip := range ips {
		if ip == nil {
			continue
		}
		if !insideRange(ip, cluster) || coveredByCIDR(rules, ip, port) {
			return ip, nil
		}
		refused = append(refused, ip.String())
	}
	if len(refused) == 0 {
		return nil, fmt.Errorf("%s has no address", host)
	}
	return nil, fmt.Errorf("%s resolves to %s, inside the cluster's, the node's or private addresses, which a host rule never reaches; a cidr rule must cover them on port %d",
		host, strings.Join(refused, ", "), port)
}

// unresolved is a host the proxy could not resolve: a DNS failure, not a
// refusal, so it answers 502.
type unresolved struct {
	host string
	err  error
}

func (u *unresolved) Error() string { return "could not resolve " + u.host + ": " + u.err.Error() }

func insideRange(ip net.IP, cluster []*net.IPNet) bool {
	for _, set := range [][]*net.IPNet{inside, cluster} {
		for _, n := range set {
			if n.Contains(ip) {
				return true
			}
		}
	}
	return false
}

// coveredByCIDR says whether one of rules' cidr rules covers ip on port
// (a rule with no ports, any port).
func coveredByCIDR(rules []c4v1.ToolEgress, ip net.IP, port int) bool {
	for _, r := range rules {
		_, n, err := net.ParseCIDR(r.CIDR)
		if err != nil || !n.Contains(ip) {
			continue
		}
		if len(r.Ports) == 0 {
			return true
		}
		for _, p := range r.Ports {
			if int(p) == port {
				return true
			}
		}
	}
	return false
}

func (p *Proxy) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	if p.Dial != nil {
		return p.Dial(ctx, network, addr)
	}
	var d net.Dialer
	return d.DialContext(ctx, network, addr)
}

// Allowed reports whether rules allow host on port: a rule whose host is
// the same (case aside), or *.domain for a name under domain, and whose
// ports, if it lists any, include port.
func Allowed(rules []c4v1.ToolEgress, host string, port int) bool {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	for _, r := range rules {
		h := strings.TrimSuffix(strings.ToLower(r.Host), ".")
		if h == "" {
			continue
		}
		match := h == host
		if suffix, ok := strings.CutPrefix(h, "*."); ok {
			match = strings.HasSuffix(host, "."+suffix)
		}
		if !match {
			continue
		}
		if len(r.Ports) == 0 {
			return true
		}
		for _, p := range r.Ports {
			if int(p) == port {
				return true
			}
		}
	}
	return false
}

// target is the host and port a request asks for.
func target(r *http.Request) (string, int) {
	hostport, def := r.Host, 443
	if r.Method != http.MethodConnect {
		hostport, def = r.URL.Host, 80
		if r.URL.Scheme == "https" {
			def = 443
		}
	}
	host, ps, err := net.SplitHostPort(hostport)
	if err != nil {
		return hostport, def
	}
	port, err := strconv.Atoi(ps)
	if err != nil {
		return host, def
	}
	return host, port
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodConnect && r.URL.Host == "" {
		// Not a proxy request: the readiness probe, or a mistake.
		if r.URL.Path == "/healthz" {
			w.WriteHeader(http.StatusOK)
			return
		}
		http.Error(w, "egress: a proxy request names its host", http.StatusBadRequest)
		return
	}
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		ip = r.RemoteAddr
	}
	host, port := target(r)
	u, rules, err := p.Resolve.Rules(r.Context(), ip)
	if err != nil {
		p.Log.Info("refused", "client", ip, "host", host, "port", port, "why", err.Error())
		http.Error(w, "egress: "+err.Error(), http.StatusForbidden)
		return
	}
	who := []any{"job", u.Namespace + "/" + u.Job, "unit", u.Unit, "pod", u.Pod, "tool", u.Tool, "host", host, "port", port}
	if !Allowed(rules, host, port) {
		p.Log.Info("refused", append(who, "why", "not in the tool")...)
		msg := "egress: " + host + ":" + strconv.Itoa(port) + " is not in tool " + u.Tool
		if a, ok := p.Resolve.(Adviser); ok {
			msg += "; to reach it, " + a.Advise(r.Context(), u, host, port)
		}
		http.Error(w, msg, http.StatusForbidden)
		return
	}
	addrIP, err := p.reachable(r.Context(), host, port, rules)
	if err != nil {
		code := http.StatusForbidden
		var u *unresolved
		if errors.As(err, &u) {
			code = http.StatusBadGateway // not a refusal: the name did not resolve
		}
		p.Log.Info("refused", append(who, "why", err.Error())...)
		http.Error(w, "egress: "+err.Error(), code)
		return
	}
	p.Log.Info("allowed", append(who, "address", addrIP.String())...)
	addr := net.JoinHostPort(addrIP.String(), strconv.Itoa(port))
	if r.Method == http.MethodConnect {
		p.tunnel(w, r, addr)
		return
	}
	p.forward(w, r, addr)
}

// tunnel joins the client to addr for a CONNECT.
func (p *Proxy) tunnel(w http.ResponseWriter, r *http.Request, addr string) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	up, err := p.dial(ctx, "tcp", addr)
	cancel()
	if err != nil {
		http.Error(w, "egress: "+err.Error(), http.StatusBadGateway)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		up.Close()
		http.Error(w, "egress: no tunnel on this connection", http.StatusInternalServerError)
		return
	}
	client, rw, err := hj.Hijack()
	if err != nil {
		up.Close()
		return
	}
	if _, err := client.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n")); err != nil {
		client.Close()
		up.Close()
		return
	}
	done := make(chan struct{}, 2)
	go func() {
		// what the client sent before the tunnel opened, then the rest
		if n := rw.Reader.Buffered(); n > 0 {
			b, _ := rw.Reader.Peek(n)
			up.Write(b)
		}
		io.Copy(up, client)
		closeWrite(up)
		done <- struct{}{}
	}()
	go func() {
		io.Copy(client, up)
		closeWrite(client)
		done <- struct{}{}
	}()
	<-done
	<-done
	client.Close()
	up.Close()
}

func closeWrite(c net.Conn) {
	if t, ok := c.(interface{ CloseWrite() error }); ok {
		t.CloseWrite()
	}
}

// hopHeaders are a connection's own, not passed on.
var hopHeaders = []string{"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
	"Te", "Trailer", "Transfer-Encoding", "Upgrade"}

// forward sends a plain HTTP request on to addr and copies the answer back.
func (p *Proxy) forward(w http.ResponseWriter, r *http.Request, addr string) {
	out := r.Clone(r.Context())
	out.RequestURI = ""
	for _, h := range hopHeaders {
		out.Header.Del(h)
	}
	tr := &http.Transport{Proxy: nil, DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return p.dial(ctx, network, addr)
	}}
	defer tr.CloseIdleConnections()
	resp, err := tr.RoundTrip(out)
	if err != nil {
		http.Error(w, "egress: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	for _, h := range hopHeaders {
		resp.Header.Del(h)
	}
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}
