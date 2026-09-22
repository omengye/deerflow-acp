"""Skill metadata stays usable without loading the model-backed installer."""

from importlib import import_module
from typing import TYPE_CHECKING, Any

from .loader import get_skills_root_path, load_skills
from .types import Skill
from .validation import ALLOWED_FRONTMATTER_PROPERTIES, _validate_skill_frontmatter

if TYPE_CHECKING:
    from .installer import SkillAlreadyExistsError, SkillSecurityScanError, ainstall_skill_from_archive, install_skill_from_archive

__all__ = [
    "load_skills",
    "get_skills_root_path",
    "Skill",
    "ALLOWED_FRONTMATTER_PROPERTIES",
    "_validate_skill_frontmatter",
    "install_skill_from_archive",
    "ainstall_skill_from_archive",
    "SkillAlreadyExistsError",
    "SkillSecurityScanError",
]

_INSTALLER_EXPORTS = {
    "install_skill_from_archive",
    "ainstall_skill_from_archive",
    "SkillAlreadyExistsError",
    "SkillSecurityScanError",
}


def __getattr__(name: str) -> Any:
    # Importing a parser submodule executes this package first. Desktop settings
    # only need metadata; security scanning is loaded when installation is used.
    if name not in _INSTALLER_EXPORTS:
        raise AttributeError(f"module {__name__!r} has no attribute {name!r}")
    value = getattr(import_module(".installer", __name__), name)
    globals()[name] = value
    return value


def __dir__() -> list[str]:
    return sorted(set(globals()) | set(__all__))
