"""Single-user, review-first Skill evolution services.

Importing a lightweight evolution helper during agent startup must not load
the proposal generator, evaluator, or security scanner until those APIs are
actually used.
"""

from importlib import import_module

_EXPORTS = {
    "EvolutionSignal": "models",
    "SkillProposal": "models",
    "ToolErrorDetail": "models",
    "SkillPublishConflict": "publisher",
    "SkillPublisher": "publisher",
    "SkillEvolutionService": "service",
    "FileEvolutionStore": "store",
    "get_evolution_store": "store",
}

__all__ = list(_EXPORTS)


def __getattr__(name: str):
    module_name = _EXPORTS.get(name)
    if module_name is None:
        raise AttributeError(f"module {__name__!r} has no attribute {name!r}")
    value = getattr(import_module(f".{module_name}", __name__), name)
    globals()[name] = value
    return value
