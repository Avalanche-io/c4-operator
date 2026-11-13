package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/Avalanche-io/c4-operator/internal/storeurl"
)

// A bucket listed over several pages is counted whole, every request is
// signed, and with delete every key is removed and nothing else.
func TestS3KeysListsEveryPageAndDeletes(t *testing.T) {
	var mu sync.Mutex
	keys := map[string]bool{}
	for i := range 7 {
		keys[fmt.Sprintf("c4/c4id%d", i)] = true
	}
	keys["other/kept"] = true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 ") {
			http.Error(w, "unsigned", http.StatusForbidden)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodGet:
			var all []string
			for k := range keys {
				if strings.HasPrefix(k, r.URL.Query().Get("prefix")) {
					all = append(all, k)
				}
			}
			sort.Strings(all)
			start := 0
			if tok := r.URL.Query().Get("continuation-token"); tok != "" {
				fmt.Sscan(tok, &start)
			}
			end := min(start+3, len(all)) // three a page
			fmt.Fprint(w, "<ListBucketResult>")
			for _, k := range all[start:end] {
				fmt.Fprintf(w, "<Contents><Key>%s</Key></Contents>", k)
			}
			if end < len(all) {
				fmt.Fprintf(w, "<IsTruncated>true</IsTruncated><NextContinuationToken>%d</NextContinuationToken>", end)
			}
			fmt.Fprint(w, "</ListBucketResult>")
		case http.MethodDelete:
			delete(keys, strings.TrimPrefix(r.URL.Path, "/b/"))
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer srv.Close()
	c := s3client{endpoint: srv.URL, bucket: "b", region: "us-east-1", http: srv.Client(),
		creds: storeurl.Credentials{AccessKeyID: "GKtest", SecretAccessKey: "secret"}}
	got, err := c.list(context.Background(), "c4/")
	if err != nil || len(got) != 7 {
		t.Fatalf("listed %d keys (%v), want 7", len(got), err)
	}
	for _, k := range got {
		if err := c.delete(context.Background(), k); err != nil {
			t.Fatal(err)
		}
	}
	if len(keys) != 1 || !keys["other/kept"] {
		t.Fatalf("left %v, want only the key outside the prefix", keys)
	}
}
