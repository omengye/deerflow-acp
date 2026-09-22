"""Expose subagent metadata without starting the execution dependency graph."""

from importlib import import_module
from typing import TYPE_CHECKING, Any

from .config import SubagentConfig

if TYPE_CHECKING:
    from .executor import SubagentExecutor, SubagentResult
    from .registry import get_available_subagent_names, get_subagent_config, list_subagents

__all__ = [
    "SubagentConfig",
    "SubagentExecutor",
    "SubagentResult",
    "get_available_subagent_names",
    "get_subagent_config",
    "list_subagents",
]

_LAZY_EXPORTS = {
    "SubagentExecutor": ".executor",
    "SubagentResult": ".executor",
    "get_available_subagent_names": ".registry",
    "get_subagent_config": ".registry",
    "list_subagents": ".registry",
}


def __getattr__(name: str) -> Any:
    # Settings read the built-in definitions, but never execute an agent.
    module = _LAZY_EXPORTS.get(name)
    if module is None:
        raise AttributeError(f"module {__name__!r} has no attribute {name!r}")
    value = getattr(import_module(module, __name__), name)
    globals()[name] = value
    return value


def __dir__() -> list[str]:
    return sorted(set(globals()) | set(__all__))
