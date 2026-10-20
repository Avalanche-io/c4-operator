"""c4_houdini: the Houdini submit nodes for a C4 farm.

Two nodes, one per way a Houdini artist renders:

- **C4 Render** (a Solaris LOP, ``c4::c4render::1.0``): renders the stage
  wired into it. The stage is written to a USD file, and each ticked render
  settings prim is a job.
- **C4 Submit** (a ROP, ``c4::c4submit::1.0``): renders the ROPs wired into
  it from the saved hip file. Each ROP is a job, after the ROPs it reads.

Both draw the tool's parameters (``nodes.py``) and read the scene through a
``c4submit`` host (``host.py``). Everything after reading the scene, from
hashing to submitting, is ``c4submit``'s, the same as in every DCC.
"""

__version__ = "0.1.0"
