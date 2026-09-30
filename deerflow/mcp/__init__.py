"""MCP (Model Context Protocol) integration using langchain-mcp-adapters."""

from importlib import import_module


_EXPORTS = {
    "build_server_params": "client",
    "build_servers_config": "client",
    "get_mcp_tools": "tools",
    "initialize_mcp_tools": "cache",
    "get_cached_mcp_tools": "cache",
    "reset_mcp_tools_cache": "cache",
}

__all__ = [
    "build_server_params",
    "build_servers_config",
    "get_mcp_tools",
    "initialize_mcp_tools",
    "get_cached_mcp_tools",
    "reset_mcp_tools_cache",
]


def __getattr__(name: str):
    """Load optional MCP clients and tools only when their API is used."""
    module_name = _EXPORTS.get(name)
    if module_name is None:
        raise AttributeError(f"module {__name__!r} has no attribute {name!r}")
    value = getattr(import_module(f".{module_name}", __name__), name)
    globals()[name] = value
    return value
