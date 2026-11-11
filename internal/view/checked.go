package view

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	"github.com/Avalanche-io/c4"
)

// ErrNotItsID is what Checked reports for an object whose bytes are not the
// C4 ID it was asked for: a store that serves them has lost or swapped it.
var ErrNotItsID = errors.New("the store served bytes that are not this ID's")

// Checked is st with every object it serves hashed first. One whose bytes
// are not its ID's is refused (ErrNotItsID), never passed on: an ID is
// checked, not trusted, and a store's own reads do not rehash.
func Checked(st Store) Store { return checkedStore{st} }

type checkedStore struct{ st Store }

func (c checkedStore) Open(id c4.ID) (io.ReadCloser, error) {
	data, err := ReadObject(c.st, id)
	if err != nil {
		return nil, err
	}
	if got := c4.Identify(bytes.NewReader(data)); got != id {
		return nil, fmt.Errorf("%s: %w (they are %s)", id, ErrNotItsID, got)
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}
