package imageref

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The user an image runs as is read from its registry: through an index
// to its Linux manifest, then the config, with the pull token the
// registry asks for.
func TestImageUser(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			w.Write([]byte(`{"token":"t0k"}`))
			return
		}
		if r.Header.Get("Authorization") != "Bearer t0k" {
			w.Header().Set("WWW-Authenticate", `Bearer realm="`+srv.URL+`/token",service="reg",scope="repository:studio/tool:pull"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/v2/studio/tool/manifests/root", "/v2/studio/tool/manifests/c4":
			tag := strings.TrimPrefix(r.URL.Path, "/v2/studio/tool/manifests/")
			w.Write([]byte(`{"manifests":[{"digest":"sha256:win","platform":{"os":"windows"}},{"digest":"sha256:` + tag + `","platform":{"os":"linux","architecture":"amd64"}}]}`))
		case "/v2/studio/tool/manifests/sha256:root":
			w.Write([]byte(`{"config":{"digest":"sha256:cfgroot"}}`))
		case "/v2/studio/tool/manifests/sha256:c4":
			w.Write([]byte(`{"config":{"digest":"sha256:cfgc4"}}`))
		case "/v2/studio/tool/blobs/sha256:cfgroot":
			w.Write([]byte(`{"config":{"Env":["PATH=/usr/bin"]}}`))
		case "/v2/studio/tool/blobs/sha256:cfgc4":
			w.Write([]byte(`{"config":{"User":"65532:65532"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")
	z := &Resolver{Client: srv.Client()}
	for tag, want := range map[string]string{"root": "", "c4": "65532:65532"} {
		got, err := z.User(context.Background(), host+"/studio/tool:"+tag)
		if err != nil || got != want {
			t.Errorf("%s: %q %v, want %q", tag, got, err, want)
		}
		if RunsAsRoot(got) != (tag == "root") {
			t.Errorf("%s: RunsAsRoot(%q) = %v", tag, got, RunsAsRoot(got))
		}
	}
	if RunsAsRoot("0:0") != true || RunsAsRoot("root") != true || RunsAsRoot("1000") != false {
		t.Error("RunsAsRoot misreads a user")
	}
	if _, err := z.User(context.Background(), host+"/studio/tool:gone"); err == nil {
		t.Error("a missing image has a user")
	}
}
