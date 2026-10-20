"""The "C4" menu in Maya's main window, with "Submit to farm...".

``install()`` adds it once Maya's UI is up (a userSetup runs before it
is); a studio whose own ``userSetup.py`` sets up its tools calls that.
Batch Maya (``mayabatch``, ``mayapy``) gets no menu.
"""

MENU = "c4Menu"
ITEM = "c4SubmitItem"


def add_menu() -> str:
    """Adds (or re-adds) the C4 menu to the main window; returns its name."""
    from maya import cmds, mel  # noqa: PLC0415 - only inside Maya

    try:
        main = mel.eval("$c4_main_window = $gMainWindow")
    except Exception:  # noqa: BLE001 - the main window's usual name
        main = "MayaWindow"
    if cmds.menu(MENU, exists=True):
        cmds.deleteUI(MENU)
    m = cmds.menu(MENU, label="C4", parent=main, tearOff=True)
    cmds.menuItem(ITEM, label="Submit to farm...", parent=m, command=_open,
                  annotation="Submit this scene's render layers to the C4 farm.")
    return m


def _open(*_):
    from c4_maya import panel  # noqa: PLC0415 - loaded when first used
    panel.open_panel()


def install() -> None:
    """Adds the menu once the UI exists; nothing in batch Maya."""
    from maya import cmds, utils  # noqa: PLC0415

    if cmds.about(batch=True):
        return
    utils.executeDeferred(add_menu)
