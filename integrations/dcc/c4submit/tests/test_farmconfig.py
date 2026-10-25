"""A panel talks to the farm `c4 farm` does: ~/.c4/farm.yaml."""

import os

import pytest

from c4submit import farmconfig
from c4submit.farm import Farm, FarmError
from c4submit.host import Host
from c4submit.testing import FakeFarm, serve

# As `c4 farm` writes it (sigs.k8s.io/yaml: block maps, plain values).
WRITTEN = """current: demo
farms:
  demo:
    as: wrangler
    hud: http://127.0.0.1:30180
    local: true
    project: saltmarsh
  studio:
    hud: https://c4.petrelfinch.example
    tokenFile: ~/.c4/studio.token
"""


@pytest.fixture
def config(tmp_path, monkeypatch):
    p = tmp_path / "farm.yaml"
    p.write_text(WRITTEN)
    monkeypatch.setenv("C4_FARM_CONFIG", str(p))
    monkeypatch.delenv("C4_HUD", raising=False)
    return p


def test_both_readers_agree():
    import builtins
    real_import = builtins.__import__

    def no_yaml(name, *a, **k):
        if name == "yaml":
            raise ImportError(name)
        return real_import(name, *a, **k)

    with_yaml = farmconfig.parse(WRITTEN)
    builtins.__import__ = no_yaml
    try:
        without = farmconfig.parse(WRITTEN)
        flow = farmconfig.parse("current: a\nfarms:\n  a: {hud: 'http://x:1', as: wrangler}  # a comment\n")
    finally:
        builtins.__import__ = real_import
    assert without == with_yaml
    assert flow == {"current": "a", "farms": {"a": {"hud": "http://x:1", "as": "wrangler"}}}


def test_resolve_picks_as_c4_farm_does(config, monkeypatch):
    assert farmconfig.resolve()["name"] == "demo"
    monkeypatch.setenv("C4_FARM", "studio")
    studio = farmconfig.resolve()
    assert studio["tokenFile"] == os.path.join(os.path.expanduser("~"), ".c4", "studio.token")
    assert farmconfig.resolve("nope") is None


def test_only_one_farm_needs_no_current(tmp_path, monkeypatch):
    p = tmp_path / "farm.yaml"
    p.write_text("farms:\n  only:\n    hud: http://x\n")
    monkeypatch.setenv("C4_FARM_CONFIG", str(p))
    assert farmconfig.resolve()["name"] == "only"


def test_a_farm_from_the_config_signs_in_as_its_viewer(config, monkeypatch):
    fake = FakeFarm()
    served = serve(fake)
    try:
        config.write_text(WRITTEN.replace("http://127.0.0.1:30180", served.url))
        farm = Farm()
        assert farm.base == served.url and farm.viewer == "wrangler" and farm.default_project == "saltmarsh"
        assert [t["name"] for t in farm.tools("saltmarsh")] == ["nuke-comp", "karma", "transcode"]
        monkeypatch.setenv("C4_HUD", "http://127.0.0.1:9")
        assert Farm().viewer == "", "C4_HUD set by the studio's launcher wins over the config, viewer and all"
    finally:
        served.close()


def test_no_farm_anywhere():
    with pytest.raises(FarmError) as e:
        Farm()
    assert e.value.message.startswith("the farm's address is not set: set C4_HUD")
    assert "c4 farm login" in e.value.message


def test_the_project_falls_back_to_the_farms(config, monkeypatch):
    monkeypatch.delenv("C4_PROJECT", raising=False)
    assert Host().project() == "saltmarsh"
    monkeypatch.setenv("C4_PROJECT", "textkit")
    assert Host().project() == "textkit"
    assert Host(env={}).project() == "", "a host given its own environment reads nothing else"


def test_a_local_farm_gets_this_machines_installer_token(config, tmp_path, monkeypatch):
    fake = FakeFarm()
    served = serve(fake, installer="ab" * 32)
    try:
        monkeypatch.setenv("HOME", str(tmp_path))
        written = WRITTEN.replace("http://127.0.0.1:30180", served.url).replace(
            "    local: true\n", "    local: true\n    installerTokenFile: ~/.c4/demo/installer-token\n")
        config.write_text(written)
        assert farmconfig.resolve()["installerTokenFile"] == str(tmp_path / ".c4" / "demo" / "installer-token")
        with pytest.raises(FarmError) as e:
            Farm().tools("saltmarsh")
        assert e.value.message.startswith("cannot read this machine's installer token") and "c4 farm open" in e.value.message
        (tmp_path / ".c4" / "demo").mkdir(parents=True)
        (tmp_path / ".c4" / "demo" / "installer-token").write_text("ab" * 32 + "\n")
        assert [t["name"] for t in Farm().tools("saltmarsh")] == ["nuke-comp", "karma", "transcode"]
        config.write_text(WRITTEN.replace("http://127.0.0.1:30180", served.url))
        with pytest.raises(FarmError) as e:
            Farm().tools("saltmarsh")
        assert e.value.status == 401 and "installer token: run c4 farm open" in e.value.message
        monkeypatch.setenv("C4_HUD", served.url)
        monkeypatch.setenv("C4_HUD_AS", "wrangler")
        monkeypatch.setenv("C4_HUD_INSTALLER_TOKEN_FILE", str(tmp_path / ".c4" / "demo" / "installer-token"))
        assert Farm().tools("saltmarsh"), "a launcher's environment can name the token too"
    finally:
        served.close()


def test_a_shared_farm_sends_no_installer_header(config, monkeypatch):
    farm = Farm()
    monkeypatch.setenv("C4_FARM", "studio")
    assert Farm()._installer() == "" and farm._installer() == ""
