package tier

import (
	"bytes"
	"fmt"
	"net/http"
	"testing"

	"github.com/Avalanche-io/c4"
	"github.com/Avalanche-io/c4-operator/internal/fill"
	"github.com/Avalanche-io/c4-operator/internal/storeurl"
)

// A project whose store the tier refuses for good (a shard on a local path)
// is answered 400, which a node's upload takes as a refusal and stops on,
// not 500, which it retries; any other failure to open the store stays 500.
func TestARefusedStoreIsNotRetried(t *testing.T) {
	h := newHarness(t, fill.Limits{}, nil)
	id := c4.Identify(bytes.NewReader([]byte("not cached")))
	for _, c := range []struct {
		err  error
		want int
	}{
		{fmt.Errorf("project show: shard s1 is the local path /var/c4: %w", storeurl.ErrLocalRefused), http.StatusBadRequest},
		{fmt.Errorf("project show store: connection refused"), http.StatusInternalServerError},
	} {
		h.srv.Store = func(string) (*storeurl.Sharded, error) { return nil, c.err }
		for _, req := range []*http.Request{
			mustRequest(t, http.MethodHead, h.http.URL+"/"+id.String(), nil),
			mustRequest(t, http.MethodPut, h.http.URL+"/", []byte("content")),
		} {
			req.Header.Set(HeaderProject, "show")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != c.want {
				t.Errorf("%s %s with the store failing as %q: %d, want %d", req.Method, req.URL.Path, c.err, resp.StatusCode, c.want)
			}
		}
	}
}

func mustRequest(t *testing.T, method, url string, body []byte) *http.Request {
	t.Helper()
	req, err := http.NewRequest(method, url, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	return req
}
