"""Unreal projects for the tests: real files, with the fake unreal's asset
registry, sequences and queue."""

import copy
import os

import fakeunreal as ue
from c4submit.testing import NUKE_COMP, SALTMARSH, FakeFarm, write

#: A tool that renders one Movie Render Queue job of a project, as a studio
#: would publish it.
UNREAL_RENDER = {
    "c4": "tool/1", "name": "unreal-render", "title": "Unreal render",
    "summary": "Renders one Movie Render Queue job of an Unreal project over frames.",
    "kind": "frames",
    "runs": {"image": "registry.petrelfinch.example/unreal:5.5", "exe": "UnrealEditor-Cmd",
             "command": ["{{Run.Exe}}", "{{Param.project}}", "{{Param.map}}", "-game",
                         "-MoviePipelineConfig={{Param.config}}", "-Unattended", "-RenderOffscreen"]},
    "frames": {"chunk": 10, "order": "binary"},
    "params": [
        {"name": "project", "type": "PATH", "objectType": "FILE", "dataFlow": "IN", "from": "scene.file",
         "userInterface": {"control": "CHOOSE_INPUT_FILE", "label": "Project",
                           "fileFilters": [{"label": "Unreal project", "patterns": ["*.uproject"]}]}},
        {"name": "map", "type": "STRING", "from": "scene.renderables", "userInterface": {"label": "Map"}},
        {"name": "sequence", "type": "STRING", "from": "scene.renderables", "userInterface": {"label": "Level sequence"}},
        {"name": "config", "type": "STRING", "from": "scene.renderables",
         "description": "The job's queue manifest, from the project's folder.", "userInterface": {"label": "Config"}},
    ],
    "needs": {"cpu": 16, "memory": "64G", "gpu": 1, "sizes": {"heavy": {"memory": "128G", "gpuMemory": "48G"}}},
    "reuse": "exact",
}


def farm(tools=None) -> FakeFarm:
    return FakeFarm(tools=tools if tools is not None else [UNREAL_RENDER, NUKE_COMP],
                    projects={"saltmarsh": dict(copy.deepcopy(SALTMARSH), root="/show")})


def env(monkeypatch, show: str) -> None:
    monkeypatch.setenv("C4_PROJECT", "saltmarsh")
    monkeypatch.setenv("C4_ROOT", show)
    monkeypatch.setenv("C4_SHOT_PATTERN", r"/(?P<shot>sh\d{3})_")


def saltmarsh(root):
    """The show's Unreal project, Saltmarsh, under ``root``: two maps (Sh010
    a World Partition map with built lighting and one external actor), two
    level sequences, a rock both read, an asset nothing reads, a game mode
    only the config names, a movie in Content, a plug-in with content, and
    this machine's folders (Saved, Intermediate, DerivedDataCache,
    Binaries). Its queue: sh010_lighting (a custom range, 1001-1010),
    sh020_lighting (its sequence's range, 1001-1024) and a disabled job.
    Returns (show folder, project folder, queue)."""
    show = str(root)
    proj = os.path.join(show, "unreal", "Saltmarsh")

    def f(rel, data=b"x"):
        return write(os.path.join(proj, *rel.split("/")), data)

    f("Saltmarsh.uproject", b'{"FileVersion": 3, "EngineAssociation": "5.5"}')
    f("Config/DefaultEngine.ini", b"[/Script/EngineSettings.GameMapsSettings]\n"
                                  b"GameDefaultMap=/Game/Maps/Sh010.Sh010\n"
                                  b"GlobalDefaultGameMode=/Game/Blueprints/BP_Mode.BP_Mode_C\n")
    f("Config/DefaultEditor.ini", b"[/Script/UnrealEd.EditorEngine]\n")
    for rel in ("Maps/Sh010.umap", "Maps/Sh010_BuiltData.uasset", "Maps/Sh020.umap",
                "__ExternalActors__/Maps/Sh010/0/AB/KJ2.uasset", "Cinematics/sh010_seq.uasset",
                "Cinematics/sh020_seq.uasset", "Env/Rock.uasset", "Env/Rock.ubulk", "Env/Unused.uasset",
                "Blueprints/BP_Mode.uasset", "Movies/intro.mp4"):
        f("Content/" + rel, rel.encode())
    f("Plugins/SaltTools/SaltTools.uplugin", b'{"CanContainContent": true}')
    f("Plugins/SaltTools/Content/Props/Lamp.uasset", b"lamp")
    f("Plugins/SaltTools/Resources/Icon128.png", b"png")
    f("Plugins/SaltTools/Binaries/Mac/libSaltTools.dylib", b"bin")
    f("Plugins/SaltTools/Intermediate/Build/x.o", b"obj")
    for rel in ("Saved/Logs/Saltmarsh.log", "Intermediate/ProjectFiles/Saltmarsh.xcodeproj",
                "DerivedDataCache/a.udd", "Binaries/Mac/Saltmarsh", ".git/config", "Content/.DS_Store"):
        f(rel)

    st = ue.reset(proj)
    st.deps = {
        "/Game/Maps/Sh010": ["/Game/Maps/Sh010_BuiltData", "/Game/Env/Rock", "/Engine/BasicShapes/Cube", "/Script/Engine"],
        "/Game/__ExternalActors__/Maps/Sh010/0/AB/KJ2": ["/SaltTools/Props/Lamp"],
        "/Game/Cinematics/sh010_seq": ["/Game/Env/Rock", "/Game/Missing/Gone"],
    }
    st.assets = {"/Game/Cinematics/sh010_seq.sh010_seq": ue.LevelSequence(1001, 1049),
                 "/Game/Cinematics/sh020_seq.sh020_seq": ue.LevelSequence(1001, 1025)}
    custom = ue.MoviePipelinePrimaryConfig([ue.MoviePipelineOutputSetting(True, 1001, 1011)])
    queue = ue.MoviePipelineQueue([
        ue.MoviePipelineExecutorJob("sh010_lighting", "/Game/Maps/Sh010.Sh010", "/Game/Cinematics/sh010_seq.sh010_seq",
                                    custom),
        ue.MoviePipelineExecutorJob("sh020_lighting", "/Game/Maps/Sh020.Sh020", "/Game/Cinematics/sh020_seq.sh020_seq"),
        ue.MoviePipelineExecutorJob("old_test", "/Game/Maps/Sh020.Sh020", "/Game/Cinematics/sh020_seq.sh020_seq",
                                    enabled=False),
    ])
    return show, proj, queue


#: What saltmarsh() sends for both jobs from all of Content: the .uproject,
#: two config files, the plug-in's three files (not its Binaries or
#: Intermediate), the eleven files in Content and the two jobs' manifests.
ALL_FILES = 1 + 2 + 3 + 11 + 2
