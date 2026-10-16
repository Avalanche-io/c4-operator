package view

import (
	"context"
	"math/rand/v2"
	"testing"

	"github.com/Avalanche-io/c4/store"
)

// TestChangedFilesMatchesTheFilesystem checks ChangedFiles against a
// brute-force comparison of the two views' file lists, after random edits
// to a tree on disk.
func TestChangedFilesMatchesTheFilesystem(t *testing.T) {
	for seed := range 8 {
		rng := rand.New(rand.NewPCG(uint64(seed), 11))
		dir := t.TempDir()
		writeTree(t, dir, show)
		base, err := ScanDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		ram := store.NewRAM()
		publish(t, ram, base)
		for step := range 6 {
			mutate(t, rng, dir, step)
			next, err := ScanDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			publish(t, ram, next)
			got, truncated, err := ChangedFiles(context.Background(), base.ID, next.ID, StoreRecords(ram), 0)
			if err != nil || truncated {
				t.Fatalf("seed %d step %d: %v (truncated %v)", seed, step, err, truncated)
			}
			want := bruteChanges(base, next)
			if len(got) != len(want) {
				t.Fatalf("seed %d step %d: %d changes, want %d\ngot  %v\nwant %v", seed, step, len(got), len(want), got, want)
			}
			for _, c := range got {
				if want[c.Path] != c.Op {
					t.Fatalf("seed %d step %d: %s is %s, want %q", seed, step, c.Path, c.Op, want[c.Path])
				}
			}
		}
	}
}

func TestChangedFilesLimit(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{"a.txt": "a"})
	from, _ := ScanDir(dir)
	writeTree(t, dir, show)
	to, _ := ScanDir(dir)
	ram := store.NewRAM()
	publish(t, ram, from)
	publish(t, ram, to)
	got, truncated, err := ChangedFiles(context.Background(), from.ID, to.ID, StoreRecords(ram), 3)
	if err != nil || !truncated || len(got) != 3 {
		t.Fatalf("limit 3: %d changes, truncated %v, %v", len(got), truncated, err)
	}
}

// bruteChanges compares two views' files (and symlinks) path by path.
func bruteChanges(a, b *View) map[string]string {
	type fe struct {
		id   string
		size int64
		exec bool
	}
	side := func(v *View) map[string]fe {
		m := map[string]fe{}
		for _, f := range v.Files {
			m[f.Path] = fe{f.ID.String(), f.Size, f.Exec}
		}
		for _, s := range v.Symlinks {
			m[s.Path] = fe{"->" + s.Target, 0, false}
		}
		return m
	}
	x, y := side(a), side(b)
	out := map[string]string{}
	for p, e := range x {
		f, ok := y[p]
		switch {
		case !ok:
			out[p] = "removed"
		case f != e:
			out[p] = "modified"
		}
	}
	for p := range y {
		if _, ok := x[p]; !ok {
			out[p] = "added"
		}
	}
	return out
}
