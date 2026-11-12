package view

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"sync"
	"time"

	"github.com/Avalanche-io/c4"

	"github.com/Avalanche-io/c4-operator/internal/storeurl"
)

// ErrNotItsID is a read whose bytes are not the C4 ID asked for: a store
// that serves them has lost or swapped the object. Such an object is not
// present (Joshua, 2026-11-11): a ReadObject that finds one returns an error
// that is also ErrIncomplete, storeurl.ErrNotFound and fs.ErrNotExist, so
// every caller degrades as it does for a missing object, and the wrong
// bytes are never passed on. An ID is checked, never trusted, and a store's
// own reads do not rehash.
var ErrNotItsID = storeurl.ErrNotItsID

// ErrRefiled is a wrong object the store has re-filed under its own ID
// (Refiler): the ID asked for no longer holds it, and a correct Put can
// store the right bytes there.
var ErrRefiled = errors.New("re-filed under its own ID")

// notItsID is the error ReadObject returns for a mismatch, and how its
// re-file went.
type notItsID struct {
	asked, got c4.ID
	refiled    error
}

func (e *notItsID) Error() string {
	if e.refiled == nil {
		return fmt.Sprintf("%s: %v (they are %s, and are under that ID now)", e.asked, ErrNotItsID, e.got)
	}
	return fmt.Sprintf("%s: %v (they are %s; not re-filed: %v)", e.asked, ErrNotItsID, e.got, e.refiled)
}

func (e *notItsID) Is(target error) bool {
	switch target {
	case ErrNotItsID, ErrIncomplete, storeurl.ErrNotFound, fs.ErrNotExist:
		return true
	case ErrRefiled:
		return e.refiled == nil
	}
	return false
}

// OnMismatch is told of every read whose bytes were not the ID asked for:
// the store, the ID asked for, the one the bytes are, and how the re-file
// went (nil: the bytes are under their own ID now, and gone from under the
// one asked for). It logs by default; the operator's main counts it too.
// It must not block.
var OnMismatch = func(store string, asked, got c4.ID, refiled error) {
	log.Printf("c4: store %s served bytes that are not %s; they are %s (re-filed: %v)", store, asked, got, refiled)
}

// Refiler is a store that can re-file a wrong object: put the bytes under
// their own ID, and take them from under the one they were stored under
// (storeurl.Sharded.Refile).
type Refiler interface {
	Refile(ctx context.Context, asked, got c4.ID, data []byte) error
}

// errNoRefile is the re-file of a store that cannot.
var errNoRefile = errors.New("this store does not re-file")

// refileTimeout bounds a re-file, which a read waits on.
const refileTimeout = 30 * time.Second

// mismatch re-files a wrong object through st, if st can, tells
// OnMismatch, and answers the error the read returns. An object read again
// within refileEvery of a re-file that could not be done is not re-filed
// or told again: every such read is still not present, but a wrong object
// a re-file cannot move (a c4d's, or two objects under each other's IDs)
// does not make every read a re-file and a log line.
func mismatch(st Store, asked, got c4.ID, data []byte) error {
	if recentlyTried(asked) {
		return &notItsID{asked: asked, got: got, refiled: errRecentlyTried}
	}
	err := errNoRefile
	if rf, ok := st.(Refiler); ok {
		ctx, cancel := context.WithTimeout(context.Background(), refileTimeout)
		err = rf.Refile(ctx, asked, got, data)
		cancel()
	}
	if err != nil {
		noteTried(asked)
	}
	OnMismatch(storeName(st, asked), asked, got, err)
	return &notItsID{asked: asked, got: got, refiled: err}
}

// refileEvery is how long a re-file that could not be done is not tried
// again for the same object.
const refileEvery = time.Minute

var errRecentlyTried = errors.New("not re-filed again yet: a re-file of it could not be done within the last minute")

var (
	triedMu sync.Mutex
	tried   = map[c4.ID]time.Time{}
)

func recentlyTried(id c4.ID) bool {
	triedMu.Lock()
	defer triedMu.Unlock()
	at, ok := tried[id]
	return ok && time.Since(at) < refileEvery
}

func noteTried(id c4.ID) {
	triedMu.Lock()
	defer triedMu.Unlock()
	if len(tried) >= 4096 {
		for k, at := range tried {
			if time.Since(at) >= refileEvery {
				delete(tried, k)
			}
		}
	}
	tried[id] = time.Now()
}

// storeName names the store id was read from, for OnMismatch: the shard
// that holds it, on a sharded store.
func storeName(st Store, id c4.ID) string {
	switch s := st.(type) {
	case *storeurl.Sharded:
		return s.NameFor(id)
	case interface{ Name() string }:
		return s.Name()
	}
	return fmt.Sprintf("%T", st)
}
