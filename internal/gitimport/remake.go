package gitimport

import (
	"bytes"
	"context"
	"fmt"

	"github.com/Avalanche-io/c4"
)

// Remake imports one commit again, as the mapping line records it, to put
// back what the store has lost of it: its changeset, and with it any record
// or file object of its view that is gone as well. An import is a function
// of the commit and its parent's view, so the commit comes to the same
// view and the same changeset, and the sink, which keeps what it has,
// writes only what was missing. Nothing is written to the mapping or to a
// chain file.
//
// It is an error when the commit is not in the repository, when its
// parent's view cannot be read from the store, and when the commit does
// not come to the view and changeset the line records: the line is then
// not a record of this commit as this importer reads it, and nothing is
// taken to stand for it. A line written before 2026-11-09 records its
// changeset in a form that ended with the view's C4 ID, which is not
// written any more; the error says so.
func Remake(ctx context.Context, opts Options, line Commit) error {
	if err := opts.defaults(); err != nil {
		return err
	}
	opts.Progress = nil
	opts.Ref = line.Ref
	if _, err := ResolveCommit(ctx, opts.Repo, line.Commit); err != nil {
		return fmt.Errorf("commit %s is not in the repository: %w", line.Commit, err)
	}
	sum := &Summary{}
	im, err := newImporter(ctx, opts, sum)
	if err != nil {
		return err
	}
	defer im.close()
	c := commit{sha: line.Commit, parent: line.Parent}
	if line.Base {
		_, err = im.importBase(c)
	} else {
		_, err = im.importDiffs([]commit{c}, line.ParentView)
	}
	if err != nil {
		return fmt.Errorf("commit %s cannot be imported again: %w", line.Commit, err)
	}
	got := sum.Commits[len(sum.Commits)-1]
	if got.View == line.View && got.Changeset == line.Changeset {
		return nil
	}
	if got.View == line.View && oldForm(got.Patch, got.View, line.Changeset) {
		return fmt.Errorf("commit %s comes to the view the mapping records, and the mapping records its changeset as %s, in the form written before 2026-11-09 (ending with the view's C4 ID), which is not written any more", line.Commit, line.Changeset)
	}
	return fmt.Errorf("imported again, commit %s comes to the view %s by the changeset %s, and the mapping records the view %s by the changeset %s", line.Commit, got.View, got.Changeset, line.View, line.Changeset)
}

// oldForm reports whether id names changeset in the form written before
// 2026-11-09: the same text followed by the C4 ID of the view it produces.
func oldForm(changeset []byte, produced, id c4.ID) bool {
	for _, end := range []string{"\n", ""} {
		old := append(append([]byte(nil), changeset...), produced.String()+end...)
		if c4.Identify(bytes.NewReader(old)) == id {
			return true
		}
	}
	return false
}
