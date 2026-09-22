from __future__ import annotations

import asyncio
from copy import deepcopy
from types import SimpleNamespace
from unittest.mock import Mock

import pytest
from langchain_core.tools import StructuredTool

from deerflow.config.agents_config import AgentConfig
from deerflow.config.extensions_config import ExtensionsConfig
from deerflow.mcp.tools import _make_session_pool_tool
from deerflow.tools import tools as tools_module
from deerflow.tools.builtins.tool_search import (
    get_deferred_registry,
    reset_deferred_registry,
    set_deferred_registry,
    tool_search,
)


def _tool(name: str, server: str | None) -> StructuredTool:
    def echo(query: str) -> str:
        return query

    return StructuredTool.from_function(echo, name=name, description="Search this instance", metadata={"mcp_server": server} if server else {})


@pytest.fixture
def mcp_selection(monkeypatch):
    # Deliberately overlap prefixes: visible names are not source identities.
    cached = [_tool("web_lookup", "web"), _tool("web_scraper_lookup", "web_scraper"), _tool("disabled_lookup", "disabled"), _tool("web_forged", None)]
    config = SimpleNamespace(
        tools=[], models=[], memory=SimpleNamespace(enabled=False),
        skills=SimpleNamespace(enabled=False), skill_evolution=SimpleNamespace(enabled=False),
        subagents=SimpleNamespace(enabled=False), tool_search=SimpleNamespace(enabled=True),
    )
    extensions = ExtensionsConfig.model_validate({"mcpServers": {
        "web": {"enabled": True}, "web_scraper": {"enabled": True}, "disabled": {"enabled": False},
    }})
    cache_reader = Mock(return_value=cached)
    monkeypatch.setattr(tools_module, "get_app_config", lambda: config)
    monkeypatch.setattr(tools_module, "is_host_bash_allowed", lambda _config: True)
    monkeypatch.setattr(tools_module, "is_host_tool_allowed", lambda _config: True)
    monkeypatch.setattr(ExtensionsConfig, "from_file", lambda: extensions)
    monkeypatch.setattr("deerflow.mcp.cache.get_cached_mcp_tools", cache_reader)
    monkeypatch.setattr("deerflow.config.acp_config.get_acp_agents", lambda: {})
    previous = get_deferred_registry()
    reset_deferred_registry()
    yield cached, config, extensions, cache_reader
    if previous is None:
        reset_deferred_registry()
    else:
        set_deferred_registry(previous)


def _mcp_names(tools) -> set[str]:
    return {tool.name for tool in tools if (tool.metadata or {}).get("mcp_server")}


def test_selection_preserves_unknown_disabled_and_empty_config_values():
    assert AgentConfig(name="worker").mcp_servers is None
    for selected in ([], ["web", "missing", "disabled"]):
        config = AgentConfig(name="worker", mcp_servers=selected)
        assert AgentConfig.model_validate(config.model_dump()).mcp_servers == selected


def test_selection_filters_by_exact_provenance_without_mutating_cache(mcp_selection):
    cached, _, _, reader = mcp_selection
    snapshot = [(tool, deepcopy(tool.metadata)) for tool in cached]
    selected = ["web", "disabled", "missing"]
    assert _mcp_names(tools_module.get_available_tools(mcp_servers=selected)) == {"web_lookup"}
    assert selected == ["web", "disabled", "missing"]
    assert get_deferred_registry().deferred_names == {"web_lookup"}
    assert "No tools found" in tool_search.invoke({"query": "select:web_scraper_lookup"})
    assert _mcp_names(tools_module.get_available_tools(mcp_servers=None)) == {"web_lookup", "web_scraper_lookup"}
    assert all(tool is snapshot[index][0] and tool.metadata == snapshot[index][1] for index, tool in enumerate(cached))
    assert reader.call_count == 2


