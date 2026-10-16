// Package hud holds the HUD web app, embedded so the manager serves it
// with no files on disk. The API contract is in API.md; the types are in
// package api; the backend is package server.
package hud

import "embed"

// Web is the HUD web app (index.html, css, js, fonts).
//
//go:embed web
var Web embed.FS
