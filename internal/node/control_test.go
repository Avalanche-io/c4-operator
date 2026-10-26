package node

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"unicode/utf8"

	"github.com/Avalanche-io/c4"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
)

type memSink map[string][]byte

func (m memSink) PutFile(context.Context, string, c4.ID, string) error { return nil }
func (m memSink) PutBytes(_ context.Context, _ string, id c4.ID, data []byte) error {
	m[id.String()] = data
	return nil
}

func TestStoreControlDocs(t *testing.T) {
	upper := t.TempDir()
	sink := memSink{}
	got, err := storeControlDocs(context.Background(), sink, "show-a", upper)
	if err != nil || len(got) != 0 {
		t.Fatalf("no documents: %v %v", got, err)
	}
	doc := []byte(`{"version":1,"units":[{"name":"a"}]}`)
	os.MkdirAll(filepath.Join(upper, ".c4"), 0o755)
	os.WriteFile(filepath.Join(upper, ".c4", "expand.json"), doc, 0o644)
	got, err = storeControlDocs(context.Background(), sink, "show-a", upper)
	id := c4.Identify(bytes.NewReader(doc)).String()
	if err != nil || got[c4v1.AnnotationOutputExpand] != id || !bytes.Equal(sink[id], doc) {
		t.Fatalf("stored %v %v", got, err)
	}
}

// A unit's summary of its task (.c4/done) is stored as the farm keeps it,
// trimmed and with no one's home path, and named on the pod; an empty one
// names nothing.
func TestStoreSummary(t *testing.T) {
	upper := t.TempDir()
	sink := memSink{}
	os.MkdirAll(filepath.Join(upper, ".c4"), 0o755)
	os.WriteFile(filepath.Join(upper, ".c4", "done"), []byte("  \n"), 0o644)
	if got, err := storeControlDocs(context.Background(), sink, "show-a", upper); err != nil || got[c4v1.AnnotationOutputSummary] != "" {
		t.Fatalf("an empty summary: %v %v", got, err)
	}
	os.WriteFile(filepath.Join(upper, ".c4", "done"), []byte("Ported the parser; notes in /home/kai/notes.md\n"), 0o644)
	got, err := storeControlDocs(context.Background(), sink, "show-a", upper)
	want := []byte("Ported the parser; notes in ~/notes.md")
	id := c4.Identify(bytes.NewReader(want)).String()
	if err != nil || got[c4v1.AnnotationOutputSummary] != id || !bytes.Equal(sink[id], want) {
		t.Fatalf("stored %v %q %v", got, sink[id], err)
	}
}

// A summary is cut to MaxSummary at a character boundary.
func TestSummaryCut(t *testing.T) {
	long := bytes.Repeat([]byte("é"), MaxSummary) // two bytes each
	got := Summary(append([]byte("x"), long...))
	if len(got) > MaxSummary || !utf8.Valid(got) || len(got) < MaxSummary-1 {
		t.Fatalf("cut to %d bytes, valid %v", len(got), utf8.Valid(got))
	}
}
