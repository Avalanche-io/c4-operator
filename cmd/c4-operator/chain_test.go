package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Avalanche-io/c4"

	"github.com/Avalanche-io/c4-operator/internal/gitimport"
	"github.com/Avalanche-io/c4-operator/internal/view"
)

// importedViews returns the views of the mapping's commit lines that
// changed their view, in order: the checkpoints a ref's chain must hold.
func importedViews(t *testing.T, mapping string) ([]gitimport.Commit, []c4.ID) {
	t.Helper()
	m, err := gitimport.ReadMapping(mapping)
	if err != nil {
		t.Fatal(err)
	}
	var commits []gitimport.Commit
	var views []c4.ID
	for _, c := range m {
		if c.Commit == "" || c.RefOnly || c.Changeset.IsNil() {
			continue
		}
		commits = append(commits, c)
		views = append(views, c.View)
	}
	return commits, views
}

func postReceive(t *testing.T, r *pushRig, old, new, ref string) {
	t.Helper()
	var out bytes.Buffer
	if err := r.svc.postReceive(context.Background(), strings.NewReader(old+" "+new+" "+ref+"\n"), &out); err != nil {
		t.Fatal(err)
	}
}

func TestBranchChainFollowsPushes(t *testing.T) {
	r := newPushRig(t)
	ctx := context.Background()
	zero := strings.Repeat("0", 40)
	prev := zero
	for i, files := range []map[string]string{
		{"a.txt": "a\n", "src/x.go": "package x\n"},
		{"src/x.go": "package x // 2\n", "docs/d.md": "d\n"},
		{"docs/d.md": "d2\n"},
	} {
		c := r.commit(files)
		if i == 1 {
			r.git(r.work, "commit", "-q", "--allow-empty", "-m", "empty")
			c = r.git(r.work, "rev-parse", "HEAD")
		}
		r.git(r.work, "push", "-q", "origin", "main")
		postReceive(t, r, prev, c, "refs/heads/main")
		prev = c
	}
	commits, views := importedViews(t, r.svc.out)
	data, err := os.ReadFile(r.svc.chainPath("refs/heads/main"))
	if err != nil {
		t.Fatal(err)
	}
	ch, err := view.ReadChain(data)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ch.Verify(ctx, view.StoreRecords(r.st))
	if err != nil {
		t.Fatal(err)
	}
	if !equalIDs(got, views) || ch.Base != view.EmptyID() {
		t.Fatalf("chain checkpoints %d, imported views %d", len(got), len(views))
	}
	// The index: each commit's chain position, and the stored changesets
	// concatenate into the same chain.
	var segs []byte
	for i, c := range commits {
		if c.ChainSeq != i+1 || c.ChainBase != ch.Base {
			t.Fatalf("commit %d indexed at %d of %s", i, c.ChainSeq, c.ChainBase)
		}
		cs, err := view.ReadObject(r.st, c.Changeset)
		if err != nil {
			t.Fatal(err)
		}
		segs = append(segs, cs...)
	}
	fromSegs, err := view.ReadChain(segs)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := fromSegs.Verify(ctx, view.StoreRecords(r.st)); !equalIDs(got, views) {
		t.Fatal("stored changesets do not concatenate into the chain")
	}

	// A force push off the chain's head seals the chain (stored whole and
	// recorded) and starts a new one from the new commit's parent.
	r.git(r.work, "reset", "-q", "--hard", "HEAD~2")
	forked := r.commit(map[string]string{"fork.txt": "f\n"})
	r.git(r.work, "push", "-q", "-f", "origin", "main")
	postReceive(t, r, prev, forked, "refs/heads/main")
	m, _ := gitimport.ReadMapping(r.svc.out)
	var seal *gitimport.Commit
	for i := range m {
		if m[i].ChainRef == "refs/heads/main" {
			seal = &m[i]
		}
	}
	if seal == nil || seal.ChainSections != len(views) || seal.View != views[len(views)-1] {
		t.Fatalf("no seal line for the old chain: %+v", seal)
	}
	stored, err := view.ReadObject(r.st, seal.Chain)
	if err != nil || !bytes.Equal(stored, data) {
		t.Fatalf("sealed chain object: %v", err)
	}
	now, _ := gitimport.OpenChainFile(r.svc.chainPath("refs/heads/main"))
	last := m[len(m)-1]
	if now.Sections != 1 || now.Head != last.View || now.Base != last.ParentView || now.Base == ch.Base {
		t.Fatalf("new chain: %+v, last commit %+v", now, last)
	}
}

func TestChainMaxSealsAndRebases(t *testing.T) {
	r := newPushRig(t)
	r.svc.chainMax = 2
	for i := range 5 {
		r.commit(map[string]string{"f.txt": strings.Repeat("x", i+1)})
	}
	r.git(r.work, "push", "-q", "origin", "main")
	res, err := r.svc.importRef(context.Background(), "refs/heads/main", "", false)
	if err != nil {
		t.Fatal(err)
	}
	m, _ := gitimport.ReadMapping(r.svc.out)
	seals := 0
	for i, c := range m {
		if c.ChainRef == "" {
			continue
		}
		seals++
		// A seal line follows the line of the commit that filled the chain.
		if prev := m[i-1]; prev.View != c.View || prev.ChainSeq != 2 {
			t.Fatalf("seal %d after %+v", seals, prev)
		}
	}
	if seals != 2 || res.ChainSections != 1 {
		t.Fatalf("%d seals, %d sections left", seals, res.ChainSections)
	}
}

