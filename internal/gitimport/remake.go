package gitimport

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/Avalanche-io/c4"

	"github.com/Avalanche-io/c4-operator/internal/storeurl"
)

// Remake imports one commit again, as the mapping line records it, to put
// back what the store has lost of it: its changeset, and the records along
// the paths it changed. The commit is imported over its parent's view as
// the line records it; when the records of that view are lost as well, the
// parent is imported again in full first, which puts back every record of
// its view, and must come to the view the line records. The sink keeps
// what it has and takes what it lacks. Nothing is written to the mapping or
// to a chain file. A file object the commit did not change is not put back:
// a commit's import does not read it.
//
// The view a commit comes to depends on the commit and, for c4git files,
// on which of their objects the c4git store and the project store hold. So
// the commit is required to come to the view and changeset the line
// records, and otherwise nothing is taken to stand for it: it is an error,
// and the objects that import stored stay in the store, named by nothing.
// It is an error too when the commit is not in the repository, and when
// the line records its changeset in the form written before 2026-11-09
// (ending with the view's C4 ID), which is not written any more. When the
// parent's view could not be put back, the error is a *ParentNotRemade.
func Remake(ctx context.Context, opts Options, line Commit) error {
	if err := opts.defaults(); err != nil {
		return err
	}
	opts.Progress = nil
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
		if err != nil && line.Parent != "" && lostFromStore(err) {
			if err = im.remakeParent(line); err == nil {
				_, err = im.importDiffs([]commit{c}, line.ParentView)
			}
		}
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
	return fmt.Errorf("imported again, commit %s comes to the view %s by the changeset %s, and the mapping records the view %s by the changeset %s%s", line.Commit, got.View, got.Changeset, line.View, line.Changeset, c4gitNote(got))
}

// ParentNotRemade is why Remake could not import a commit again: the
// records of its parent's view are lost from the store too, and importing
// the parent again in full did not put them back.
type ParentNotRemade struct {
	Parent string // the parent commit
	Err    error
}

func (e *ParentNotRemade) Error() string {
	return fmt.Sprintf("the records of the view of its parent %s are lost from the store as well, and importing the parent again in full did not put them back: %v", e.Parent, e.Err)
}

func (e *ParentNotRemade) Unwrap() error { return e.Err }

// remakeParent imports line's parent again in full, which writes every
// record of its view, and requires it to come to the view line applies to.
func (im *importer) remakeParent(line Commit) error {
	p, err := im.importBase(commit{sha: line.Parent})
	if err == nil && p.View != line.ParentView {
		err = fmt.Errorf("it comes to the view %s, and the mapping records the commit after it over %s%s", p.View, line.ParentView, c4gitNote(*p))
	}
	if err != nil {
		return &ParentNotRemade{Parent: line.Parent, Err: err}
	}
	return nil
}

// lostFromStore reports whether err is the store answering that it does
// not hold an object.
func lostFromStore(err error) bool {
	return errors.Is(err, storeurl.ErrNotFound) || errors.Is(err, os.ErrNotExist)
}

// c4gitNote says, for an import with c4git files, that its view depends on
// the c4git objects the stores hold, which may have changed since.
func c4gitNote(c Commit) string {
	if c.C4GitObjects == 0 && len(c.C4GitMissing) == 0 {
		return ""
	}
	return fmt.Sprintf(" (it has c4git files, %d missing now, and a c4git file's entry depends on whether the c4git store or the project store holds its object, so one that has arrived or been lost since the line was written gives another view)", len(c.C4GitMissing))
}

// oldForm reports whether id names changeset in the form written before
// 2026-11-09: the same text followed by the C4 ID of the view it produces,
// on a line of its own.
func oldForm(changeset []byte, produced, id c4.ID) bool {
	old := append(append([]byte(nil), changeset...), produced.String()+"\n"...)
	return c4.Identify(bytes.NewReader(old)) == id
}
