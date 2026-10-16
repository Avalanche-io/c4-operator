// Package fill coalesces concurrent fetches of one object into a single
// upstream transfer and streams it to every waiter as it arrives.
//
// The leader writes the object to a temporary file. Each waiter reads that
// file at its own offset, waiting for more bytes as needed, so a waiter is
// never held until the transfer ends: a chain of tiers (store, regional,
// home, node) pipelines instead of costing one full transfer per hop.
// When the leader commits, the file is renamed into the cache; descriptors
// already open on it stay valid.
//
// A Group also does admission control. It caps concurrent fills and
// concurrent waiters, and refuses beyond that with ErrBusy instead of
// queueing without bound or forwarding the excess upstream.
package fill

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
)

// ErrBusy means the group is at capacity. Servers answer 503 with
// Retry-After.
var ErrBusy = errors.New("at capacity")

// Limits bound a group. Zero means unlimited.
type Limits struct {
	Fills   int // concurrent leaders (upstream transfers)
	Waiters int // concurrent waiters across all fills
}

// Group is a set of fills keyed by (partition, ID).
type Group struct {
	limits Limits

	mu      sync.Mutex
	m       map[string]*Flight
	fills   int
	waiters int
}

// NewGroup returns a group with limits.
func NewGroup(l Limits) *Group { return &Group{limits: l, m: map[string]*Flight{}} }

// Stats is a snapshot of a group.
type Stats struct{ Fills, Waiters int }

// Stats returns the current number of fills and waiters.
func (g *Group) Stats() Stats {
	g.mu.Lock()
	defer g.mu.Unlock()
	return Stats{g.fills, g.waiters}
}

// Lookup returns the flight in progress for key, if any, and registers a
// waiter on it. The caller must call Release.
func (g *Group) Lookup(key string) (*Flight, bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	f, ok := g.m[key]
	if !ok {
		return nil, false, nil
	}
	if g.limits.Waiters > 0 && g.waiters >= g.limits.Waiters {
		return nil, false, ErrBusy
	}
	g.waiters++
	f.waiters++
	return f, true, nil
}

// Join returns the flight for key, starting one if there is none. A new
// flight writes to tmp, and run is called in its own goroutine, detached
// from the caller's context, and must end with Commit or Fail. The second
// result reports whether this call started the flight. The caller is a
// waiter either way and must call Release.
func (g *Group) Join(key string, tmp func() (*os.File, error), run func(f *Flight)) (*Flight, bool, error) {
	g.mu.Lock()
	if f, ok := g.m[key]; ok {
		if g.limits.Waiters > 0 && g.waiters >= g.limits.Waiters {
			g.mu.Unlock()
			return nil, false, ErrBusy
		}
		g.waiters++
		f.waiters++
		g.mu.Unlock()
		return f, false, nil
	}
	if g.limits.Fills > 0 && g.fills >= g.limits.Fills {
		g.mu.Unlock()
		return nil, false, ErrBusy
	}
	if g.limits.Waiters > 0 && g.waiters >= g.limits.Waiters {
		g.mu.Unlock()
		return nil, false, ErrBusy
	}
	file, err := tmp()
	if err != nil {
		g.mu.Unlock()
		return nil, false, err
	}
	f := &Flight{key: key, g: g, tmp: file.Name(), w: file, size: -1, notify: make(chan struct{})}
	g.m[key] = f
	g.fills++
	g.waiters++
	f.waiters++
	g.mu.Unlock()
	go run(f)
	return f, true, nil
}

// Flight is one object being filled.
type Flight struct {
	key string
	g   *Group
	w   *os.File

	mu      sync.Mutex
	tmp     string
	final   string
	written int64
	size    int64
	done    bool
	err     error
	notify  chan struct{} // closed and replaced when state changes
	waiters int
}

// Release ends one waiter's interest.
func (f *Flight) Release() {
	f.g.mu.Lock()
	f.g.waiters--
	f.waiters--
	f.g.mu.Unlock()
}

func (f *Flight) changed() {
	close(f.notify)
	f.notify = make(chan struct{})
}

// SetSize records the object's total size once the leader learns it.
func (f *Flight) SetSize(n int64) {
	f.mu.Lock()
	f.size = n
	f.changed()
	f.mu.Unlock()
}

// Written is how many bytes the leader has written so far, the offset to
// resume from after an upstream failure.
func (f *Flight) Written() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.written
}

