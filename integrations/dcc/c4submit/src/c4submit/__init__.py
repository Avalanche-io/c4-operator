"""c4submit: submit to a C4 farm from inside a DCC.

The shared library under every DCC panel (Nuke, Houdini, and the rest):
it collects the scene's files, reads the production context, checks the
submission with the farm, uploads only what the farm lacks, and submits.
Pure Python, standard library only, so it runs in each application's own
interpreter. See README.md and submitter.py for the flow.
"""

__version__ = "0.1.0"
