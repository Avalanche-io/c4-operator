package fill

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A flight that holds its last byte gives readers everything else as it
// arrives, the last byte only when the flight commits, and never the last
// byte of a flight that fails.
func TestHoldLast(t *testing.T) {
	data := []byte("0123456789")
	for _, fail := range []bool{false, true} {
		dir := t.TempDir()
		g := NewGroup(Limits{})
		step := make(chan struct{})
		f, _, err := g.Join("k", func() (*os.File, error) { return os.CreateTemp(dir, "t") },
			func(f *Flight) {
				f.HoldLast()
				f.SetSize(int64(len(data)))
				f.Write(data)
				<-step
				if fail {
					f.Fail(errors.New("hash mismatch"))
					return
				}
				f.Commit(func(tmp string) (string, error) {
					final := filepath.Join(dir, "final")
					return final, os.Rename(tmp, final)
				})
			})
		if err != nil {
			t.Fatal(err)
		}
		r, err := f.Reader(context.Background(), 0)
		if err != nil {
			t.Fatal(err)
		}
		got := make([]byte, len(data))
		n, _ := io.ReadAtLeast(r, got, len(data)-1)
		if n != len(data)-1 {
			t.Fatalf("fail=%v: read %d bytes before the flight ended, want %d", fail, n, len(data)-1)
		}
		last := make(chan error, 1)
		go func() { _, err := io.ReadFull(r, got[n:]); last <- err }()
		select {
		case err := <-last:
			t.Fatalf("fail=%v: the last byte came before the flight ended (%v)", fail, err)
		case <-time.After(50 * time.Millisecond):
		}
		close(step)
		err = <-last
		if fail && err == nil {
			t.Fatal("the last byte of a failed flight was delivered")
		}
		if !fail && (err != nil || string(got) != string(data)) {
			t.Fatalf("committed flight: %q, %v", got, err)
		}
		r.Close()
		f.Release()
	}
}
