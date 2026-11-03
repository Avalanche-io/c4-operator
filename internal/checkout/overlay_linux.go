//go:build linux

package checkout

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Avalanche-io/c4-operator/internal/capture"
	"github.com/Avalanche-io/c4-operator/internal/layer"
	"github.com/Avalanche-io/c4-operator/internal/overlay"
	"github.com/Avalanche-io/c4-operator/internal/view"
)

// Overlay makes each workspace an overlayfs mount of its view's full
// layer, with an upper and a work directory of its own beside the
// target: the farm's form, without the CSI driver. Its writes are read
// with overlay.Walk, after the mount is gone.
type Overlay struct {
	Layers *layer.Store
}

func (o *Overlay) Name() string { return "overlay" }

func (o *Overlay) Mount(_ context.Context, v *view.View, target string) (*Workspace, error) {
	m, err := o.Layers.BuildFull(v)
	if err != nil {
		return nil, fmt.Errorf("overlay workspace of %s: %w", v.ID, err)
	}
	state := target + ".c4"
	upper, work := filepath.Join(state, "upper"), filepath.Join(state, "work")
	for _, d := range []string{upper, work} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, err
		}
	}
	if err := overlay.Mount(target, []string{o.Layers.Dir(m.Key)}, upper, work); err != nil {
		os.RemoveAll(state)
		return nil, fmt.Errorf("overlay workspace of %s: %w", v.ID, err)
	}
	return &Workspace{View: v, Target: target, Upper: upper, Env: GitEnv(nil)}, nil
}

// Capture unmounts the workspace first: only then does its upper hold
// exactly what was written (capture's package comment).
func (o *Overlay) Capture(ctx context.Context, ws *Workspace, in capture.Input) (*capture.Result, error) {
	if err := overlay.Unmount(ws.Target); err != nil {
		return nil, err
	}
	in.Parent, in.Upper, in.Walk = ws.View.ID, ws.Upper, nil
	return capture.Run(ctx, in)
}

func (o *Overlay) Unmount(_ context.Context, ws *Workspace) error {
	if err := overlay.Unmount(ws.Target); err != nil {
		return err
	}
	if err := os.RemoveAll(filepath.Dir(ws.Upper)); err != nil {
		return err
	}
	return os.Remove(ws.Target)
}
