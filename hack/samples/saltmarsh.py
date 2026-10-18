#!/usr/bin/env python3
"""Build the c4 farm sample set: Petrel & Finch's feature Saltmarsh, a CI
repository, LoRA style frames and Deadline 10 job pairs.

    python3 -I hack/samples/saltmarsh.py OUT [--plates] [--frames 1001-1048]

Writes OUT/saltmarsh (shot sm0140 as a USD stage with its comp scripts,
the sm0220 fog sim's setup, and with --plates the sm0140 plates as ACES
OpenEXR made by oiiotool), OUT/pipeline-tools (a git repository),
OUT/lora-saltmarsh and OUT/deadline. Everything is text made here, or
images made by oiiotool, so the set ships under the repository's license.
Run build.sh to add what needs the job images (fog.blend, the style
frames) and pack the release archive.
"""

import argparse
import math
import os
import random
import shutil
import struct
import subprocess
import sys
import zlib

FPS = 24


def write(path, text):
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, "w") as f:
        f.write(text)


def png(path, w, h, pixel):
    """A small RGB PNG from pixel(x, y) -> (r, g, b), stdlib only."""
    raw = bytearray()
    for y in range(h):
        raw.append(0)
        for x in range(w):
            raw.extend(bytes(max(0, min(255, int(c))) for c in pixel(x, y)))

    def chunk(kind, data):
        return struct.pack(">I", len(data)) + kind + data + struct.pack(">I", zlib.crc32(kind + data) & 0xFFFFFFFF)

    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, "wb") as f:
        f.write(b"\x89PNG\r\n\x1a\n")
        f.write(chunk(b"IHDR", struct.pack(">IIBBBBB", w, h, 8, 2, 0, 0, 0)))
        f.write(chunk(b"IDAT", zlib.compress(bytes(raw), 9)))
        f.write(chunk(b"IEND", b""))


def frames(spec):
    a, b = spec.split("-")
    return int(a), int(b)


# --- shot sm0140: a heron lifting off the marsh at dusk -------------------

