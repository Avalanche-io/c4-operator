// Package hud holds the HUD web app, embedded so the manager serves it
// with no files on disk. The API contract is in API.md; the types are in
// package api; the backend is package server.
package hud

import "embed"

// Web is the HUD web app (index.html, css, js, fonts) and its design system
// (design/: tokens, components, previews). The previews' fixtures stay out
// of the binary; served by the manager, the previews read the live API.
//
//go:embed web/index.html web/css web/js web/fonts web/design/tokens.css web/design/components web/design/previews web/design/README.md
var Web embed.FS
