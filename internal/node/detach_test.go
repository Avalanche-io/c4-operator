package node

import (
	"errors"
	"os"
	"testing"
)

// unmounts plays the mount under a volume: each call to the daemon's
// unmount takes the next answer, and the last answer repeats.
type unmounts struct {
	answers []unmountAnswer
	calls   int
	// during runs inside a call, before it answers, for what another
	// Unpublish does meanwhile.
	during func(call int)
}

type unmountAnswer struct {
	clean, busy bool
	err         error
}

var (
	unmountedOutright = unmountAnswer{clean: true}
	busyAndDetached   = unmountAnswer{busy: true}
	nothingMounted    = unmountAnswer{}
)

func (u *unmounts) do(string) (bool, bool, error) {
	i := min(u.calls, len(u.answers)-1)
	u.calls++
	if u.during != nil {
		u.during(u.calls)
	}
	a := u.answers[i]
	return a.clean, a.busy, a.err
}

// detachDaemon is a daemon with a volume on disk and a scripted mount.
func detachDaemon(t *testing.T, u *unmounts) (*Daemon, *Volume) {
	t.Helper()
	d := &Daemon{Cfg: Config{Root: t.TempDir()}, unmount: u.do}
	if err := os.MkdirAll(d.volDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	vol := &Volume{ID: "vol-1", Target: "/var/lib/kubelet/pods/x/volumes/vol-1/mount", Project: "show", Capture: true}
	if err := d.saveVolume(vol); err != nil {
		t.Fatal(err)
	}
	return d, vol
}

// saved is what a daemon that restarts now would find.
func saved(t *testing.T, d *Daemon) *Volume {
	t.Helper()
	v, err := d.loadVolume("vol-1")
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// A capture links a volume's upper into the cache only when the upper is
// dead: its overlay unmounted outright. Every other way an unmount can go
// leaves the volume marked, in memory and on disk, so the capture copies.
func TestUnmountSettlesTheDetachedMark(t *testing.T) {
	boom := errors.New("unmount failed")
	for _, c := range []struct {
		name       string
		answers    []unmountAnswer
		calls      int // Unpublish attempts, each loading the volume afresh
		wantErr    bool
		consumable bool
	}{
		{name: "unmounted outright", answers: []unmountAnswer{unmountedOutright}, calls: 1, consumable: true},
		{name: "busy, so only detached", answers: []unmountAnswer{busyAndDetached}, calls: 1},
		{name: "nothing was mounted", answers: []unmountAnswer{nothingMounted}, calls: 1},
		{name: "the unmount failed", answers: []unmountAnswer{{err: boom}}, calls: 1, wantErr: true},
		{name: "busy, then a retry finds nothing mounted", answers: []unmountAnswer{busyAndDetached, nothingMounted}, calls: 2},
		{name: "busy, then two retries", answers: []unmountAnswer{busyAndDetached, nothingMounted}, calls: 3},
		{name: "failed, then unmounted outright on the retry", answers: []unmountAnswer{{err: boom}, unmountedOutright}, calls: 2},
		{name: "unmounted outright, then a retry after the capture failed", answers: []unmountAnswer{unmountedOutright, nothingMounted}, calls: 2},
	} {
		t.Run(c.name, func(t *testing.T) {
			u := &unmounts{answers: c.answers}
			d, _ := detachDaemon(t, u)
			var err error
			var vol *Volume
			for range c.calls {
				vol = saved(t, d)
				err = d.unmountVolume(vol)
			}
			if (err != nil) != c.wantErr {
				t.Fatalf("the last unmount: %v", err)
			}
			if vol.consumable() != c.consumable {
				t.Errorf("the capture would be told consumable=%v, want %v", vol.consumable(), c.consumable)
			}
			if on := saved(t, d); on.consumable() != c.consumable {
				t.Errorf("on disk the volume is consumable=%v, want %v", on.consumable(), c.consumable)
			}
			if u.calls != c.calls {
				t.Errorf("%d unmounts for %d calls", u.calls, c.calls)
			}
		})
	}
}

// The mark is on disk before the unmount is tried. A daemon that dies in
// the unmount, after a busy mount was detached, leaves a volume its
// successor will not take for dead.
func TestTheDetachedMarkIsSavedBeforeTheUnmount(t *testing.T) {
	u := &unmounts{answers: []unmountAnswer{busyAndDetached}}
	d, _ := detachDaemon(t, u)
	u.during = func(int) {
		if saved(t, d).consumable() {
			t.Error("while the unmount runs, the volume on disk is still consumable")
		}
	}
	vol := saved(t, d)
	if !vol.consumable() {
		t.Fatal("a volume starts consumable")
	}
	if err := d.unmountVolume(vol); err != nil {
		t.Fatal(err)
	}
	// The successor: nothing is mounted any more, and it must not clear
	// a mark it did not set.
	next := &Daemon{Cfg: d.Cfg, unmount: (&unmounts{answers: []unmountAnswer{nothingMounted}}).do}
	again := saved(t, next)
	if err := next.unmountVolume(again); err != nil {
		t.Fatal(err)
	}
	if again.consumable() || saved(t, next).consumable() {
		t.Error("a daemon that found nothing mounted took the upper for dead")
	}
}

// Two Unpublish calls for one volume overlap. The first finds the mount
// busy and detaches it; the second then finds nothing mounted. Neither may
// leave the volume consumable, whichever saves last.
func TestOverlappingUnmountsKeepTheMark(t *testing.T) {
	first := &unmounts{answers: []unmountAnswer{busyAndDetached}}
	d, _ := detachDaemon(t, first)
	a, b := saved(t, d), saved(t, d) // both loaded before either unmounts
	second := &Daemon{Cfg: d.Cfg, unmount: (&unmounts{answers: []unmountAnswer{nothingMounted}}).do}
	first.during = func(int) {
		// While the first is in its unmount, the second runs to its end.
		if err := second.unmountVolume(b); err != nil {
			t.Error(err)
		}
	}
	if err := d.unmountVolume(a); err != nil {
		t.Fatal(err)
	}
	if a.consumable() || b.consumable() || saved(t, d).consumable() {
		t.Errorf("consumable: first %v, second %v, on disk %v; want none", a.consumable(), b.consumable(), saved(t, d).consumable())
	}

	// The other order: the second call finishes first and finds nothing
	// mounted, then the first detaches a busy mount.
	first2 := &unmounts{answers: []unmountAnswer{busyAndDetached}}
	d2, _ := detachDaemon(t, first2)
	a2, b2 := saved(t, d2), saved(t, d2)
	second2 := &Daemon{Cfg: d2.Cfg, unmount: (&unmounts{answers: []unmountAnswer{nothingMounted}}).do}
	if err := second2.unmountVolume(b2); err != nil {
		t.Fatal(err)
	}
	if err := d2.unmountVolume(a2); err != nil {
		t.Fatal(err)
	}
	if a2.consumable() || b2.consumable() || saved(t, d2).consumable() {
		t.Error("consumable after the second order")
	}
}
