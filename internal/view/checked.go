package view

import (
	"errors"
	"fmt"
	"io/fs"
	"log"

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
var ErrNotItsID = errors.New("the store served bytes that are not this ID's")

// notItsID is the error ReadObject returns for a mismatch.
type notItsID struct{ asked, got c4.ID }

func (e *notItsID) Error() string {
	return fmt.Sprintf("%s: %v (they are %s)", e.asked, ErrNotItsID, e.got)
}

func (e *notItsID) Is(target error) bool {
	switch target {
	case ErrNotItsID, ErrIncomplete, storeurl.ErrNotFound, fs.ErrNotExist:
		return true
	}
	return false
}

// OnMismatch is told of every read whose bytes were not the ID asked for:
// the ID asked for, the one the bytes are, and the store. It logs by
// default; the operator's main counts it too. It must not block.
var OnMismatch = func(store string, asked, got c4.ID) {
	log.Printf("c4: store %s served bytes that are not %s; they are %s", store, asked, got)
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
