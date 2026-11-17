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
	Entry  string // the path of the entry that does not read back, when it can be told
	Reason string
}

func (e *NotReadBack) Error() string {
	if e.Entry == "" {
		return fmt.Sprintf("the c4m library writes a %s it does not read back as written (%s), so it is not stored", e.What, e.Reason)
	}
	return fmt.Sprintf("the c4m library writes the entry %q of a %s in a form it does not read back as written (%s), so it is not stored", e.Entry, e.What, e.Reason)
}

// readBack is applied to every entry the checks below read back, once the
// library has decoded it and the projection has taken it. It is the
// identity, always, unless a test swaps it in Go code through
// MisreadForTest: no environment variable, flag or build tag sets it, so no
// build of a release can. It exists only so that a test can stand in for a
// decoder that disagrees with the encoder, by altering one entry: the c4m
// library this package is built with has no entry known not to read back (a
// null-mode link to host:path was one until views kept a link's type).
// TestTheReadBackSeamIsForTestsOnly holds this.
var readBack = func(e *c4m.Entry) *c4m.Entry { return e }

// MisreadForTest makes the read-back checks read every entry named name
// with its link target altered, until restore is called: a decoder that
// disagrees with the encoder, for the tests of this package and of those
// that store what it writes (capture, ingest, the git import), which must
// still show they refuse such an entry and store nothing. Only test code
// calls it, in Go: no environment variable, flag or build tag reaches it,
// and outside a test readBack is the identity
// (TestTheReadBackSeamIsForTestsOnly). It is not safe while checks run.
func MisreadForTest(name string) (restore func()) {
	prev := readBack
	readBack = func(e *c4m.Entry) *c4m.Entry {
		if e.Name != name {
			return e
		}
		cp := *e
		cp.Target += "~misread"
		return &cp
	}
	return func() { readBack = prev }
}

// readBackRecord is ParseRecord as the checks read a record.
func readBackRecord(data []byte) ([]*c4m.Entry, error) {
	back, err := ParseRecord(data)
	if err != nil {
		return nil, err
	}
	for i, e := range back {
		back[i] = readBack(e)
	}
	return back, nil
}

// checkRecord reads a record as every reader does (ParseRecord) and
// requires it to write again to the same bytes. dir is the record's
// directory in its view ("" for the root, else ending in "/").
func checkRecord(data []byte, entries []*c4m.Entry, dir string) error {
	paths := make([]string, len(entries))
	for i, e := range entries {
		paths[i] = dir + e.Name
	}
	back, err := readBackRecord(data)
	if err != nil {
		return notReadBack("record", entries, paths, err.Error())
	}
	if again := listingOf(back).Canonical(); again != string(data) {
		w, r := firstDiff(string(data), again)
		return notReadBack("record", entries, paths, fmt.Sprintf("written as %q, read back as %q", w, r))
	}
	return nil
}

// checkChangeset reads a changeset as every reader does: ReadChain, then
// each entry as the patch appliers take it (Project). It requires one
// section over parent that writes again to the same bytes. The projection
// matters: an entry the library reads as another kind (a link read as a
// flow link) writes again unchanged as c4m text, and only the projection,
// which keeps what a view holds, shows what a reader takes it for.
func checkChangeset(parent c4.ID, data []byte, entries []*c4m.Entry) error {
	// An entry's path is the directories above it, by depth.
	paths := make([]string, len(entries))
	var stack []string
	for i, e := range entries {
		if e.Depth <= len(stack) {
			stack = stack[:e.Depth]
		}
		paths[i] = strings.Join(stack, "") + e.Name
		if e.IsDir() {
			stack = append(stack, strings.TrimSuffix(e.Name, "/")+"/")
		}
	}
	c, err := ReadChain(data)
	if err != nil {
		return notReadBack("changeset", entries, paths, err.Error())
	}
	if len(c.Sections) != 1 || c.Base != parent {
		return notReadBack("changeset", entries, paths, fmt.Sprintf("read back as %d sections over %s", len(c.Sections), c.Base))
	}
	read := c4m.NewManifest()
	for _, e := range c.Sections[0].Patch.Entries {
		read.Entries = append(read.Entries, readBack(Project(e)))
	}
	again, err := EncodeChangeset(c.Base, read)
	if err != nil {
		return notReadBack("changeset", entries, paths, err.Error())
	}
	if !bytes.Equal(again, data) {
		w, r := firstDiff(string(data), string(again))
		return notReadBack("changeset", entries, paths, fmt.Sprintf("written as %q, read back as %q", w, r))
	}
	return nil
}

// notReadBack names, where it can, the entry the library does not read
// back: each entry is written alone, as a one-level listing, and read back
// the way a record is read (ParseRecord, which projects what it reads);
// the first that does not come back as written is the one.
func notReadBack(what string, entries []*c4m.Entry, paths []string, reason string) error {
	for i, e := range entries {
		cp := *e
		cp.Depth = 0
		one := listingOf([]*c4m.Entry{&cp}).Canonical()
		back, err := readBackRecord([]byte(one))
		if err != nil {
			return &NotReadBack{What: what, Entry: paths[i], Reason: fmt.Sprintf("written as %q, read back: %v", strings.TrimSpace(one), err)}
		}
		if again := listingOf(back).Canonical(); again != one {
			return &NotReadBack{What: what, Entry: paths[i], Reason: fmt.Sprintf("written as %q, read back as %q", strings.TrimSpace(one), strings.TrimSpace(again))}
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