def stage(out, first, last):
    shot = os.path.join(out, "saltmarsh", "sm0140")
    write(os.path.join(shot, "sm0140.usda"), f'''#usda 1.0
(
    doc = "Saltmarsh sm0140: a heron lifting off the marsh at dusk. Petrel & Finch."
    defaultPrim = "World"
    startTimeCode = {first}
    endTimeCode = {last}
    timeCodesPerSecond = {FPS}
    framesPerSecond = {FPS}
    metersPerUnit = 1
    upAxis = "Y"
    subLayers = [
        @lights/key.usda@,
        @layout.usda@
    ]
)

def "Render"
{{
    def RenderSettings "settings"
    {{
        rel camera = </World/cam/shotCam>
        int2 resolution = (1920, 1080)
        token[] includedPurposes = ["default", "render"]
    }}
}}
''')

    write(os.path.join(shot, "layout.usda"), f'''#usda 1.0
(
    doc = "sm0140 layout: the marsh, the reeds, the heron and the camera."
    defaultPrim = "World"
    startTimeCode = {first}
    endTimeCode = {last}
    timeCodesPerSecond = {FPS}
    upAxis = "Y"
)

def Xform "World"
{{
    def "marsh" (prepend references = @geo/marsh.usda@) {{}}
    def "reeds" (prepend references = @geo/reeds.usda@) {{}}
    def "heron" (prepend references = @geo/heron.usda@) {{}}

    def Xform "cam"
    {{
        def Camera "shotCam"
        {{
            float focalLength = 50
            float horizontalAperture = 36
            float verticalAperture = 20.25
            float2 clippingRange = (0.1, 1000)
            double3 xformOp:translate.timeSamples = {{
                {first}: (0, 1.4, 9),
                {last}: (0.6, 1.9, 8.2),
            }}
            float3 xformOp:rotateXYZ.timeSamples = {{
                {first}: (-4, 0, 0),
                {last}: (-1, 3, 0),
            }}
            uniform token[] xformOpOrder = ["xformOp:translate", "xformOp:rotateXYZ"]
        }}
    }}
}}
''')

    # the marsh: a gently rolling ground and a strip of water
    n = 24
    pts, idx, counts = [], [], []
    for j in range(n + 1):
        for i in range(n + 1):
            x, z = (i / n - 0.5) * 40, (j / n - 0.5) * 40
            y = 0.15 * math.sin(x * 0.4) * math.cos(z * 0.3) - (0.3 if abs(z + 2) < 2 else 0)
            pts.append(f"({x:.3f}, {y:.3f}, {z:.3f})")
    for j in range(n):
        for i in range(n):
            a = j * (n + 1) + i
            idx += [a, a + 1, a + n + 2, a + n + 1]
            counts.append(4)
    write(os.path.join(shot, "geo", "marsh.usda"), f'''#usda 1.0
(
    defaultPrim = "marsh"
    upAxis = "Y"
)

def Xform "marsh"
{{
    def Mesh "ground"
    {{
        int[] faceVertexCounts = [{", ".join(map(str, counts))}]
        int[] faceVertexIndices = [{", ".join(map(str, idx))}]
        point3f[] points = [{", ".join(pts)}]
        color3f[] primvars:displayColor = [(0.22, 0.20, 0.12)]
        rel material:binding = </marsh/looks/mud>
    }}

    def Mesh "water"
    {{
        int[] faceVertexCounts = [4]
        int[] faceVertexIndices = [0, 1, 2, 3]
        point3f[] points = [(-20, -0.12, -4), (20, -0.12, -4), (20, -0.12, 0), (-20, -0.12, 0)]
        color3f[] primvars:displayColor = [(0.30, 0.35, 0.42)]
        rel material:binding = </marsh/looks/water>
    }}

    def Scope "looks"
    {{
        def Material "mud"
        {{
            token outputs:surface.connect = </marsh/looks/mud/surface.outputs:surface>
            def Shader "surface"
            {{
                uniform token info:id = "UsdPreviewSurface"
                color3f inputs:diffuseColor = (0.22, 0.20, 0.12)
                float inputs:roughness = 0.9
                token outputs:surface
            }}
        }}
        def Material "water"
        {{
            token outputs:surface.connect = </marsh/looks/water/surface.outputs:surface>
            def Shader "surface"
            {{
                uniform token info:id = "UsdPreviewSurface"
                color3f inputs:diffuseColor = (0.05, 0.07, 0.09)
                float inputs:roughness = 0.08
                float inputs:metallic = 0
                token outputs:surface
            }}
        }}
    }}
}}
''')

    # the reeds: one reed instanced over the marsh's near bank
    rnd = random.Random(140)
    positions, orients, scales = [], [], []
    for _ in range(600):
        x, z = rnd.uniform(-14, 14), rnd.uniform(0.5, 6)
        positions.append(f"({x:.3f}, 0, {z:.3f})")
        a = rnd.uniform(-0.12, 0.12)
        orients.append(f"({math.cos(a / 2):.4f}, {math.sin(a / 2):.4f}, 0, 0)")
        s = rnd.uniform(0.6, 1.4)
        scales.append(f"({s:.3f}, {s:.3f}, {s:.3f})")
    write(os.path.join(shot, "geo", "reeds.usda"), f'''#usda 1.0
(
    defaultPrim = "reeds"
    upAxis = "Y"
)

def PointInstancer "reeds"
{{
    rel prototypes = [</reeds/prototypes/reed>]
    int[] protoIndices = [{", ".join("0" for _ in positions)}]
    point3f[] positions = [{", ".join(positions)}]
    quath[] orientations = [{", ".join(orients)}]
    float3[] scales = [{", ".join(scales)}]

    def Scope "prototypes"
    {{
        def Cylinder "reed"
        {{
            double height = 1.6
            double radius = 0.012
            token axis = "Y"
            double3 xformOp:translate = (0, 0.8, 0)
            uniform token[] xformOpOrder = ["xformOp:translate"]
            color3f[] primvars:displayColor = [(0.55, 0.48, 0.26)]
        }}
    }}
}}
''')

    # the heron: body, neck and wings; it lifts off and the wings beat
    lift = {f: (0.9 + max(0, (f - first - 12)) ** 1.6 * 0.004) for f in range(first, last + 1, 4)}
    beat = {f: 35 * math.sin((f - first) / FPS * 2 * math.pi * 1.6) for f in range(first, last + 1, 2)}

    def samples(d, fmt):
        return ",\n                ".join(f"{k}: {fmt(v)}" for k, v in d.items())

    write(os.path.join(shot, "geo", "heron.usda"), f'''#usda 1.0
(
    defaultPrim = "heron"
    upAxis = "Y"
)

def Xform "heron"
{{
    double3 xformOp:translate.timeSamples = {{
                {samples(lift, lambda y: f"(0, {y:.3f}, 2)")}
    }}
    uniform token[] xformOpOrder = ["xformOp:translate"]

    def Sphere "body"
    {{
        double radius = 0.28
        float3 xformOp:scale = (1.6, 0.8, 0.8)
        uniform token[] xformOpOrder = ["xformOp:scale"]
        rel material:binding = </heron/looks/feathers>
    }}
    def Capsule "neck"
    {{
        double height = 0.5
        double radius = 0.05
        token axis = "Y"
        double3 xformOp:translate = (0.42, 0.32, 0)
        float3 xformOp:rotateXYZ = (0, 0, -25)
        uniform token[] xformOpOrder = ["xformOp:translate", "xformOp:rotateXYZ"]
        rel material:binding = </heron/looks/feathers>
    }}
    def Xform "wingL"
    {{
        float3 xformOp:rotateXYZ.timeSamples = {{
                {samples(beat, lambda a: f"({a:.2f}, 0, 0)")}
        }}
        uniform token[] xformOpOrder = ["xformOp:rotateXYZ"]
        def Cube "wing"
        {{
            double size = 1
            float3 xformOp:scale = (0.6, 0.02, 0.9)
            double3 xformOp:translate = (0, 0, 0.5)
            uniform token[] xformOpOrder = ["xformOp:translate", "xformOp:scale"]
            rel material:binding = </heron/looks/feathers>
        }}
    }}
    def Xform "wingR"
    {{
        float3 xformOp:rotateXYZ.timeSamples = {{
                {samples(beat, lambda a: f"({-a:.2f}, 0, 0)")}
        }}
        uniform token[] xformOpOrder = ["xformOp:rotateXYZ"]
        def Cube "wing"
        {{
            double size = 1
            float3 xformOp:scale = (0.6, 0.02, 0.9)
            double3 xformOp:translate = (0, 0, -0.5)
            uniform token[] xformOpOrder = ["xformOp:translate", "xformOp:scale"]
            rel material:binding = </heron/looks/feathers>
        }}
    }}

    def Scope "looks"
    {{
        def Material "feathers"
        {{
            token outputs:surface.connect = </heron/looks/feathers/surface.outputs:surface>
            def Shader "surface"
            {{
                uniform token info:id = "UsdPreviewSurface"
                color3f inputs:diffuseColor.connect = </heron/looks/feathers/tex.outputs:rgb>
                float inputs:roughness = 0.7
                token outputs:surface
            }}
            def Shader "tex"
            {{
                uniform token info:id = "UsdUVTexture"
                asset inputs:file = @../tex/feathers.png@
                float3 outputs:rgb
            }}
        }}
    }}
}}
''')

    write(os.path.join(shot, "lights", "key.usda"), '''#usda 1.0
(
    doc = "sm0140 lights: a low dusk sun from the west and the evening sky. Raise the key's intensity to brighten the shot."
    defaultPrim = "World"
    upAxis = "Y"
)

def Xform "World"
{
    def Scope "lights"
    {
        def DistantLight "key"
        {
            float inputs:intensity = 8
            float inputs:angle = 1.5
            color3f inputs:color = (1.0, 0.72, 0.48)
            float3 xformOp:rotateXYZ = (-8, -70, 0)
            uniform token[] xformOpOrder = ["xformOp:rotateXYZ"]
        }
        def DomeLight "sky"
        {
            float inputs:intensity = 0.6
            color3f inputs:color = (0.45, 0.52, 0.70)
        }
    }
}
''')

    png(os.path.join(shot, "tex", "feathers.png"), 256, 256,
        lambda x, y: [200 + 30 * math.sin(x * 0.4 + y * 0.05)] * 3)

    write(os.path.join(shot, "comp.nk"), f'''#! nuke -nx
version 16.0 v1
Root {{
 inputs 0
 name sm0140_comp
 first_frame {first}
 last_frame {last}
 fps {FPS}
 format "1920 1080 0 0 1920 1080 1 HD_1080"
 colorManagement OCIO
 OCIO_config aces_1.3
}}
Read {{
 inputs 0
 file /in/render/sm0140.####.exr
 first {first}
 last {last}
 colorspace "ACES - ACEScg"
 name Read_render
}}
Grade {{
 white 1.08
 gamma 0.96
 name Grade_dusk
}}
Write {{
 file /show/comp/sm0140.####.exr
 file_type exr
 colorspace "ACES - ACES2065-1"
 name Write_comp
}}
''')
    write(os.path.join(shot, "comp.sh"), '''#!/bin/sh
# sm0140's comp as OpenImageIO: the render, graded a little warmer for dusk.
# The farm runs it in each unit with C4_FRAME_START and C4_FRAME_END set.
set -eu
mkdir -p /show/comp
for f in $(seq "$C4_FRAME_START" "$C4_FRAME_END"); do
  oiiotool "/in/render/sm0140.$f.exr" --mulc 1.08,1.04,0.98,1 --powc 0.96,0.96,0.96,1 \\
    -d half -o "/show/comp/sm0140.$f.exr"
done
''')
    os.chmod(os.path.join(shot, "comp.sh"), 0o755)


