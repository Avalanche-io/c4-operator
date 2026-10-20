#!/usr/bin/env python3
"""Depth maps for this unit's frames, with a monocular depth model."""
import json
import os

import OpenImageIO as oiio
import torch
from transformers import pipeline

from depthio import image_to_pil, write_depth_exr  # the studio's helpers, beside this file

p = json.loads(os.environ["C4_PARAMS"])
start, end = int(os.environ["C4_FRAME_START"]), int(os.environ["C4_FRAME_END"])
frames = range(start, end + 1)
out_dir = os.path.join(os.environ["C4_TREE"], "depth")
os.makedirs(out_dir, exist_ok=True)

print("openjd_status: loading model " + p["model"], flush=True)
device = 0 if torch.cuda.is_available() else -1
depth = pipeline("depth-estimation", model=os.path.join(os.environ["C4_TOOLS"], "models", p["model"]), device=device)

for i, f in enumerate(frames):
    src = p["plates"].replace("$F4", f"{f:04d}")
    print(f"openjd_status: frame {f}", flush=True)
    image = oiio.ImageBuf(src)
    result = depth(image_to_pil(image, p["colorspace"]))
    out = os.path.join(out_dir, f"{os.path.basename(src)[:-9]}.{f:04d}.exr")
    write_depth_exr(result["predicted_depth"], out)
    print(f"c4_frame: {f} {out}", flush=True)
