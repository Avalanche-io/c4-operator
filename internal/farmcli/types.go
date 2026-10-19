package farmcli

import (
	"bytes"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
	"text/template"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/jobspec"
)

// ImageRepo is where the prebuilt job images live; ImageTag follows the
// binary's version.
var (
	ImageRepo = "ghcr.io/avalanche-io"
	ImageTag  = "v0.1.0"
)

// JobType is a prebuilt job type: what `c4 farm submit TYPE` makes.
type JobType struct {
	Name    string
	Stage   string // the middle of the default job name
	Class   string // render, ci or ml
	Summary string
	Image   string // c4-job-<image>
	Pools   []string
	Mount   string // where the asset's folder is mounted
	// Frames says the type needs --frames; Order and Chunk are its
	// defaults ("" leaves the class's).
	Frames bool
	Order  string
	Chunk  string
	Params map[string]string // further class params
	// Script is run by sh -euc in each unit, a text/template over Vars.
	Script string
	// Command, when set, runs instead of a script.
	Command []string
	Args    []string
	// WorkDir is the container's working directory.
	WorkDir string
	// Set lists the --set keys the type reads, with their defaults.
	Set map[string]string
	// AIUsage is the declared AI use of the type's jobs, if any.
	AIUsage map[string]any
	// Renderers, for render, maps --renderer to the Hydra delegate name.
	Renderers map[string]string
}

// Vars are what a type's script sees.
type Vars struct {
	Asset    string // the asset's path under the mount, e.g. sm0140.usda
	Dir      string // the asset's folder under the mount, with a trailing slash, or ""
	Stem     string // its base name without extension, e.g. sm0140
	Seq      string // a frame's sequence pattern: sm0140.1001.exr gives sm0140.#.exr
	SeqF     string // the same as a printf pattern: sm0140.%04d.exr
	Shot     string
	Mount    string
	Renderer string
	Set      map[string]string
}

