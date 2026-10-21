"""Builds the katana-render tool's test project. Katana's script mode runs it::

    katana --script=build_project.py OUT.katana

A sphere seen by a camera, rendered by a Render node named Render_test with
3Delight (which Katana ships) to render/test.####.exr in this test tree,
frames 1001-1002. A project file is a large document nobody writes by
hand, so the test builds one.
"""

import os
import sys

from Katana import KatanaFile, NodegraphAPI  # noqa: E402 - only inside Katana

HERE = os.path.dirname(os.path.abspath(__file__))
TREE = os.path.dirname(HERE)


def put(node, path: str, value) -> None:
    """Set a parameter, turning it on when it is an enable/value pair."""
    p = node.getParameter(path)
    if p is None:
        raise SystemExit(f"build_project.py: {node.getName()} has no parameter {path}")
    if p.getChild("enable") is not None and p.getChild("value") is not None:
        p.getChild("enable").setValue(1, 0)
        p = p.getChild("value")
    p.setValue(value, 0)


def link(src, dst, port: str = "input") -> None:
    src.getOutputPort("out").connect(dst.getInputPort(port))


def main(out: str) -> None:
    root = NodegraphAPI.GetRootNode()
    root.getParameter("inTime").setValue(1001, 0)
    root.getParameter("outTime").setValue(1002, 0)

    sphere = NodegraphAPI.CreateNode("PrimitiveCreate", root)
    camera = NodegraphAPI.CreateNode("CameraCreate", root)
    put(camera, "transform.translate.z", 6.0)
    merge = NodegraphAPI.CreateNode("Merge", root)
    merge.addInputPort("sphere")
    merge.addInputPort("camera")
    link(sphere, merge, "sphere")
    link(camera, merge, "camera")

    settings = NodegraphAPI.CreateNode("RenderSettings", root)
    link(merge, settings)
    put(settings, "args.renderSettings.renderer", "dl")
    put(settings, "args.renderSettings.cameraName", "/root/world/cam/camera")

    outputs = NodegraphAPI.CreateNode("RenderOutputDefine", root)
    link(settings, outputs)
    outputs.checkDynamicParameters()
    put(outputs, "args.renderSettings.outputs.outputName.locationType", "file")
    outputs.checkDynamicParameters()
    put(outputs, "args.renderSettings.outputs.outputName.locationSettings.renderLocation",
        os.path.join(TREE, "render", "test.#.exr"))

    render = NodegraphAPI.CreateNode("Render", root)
    render.setName("Render_test")
    link(outputs, render)

    KatanaFile.Save(out)


if __name__ == "__main__":
    main(sys.argv[-1])
