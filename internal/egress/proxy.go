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

// Resolver says which unit is at an address, and what its tool allows.
type Resolver interface {
	Rules(ctx context.Context, ip string) (Unit, []c4v1.ToolEgress, error)
}

// Proxy is an HTTP forward proxy: CONNECT tunnels and absolute-form
// requests, each to a host the client unit's tool allows.
type Proxy struct {
	Resolve Resolver
	Log     logr.Logger
	// Dial reaches upstream hosts (default net.Dialer).
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
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
	p.Log.Info("allowed", who...)
	if r.Method == http.MethodConnect {
		p.tunnel(w, r, net.JoinHostPort(host, strconv.Itoa(port)))
		return
	}
	p.forward(w, r, net.JoinHostPort(host, strconv.Itoa(port)))
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
