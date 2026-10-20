package server

import (
	"context"
	"fmt"

	"github.com/Avalanche-io/c4-operator/internal/authoring"
)

// The published tools, until tools on the server (authoring slice 1 item
// 1.3) replaces this file with its own toolRefs and toolAt: no tools.

func (s *Server) toolRefs(ctx context.Context, m *model, project string) []authoring.ToolRef {
	return nil
}

func (s *Server) toolAt(ctx context.Context, m *model, project, name, version string) (*authoring.Tool, authoring.ToolRef, error) {
	return nil, authoring.ToolRef{}, fmt.Errorf("%s in %s: %w", name, project, authoring.ErrNoTool)
}
