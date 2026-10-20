// Package tools holds the farm's prebuilt tools: the folders beside this
// file, each a tool.yaml and what it runs (c4: tool/1). c4 farm up
// publishes them to every project of the local farm, so a new farm can
// render, comp, transcode, flatten, simulate, test, train and infer
// before anyone writes a tool of their own.
package tools

import (
	"embed"
	"io/fs"
)

//go:embed all:ci all:comp all:infer all:lora all:render all:sim all:transcode all:usd
var folders embed.FS

// Names are the prebuilt tools, by folder.
var Names = []string{"ci", "comp", "infer", "lora", "render", "sim", "transcode", "usd"}

// Folder is one prebuilt tool's folder.
func Folder(name string) (fs.FS, error) {
	return fs.Sub(folders, name)
}