def test_sdk_additions_preserve_unscoped_legacy_tools_only_without_selection(mcp_selection, monkeypatch):
    cached, _, _, _ = mcp_selection
    monkeypatch.setattr(ExtensionsConfig, "from_file", Mock(side_effect=AssertionError("Private bindings must not read operator configuration")))
    default = tools_module.filter_mcp_tools_by_servers(cached, None)
    assert default == cached and default is not cached
    assert {tool.name for tool in tools_module.filter_mcp_tools_by_servers(cached, ["web"])} == {"web_lookup"}
    assert {tool.name for tool in tools_module.filter_mcp_tools_by_servers(cached, ["disabled"])} == {"disabled_lookup"}
    assert tools_module.filter_mcp_tools_by_servers(cached, []) == []


@pytest.mark.parametrize("selected", [[], ["missing"], ["disabled"]])
def test_unavailable_selection_clears_discovery_without_loading_cache(mcp_selection, selected):
    _, _, _, reader = mcp_selection
    tools_module.get_available_tools()
    parent = get_deferred_registry()
    reader.reset_mock()
    result = tools_module.get_available_tools(mcp_servers=selected)
    assert _mcp_names(result) == set()
    assert not any(tool.name == "tool_search" for tool in result)
    assert get_deferred_registry() is None
    assert "No deferred tools available" in tool_search.invoke({"query": "web"})
    assert parent.deferred_names == {"web_lookup", "web_scraper_lookup"}
    reader.assert_not_called()


def test_reassembly_preserves_current_promotions_and_defers_new_schemas(mcp_selection):
    cached, _, _, _ = mcp_selection
    tools_module.get_available_tools(mcp_servers=["web"])
    tool_search.invoke({"query": "select:web_lookup"})
    original = get_deferred_registry()
    tools_module.get_available_tools(mcp_servers=["web", "web_scraper"])
    assert get_deferred_registry() is not original
    assert get_deferred_registry().deferred_names == {"web_scraper_lookup"}
    assert original.deferred_names == set()
    # A cache reload may change schema without changing the server/tool name.
    cached[0] = _tool("web_lookup", "web")
    tools_module.get_available_tools(mcp_servers=["web"])
    assert get_deferred_registry().deferred_names == {"web_lookup"}


@pytest.mark.asyncio
async def test_concurrent_agent_scopes_do_not_share_discovery_mutations(mcp_selection):
    tools_module.get_available_tools()
    inherited = get_deferred_registry()
    ready = asyncio.Event()
    arrived = 0

    async def run(server: str, own: str, other: str):
        nonlocal arrived
        result = tools_module.get_available_tools(mcp_servers=[server])
        arrived += 1
        if arrived == 2:
            ready.set()
        await ready.wait()
        assert _mcp_names(result) == {own}
        assert "No tools found" in tool_search.invoke({"query": "select:" + other})
        assert own in tool_search.invoke({"query": "select:" + own})
        assert get_deferred_registry().deferred_names == set()

    await asyncio.gather(run("web", "web_lookup", "web_scraper_lookup"), run("web_scraper", "web_scraper_lookup", "web_lookup"))
    assert get_deferred_registry() is inherited
    assert inherited.deferred_names == {"web_lookup", "web_scraper_lookup"}


def test_loader_source_metadata_overrides_provider_claims(monkeypatch):
    original = _tool("unprefixed_lookup", "forged-instance")
    original.metadata["provider_hint"] = "kept"
    monkeypatch.setattr("deerflow.mcp.tools.get_session_pool", lambda: Mock())
    wrapped = _make_session_pool_tool(original, "actual-instance", {}, tool_name_prefix=False)
    assert wrapped.metadata["mcp_server"] == "actual-instance"
    assert wrapped.metadata["mcp_original_tool_name"] == "unprefixed_lookup"
    assert wrapped.metadata["provider_hint"] == "kept"
    assert original.metadata["mcp_server"] == "forged-instance"