var types = map[string]JobType{
	"render": {Name: "render", Stage: "light", Class: "render", Image: "hydra", Pools: []string{"cpu"}, Mount: "/show",
		Summary: "render a USD stage through a Hydra delegate to OpenEXR, frame by frame",
		Frames:  true, Order: "binary", Chunk: "1", Params: map[string]string{"pass": "beauty"},
		Renderers: map[string]string{"cycles": "Cycles", "karma-xpu": "BRAY_HdKarmaXPU", "karma-cpu": "BRAY_HdKarma",
			"prman": "HdPrmanLoaderRendererPlugin", "arnold": "HdArnoldRendererPlugin", "vray": "HdVRayRendererPlugin"},
		Script: `mkdir -p {{.Mount}}/render
usdrecord --disableGpu --renderer {{.Renderer}} --frames "$C4_FRAME_START:$C4_FRAME_END" \
  --imageWidth {{index .Set "width"}} {{.Mount}}/{{.Asset}} {{.Mount}}/render/{{.Stem}}.####.exr
for f in $(seq "$C4_FRAME_START" "$C4_FRAME_END"); do
  c4-unit preview "{{.Mount}}/render/{{.Stem}}.$f.exr" "$f"
done`, Set: map[string]string{"width": "1920"}},
	"usd": {Name: "usd", Stage: "layout", Class: "render", Image: "usd", Pools: []string{"cpu"}, Mount: "/show",
		Summary: "flatten a USD stage into one layer and check it",
		Params:  map[string]string{"frames": "1", "order": "sequential"},
		Set:     map[string]string{"validate": "true"},
		Script: `mkdir -p {{.Mount}}/flat
usdcat --flatten {{.Mount}}/{{.Asset}} -o {{.Mount}}/flat/{{.Stem}}.flat.usda
{{if eq (index .Set "validate") "true"}}usdchecker {{.Mount}}/flat/{{.Stem}}.flat.usda | tee {{.Mount}}/flat/usdchecker.txt{{end}}`},
	"sim": {Name: "sim", Stage: "fx", Class: "render", Image: "sim", Pools: []string{"cpu"}, Mount: "/show",
		Summary: "bake a fluid sim to OpenVDB caches, the whole range in one unit (Blender Mantaflow, or Houdini for a .hip)",
		Frames:  true, Order: "sequential", Chunk: "all",
		Set: map[string]string{"domain": "FogDomain", "node": "/obj/fog/filecache1"},
		Script: `{{if hasSuffix .Asset ".hip"}}hython -c "
import hou, os
hou.hipFile.load('{{.Mount}}/{{.Asset}}')
hou.node('{{index .Set "node"}}').render(frame_range=(int(os.environ['C4_FRAME_START']), int(os.environ['C4_FRAME_END'])))
"{{else}}blender -b {{.Mount}}/{{.Asset}} --python-expr "
import bpy, os
s = bpy.context.scene
s.frame_start, s.frame_end = int(os.environ['C4_FRAME_START']), int(os.environ['C4_FRAME_END'])
d = bpy.data.objects['{{index .Set "domain"}}'].modifiers['Fluid'].domain_settings
d.cache_directory, d.cache_data_format = '{{.Mount}}/{{.Dir}}cache', 'OPENVDB'
bpy.ops.fluid.bake_all()
"{{end}}`},
	"comp": {Name: "comp", Stage: "comp", Class: "render", Image: "oiio", Pools: []string{"cpu"}, Mount: "/show",
		Summary: "composite frames: a Nuke script (.nk, on farms with Nuke), or an OpenImageIO script on any farm",
		Frames:  true, Order: "binary", Chunk: "4",
		Set: map[string]string{"engine": "auto"},
		Script: `mkdir -p {{.Mount}}/comp
{{if or (eq (index .Set "engine") "nuke") (and (eq (index .Set "engine") "auto") (hasSuffix .Asset ".nk"))}}nuke -x -F "$C4_FRAME_START-$C4_FRAME_END" {{.Mount}}/{{.Asset}}{{else}}sh {{.Mount}}/{{.Asset}}{{end}}
for f in $(seq "$C4_FRAME_START" "$C4_FRAME_END"); do
  c4-unit preview "{{.Mount}}/comp/{{.Shot}}.$f.exr" "$f"
done`},
	"transcode": {Name: "transcode", Stage: "review", Class: "render", Image: "media", Pools: []string{"cpu"}, Mount: "/show",
		Summary: "ACES-managed review movie (ProRes 422 HQ, ProRes 4444 or DNxHR HQX) and a contact sheet",
		Frames:  true, Order: "sequential", Chunk: "all",
		Set: map[string]string{"codec": "prores-422hq", "fps": "24", "frames": "{{.Seq}}"},
		Script: `mkdir -p {{.Mount}}/review /tmp/f
oiiotool --frames "$C4_FRAME_START-$C4_FRAME_END" '{{.Mount}}/{{.Dir}}{{index .Set "frames"}}' \
  --colorconfig ocio://studio-config-latest --iscolorspace ACES2065-1 \
  --ociodisplay "Rec.1886 Rec.709 - Display" "ACES 2.0 - SDR 100 nits (Rec.709)" \
  -d uint16 -o /tmp/f/f.#.tif
ffmpeg -loglevel error -framerate {{index .Set "fps"}} -start_number "$C4_FRAME_START" -i /tmp/f/f.%04d.tif \
  {{codecArgs (index .Set "codec")}} {{.Mount}}/review/{{.Shot}}_review.mov
oiiotool --frames "$C4_FRAME_START-$C4_FRAME_END" /tmp/f/f.#.tif --resize 320x0 \
  --mosaic:pad=4 8x6 -o {{.Mount}}/review/{{.Shot}}_contact.jpg`},
	"ci": {Name: "ci", Stage: "ci", Class: "ci", Image: "ci", Pools: []string{"cpu"}, Mount: "/src", WorkDir: "/src",
		Summary: "run a repository's tests in shards",
		Params:  map[string]string{"shards": "8", "target": "//..."},
		Script: `mkdir -p results
c4-test --shard "$C4_SHARD" --shards "$C4_SHARDS" --target "$C4_TARGET" --junit "results/shard-$C4_SHARD.xml"`},
	"lora": {Name: "lora", Stage: "train", Class: "ml", Image: "train", Pools: []string{"gpu"}, Mount: "/work", WorkDir: "/work",
		Summary: "fine-tune a LoRA (low-rank adapter) on a folder of captioned frames, with checkpoints",
		Params:  map[string]string{"ranks": "1", "steps": "400", "checkpointEvery": "50"},
		Set:     map[string]string{"base": "models/tiny-sd", "data": "{{.Asset}}", "rank": "16"},
		Script:  `c4-train-lora --base /work/{{index .Set "base"}} --data /work/{{index .Set "data"}} --out /work/out --rank {{index .Set "rank"}}`},
	"infer": {Name: "infer", Stage: "depth", Class: "render", Image: "infer", Pools: []string{"cpu"}, Mount: "/show",
		Summary: "run a model over every frame (depth, matte or upscale), with its AI use declared",
		Frames:  true, Order: "binary", Chunk: "2",
		Set: map[string]string{"task": "depth", "model": "depth-anything-v2-small"},
		Script: `mkdir -p {{.Mount}}/{{index .Set "task"}}
c4-infer --task {{index .Set "task"}} --model {{index .Set "model"}} \
  --in '{{.Mount}}/{{.Dir}}{{.SeqF}}' --out '{{.Mount}}/{{index .Set "task"}}/{{.Shot}}.%04d.exr' \
  --frames "$C4_FRAME_START-$C4_FRAME_END"
for f in $(seq "$C4_FRAME_START" "$C4_FRAME_END"); do
  c4-unit preview "{{.Mount}}/{{index .Set "task"}}/{{.Shot}}.$f.exr" "$f"
done`},
}