def plates(out, first, last, tool):
    """sm0140's plates: 960x540 ACES2065-1 OpenEXR, a dusk gradient with grain."""
    d = os.path.join(out, "saltmarsh", "plates", "sm0140")
    os.makedirs(d, exist_ok=True)
    for f in range(first, last + 1):
        t = (f - first) / max(1, last - first)
        top = (0.20 + 0.05 * t, 0.22, 0.34)
        bottom = (0.62, 0.40 - 0.05 * t, 0.22)
        cmd = [tool, "--pattern", "fill:top={:.3f},{:.3f},{:.3f}:bottom={:.3f},{:.3f},{:.3f}".format(*top, *bottom), "960x540", "3",
               "--pattern", "noise:type=gaussian:mean=0:stddev=0.01:seed={}".format(f), "960x540", "3", "--add",
               "--attrib", "oiio:ColorSpace", "ACES2065-1", "-d", "half", "-o", os.path.join(d, "sm0140.{}.exr".format(f))]
        subprocess.run(cmd, check=True)


def fog(out):
    d = os.path.join(out, "saltmarsh", "sm0220_fx")
    write(os.path.join(d, "fog_setup.py"), '''# Builds fog.blend for Saltmarsh sm0220: low marsh fog rolling over the
# water at dusk, a Mantaflow smoke domain caching to OpenVDB.
#   blender -b --python fog_setup.py -- fog.blend     (build.sh runs it in c4-job-sim)
import sys
import bpy

out = sys.argv[sys.argv.index("--") + 1]
bpy.ops.wm.read_factory_settings(use_empty=True)
s = bpy.context.scene
s.frame_start, s.frame_end = 1001, 1120
bpy.ops.mesh.primitive_cube_add(size=1, location=(0, 0, 1.5))
dom = bpy.context.object
dom.name = "FogDomain"
dom.scale = (12, 6, 1.5)
m = dom.modifiers.new("Fluid", "FLUID")
m.fluid_type = "DOMAIN"
ds = m.domain_settings
ds.domain_type = "GAS"
ds.resolution_max = 96
ds.cache_data_format = "OPENVDB"
ds.cache_type = "ALL"
ds.cache_frame_start, ds.cache_frame_end = 1001, 1120
bpy.ops.mesh.primitive_plane_add(size=1, location=(0, 0, 0.3))
src = bpy.context.object
src.name = "FogSource"
src.scale = (10, 4, 1)
f = src.modifiers.new("Fluid", "FLUID")
f.fluid_type = "FLOW"
f.flow_settings.flow_type = "SMOKE"
f.flow_settings.flow_behavior = "INFLOW"
f.flow_settings.smoke_color = (0.8, 0.8, 0.85)
f.flow_settings.temperature = -0.2
bpy.ops.wm.save_as_mainfile(filepath=out)
''')
    write(os.path.join(d, "README.md"), "Saltmarsh sm0220: marsh fog. `fog.blend` is built from `fog_setup.py` by\n`hack/samples/build.sh`; a Houdini studio uses `fog.hip` with a File Cache node at\n`/obj/fog/filecache1` instead.\n")


