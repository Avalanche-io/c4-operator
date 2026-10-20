package farmcli

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// NewTool writes a starting folder for a tool of a kind: a tool.yaml
// with the common fields, a script that reports its progress, and a test
// tree, so `c4 farm tool test` passes before the TD changes anything.
func NewTool(o Out, dir, kind string) error {
	name := filepath.Base(dir)
	if !toolNameRE.MatchString(name) {
		return fmt.Errorf("%q: a tool's folder is its name: lowercase letters, digits and -, starting with a letter", name)
	}
	files, ok := toolTemplates(name)[kind]
	if !ok {
		return fmt.Errorf("no template for kind %q; there are: frames, once", kind)
	}
	if _, err := os.Stat(dir); err == nil {
		return fmt.Errorf("%s already exists", dir)
	}
	for _, f := range files {
		p := filepath.Join(dir, filepath.FromSlash(strings.ReplaceAll(f.path, "NAME", name)))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(p, []byte(f.body), f.mode); err != nil {
			return err
		}
	}
	o.printf("made      %s: a %s tool, %s\n", dir, kind, plural(len(files), "file", "files"))
	o.printf("next      edit %s/tool.yaml and %s/%s.sh, then: c4 farm tool test %s\n", dir, dir, name, dir)
	return nil
}

// toolNameRE is a tool's name: lowercase letters, digits and -.
var toolNameRE = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

type toolTemplateFile struct {
	path, body string
	mode       os.FileMode
}

// toolTemplates are the starting folders, by kind.
func toolTemplates(name string) map[string][]toolTemplateFile {
	r := strings.NewReplacer("NAME", name)
	frames := []toolTemplateFile{
		{"tool.yaml", r.Replace(`c4: tool/1
name: NAME
title: NAME
summary: One sentence for the Submit panel and for agents.
kind: frames
runs:
  image: c4/runtime-base
  command: [bash, "{{Tool.Dir}}/NAME.sh", "{{Param.input}}"]
frames: {chunk: 4, order: binary}
params:
- name: input
  type: PATH
  objectType: FILE
  dataFlow: IN
  description: The file each frame reads, with $F4 where the frame number goes.
  userInterface: {control: CHOOSE_INPUT_FILE, label: Input}
writes: [out/]
reuse: exact
test:
  tree: test/
  frames: 1001-1002
  with: {input: in/frame.$F4.txt}
  expect: [out/frame.1001.txt, out/frame.1002.txt]
`), 0o644},
		{"NAME.sh", r.Replace(`#!/bin/bash
# NAME: one unit's frames. The farm sets C4_FRAME_START, C4_FRAME_END and
# C4_TREE (the job's tree, writable); the input arrives as an absolute path.
set -euo pipefail
input="$1"
mkdir -p "$C4_TREE/out"
for f in $(seq "$C4_FRAME_START" "$C4_FRAME_END"); do
  src="${input/\$F4/$(printf %04d "$f")}"
  out="$C4_TREE/out/$(basename "$src")"
  cp "$src" "$out"          # the work goes here
  echo "c4_frame: $f"       # one line per finished frame: the farm's progress
  # (with a path, as c4_frame: $f $out.exr, the frame is also the HUD's preview)
done
`), 0o755},
		{"test/in/frame.1001.txt", "frame 1001\n", 0o644},
		{"test/in/frame.1002.txt", "frame 1002\n", 0o644},
	}
	once := []toolTemplateFile{
		{"tool.yaml", r.Replace(`c4: tool/1
name: NAME
title: NAME
summary: One sentence for the Submit panel and for agents.
kind: once
runs:
  image: c4/runtime-base
  command: [bash, "{{Tool.Dir}}/NAME.sh", "{{Param.input}}"]
params:
- name: input
  type: PATH
  objectType: DIRECTORY
  dataFlow: IN
  description: The folder it reads.
  userInterface: {control: CHOOSE_DIRECTORY, label: Input}
writes: [out/]
reuse: exact
test:
  tree: test/
  with: {input: in}
  expect: [out/listing.txt]
`), 0o644},
		{"NAME.sh", r.Replace(`#!/bin/bash
# NAME: one run over the whole job. C4_TREE is the job's tree, writable.
set -euo pipefail
input="$1"
mkdir -p "$C4_TREE/out"
ls -l "$input" >"$C4_TREE/out/listing.txt"   # the work goes here
echo "openjd_progress: 100"
`), 0o755},
		{"test/in/a.txt", "a\n", 0o644},
	}
	return map[string][]toolTemplateFile{"frames": frames, "once": once}
}
