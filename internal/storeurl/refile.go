package storeurl

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"os"
	"path/filepath"

	"github.com/Avalanche-io/c4"
)

// The re-file (Joshua, 2026-11-11, on a read whose bytes are not the ID
// asked for): "Not present, and rename the 'corrupted' data to its actual
// id (if this data is never referred to by anything then it should
// eventually get garbage collected)". The bytes go under their own ID, and
// come out from under the one they were stored under, which frees it for a
// correct Put: Head no longer says it is there. Nothing pins the re-filed
// object; ordinary GC reclaims it. Removing a key is the re-file's alone:
// no shard deletes for any other reason.

// ErrNotItsID is bytes that are not the C4 ID they were stored or asked
// under: never kept under it, and never served (view.ReadObject).
var ErrNotItsID = errors.New("the store served bytes that are not this ID's")

// Copy stores the object id from another store in this one, hashed as it
// streams: bytes that are not id's are refused at their end (ErrNotItsID),
// and no shard keeps them under id, since a Put whose reader fails stores
// nothing. The source's object is not re-filed here (the bytes stream past
// and are not kept); the next whole read of it re-files it.
func (s *Sharded) Copy(ctx context.Context, from *Sharded, id c4.ID, size int64) error {
	rc, err := from.Open(id)
	if err != nil {
		return err
	}
	defer rc.Close()
	return s.Put(ctx, id, &checking{r: rc, id: id, h: sha512.New()}, size)
}

// checking reads r, the bytes of id, hashing them as they pass (a C4 ID is
// their SHA-512, c4.Identify), and fails at their end, instead of ending,
// if they are not id's.
type checking struct {
	r  io.Reader
	id c4.ID
	h  hash.Hash
}

func (c *checking) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.h.Write(p[:n])
	if err == io.EOF {
		var got c4.ID
		copy(got[:], c.h.Sum(nil))
		if got != c.id {
			return n, fmt.Errorf("%s: %w (they are %s)", c.id, ErrNotItsID, got)
		}
	}
	return n, err
}

// ErrNoRefile is a shard that cannot take a wrong object out from under
// its ID: a c4d store, which computes the ID of what it is given and so
// never takes a wrong object, and repairs its own.
var ErrNoRefile = errors.New("this store does not re-file a wrong object: it repairs its own")

// refiler is a shard that can take a wrong object out from under its ID.
type refiler interface {
	// removeWrong removes the object stored under id, unless it is id's
	// own: what it holds is read again and hashed first, and kept if it is
	// id's (a correct Put since). An object not there is nothing to do.
	removeWrong(ctx context.Context, id c4.ID) error
}

// Refile takes data, read from under asked and found to be got's, out
// from under asked: written under got, unless an object there is got's
// already (then nothing is written), then removed from asked, unless what
// asked holds by then is asked's own. Done again, it does nothing more.
func (s *Sharded) Refile(ctx context.Context, asked, got c4.ID, data []byte) error {
	if c4.Identify(bytes.NewReader(data)) != got {
		return fmt.Errorf("re-file %s: the bytes are not %s", asked, got)
	}
	rf, ok := s.For(asked).(refiler)
	if !ok {
		return ErrNoRefile
	}
	if !s.holdsOwn(ctx, got) {
		if err := s.Put(ctx, got, bytes.NewReader(data), int64(len(data))); err != nil {
			return fmt.Errorf("re-file %s under %s: %w", asked, got, err)
		}
		// A store keeps what it has under an ID: if got held another wrong
		// object, the Put took nothing, and these bytes are got's only copy.
		if !s.holdsOwn(ctx, got) {
			return fmt.Errorf("re-file %s: %s holds other bytes too, so these stay where they are", asked, got)
		}
	}
	if err := rf.removeWrong(ctx, asked); err != nil {
		return fmt.Errorf("re-file %s: taking it from under its ID: %w", asked, err)
	}
	return nil
}

// holdsOwn reports whether the store holds id's own bytes under id.
func (s *Sharded) holdsOwn(ctx context.Context, id c4.ID) bool {
	rc, err := s.Open(id)
	if err != nil {
		return false
	}
	defer rc.Close()
	return c4.Identify(rc) == id
}

// removeWrong on a tree store renames the object's file aside first, which
// takes that file whatever is put at its path afterwards, then hashes what
// it took: id's own goes back (unless a new file is there by then, which
// stays), anything else is removed. An object still pending publication is
// left alone: it is being written.
func (s *treeShard) removeWrong(_ context.Context, id c4.ID) error {
	p, ok := s.ts.ContentPath(id)
	if !ok {
		return nil
	}
	if filepath.Base(p) != id.String() {
		return fmt.Errorf("%s is still being written", id)
	}
	var suffix [8]byte
	rand.Read(suffix[:])
	aside := p + ".refile." + hex.EncodeToString(suffix[:])
	if err := os.Rename(p, aside); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if refileTook != nil {
		refileTook(p)
	}
	f, err := os.Open(aside)
	if err != nil {
		return err
	}
	own := c4.Identify(f) == id
	f.Close()
	if own {
		// a correct object, put since the read: it goes back where it was,
		// unless another has been put there meanwhile, which stays
		if err := os.Link(aside, p); err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
	}
	return os.Remove(aside)
}

// refileTook, in tests, runs after a tree store has taken an object's file
// aside and before it hashes it.
var refileTook func(path string)

// removeWrong on S3 reads the object and its ETag, hashes it, and deletes
// it only if it is not id's, on that ETag (If-Match): an object put since
// the read has another, and is not deleted. A store that does not honour
// If-Match on a DELETE falls back to the read just before it.
func (s *s3Shard) removeWrong(ctx context.Context, id c4.ID) error {
	resp, err := s.do(ctx, http.MethodGet, id, nil)
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusNotFound {
		resp.Body.Close()
		return nil
	}
	if resp.StatusCode != http.StatusOK {
		return s3Error(resp, "get", id)
	}
	own := c4.Identify(resp.Body) == id
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	etag := resp.Header.Get("ETag")
	if own {
		return nil
	}
	if etag == "" {
		return fmt.Errorf("s3 get %s gave no ETag to delete on", id)
	}
	del, err := s.do(ctx, http.MethodDelete, id, http.Header{"If-Match": {etag}})
	if err != nil {
		return err
	}
	defer del.Body.Close()
	switch del.StatusCode {
	case http.StatusNoContent, http.StatusOK, http.StatusNotFound:
		return nil
	case http.StatusPreconditionFailed:
		// another object under id since the read: not this one to delete
		return nil
	}
	return s3Error(del, "delete", id)
}