# --- pipeline-tools: a small repository for the CI batch -----------------

def pipeline_tools(out):
    d = os.path.join(out, "pipeline-tools")
    write(os.path.join(d, "README.md"), "Petrel & Finch's pipeline tools: shot names, frame ranges and review-movie names.\nThe tests run in eight shards on the farm (`c4 farm submit ci . --set shards=8`).\n")
    write(os.path.join(d, "pftools", "__init__.py"), "")
    write(os.path.join(d, "pftools", "shots.py"), '''"""Shot names and frame ranges, as Petrel & Finch writes them."""
import re

SHOT = re.compile(r"^(?P<show>[a-z]{2})(?P<num>\\d{4})$")


def parse(name):
    m = SHOT.match(name)
    if not m:
        raise ValueError(f"{name!r} is not a shot name like sm0140")
    return m["show"], int(m["num"])


def frames(spec):
    first, _, last = spec.partition("-")
    return list(range(int(first), int(last or first) + 1))


def review_name(shot, version):
    return f"{shot}_review_v{version:03d}.mov"
''')
    tests = []
    for i in range(1, 25):
        tests.append(f'''
def test_frames_{i:02d}():
    assert len(shots.frames("1001-{1000 + i * 2}")) == {i * 2}
''')
    write(os.path.join(d, "tests", "test_shots.py"), '''import random
import time

import pytest

from pftools import shots


def test_parse():
    assert shots.parse("sm0140") == ("sm", 140)


def test_parse_rejects():
    with pytest.raises(ValueError):
        shots.parse("shot140")


def test_review_name():
    assert shots.review_name("sm0140", 3) == "sm0140_review_v003.mov"


def test_cache_warm():
    # flaky on purpose: the CI view ranks it among the flaky tests
    time.sleep(0.2)
    assert random.random() > 0.15
''' + "".join(tests))
    write(os.path.join(d, "go.mod"), "module petrelfinch.example/pipeline-tools\n\ngo 1.24\n")
    write(os.path.join(d, "frames", "frames.go"), '''// Package frames counts frame ranges like 1001-1048.
package frames

import (
	"fmt"
	"strconv"
	"strings"
)

// Count is the number of frames in a range such as "1001-1048".
func Count(spec string) (int, error) {
	a, b, ok := strings.Cut(spec, "-")
	if !ok {
		b = a
	}
	first, err := strconv.Atoi(a)
	if err != nil {
		return 0, fmt.Errorf("%q: %w", spec, err)
	}
	last, err := strconv.Atoi(b)
	if err != nil || last < first {
		return 0, fmt.Errorf("%q is not a range", spec)
	}
	return last - first + 1, nil
}
''')
    write(os.path.join(d, "frames", "frames_test.go"), '''package frames

import "testing"

func TestCount(t *testing.T) {
	for in, want := range map[string]int{"1001-1048": 48, "1001": 1, "1-1": 1} {
		if got, err := Count(in); err != nil || got != want {
			t.Errorf("%s: %d %v", in, got, err)
		}
	}
	if _, err := Count("48-1"); err == nil {
		t.Error("a backwards range counted")
	}
}
''')
    env = dict(os.environ, GIT_AUTHOR_NAME="Petrel & Finch", GIT_AUTHOR_EMAIL="pipeline@petrelfinch.example",
               GIT_COMMITTER_NAME="Petrel & Finch", GIT_COMMITTER_EMAIL="pipeline@petrelfinch.example",
               GIT_AUTHOR_DATE="2026-10-01T10:00:00-04:00", GIT_COMMITTER_DATE="2026-10-01T10:00:00-04:00")
    if shutil.which("git") and not os.path.exists(os.path.join(d, ".git")):
        subprocess.run(["git", "init", "-q", "-b", "main", d], check=True, env=env)
        subprocess.run(["git", "-C", d, "add", "-A"], check=True, env=env)
        subprocess.run(["git", "-C", d, "commit", "-q", "-m", "pipeline tools: shots and frames, with tests"], check=True, env=env)


