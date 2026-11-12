package queue

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Avalanche-io/c4"
	"github.com/Avalanche-io/c4/c4m"

	"github.com/Avalanche-io/c4-operator/internal/storeurl"
	c4view "github.com/Avalanche-io/c4-operator/internal/view"
)

// What a tool's tool.yaml declares becomes its egress, so it is checked as
// it is read: bytes under its ID that are not that tool.yaml (here, one
// that asks for the internet) are not present, and give no network and
// nothing remembered. Once, they were read as they came.
func TestAToolsEgressComesOnlyFromItsOwnToolYAML(t *testing.T) {
	ctx := context.Background()
	st, dir := foldStore(t)
	yaml := []byte("c4: tool/1\nname: x\n")
	id, err := st.PutBytes(ctx, yaml)
	if err != nil {
		t.Fatal(err)
	}
	m := c4m.NewManifest()
	m.AddEntry(&c4m.Entry{Name: "tool.yaml", Size: int64(len(yaml)), Timestamp: c4m.NullTimestamp(), C4ID: id})
	b, err := c4m.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	all, _ := c4view.NewFilter(nil)
	res, err := c4view.FromBytes(b, st, all)
	if err != nil {
		t.Fatal(err)
	}
	v := res.View()
	recs, _ := v.Records()
	for _, r := range recs {
		if _, err := st.PutBytes(ctx, r.Data); err != nil {
			t.Fatal(err)
		}
	}
	q := &Queue{Stores: func(context.Context, string) (*storeurl.Sharded, error) { return st, nil }}
	if nets, err := q.declaredNetworks(ctx, "show", v.ID.String()); err != nil || len(nets) != 0 {
		t.Fatalf("its own tool.yaml: %v, %v", nets, err)
	}

	q = &Queue{Stores: func(context.Context, string) (*storeurl.Sharded, error) { return st, nil }}
	was := c4view.OnMismatch
	t.Cleanup(func() { c4view.OnMismatch = was })
	var told c4.ID
	c4view.OnMismatch = func(_ string, asked, _ c4.ID, _ error) { told = asked }
	overwriteObject(t, dir, id.String(), []byte("c4: tool/1\nname: x\nneeds: {network: [the-internet]}\n"))
	nets, err := q.declaredNetworks(ctx, "show", v.ID.String())
	if !errors.Is(err, c4view.ErrNotItsID) || len(nets) != 0 || told != id {
		t.Fatalf("another tool.yaml under its ID: networks %v, %v, told %s", nets, err, told)
	}
	if got, ok := q.toolNets[v.ID.String()]; ok {
		t.Fatalf("remembered %v for the tree", got)
	}
	if !strings.Contains(err.Error(), id.String()) {
		t.Errorf("the error does not name the object: %v", err)
	}
}
