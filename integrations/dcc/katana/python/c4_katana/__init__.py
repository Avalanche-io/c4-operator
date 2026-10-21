"""c4_katana: submit a Katana project to a C4 farm, as a Katana farm plug-in.

``host.KatanaHost`` reads the open project for c4submit (its Render nodes,
what each waits for, every file it reads); ``dialog`` is the submit dialog,
in Katana's own Qt; ``farm`` registers both on Katana's farm menus through
``FarmAPI``. ``Startup/init.py`` beside this folder installs it.
"""

__version__ = "0.1.0"
