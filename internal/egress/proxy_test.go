package egress

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/go-logr/logr"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
)

type fakeResolver map[string][]c4v1.ToolEgress

func (f fakeResolver) Rules(_ context.Context, ip string) (Unit, []c4v1.ToolEgress, error) {
	rules, ok := f[ip]
	if !ok {
		return Unit{}, nil, errors.New("no unit pod at " + ip)
	}
	return Unit{Namespace: "show-a", Job: "sh040-research", Unit: "3", Pod: "sh040-research-u3", Tool: "search"}, rules, nil
}

func TestAllowed(t *testing.T) {
	rules := []c4v1.ToolEgress{{Host: "api.search.example", Ports: []int32{443}}, {Host: "*.docs.example"}, {CIDR: "203.0.113.0/24"}}
	for _, c := range []struct {
		host string
		port int
		want bool
	}{
		{"api.search.example", 443, true},
		{"API.Search.Example.", 443, true},
		{"api.search.example", 80, false},
		{"a.docs.example", 8443, true},
		{"docs.example", 443, false}, // the wildcard is for names under it
		{"evil.example", 443, false},
		{"203.0.113.7", 443, false}, // address blocks are not the proxy's
	} {
		if got := Allowed(rules, c.host, c.port); got != c.want {
			t.Errorf("%s:%d: %v, want %v", c.host, c.port, got, c.want)
		}
	}
}

// The proxy lets a unit reach only the hosts its tool lists, by plain
// HTTP and through a CONNECT tunnel; anything else, or a client that is no
// unit, is refused.
func TestProxy(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "upstream %s %s", r.Host, r.URL.Path)
	}))
	defer up.Close()
	upAddr := strings.TrimPrefix(up.URL, "http://")
	p := &Proxy{Log: logr.Discard(),
		Resolve: fakeResolver{"127.0.0.1": {{Host: "api.search.example"}, {Host: "plain.example", Ports: []int32{80}}}},
		Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if addr != "api.search.example:443" && addr != "plain.example:80" {
				return nil, fmt.Errorf("dialed %s", addr)
			}
			var d net.Dialer
			return d.DialContext(ctx, network, upAddr)
		}}
	srv := httptest.NewServer(p)
	defer srv.Close()
	proxyURL, _ := url.Parse(srv.URL)
	c := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}

	// Plain HTTP to an allowed host.
	resp, err := c.Get("http://plain.example/v1/search")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(b) != "upstream plain.example /v1/search" {
		t.Fatalf("plain: %d %q", resp.StatusCode, b)
	}
	// A host the tool does not list.
	resp, err = c.Get("http://evil.example/")
	if err != nil {
		t.Fatal(err)
	}
	b, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(string(b), "not in tool search") {
		t.Fatalf("evil: %d %q", resp.StatusCode, b)
	}

	// A CONNECT tunnel to an allowed host, then a request through it.
	conn, err := net.Dial("tcp", proxyURL.Host)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "CONNECT api.search.example:443 HTTP/1.1\r\nHost: api.search.example:443\r\n\r\n")
	br := bufio.NewReader(conn)
	cr, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil || cr.StatusCode != 200 {
		t.Fatalf("CONNECT: %v %v", cr, err)
	}
	fmt.Fprintf(conn, "GET /q HTTP/1.1\r\nHost: api.search.example\r\nConnection: close\r\n\r\n")
	tr, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ = io.ReadAll(tr.Body)
	if tr.StatusCode != 200 || string(b) != "upstream api.search.example /q" {
		t.Fatalf("through the tunnel: %d %q", tr.StatusCode, b)
	}

	// A client that is no unit is refused.
	p.Resolve = fakeResolver{}
	resp, err = c.Get("http://plain.example/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("no unit: %d", resp.StatusCode)
	}
}

type advising struct{ fakeResolver }

func (advising) Advise(_ context.Context, u Unit, host string, port int) string {
	return fmt.Sprintf("add needs: {network: [x]} to %s's tool.yaml", u.Tool)
}

// The proxy's refusal names the line that would allow the host.
func TestRefusalNamesTheLine(t *testing.T) {
	p := &Proxy{Log: logr.Discard(), Resolve: advising{fakeResolver{"127.0.0.1": {{Host: "api.search.example"}}}}}
	srv := httptest.NewServer(p)
	defer srv.Close()
	proxyURL, _ := url.Parse(srv.URL)
	c := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}
	resp, err := c.Get("http://evil.example/")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(string(b), "not in tool search; to reach it, add needs: {network: [x]} to search's tool.yaml") {
		t.Fatalf("%d %q", resp.StatusCode, b)
	}
}
