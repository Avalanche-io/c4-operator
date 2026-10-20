package imageref

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

const dig = "sha256:5e1c0aa1c6dc0f7b8e1cf0b6a6e1b9fbd0b2f2e1a7f43d1a9e6c2b7f0d8a1c3e"

func TestParse(t *testing.T) {
	for in, want := range map[string]Ref{
		"busybox:1.37":    {Domain: "docker.io", Repository: "library/busybox", Tag: "1.37"},
		"c4/runtime-oiio": {Domain: "docker.io", Repository: "c4/runtime-oiio", Tag: "latest"},
		"ghcr.io/avalanche-io/c4-job-hydra:v0.1.0": {Domain: "ghcr.io", Repository: "avalanche-io/c4-job-hydra", Tag: "v0.1.0"},
		"localhost:5001/nuke:16.0":                 {Domain: "localhost:5001", Repository: "nuke", Tag: "16.0"},
		"registry.example:443/a/b@" + dig:          {Domain: "registry.example:443", Repository: "a/b", Digest: dig},
	} {
		got, err := Parse(in)
		if err != nil || got != want {
			t.Errorf("%s: %+v %v, want %+v", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "Busybox", "a b", "x@sha256:short"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}

// A registry that asks for a bearer token, as ghcr.io and Docker Hub do
// for anonymous pulls, then answers HEAD with the digest.
func TestPinAsksTheRegistry(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/token":
			if r.URL.Query().Get("scope") != "repository:studio/nuke:pull" {
				http.Error(w, "scope", http.StatusBadRequest)
				return
			}
			w.Write([]byte(`{"token":"t0k"}`))
		case r.URL.Path == "/v2/studio/nuke/manifests/16.0":
			if r.Header.Get("Authorization") != "Bearer t0k" {
				w.Header().Set("WWW-Authenticate", `Bearer realm="`+srv.URL+`/token",service="reg",scope="repository:studio/nuke:pull"`)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			if !strings.Contains(r.Header.Get("Accept"), "image.index") {
				http.Error(w, "accept", http.StatusBadRequest)
				return
			}
			w.Header().Set("Docker-Content-Digest", dig)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")
	z := &Resolver{Client: srv.Client()}
	got, err := z.Pin(context.Background(), host+"/studio/nuke:16.0")
	if err != nil || got != host+"/studio/nuke@"+dig {
		t.Fatalf("pinned %q %v", got, err)
	}
	if got, err := z.Pin(context.Background(), host+"/studio/nuke@"+dig); err != nil || got != host+"/studio/nuke@"+dig {
		t.Fatalf("already pinned: %q %v", got, err)
	}
	if _, err := z.Pin(context.Background(), host+"/studio/missing:1"); err == nil || !strings.Contains(err.Error(), "has no studio/missing:1") {
		t.Fatalf("unknown tag: %v", err)
	}
}

func TestNodeDigest(t *testing.T) {
	nodes := []corev1.Node{{Status: corev1.NodeStatus{Images: []corev1.ContainerImage{
		{Names: []string{"docker.io/library/busybox@" + dig, "docker.io/library/busybox:1.37"}},
		{Names: []string{"docker.io/library/alpine:3"}},
	}}}}
	if got, ok := NodeDigest(nodes, "busybox:1.37"); !ok || got != "docker.io/library/busybox@"+dig {
		t.Fatalf("busybox: %q %v", got, ok)
	}
	if _, ok := NodeDigest(nodes, "alpine:3"); ok {
		t.Fatal("alpine has no digest on the node")
	}
}
