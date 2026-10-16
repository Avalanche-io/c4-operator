package view

import (
	"bytes"
	"context"
	"math/rand/v2"
	"os"
	"strings"
	"testing"

	"github.com/Avalanche-io/c4"
	"github.com/Avalanche-io/c4/c4m"
	"github.com/Avalanche-io/c4/store"
)

// buildChain makes steps random changes to a tree on disk, captures each
// as a changeset with Apply, appends each to a chain, and returns the
// chain, the views the filesystem actually had, and the store.
func buildChain(t *testing.T, seed uint64, steps int) ([]byte, []c4.ID, *store.RAM) {
	t.Helper()
	rng := rand.New(rand.NewPCG(seed, 3))
	dir := t.TempDir()
	writeTree(t, dir, show)
	base, err := ScanDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	ram := store.NewRAM()
	publish(t, ram, base)
	var chain []byte
	views := []c4.ID{base.ID}
	cur := base.ID
	for step := range steps {
		edits := mutate(t, rng, dir, step)
		applied, err := Apply(context.Background(), cur, edits, StoreRecords(ram))
		if err != nil {
			t.Fatal(err)
		}
		want, _ := ScanDir(dir)
		if applied.ID != want.ID {
			t.Fatalf("step %d: Apply %s, filesystem %s", step, applied.ID, want.ID)
		}
		if applied.ID == cur {
			continue
		}
		for _, r := range applied.Records {
			putBytes(t, ram, r.ID, r.Data)
		}
		if chain == nil {
			chain = applied.Changeset
		} else {
			chain, err = AppendSection(chain, applied.Changeset)
			if err != nil {
				t.Fatal(err)
			}
		}
		cur = applied.ID
		views = append(views, cur)
	}
	return chain, views, ram
}

func TestChainCheckpointsAreTheViews(t *testing.T) {
	for seed := range 10 {
		chain, views, ram := buildChain(t, uint64(seed), 12)
		if chain == nil {
			continue
		}
		ch, err := ReadChain(chain)
		if err != nil {
			t.Fatal(err)
		}
		if ch.Base != views[0] || ch.Head() != views[len(views)-1] || len(ch.Sections) != len(views)-1 {
			t.Fatalf("seed %d: chain base/head/sections do not match the history", seed)
		}
		got, err := ch.Verify(context.Background(), StoreRecords(ram))
		if err != nil {
			t.Fatalf("seed %d: %v\n%s", seed, err, chain)
		}
		for i, id := range got {
			if id != views[i+1] {
				t.Fatalf("seed %d: checkpoint %d is %s, the filesystem had %s", seed, i+1, id, views[i+1])
			}
		}
		// The c4 library reads the same chain as the same sections, so
		// c4 log and c4 split work on it.
		secs, err := c4m.DecodePatchChain(bytes.NewReader(chain))
		if err != nil || len(secs) != len(ch.Sections) {
			t.Fatalf("seed %d: c4m sees %d sections (%v), we see %d", seed, len(secs), err, len(ch.Sections))
		}
		// Stored, the chain resolves like any description: one view per
		// checkpoint, the head last.
		chainID := c4.Identify(bytes.NewReader(chain))
		putBytes(t, ram, chainID, chain)
		r, err := FromID(chainID, ram, Filter{})
		if err != nil {
			t.Fatalf("seed %d: resolve chain: %v", seed, err)
		}
		if r.View().ID != ch.Head() || r.Levels != len(views) {
			t.Fatalf("seed %d: resolved head %s with %d levels, want %s and %d", seed, r.View().ID, r.Levels, ch.Head(), len(views))
		}
		for i, v := range r.Chain {
			off := r.Levels - len(r.Chain)
			if v.ID != views[off+i] {
				t.Fatalf("seed %d: level %d is %s, want %s", seed, off+i, v.ID, views[off+i])
			}
		}
	}
}

func TestTamperedSectionKeepsTheVerifiedPrefix(t *testing.T) {
	chain, views, ram := buildChain(t, 99, 10)
	ch, err := ReadChain(chain)
	if err != nil || len(ch.Sections) < 4 {
		t.Fatalf("need a longer chain: %v", err)
	}
	// Change one entry's ID in the third section.
	target := ch.Sections[2].Patch.Entries[len(ch.Sections[2].Patch.Entries)-1].C4ID.String()
	forged := c4.Identify(strings.NewReader("forged")).String()
	idx := bytes.LastIndex(chain, []byte(target))
	tampered := append(append(append([]byte(nil), chain[:idx]...), forged...), chain[idx+len(target):]...)
	tch, err := ReadChain(tampered)
	if err != nil {
		t.Fatal(err)
	}
	got, err := tch.Verify(context.Background(), StoreRecords(ram))
	if err == nil {
		t.Fatal("a tampered section verified")
	}
	if len(got) < 1 || len(got) > len(views)-1 {
		t.Fatalf("verified prefix of %d checkpoints", len(got))
	}
	for i, id := range got {
		if id != views[i+1] {
			t.Fatal("the verified prefix is wrong")
		}
	}
}

func TestAppendRefusesAChangesetFromAnotherHead(t *testing.T) {
	chain, _, ram := buildChain(t, 7, 4)
	ch, _ := ReadChain(chain)
	other := Edit{Path: "elsewhere.txt", Entry: FileEntry("", 1, c4.Identify(strings.NewReader("e")), false)}
	applied, err := Apply(context.Background(), ch.Base, []Edit{other}, StoreRecords(ram))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := AppendSection(chain, applied.Changeset); err == nil {
		t.Fatal("appended a changeset whose parent is not the chain's head")
	}
}

// TestWriteChainForCLI writes a chain for checking with the c4 CLI:
// C4_CHAIN_OUT=/tmp/x.c4m go test -run TestWriteChainForCLI ./internal/view
func TestWriteChainForCLI(t *testing.T) {
	out := os.Getenv("C4_CHAIN_OUT")
	if out == "" {
		t.Skip("set C4_CHAIN_OUT")
	}
	chain, views, ram := buildChain(t, 5, 6)
	if err := os.WriteFile(out, chain, 0o644); err != nil {
		t.Fatal(err)
	}
	full, err := ExportChain(context.Background(), chain, StoreRecords(ram))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(out+".full", full, 0o644); err != nil {
		t.Fatal(err)
	}
	for i, v := range views {
		t.Logf("view %d %s", i, v)
	}
}