// Write appends to the object. Only the leader writes.
func (f *Flight) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	if n > 0 {
		f.mu.Lock()
		f.written += int64(n)
		f.changed()
		f.mu.Unlock()
	}
	return n, err
}

// Truncate restarts the object from zero (an upstream that ignored a
// ranged request sends the whole object again).
func (f *Flight) Truncate() error {
	if err := f.w.Truncate(0); err != nil {
		return err
	}
	if _, err := f.w.Seek(0, io.SeekStart); err != nil {
		return err
	}
	f.mu.Lock()
	f.written = 0
	f.mu.Unlock()
	return nil
}

// Commit closes the temporary file and hands its path to commit, which
// moves it into the cache and returns the final path. Waiters then finish
// reading from descriptors they hold, or open the final path.
func (f *Flight) Commit(commit func(tmp string) (string, error)) error {
	err := f.w.Close()
	f.mu.Lock()
	if err == nil && f.size >= 0 && f.written != f.size {
		err = fmt.Errorf("short object: %d of %d bytes", f.written, f.size)
	}
	f.mu.Unlock()
	if err != nil {
		os.Remove(f.tmp)
		f.finish("", err)
		return err
	}
	// Rename under the lock, so a waiter opening the object sees either
	// the temporary path or the final one, never a path in between.
	f.mu.Lock()
	final, err := commit(f.tmp)
	if err == nil {
		f.final = final
	}
	f.mu.Unlock()
	if err != nil {
		os.Remove(f.tmp)
		f.finish("", err)
		return err
	}
	f.finish(final, nil)
	return nil
}

// Fail ends the flight with err and removes the temporary file.
func (f *Flight) Fail(err error) {
	f.w.Close()
	os.Remove(f.tmp)
	f.finish("", err)
}

func (f *Flight) finish(final string, err error) {
	f.mu.Lock()
	f.done = true
	f.err = err
	f.final = final
	if err == nil && f.size < 0 {
		f.size = f.written
	}
	f.changed()
	f.mu.Unlock()

	f.g.mu.Lock()
	delete(f.g.m, f.key)
	f.g.fills--
	f.g.mu.Unlock()
}

// Size waits until the total size is known (or the flight ends) and
// returns it.
func (f *Flight) Size(ctx context.Context) (int64, error) {
	for {
		f.mu.Lock()
		size, done, err, ch := f.size, f.done, f.err, f.notify
		f.mu.Unlock()
		if err != nil {
			return 0, err
		}
		if size >= 0 || done {
			return size, nil
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
}

// Wait blocks until the flight ends and returns its error.
func (f *Flight) Wait(ctx context.Context) error {
	for {
		f.mu.Lock()
		done, err, ch := f.done, f.err, f.notify
		f.mu.Unlock()
		if done {
			return err
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// Reader returns a reader of the object from offset, which reads bytes as
// the leader writes them and ends at the object's end. If the fill fails,
// Read returns the error after the bytes that did arrive.
func (f *Flight) Reader(ctx context.Context, offset int64) (io.ReadCloser, error) {
	f.mu.Lock()
	path := f.tmp
	if f.final != "" {
		path = f.final
	}
	failed := f.err
	file, err := os.Open(path)
	f.mu.Unlock()
	if failed != nil {
		return nil, failed
	}
	if err != nil {
		return nil, err
	}
	return &reader{ctx: ctx, f: f, file: file, off: offset}, nil
}

type reader struct {
	ctx  context.Context
	f    *Flight
	file *os.File
	off  int64
}

func (r *reader) Read(p []byte) (int, error) {
	for {
		r.f.mu.Lock()
		avail := r.f.written - r.off
		done, err, ch := r.f.done, r.f.err, r.f.notify
		r.f.mu.Unlock()
		if avail > 0 {
			if int64(len(p)) > avail {
				p = p[:avail]
			}
			n, rerr := r.file.ReadAt(p, r.off)
			r.off += int64(n)
			if n > 0 {
				return n, nil
			}
			if rerr != nil && rerr != io.EOF {
				return 0, rerr
			}
			// The leader restarted the object (Truncate) after avail was
			// read; look at the state again.
			continue
		}
		if err != nil {
			return 0, err
		}
		if done {
			return 0, io.EOF
		}
		select {
		case <-ch:
		case <-r.ctx.Done():
			return 0, r.ctx.Err()
		}
	}
}

func (r *reader) Close() error { return r.file.Close() }