# --- LoRA style frames and Deadline job pairs -----------------------------

def lora(out):
    d = os.path.join(out, "lora-saltmarsh")
    write(os.path.join(d, ".c4project"), "")
    write(os.path.join(d, "consent.json"), '''{"record": "saltmarsh-style-frames-2026-09",
 "grantor": "Petrel & Finch, Saltmarsh art department",
 "use": "fine-tuning a style adapter for Saltmarsh only",
 "source": "frames rendered from the Saltmarsh sample shots"}
''')
    moods = ["dusk", "fog", "low sun", "overcast", "first light"]
    subjects = ["a heron over the reeds", "reeds against the water", "the marsh at the tide line", "mudflats and a channel"]
    for i in range(40):
        write(os.path.join(d, "frames", f"style_{i:03d}.txt"),
              f"saltmarsh style, {subjects[i % len(subjects)]}, {moods[i % len(moods)]}, painterly, muted color\n")
    write(os.path.join(d, "models", "README.md"), "The base model downloads here on first use (tiny-sd for the local farm).\n")


def deadline(out):
    d = os.path.join(out, "deadline")
    write(os.path.join(d, "cmdline", "job.info"), '''Plugin=CommandLine
Name=sm0140 proxies
Comment=half-resolution proxies of the sm0140 plates
Frames=1001-1048
ChunkSize=4
Pool=cpu
Priority=70
UserName=noor
Department=comp
OutputDirectory0=/show/proxy
''')
    write(os.path.join(d, "cmdline", "plugin.info"), '''Executable=/usr/bin/oiiotool
Arguments=/show/sm0140.<STARTFRAME>.exr --resize 50% -o /show/proxy/sm0140.<STARTFRAME>.exr
StartupDirectory=/show
''')
    write(os.path.join(d, "nuke", "job.info"), '''Plugin=Nuke
Name=sm0140 comp
Frames=1001-1048
ChunkSize=4
Pool=cpu
Group=nuke
Priority=60
MachineLimit=4
UserName=noor
OutputFilename0=/show/comp/sm0140.####.exr
''')
    write(os.path.join(d, "nuke", "plugin.info"), '''SceneFile=/show/sm0140/comp.nk
Version=16.0
WriteNode=Write_comp
''')


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("out")
    ap.add_argument("--plates", action="store_true", help="make the sm0140 plates with oiiotool")
    ap.add_argument("--frames", default="1001-1048")
    ap.add_argument("--oiiotool", default=shutil.which("oiiotool") or "oiiotool")
    a = ap.parse_args()
    first, last = frames(a.frames)
    out = os.path.abspath(a.out)
    write(os.path.join(out, "saltmarsh", ".c4project"), "")
    write(os.path.join(out, "saltmarsh", "README.md"),
          "Saltmarsh, Petrel & Finch's feature. sm0140: a heron lifting off the marsh at dusk (a USD stage).\n"
          "sm0220_fx: marsh fog (a sim). plates/sm0140: the shot's plates, ACES2065-1 OpenEXR.\n")
    stage(out, first, last)
    fog(out)
    if a.plates:
        plates(out, first, last, a.oiiotool)
    pipeline_tools(out)
    lora(out)
    deadline(out)
    print(out)


if __name__ == "__main__":
    sys.exit(main())
