package tier

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/Avalanche-io/c4"

	"github.com/Avalanche-io/c4-operator/internal/fill"
	"github.com/Avalanche-io/c4-operator/internal/storeurl"
)

func head(t *testing.T, h *harness, id c4.ID, cachedOnly bool) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodHead, h.http.URL+"/"+id.String(), nil)
	req.Header.Set(HeaderProject, "show")
	if cachedOnly {
		req.Header.Set(HeaderCached, "only")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// HEAD answers from the store behind the tier on a miss; with
// X-C4-Cached: only it says what the tier itself holds.
func TestHeadCachedOnly(t *testing.T) {
	data := randomBytes(1000)
	id := c4.Identify(bytes.NewReader(data))
	src := &fakeSource{name: "store", kind: "store", auth: true, data: data}
	h := newHarness(t, fill.Limits{}, func(string, c4.ID) []Source { return []Source{src} })
	shard, err := storeurl.OpenSharded([]storeurl.Spec{{Name: "s0", URL: t.TempDir()}}, storeurl.Credentials{})
	if err != nil {
		t.Fatal(err)
	}
	if err := shard.Put(context.Background(), id, bytes.NewReader(data), int64(len(data))); err != nil {
		t.Fatal(err)
	}
	h.srv.Store = func(string) (*storeurl.Sharded, error) { return shard, nil }
	if got := head(t, h, id, false); got != http.StatusOK {
		t.Fatalf("HEAD %d, want 200 from the store", got)
	}
	if got := head(t, h, id, true); got != http.StatusNotFound {
		t.Fatalf("cached-only HEAD %d before a fetch, want 404", got)
	}
	h.get(t, id, 0)
	if got := head(t, h, id, true); got != http.StatusOK {
		t.Fatalf("cached-only HEAD %d after a fetch, want 200", got)
	}
}

func warm(t *testing.T, h *harness, caller string, objs ...WarmObject) int {
	t.Helper()
	body, _ := json.Marshal(WarmRequest{Objects: objs})
	req, _ := http.NewRequest(http.MethodPost, h.http.URL+"/warm", bytes.NewReader(body))
	req.Header.Set(HeaderProject, "show")
	req.Header.Set("X-Test-Caller", caller)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// POST /warm fetches for the operator only, within a daily byte budget,
// and records every request.
func TestWarm(t *testing.T) {
	a, b := randomBytes(50), randomBytes(60)
	ida, idb := c4.Identify(bytes.NewReader(a)), c4.Identify(bytes.NewReader(b))
	srcs := map[c4.ID]*fakeSource{ida: {name: "store", kind: "store", auth: true, data: a}, idb: {name: "store", kind: "store", auth: true, data: b}}
	h := newHarness(t, fill.Limits{}, func(_ string, id c4.ID) []Source { return []Source{srcs[id]} })
	var mu sync.Mutex
	var recs []WarmRecord
	h.srv.Warm = &WarmPolicy{Callers: []string{"system:serviceaccount:c4-system:c4-manager"}, BytesPerDay: 100,
		Record: func(r WarmRecord) { mu.Lock(); recs = append(recs, r); mu.Unlock() }}
	objA := WarmObject{ID: ida.String(), Size: 50}

	// Without authentication the tier never warms.
	if got := warm(t, h, "anyone", objA); got != http.StatusForbidden {
		t.Fatalf("unauthenticated warm: %d", got)
	}
	h.srv.Authorize = func(r *http.Request) (string, int, error) {
		if c := r.Header.Get("X-Test-Caller"); c != "" {
			return c, 0, nil
		}
		return "", http.StatusUnauthorized, errors.New("no token")
	}
	if got := warm(t, h, "system:serviceaccount:show:render", objA); got != http.StatusForbidden {
		t.Fatalf("warm by another identity: %d", got)
	}
	if got := warm(t, h, "system:serviceaccount:c4-system:c4-manager", objA); got != http.StatusAccepted {
		t.Fatalf("operator warm: %d", got)
	}
	cache, _ := h.caches.Partition("p-show")
	for i := 0; !cache.Has(ida); i++ {
		if i > 200 {
			t.Fatal("warmed object never arrived")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// 50 of 100 bytes spent: the 60-byte object is over budget; the one
	// already held costs nothing.
	if got := warm(t, h, "system:serviceaccount:c4-system:c4-manager", objA, WarmObject{ID: idb.String(), Size: 60}); got != http.StatusTooManyRequests {
		t.Fatalf("over-budget warm: %d", got)
	}
	if got := warm(t, h, "system:serviceaccount:c4-system:c4-manager", objA); got != http.StatusAccepted {
		t.Fatalf("warm of a held object: %d", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(recs) != 5 || recs[0].Refused == "" || recs[1].Refused == "" || recs[2].Bytes != 50 || recs[2].Refused != "" ||
		recs[3].Refused == "" || recs[4].Bytes != 0 {
		t.Fatalf("records %+v", recs)
	}
}