// TestChainsReadByC4CLI checks the c4 command reads the chains as they are.
func TestChainsReadByC4CLI(t *testing.T) {
	c4bin, err := exec.LookPath("c4")
	if err != nil {
		t.Skip("c4 is not on PATH")
	}
	r := newPushRig(t)
	// A symlink beside a file in a subdirectory: the case where a view's
	// records once disagreed with c4m's nested ID.
	os.MkdirAll(filepath.Join(r.work, "tools"), 0o755)
	os.WriteFile(filepath.Join(r.work, "tools", "build.sh"), []byte("#!/bin/sh\n"), 0o755)
	if err := os.Symlink("build.sh", filepath.Join(r.work, "tools", "b")); err != nil {
		t.Fatal(err)
	}
	for i := range 4 {
		r.commit(map[string]string{"src/f.go": strings.Repeat("y", i+1), "n" + string(rune('a'+i)) + ".txt": "n"})
	}
	r.git(r.work, "push", "-q", "origin", "main")
	r.svc.storeChain = true
	if _, err := r.svc.importRef(context.Background(), "refs/heads/main", "", false); err != nil {
		t.Fatal(err)
	}
	_, views := importedViews(t, r.svc.out)
	chainFile := r.svc.chainPath("refs/heads/main")

	// From the root commit the chain's base is the empty view, and c4 log
	// lists every commit's view.
	if got := c4LogIDs(t, c4bin, chainFile); !equalIDs(got, views) {
		t.Fatalf("c4 log: %d versions, want %d", len(got), len(views))
	}
	dir := t.TempDir()
	before, after := filepath.Join(dir, "before.c4m"), filepath.Join(dir, "after.c4m")
	run(t, c4bin, "split", chainFile, "2", before, after)
	tail, _ := os.ReadFile(after)
	if !strings.HasPrefix(string(tail), views[1].String()+"\n") {
		t.Fatalf("c4 split: after starts %q", firstLine(tail))
	}
	// The head is the full tree at version 2; the tail is a chain over it.
	// (c4 1.0.18's log cannot resolve the tail's external base; DESIGN.md
	// 2.3 records that as a proposed c4 change.)
	head, _ := os.ReadFile(before)
	if _, err := view.Parse(head, views[1]); err != nil {
		t.Fatalf("c4 split: before is not version 2: %v", err)
	}
	tch, err := view.ReadChain(tail)
	if err != nil {
		t.Fatal(err)
	}
	// c4 1.0.18's split drops the closing checkpoint of the last section,
	// so the tail's sealed sections verify and its last patch, unsealed,
	// still produces the head view.
	src := view.StoreRecords(r.st)
	got, err := tch.Verify(context.Background(), src)
	if err != nil || !equalIDs(got, views[2:len(views)-1]) {
		t.Fatalf("c4 split: tail verifies to %d views: %v", len(got), err)
	}
	lastSec := tch.Sections[len(tch.Sections)-1]
	applied, err := view.ApplyPatch(context.Background(), views[len(views)-2], lastSec.Patch, src)
	if err != nil || applied.ID != views[len(views)-1] {
		t.Fatalf("c4 split: tail's last patch does not produce the head view: %v", err)
	}

	// A chain over an external base (a branch from an imported commit)
	// is exported with its base in full for c4.
	r.git(r.work, "checkout", "-q", "-b", "feature", "HEAD~1")
	r.commit(map[string]string{"feature.txt": "f"})
	r.commit(map[string]string{"feature.txt": "f2"})
	r.git(r.work, "push", "-q", "origin", "feature")
	res, err := r.svc.importRef(context.Background(), "refs/heads/feature", "", true)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(r.svc.chainPath("refs/heads/feature"))
	full, err := view.ExportChain(context.Background(), data, view.StoreRecords(r.st))
	if err != nil {
		t.Fatal(err)
	}
	exported := filepath.Join(dir, "feature.c4m")
	os.WriteFile(exported, full, 0o644)
	want := []c4.ID{views[2], res.Commits[0].View, res.Commits[1].View}
	if got := c4LogIDs(t, c4bin, exported); !equalIDs(got, want) {
		t.Fatalf("c4 log of the exported branch chain: %v", got)
	}
}

// c4LogIDs runs c4 log and returns the version IDs it lists.
func c4LogIDs(t *testing.T, bin, file string) []c4.ID {
	t.Helper()
	var ids []c4.ID
	for _, line := range strings.Split(run(t, bin, "log", file), "\n") {
		for _, f := range strings.Fields(line) {
			if id, err := c4.Parse(f); err == nil && len(f) == 90 && strings.HasPrefix(f, "c4") {
				ids = append(ids, id)
			}
		}
	}
	return ids
}

func run(t *testing.T, bin string, args ...string) string {
	t.Helper()
	out, err := exec.Command(bin, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v: %s", bin, strings.Join(args, " "), err, out)
	}
	return string(out)
}

func firstLine(b []byte) string {
	if i := bytes.IndexByte(b, '\n'); i >= 0 {
		return string(b[:i])
	}
	return string(b)
}

func equalIDs(a, b []c4.ID) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
