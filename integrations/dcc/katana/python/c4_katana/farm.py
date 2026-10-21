"""C4 on Katana's farm menus, through ``FarmAPI``.

- "Submit to C4..." on the farm menu opens the dialog with every Render
  node ticked;
- "Submit this Render node to C4..." on a Render node's farm menu opens it
  with only that node ticked (and the ones it depends on, which it waits
  for, left for the artist to tick).

``install()`` registers both once Katana has started (its
``onStartupComplete`` callback), as farm plug-ins do; a studio's own
start-up script calls it, or ``Startup/init.py`` beside this folder does.
"""

from typing import List, Optional

MENU_ALL = "Submit to C4..."
MENU_NODE = "Submit this Render node to C4..."

_registered: List[str] = []


def register() -> None:
    """Adds C4's entries to Katana's farm menus (once)."""
    from Katana import FarmAPI  # noqa: PLC0415 - only inside Katana

    if _registered:
        return
    FarmAPI.AddFarmMenuOption(MENU_ALL, submit_all)
    FarmAPI.AddFarmPopupMenuOption(MENU_NODE, submit_this_node)
    _registered.extend([MENU_ALL, MENU_NODE])


def install() -> None:
    """Registers the menus when Katana's start-up completes, or now when
    Katana has no such callback."""
    try:
        from Katana import Callbacks  # noqa: PLC0415
        Callbacks.addCallback(Callbacks.Type.onStartupComplete, _on_startup)
    except (ImportError, AttributeError):
        register()


def _on_startup(**_kwargs) -> None:
    register()


def submit_all(*_args, **_kwargs):
    from c4_katana import dialog  # noqa: PLC0415 - Qt is loaded only when first used
    return dialog.open_dialog()


def clicked_render_nodes() -> List[str]:
    """The Render node the farm menu was opened on, else the selected ones."""
    from Katana import FarmAPI, NodegraphAPI  # noqa: PLC0415

    node = None
    for getter in ("GetCurrentNode", "GetClickedNode"):
        fn = getattr(FarmAPI, getter, None)
        if fn is None:
            continue
        try:
            node = fn()
        except Exception:  # noqa: BLE001 - no node in this scope
            node = None
        if node is not None:
            break
    if node is not None:
        return [node.getName()]
    try:
        selected = NodegraphAPI.GetAllSelectedNodes() or []
    except Exception:  # noqa: BLE001
        selected = []
    return [n.getName() for n in selected if n.getType() == "Render"]


def submit_this_node(*_args, **_kwargs):
    from c4_katana import dialog  # noqa: PLC0415

    nodes: Optional[List[str]] = clicked_render_nodes() or None
    return dialog.open_dialog(nodes=nodes)
