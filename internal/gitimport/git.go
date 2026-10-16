package gitimport

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
)

// Git modes the importer understands.
const (
	modeFile    = "100644"
	modeExec    = "100755"
	modeLink    = "120000"
	modeTree    = "040000"
	modeGitlink = "160000"
	modeNone    = "000000"
)

// git runs one git command in repo and returns its standard output.
func git(ctx context.Context, repo string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", repo}, args...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// ResolveCommit returns the commit SHA rev names in repo.
func ResolveCommit(ctx context.Context, repo, rev string) (string, error) {
	out, err := git(ctx, repo, "rev-parse", "--verify", "--end-of-options", rev+"^{commit}")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// commit is one commit of the first-parent history.
type commit struct {
	sha    string
	parent string // first parent; empty for a root commit
}

// firstParentHistory lists rev's first-parent history oldest to newest:
// at most max commits (the newest ones) when max > 0, and only commits
// after since when since is set.
func firstParentHistory(ctx context.Context, repo, rev, since string, max int) ([]commit, error) {
	args := []string{"rev-list", "--first-parent", "--parents"}
	if max > 0 {
		args = append(args, "--max-count="+strconv.Itoa(max))
	}
	args = append(args, rev)
	if since != "" {
		args = append(args, "^"+since)
	}
	out, err := git(ctx, repo, args...)
	if err != nil {
		return nil, err
	}
	var list []commit
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		c := commit{sha: f[0]}
		if len(f) > 1 {
			c.parent = f[1]
		}
		list = append(list, c)
	}
	// rev-list prints newest first; --max-count must keep the newest, so
	// the order is reversed here rather than with --reverse.
	for i, j := 0, len(list)-1; i < j; i, j = i+1, j-1 {
		list[i], list[j] = list[j], list[i]
	}
	return list, nil
}

// treeEntry is one entry of ls-tree or one side of a diff-tree change.
type treeEntry struct {
	mode string
	sha  string
	path string
	size int64 // ls-tree -l only; -1 otherwise
}

// lsTree lists every entry of a commit's tree recursively, limited to
// the directories dirs ("a/b/") when any are given.
func lsTree(ctx context.Context, repo, rev string, dirs ...string) ([]treeEntry, error) {
	args := []string{"ls-tree", "-r", "-l", "-z", "--full-tree", rev}
	if len(dirs) > 0 {
		args = append(append(args, "--"), dirs...)
	}
	out, err := git(ctx, repo, args...)
	if err != nil {
		return nil, err
	}
	var list []treeEntry
	for _, rec := range bytes.Split(out, []byte{0}) {
		if len(rec) == 0 {
			continue
		}
		// <mode> SP <type> SP <object> SP+ <size> TAB <path>
		tab := bytes.IndexByte(rec, '\t')
		if tab < 0 {
			return nil, fmt.Errorf("ls-tree: malformed record %q", rec)
		}
		f := strings.Fields(string(rec[:tab]))
		if len(f) != 4 {
			return nil, fmt.Errorf("ls-tree: malformed record %q", rec)
		}
		size := int64(-1)
		if f[3] != "-" {
			size, err = strconv.ParseInt(f[3], 10, 64)
			if err != nil {
				return nil, fmt.Errorf("ls-tree: size in %q: %w", rec, err)
			}
		}
		list = append(list, treeEntry{mode: f[0], sha: f[2], path: string(rec[tab+1:]), size: size})
	}
	return list, nil
}

// change is one record of diff-tree --raw.
type change struct {
	status   byte // A, D, M, T
	old, new treeEntry
}

// differ is one long-running `git diff-tree --stdin`, fed "<commit>
// <parent>" lines, so a whole history costs one process.
type differ struct {
	cmd    *exec.Cmd
	in     io.WriteCloser
	out    *bufio.Reader
	stderr bytes.Buffer
}

func startDiffer(ctx context.Context, repo string) (*differ, error) {
	d := &differ{}
	// -t reports trees as well as blobs, so a directory git removes is
	// removed from the view, not left behind empty.
	d.cmd = exec.CommandContext(ctx, "git", "-C", repo, "diff-tree", "--stdin", "-r", "-t", "-z",
		"--no-renames", "--raw", "--always")
	d.cmd.Stderr = &d.stderr
	var err error
	if d.in, err = d.cmd.StdinPipe(); err != nil {
		return nil, err
	}
	stdout, err := d.cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	d.out = bufio.NewReaderSize(stdout, 1<<16)
	if err := d.cmd.Start(); err != nil {
		return nil, err
	}
	return d, nil
}

// queue asks for the changes of c relative to its first parent. next
// returns them in the order they were queued.
func (d *differ) queue(c commit) error {
	_, err := fmt.Fprintf(d.in, "%s %s\n", c.sha, c.parent)
	return err
}

// closeInput ends the request stream; diff can still read what was queued.
func (d *differ) closeInput() error { return d.in.Close() }

// next reads the changes of the next queued commit. --always makes git
// print the commit's header even for an empty diff, and the header of the
// following commit (or EOF) ends this one.
func (d *differ) next(want string) ([]change, error) {
	header, err := d.token()
	if err != nil {
		return nil, d.fail(fmt.Errorf("diff-tree header for %s: %w", want, err))
	}
	if header != want {
		return nil, d.fail(fmt.Errorf("diff-tree answered for %q, expected %s", header, want))
	}
	var out []change
	for {
		b, err := d.out.Peek(1)
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, d.fail(err)
		}
		if b[0] != ':' {
			return out, nil // the next commit's header
		}
		meta, err := d.token()
		if err != nil {
			return nil, d.fail(err)
		}
		path, err := d.token()
		if err != nil {
			return nil, d.fail(err)
		}
		c, err := parseRaw(meta, path)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
}

func (d *differ) token() (string, error) {
	s, err := d.out.ReadString(0)
	if err != nil {
		return "", err
	}
	return s[:len(s)-1], nil
}

func (d *differ) fail(err error) error {
	if msg := strings.TrimSpace(d.stderr.String()); msg != "" {
		return fmt.Errorf("%w: %s", err, msg)
	}
	return err
}

func (d *differ) wait() error {
	d.in.Close()
	io.Copy(io.Discard, d.out)
	return d.cmd.Wait()
}

// parseRaw parses ":<old mode> <new mode> <old sha> <new sha> <status>".
func parseRaw(meta, path string) (change, error) {
	f := strings.Fields(strings.TrimPrefix(meta, ":"))
	if len(f) != 5 || f[4] == "" {
		return change{}, fmt.Errorf("diff-tree: malformed record %q", meta)
	}
	return change{
		status: f[4][0],
		old:    treeEntry{mode: f[0], sha: f[2], path: path, size: -1},
		new:    treeEntry{mode: f[1], sha: f[3], path: path, size: -1},
	}, nil
}

// catFile is one long-running `git cat-file --batch`.
type catFile struct {
	cmd    *exec.Cmd
	in     *bufio.Writer
	inPipe io.WriteCloser
	out    *bufio.Reader
	stderr bytes.Buffer
}

func startCatFile(ctx context.Context, repo string) (*catFile, error) {
	c := &catFile{}
	c.cmd = exec.CommandContext(ctx, "git", "-C", repo, "cat-file", "--batch")
	c.cmd.Stderr = &c.stderr
	var err error
	if c.inPipe, err = c.cmd.StdinPipe(); err != nil {
		return nil, err
	}
	c.in = bufio.NewWriter(c.inPipe)
	stdout, err := c.cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	c.out = bufio.NewReaderSize(stdout, 1<<20)
	if err := c.cmd.Start(); err != nil {
		return nil, err
	}
	return c, nil
}

// blob requests a blob and returns its size and a reader of exactly that
// many bytes. The reader must be drained (or done called) before the next
// request.
func (c *catFile) blob(sha string) (int64, io.Reader, func() error, error) {
	if _, err := c.in.WriteString(sha + "\n"); err != nil {
		return 0, nil, nil, err
	}
	if err := c.in.Flush(); err != nil {
		return 0, nil, nil, err
	}
	header, err := c.out.ReadString('\n')
	if err != nil {
		return 0, nil, nil, fmt.Errorf("cat-file %s: %w: %s", sha, err, strings.TrimSpace(c.stderr.String()))
	}
	f := strings.Fields(header)
	if len(f) == 2 && f[1] == "missing" {
		return 0, nil, nil, fmt.Errorf("cat-file: object %s is missing", sha)
	}
	if len(f) != 3 || f[1] != "blob" {
		return 0, nil, nil, fmt.Errorf("cat-file %s: unexpected header %q", sha, strings.TrimSpace(header))
	}
	size, err := strconv.ParseInt(f[2], 10, 64)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("cat-file %s: size: %w", sha, err)
	}
	r := io.LimitReader(c.out, size)
	done := func() error {
		if _, err := io.Copy(io.Discard, r); err != nil {
			return err
		}
		b, err := c.out.ReadByte()
		if err != nil {
			return err
		}
		if b != '\n' {
			return fmt.Errorf("cat-file %s: missing record terminator", sha)
		}
		return nil
	}
	return size, r, done, nil
}

func (c *catFile) close() error {
	c.inPipe.Close()
	io.Copy(io.Discard, c.out)
	return c.cmd.Wait()
}

// checkAttrFilter returns the value of the filter attribute for each path
// as of the tree of rev, the way git applies it on checkout: .gitattributes
// files of that tree, then $GIT_DIR/info/attributes and core.attributesFile.
func checkAttrFilter(ctx context.Context, repo, rev string, paths []string) (map[string]string, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", repo, "check-attr", "--source="+rev, "-z", "--stdin", "filter")
	var in bytes.Buffer
	for _, p := range paths {
		in.WriteString(p)
		in.WriteByte(0)
	}
	cmd.Stdin = &in
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git check-attr: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	// -z output: <path> NUL <attribute> NUL <value> NUL, repeated.
	f := bytes.Split(out, []byte{0})
	res := make(map[string]string, len(paths))
	for i := 0; i+2 < len(f); i += 3 {
		res[string(f[i])] = string(f[i+2])
	}
	if len(res) != len(paths) {
		return nil, fmt.Errorf("git check-attr answered for %d of %d paths", len(res), len(paths))
	}
	return res, nil
}