// Types lists the prebuilt types, sorted by name.
func Types() []JobType {
	out := make([]JobType, 0, len(types))
	for _, t := range types {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// LookupType finds a prebuilt type.
func LookupType(name string) (JobType, error) {
	t, ok := types[strings.ToLower(name)]
	if !ok {
		var names []string
		for _, x := range Types() {
			names = append(names, x.Name)
		}
		return JobType{}, fmt.Errorf("no job type %q; there are: %s (or --template FILE)", name, strings.Join(names, ", "))
	}
	return t, nil
}

// Options are the flags a submit gives a type.
type Options struct {
	Asset    string // relative to the mounted folder
	Shot     string
	Frames   string
	Pools    []string
	Renderer string
	Image    string // overrides the type's image
	// AssetIsDir says the asset is a folder (of frames, a repository, a
	// dataset) rather than a file.
	AssetIsDir bool
	Set        map[string]string
}

// Spec builds the job's WorkJob spec, before inputs and deadline.
func (t JobType) Spec(o Options) (c4v1.WorkJobSpec, error) {
	params := map[string]string{}
	for k, v := range t.Params {
		params[k] = v
	}
	set := map[string]string{}
	for k, v := range t.Set {
		set[k] = v
	}
	for k, v := range o.Set {
		if _, ok := t.Params[k]; ok {
			params[k] = v // a class parameter: shards, steps, checkpointEvery, ...
			continue
		}
		if _, ok := t.Set[k]; !ok {
			all := map[string]string{}
			for x := range t.Set {
				all[x] = ""
			}
			for x := range t.Params {
				all[x] = ""
			}
			return c4v1.WorkJobSpec{}, fmt.Errorf("%s takes no --set %s (it reads: %s)", t.Name, k, keys(all))
		}
		set[k] = v
	}
	if t.Frames {
		if o.Frames == "" {
			return c4v1.WorkJobSpec{}, fmt.Errorf("%s needs --frames, for example --frames 1001-1048", t.Name)
		}
		params["frames"] = o.Frames
		if t.Order != "" {
			params["order"] = t.Order
		}
		if t.Chunk == "all" {
			params["chunk"] = fmt.Sprint(frameCount(o.Frames))
		} else if t.Chunk != "" {
			params["chunk"] = t.Chunk
		}
	}
	renderer := ""
	if len(t.Renderers) > 0 {
		r := strings.ToLower(o.Renderer)
		if r == "" {
			r = "cycles"
		}
		d, ok := t.Renderers[r]
		if !ok {
			return c4v1.WorkJobSpec{}, fmt.Errorf("renderer %q: use %s", o.Renderer, keys(t.Renderers))
		}
		renderer = d
		params["renderer"] = r
	} else if o.Renderer != "" {
		return c4v1.WorkJobSpec{}, fmt.Errorf("%s takes no --renderer", t.Name)
	}
	stem := strings.TrimSuffix(path.Base(o.Asset), path.Ext(o.Asset))
	if o.AssetIsDir {
		stem = path.Base(o.Asset)
	}
	shot := o.Shot
	if shot == "" {
		shot = stem
	}
	dir := path.Dir(o.Asset) + "/"
	if o.AssetIsDir {
		dir = o.Asset + "/"
	}
	if dir == "./" || dir == "/" {
		dir = ""
	}
	seq := path.Base(o.Asset)
	if o.AssetIsDir {
		seq = shot + ".#.exr" // a folder of frames named for the shot
	} else if m := frameFile.FindStringSubmatch(seq); m != nil {
		seq = m[1] + ".#" + m[3]
	}
	seqF := strings.Replace(seq, "#", "%04d", 1)
	v := Vars{Asset: o.Asset, Dir: dir, Stem: stem, Seq: seq, SeqF: seqF, Shot: shot, Mount: t.Mount, Renderer: renderer, Set: set}
	// a --set value may itself use the vars ({{.Stem}})
	for k, val := range set {
		if strings.Contains(val, "{{") {
			x, err := expand(k, val, v)
			if err != nil {
				return c4v1.WorkJobSpec{}, err
			}
			set[k] = x
		}
	}
	c := jobspec.Container{Image: o.Image}
	if c.Image == "" {
		c.Image = ImageRepo + "/c4-job-" + t.Image + ":" + ImageTag
	}
	if t.Script != "" {
		script, err := expand(t.Name, t.Script, v)
		if err != nil {
			return c4v1.WorkJobSpec{}, err
		}
		c.Command, c.Args = []string{"sh", "-euc"}, []string{script}
	} else {
		c.Command, c.Args = t.Command, t.Args
	}
	pod, err := jobspec.Pod(c)
	if err != nil {
		return c4v1.WorkJobSpec{}, err
	}
	pod.Spec.Containers[0].Name = t.Stage
	if t.WorkDir != "" {
		pod.Spec.Containers[0].WorkingDir = t.WorkDir
	}
	pools := o.Pools
	if len(pools) == 0 {
		pools = t.Pools
	}
	return c4v1.WorkJobSpec{Class: t.Class, Params: params, Pools: pools, Template: pod}, nil
}

// frameFile is name.FRAME.ext.
var frameFile = regexp.MustCompile(`^(.*)\.(\d+)(\.[A-Za-z0-9]+)$`)

var funcs = template.FuncMap{
	"hasSuffix": strings.HasSuffix,
	"codecArgs": func(codec string) (string, error) {
		switch codec {
		case "prores-422hq":
			return "-c:v prores_ks -profile:v 3 -pix_fmt yuv422p10le", nil
		case "prores-4444":
			return "-c:v prores_ks -profile:v 4 -pix_fmt yuva444p10le", nil
		case "dnxhr-hqx":
			return "-c:v dnxhd -profile:v dnxhr_hqx -pix_fmt yuv422p10le", nil
		}
		return "", fmt.Errorf("codec %q: use prores-422hq, prores-4444 or dnxhr-hqx", codec)
	},
}

func expand(name, text string, v Vars) (string, error) {
	tpl, err := template.New(name).Funcs(funcs).Option("missingkey=error").Parse(text)
	if err != nil {
		return "", err
	}
	var b bytes.Buffer
	if err := tpl.Execute(&b, v); err != nil {
		return "", err
	}
	return b.String(), nil
}

// frameCount counts the frames of a range set like "1001-1048,1050".
func frameCount(s string) int {
	n := 0
	for _, part := range strings.Split(s, ",") {
		var a, b int
		if k, _ := fmt.Sscanf(part, "%d-%d", &a, &b); k == 2 && b >= a {
			n += b - a + 1
		} else if k, _ := fmt.Sscanf(part, "%d", &a); k == 1 {
			n++
		}
	}
	return max(n, 1)
}

func keys[V any](m map[string]V) string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}
