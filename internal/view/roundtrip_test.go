package view

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Avalanche-io/c4"
	"github.com/Avalanche-io/c4/c4m"

	"github.com/Avalanche-io/c4/store"
)

// A record or changeset the c4m library writes and does not read back as
// written is never returned to be stored: Apply refuses the edits with an
// error that names the entry. The library this package is built with
// writes a symlink whose target starts with "c4" (and whose mode, as in
// every view, is null) in a form it reads as a hard link: the target
// taken for an ID. Any other disagreement between the encoder and the
// decoder is caught the same way.
func TestApplyRefusesWhatDoesNotReadBack(t *testing.T) {
	ctx := context.Background()
	ram := store.NewRAM()
	src := StoreRecords(ram)
	putBytes(t, ram, EmptyID(), nil)
	idText := c4.Identify(strings.NewReader("some content")).String()
	ok := []string{"a.txt", "../objects/" + idText, "studio/c4"}
	refused := []string{idText, idText + ".exr", "c4studio"}

	for _, target := range ok {
		if _, err := Apply(ctx, EmptyID(), []Edit{{Path: "d/link", Entry: SymlinkEntry("", target)}}, src); err != nil {
			t.Errorf("a link to %q: %v", target, err)
		}
	}
	for _, target := range refused {
		res, err := Apply(ctx, EmptyID(), []Edit{fileEdit("keep.txt", "keep"), {Path: "d/link", Entry: SymlinkEntry("", target)}}, src)
		var nr *NotReadBack
		if !errors.As(err, &nr) {
			// Until the operator is on a c4m library with the decoder fixed
			// (c4 PR #92), this library does not read these back. On one
			// that does, nothing is refused, and this case is to go.
			t.Fatalf("a link to %q was not refused: %v", target, err)
		}
		if res != nil || nr.Entry != "link" || nr.What != "record" {
			t.Errorf("a link to %q: refused as %+v, with a result %v", target, nr, res != nil)
		}
		if !strings.Contains(err.Error(), `entry "link"`) || !strings.Contains(err.Error(), "so it is not stored") {
			t.Errorf("a link to %q: %v", target, err)
		}
	}
	// What is checked is what was written: a record that does not come
	// back the same is caught even when every entry, written alone, does.
	if err := checkRecord([]byte("not a record\n"), nil); err == nil {
		t.Fatal("a record that does not parse was taken")
	}
	var nr *NotReadBack
	if err := checkChangeset(EmptyID(), []byte(EmptyID().String()+"\n"), nil); !errors.As(err, &nr) || nr.Entry != "" {
		t.Fatalf("a changeset with no section: %v", err)
	}
}

// A link whose target reads as a location (`host:path`, `C:foo`) is, under
// a null mode, read back by the library as a flow link: not as written, so
// it is refused too, until Joshua rules on that shape.
func TestApplyRefusesALinkThatReadsAsAFlowLink(t *testing.T) {
	ctx := context.Background()
	ram := store.NewRAM()
	src := StoreRecords(ram)
	putBytes(t, ram, EmptyID(), nil)
	for _, target := range []string{"host:path", "C:foo", "nas:plates/"} {
		_, err := Apply(ctx, EmptyID(), []Edit{{Path: "link", Entry: SymlinkEntry("", target)}}, src)
		var nr *NotReadBack
		if !errors.As(err, &nr) || nr.Entry != "link" {
			t.Errorf("a link to %q: %v", target, err)
		} else {
			t.Logf("a link to %q: %v", target, err)
		}
	}
}

// The changeset is checked as its readers read it: one section, over the
// parent it was written over, that writes again to the same bytes. A link
// the library cannot read back is named in a changeset as in a record.
func TestAChangesetIsCheckedAsItIsRead(t *testing.T) {
	parent := EmptyID()
	ok := c4m.NewManifest()
	ok.Entries = []*c4m.Entry{SymlinkEntry("link", "a.txt")}
	good, err := EncodeChangeset(parent, ok)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkChangeset(parent, good, ok.Entries); err != nil {
		t.Fatalf("a changeset that reads back: %v", err)
	}
	var nr *NotReadBack
	other := c4.Identify(strings.NewReader("another view"))
	if err := checkChangeset(other, good, ok.Entries); !errors.As(err, &nr) || !strings.Contains(err.Error(), "sections over") {
		t.Fatalf("a changeset over another parent than the one it was written over: %v", err)
	}
	bad := c4m.NewManifest()
	bad.Entries = []*c4m.Entry{SymlinkEntry("link", "host:path")}
	text, err := EncodeChangeset(parent, bad)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkChangeset(parent, text, bad.Entries); !errors.As(err, &nr) || nr.What != "changeset" || nr.Entry != "link" {
		t.Fatalf("a changeset the library does not read back as written: %v", err)
	}
}
