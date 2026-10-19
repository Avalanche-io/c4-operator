package node

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

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
