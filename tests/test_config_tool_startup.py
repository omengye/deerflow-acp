"""Keep settings reads independent of agent execution imports."""
from __future__ import annotations

import importlib
import json
from pathlib import Path
import subprocess
import sys

import pytest


_OFFLINE_COMMAND = """
import runpy
import sys

class NoRuntimeImports:
    def find_spec(self, fullname, path=None, target=None):
        blocked = (
            'langchain', 'langchain_core', 'langchain_openai', 'langgraph',
            'langsmith', 'openai', 'anthropic', 'deerflow.models',
            'deerflow.skills.installer', 'deerflow.skills.security_scanner',
            'deerflow.subagents.executor', 'deerflow.subagents.registry',
        )
        if any(fullname == name or fullname.startswith(name + '.') for name in blocked):
            raise AssertionError('Offline settings loaded runtime dependency: ' + fullname)

def no_network(event, args):
    if event in {'socket.connect', 'socket.getaddrinfo'}:
        raise AssertionError('Offline settings attempted network access')

# Save/validate also check the configured memory manager. Only first-open
# metadata reads must remain independent of the execution dependencies.
if sys.argv[-1] in {'init', 'snapshot', 'bridge-policy'}:
    sys.meta_path.insert(0, NoRuntimeImports())
sys.addaudithook(no_network)
sys.argv[0] = 'deerflow.config_tool'
runpy.run_module('deerflow.config_tool', run_name='__main__', alter_sys=True)
"""


def test_settings_reads_do_not_load_agent_runtime(tmp_path: Path) -> None:
    repo = Path(__file__).resolve().parents[1]
    resources = tmp_path / "resources"
    resources.mkdir()
    (resources / "default-config.yaml").write_bytes((repo / "resources/default-config.yaml").read_bytes())
    skill = resources / "skills/public/sample/SKILL.md"
    skill.parent.mkdir(parents=True)
    skill.write_text("---\nname: sample\ndescription: Offline metadata\n---\nBody.\n", encoding="utf-8")
    user_data = tmp_path / "user-data"
    config = user_data / "config/config.yaml"

    def command(operation: str, data=None, *, ok=True):
        process = subprocess.run(
            [sys.executable, "-c", _OFFLINE_COMMAND, "--config", str(config),
             "--user-data", str(user_data), "--resources", str(resources), operation],
            cwd=repo, input=json.dumps(data), capture_output=True, text=True,
            encoding="utf-8", timeout=60,
        )
        assert process.returncode == (0 if ok else 1), process.stderr or process.stdout
        envelope = json.loads(process.stdout)
        assert envelope["ok"] is ok
        return envelope["data"] if ok else envelope["error"]

    assert command("init")["initialized"]
    snapshot = command("snapshot")
    assert command("bridge-policy") == {"enabled": False, "allowed_commands": []}
    assert snapshot["skills"][0]["name"] == "sample"
    assert {agent["name"] for agent in snapshot["subagents"]["builtin_agents"]} == {"bash", "general-purpose"}
    snapshot["models"][0]["display_name"] = "Saved offline"
    assert command("validate", snapshot)["valid"]
    saved = command("save", snapshot)
    assert saved["config_revision"] != snapshot["config_revision"]
    assert command("snapshot")["models"][0]["display_name"] == "Saved offline"
    # Optimistic concurrency and malformed-file errors still use JSON envelopes.
    assert command("save", snapshot, ok=False)
    config.write_text("models: [", encoding="utf-8")
    assert "Unable to read" in command("snapshot", ok=False)


@pytest.mark.parametrize("package_name, exports", [
    ("deerflow.skills", {
        "installer": ("install_skill_from_archive", "ainstall_skill_from_archive",
                      "SkillAlreadyExistsError", "SkillSecurityScanError"),
    }),
    ("deerflow.subagents", {
        "executor": ("SubagentExecutor", "SubagentResult"),
        "registry": ("get_available_subagent_names", "get_subagent_config", "list_subagents"),
    }),
])
def test_package_exports_remain_compatible(package_name, exports) -> None:
    package = importlib.import_module(package_name)
    for module_name, names in exports.items():
        for name in names:
            assert name in dir(package)
            value = getattr(package, name)
            assert value is getattr(importlib.import_module(f"{package_name}.{module_name}"), name)
    with pytest.raises(AttributeError):
        getattr(package, "missing_export")
