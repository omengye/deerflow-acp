from importlib import import_module

_EXPORTS = {
    "get_checkpointer": "checkpointer",
    "make_checkpointer": "checkpointer",
    "reset_checkpointer": "checkpointer",
    "create_deerflow_agent": "factory",
    "Next": "features",
    "Prev": "features",
    "RuntimeFeatures": "features",
    "SandboxState": "thread_state",
    "ThreadState": "thread_state",
}


def __getattr__(name: str):
    module_name = _EXPORTS.get(name)
    if module_name is None:
        raise AttributeError(f"module {__name__!r} has no attribute {name!r}")
    value = getattr(import_module(f".{module_name}", __name__), name)
    globals()[name] = value
    return value


def make_lead_agent(*args, **kwargs):
    from .lead_agent import make_lead_agent as _make_lead_agent

    return _make_lead_agent(*args, **kwargs)


def prime_enabled_skills_cache() -> None:
    from .lead_agent.prompt import prime_enabled_skills_cache as _prime_enabled_skills_cache

    _prime_enabled_skills_cache()


# LangGraph imports deerflow.agents when registering the graph. Prime the
# enabled-skills cache here so the request path can usually read a warm cache
# without forcing synchronous filesystem work during prompt module import.
prime_enabled_skills_cache()

__all__ = [
    "create_deerflow_agent",
    "RuntimeFeatures",
    "Next",
    "Prev",
    "make_lead_agent",
    "SandboxState",
    "ThreadState",
    "get_checkpointer",
    "reset_checkpointer",
    "make_checkpointer",
]
