package view

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/Avalanche-io/c4"
	"github.com/Avalanche-io/c4/c4m"
)

// NotReadBack is a record or changeset the c4m library wrote and does not
// read back as what it wrote. Nothing that holds one is stored: a reader
// of it would fail, or read other entries than were written, and every
// later read and import built on it would too. Checked, never trusted,
// applies to this package's own output as much as to anything it reads.
type NotReadBack struct {
	What   string // "record" or "changeset"
	Entry  string // the entry that does not read back, when it can be told
	Reason string
}

func (e *NotReadBack) Error() string {
	if e.Entry == "" {
		return fmt.Sprintf("the c4m library writes a %s it does not read back as written (%s), so it is not stored", e.What, e.Reason)
	}
	return fmt.Sprintf("the c4m library writes the entry %q of a %s in a form it does not read back as written (%s), so it is not stored", e.Entry, e.What, e.Reason)
}

// checkRecord reads a record as every reader does (ParseRecord) and
// requires it to write again to the same bytes.
func checkRecord(data []byte, entries []*c4m.Entry) error {
	back, err := ParseRecord(data)
	if err != nil {
		return notReadBack("record", entries, err.Error())
	}
	if again := listingOf(back).Canonical(); again != string(data) {
		w, r := firstDiff(string(data), again)
		return notReadBack("record", entries, fmt.Sprintf("written as %q, read back as %q", w, r))
	}
	return nil
}

// checkChangeset reads a changeset as every reader does (ReadChain) and
// requires one section over parent that writes again to the same bytes.
func checkChangeset(parent c4.ID, data []byte, entries []*c4m.Entry) error {
	c, err := ReadChain(data)
	if err != nil {
		return notReadBack("changeset", entries, err.Error())
	}
	if len(c.Sections) != 1 || c.Base != parent {
		return notReadBack("changeset", entries, fmt.Sprintf("read back as %d sections over %s", len(c.Sections), c.Base))
	}
	again, err := EncodeChangeset(c.Base, c.Sections[0].Patch)
	if err != nil {
		return notReadBack("changeset", entries, err.Error())
	}
	if !bytes.Equal(again, data) {
		w, r := firstDiff(string(data), string(again))
		return notReadBack("changeset", entries, fmt.Sprintf("written as %q, read back as %q", w, r))
	}
	return nil
}

// notReadBack names, where it can, the entry the library does not read
// back: each entry is written alone, as a one-level listing, and read back
// the way a record is read (ParseRecord, which projects what it reads);
// the first that does not come back as written is the one.
func notReadBack(what string, entries []*c4m.Entry, reason string) error {
	for _, e := range entries {
		cp := *e
		cp.Depth = 0
		one := listingOf([]*c4m.Entry{&cp}).Canonical()
		back, err := ParseRecord([]byte(one))
		if err != nil {
			return &NotReadBack{What: what, Entry: e.Name, Reason: fmt.Sprintf("written as %q, read back: %v", strings.TrimSpace(one), err)}
		}
		if again := listingOf(back).Canonical(); again != one {
			return &NotReadBack{What: what, Entry: e.Name, Reason: fmt.Sprintf("written as %q, read back as %q", strings.TrimSpace(one), strings.TrimSpace(again))}
		}
	}
	return &NotReadBack{What: what, Reason: reason}
}

// firstDiff returns the first line where two texts differ.
func firstDiff(a, b string) (string, string) {
	al, bl := strings.Split(a, "\n"), strings.Split(b, "\n")
	for i := range al {
		if i >= len(bl) || al[i] != bl[i] {
			if i >= len(bl) {
				return al[i], ""
			}
			return al[i], bl[i]
		}
	}
	if len(bl) > len(al) {
		return "", bl[len(al)]
	}
	return "", ""
}
