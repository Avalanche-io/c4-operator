package storeurl

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Avalanche-io/c4"
)

// Every shard publishes only bytes that are the ID's. On S3 a single PUT
// is not sent, and a multipart upload is not completed (it is aborted),
// when the bytes read are not the ID's; an object already under the ID is
// not replaced.
func TestS3PutPublishesOnlyItsBytes(t *testing.T) {
	defer func(p int64) { minPartSize = p }(minPartSize)
	minPartSize = 5
	ctx := context.Background()
	for _, n := range []int{4, 23} { // one PUT; five parts
		for _, c := range []struct {
			size int64
			last bool // only the last byte differs
		}{{int64(n), false}, {-1, false}, {int64(n), true}, {-1, true}} {
			size, last := c.size, c.last
			bucket, sh := testBucket(t)
			right := bytes.Repeat([]byte("abcdefg"), 4)[:n]
			wrong := bytes.Repeat([]byte("gfedcba"), 4)[:n]
			if last {
				wrong = append(bytes.Clone(right[:n-1]), right[n-1]^1)
			}
			id := c4.Identify(bytes.NewReader(right))
			key := "p/" + id.String()

			err := sh.Put(ctx, id, bytes.NewReader(wrong), size)
			if !errors.Is(err, ErrNotItsID) {
				t.Fatalf("%d bytes (size %d), wrong: %v, want ErrNotItsID", n, size, err)
			}
			if _, ok := bucket.objects[key]; ok || len(bucket.objects) != 0 {
				t.Fatalf("%d bytes (size %d): other bytes published: %d objects", n, size, len(bucket.objects))
			}
			if len(bucket.uploads) != 0 {
				t.Fatalf("%d bytes (size %d): %d uploads left open", n, size, len(bucket.uploads))
			}
			if n > int(minPartSize) && bucket.aborted != 1 {
				t.Fatalf("%d bytes (size %d): %d uploads aborted, want 1", n, size, bucket.aborted)
			}

			if err := sh.Put(ctx, id, bytes.NewReader(right), size); err != nil {
				t.Fatalf("%d bytes (size %d), right: %v", n, size, err)
			}
			if err := sh.Put(ctx, id, bytes.NewReader(wrong), size); !errors.Is(err, ErrNotItsID) {
				t.Fatalf("%d bytes (size %d), wrong over right: %v", n, size, err)
			}
			if got := bucket.objects[key]; !bytes.Equal(got, right) {
				t.Fatalf("%d bytes (size %d): the object under its ID is now %q", n, size, got)
			}
		}
	}
}

// A c4d computes the ID itself; a reply naming another is ErrNotItsID, as
// on every other shard.
func TestC4dPutOfOtherBytesIsNotItsID(t *testing.T) {
	right, wrong := []byte("the record"), []byte("not the record")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, c4.Identify(r.Body).String()+"\n")
	}))
	defer srv.Close()
	sh := newC4d(srv.URL)
	id := c4.Identify(bytes.NewReader(right))
	if err := sh.Put(context.Background(), id, bytes.NewReader(right), int64(len(right))); err != nil {
		t.Fatal(err)
	}
	if err := sh.Put(context.Background(), id, bytes.NewReader(wrong), int64(len(wrong))); !errors.Is(err, ErrNotItsID) {
		t.Fatalf("other bytes: %v, want ErrNotItsID", err)
	}
}

// A tree shard that already holds an ID hashes what a Put of it is given:
// other bytes are ErrNotItsID, as on S3, and the object stays.
func TestTreePutOverItsIDChecksTheBytes(t *testing.T) {
	ctx := context.Background()
	right, wrong := []byte("the record"), []byte("not the record")
	id := c4.Identify(bytes.NewReader(right))
	s, _ := treeWith(t, c4.Identify(bytes.NewReader([]byte("unrelated"))), []byte("unrelated"))
	if err := s.Put(ctx, id, bytes.NewReader(right), int64(len(right))); err != nil {
		t.Fatal(err)
	}
	if err := s.Put(ctx, id, bytes.NewReader(right), int64(len(right))); err != nil {
		t.Fatalf("the same bytes again: %v", err)
	}
	if err := s.Put(ctx, id, bytes.NewReader(wrong), int64(len(wrong))); !errors.Is(err, ErrNotItsID) {
		t.Fatalf("other bytes over a held ID: %v, want ErrNotItsID", err)
	}
	if b, ok := read(t, s, id); !ok || !bytes.Equal(b, right) {
		t.Fatalf("the object under its ID is now %q (%v)", b, ok)
	}
}
