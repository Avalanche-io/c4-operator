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
	// removeWrong removes the object stored under id if it is still got's
	// bytes, the ones just re-filed under got: what id holds is read again
	// and hashed first, and anything else (id's own, a correct Put since;
	// or other bytes, which may be their only copy) is left as it is. An
	// object not there is nothing to do.
	removeWrong(ctx context.Context, id, got c4.ID) error
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
	if err := rf.removeWrong(ctx, asked, got); err != nil {
		return fmt.Errorf("re-file %s: taking it from under its ID: %w", asked, err)
	}
	return nil
}

// holdsOwn reports whether the store holds id's own bytes under id.
func (s *Sharded) holdsOwn(ctx context.Context, id c4.ID) bool {
	rc, _, _, err := s.For(id).Get(ctx, id, 0)
	if err != nil {
		return false
	}
	defer rc.Close()
	return c4.Identify(rc) == id
}

// removeWrong on a tree store renames the object's file aside first,
// which takes that file whatever is put at its path afterwards, into the
// store's root under a dot name (the tree store takes it for a temp file,
// so a split never moves it), then hashes what it took: got's bytes, the
// ones re-filed, are removed; anything else goes back under id through the
// store's own Create, which finds id's path as it is now and keeps an
// object put there since. An object still pending publication is left
// alone: it is being written. sweepRefiles mends what a crash leaves.
func (s *treeShard) removeWrong(_ context.Context, id, got c4.ID) error {
	p, ok := s.ts.ContentPath(id)
	if !ok {
		return nil
	}
	if filepath.Base(p) != id.String() {
		return fmt.Errorf("%s is still being written", id)
	}
	var suffix [8]byte
	rand.Read(suffix[:])
	aside := filepath.Join(s.ts.Root(), refileAside+id.String()+"."+hex.EncodeToString(suffix[:]))
	if err := os.Rename(p, aside); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if refileTook != nil {
		refileTook(p)
	}
	return s.settleAside(aside, id, got)
}

// refileAside starts the name of a file a re-file has taken aside.
const refileAside = ".refile."

// settleAside removes a file taken aside from under id if it is got's
// bytes, and puts anything else back under id.
func (s *treeShard) settleAside(aside string, id, got c4.ID) error {
	f, err := os.Open(aside)
	if err != nil {
		return err
	}
	held := c4.Identify(f)
	f.Close()
	if held == got && got != id {
		return os.Remove(aside)
	}
	f, err = os.Open(aside)
	if err != nil {
		return err
	}
	defer f.Close()
	w, err := s.ts.Create(id)
	if errors.Is(err, os.ErrExist) {
		// another object under id since: it stays, and this one, which is
		// not got's, was id's own or under id before it
		return os.Remove(aside)
	}
	if err != nil {
		return err
	}
	if _, err := io.Copy(w, f); err != nil {
		w.Close()
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return os.Remove(aside)
}

// sweepRefiles mends what a re-file a crash cut short left in the store's
// root: a file taken aside that is the ID it was taken from goes back under
// it; any other was got's, filed under got before it was taken, and goes.
func (s *treeShard) sweepRefiles() {
	entries, _ := os.ReadDir(s.ts.Root())
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || len(name) <= len(refileAside) || name[:len(refileAside)] != refileAside {
			continue
		}
		rest := name[len(refileAside):]
		dot := len(rest) - 17 // "." and 16 hex digits
		if dot <= 0 || rest[dot] != '.' {
			continue
		}
		id, err := c4.Parse(rest[:dot])
		if err != nil {
			continue
		}
		aside := filepath.Join(s.ts.Root(), name)
		f, err := os.Open(aside)
		if err != nil {
			continue
		}
		held := c4.Identify(f)
		f.Close()
		got := held
		if held == id {
			got = c4.ID{} // id's own: back under it
		}
		s.settleAside(aside, id, got)
	}
}

// refileTook, in tests, runs after a tree store has taken an object's file
// aside and before it hashes it.
var refileTook func(path string)

// removeWrong on S3 reads the object and its ETag, hashes it, and deletes
// it only if it is got's bytes (the ones re-filed), on that ETag
// (If-Match): an object put since the read has another, and is not
// deleted. AWS honours If-Match on a DELETE (general-purpose buckets since
// 2025-09); a store that ignores it deletes whatever is there by then,
// which leaves id absent, never wrong.
func (s *s3Shard) removeWrong(ctx context.Context, id, got c4.ID) error {
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
	held := c4.Identify(resp.Body)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	etag := resp.Header.Get("ETag")
	if held != got || got == id {
		// id's own (a correct Put since), or other bytes, which may be
		// their only copy: not the ones re-filed, so not deleted
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
