"""Build discovery tools with immutable per-agent visibility restrictions."""

from __future__ import annotations

import json
from collections.abc import Callable
from pathlib import Path

from langchain_core.tools import BaseTool, StructuredTool

from deerflow.config.skills_config import SkillsConfig
from deerflow.skills.catalog import SkillCatalog, catalog_from_signature


def build_explicit_skill_catalog_provider(config: SkillsConfig) -> Callable[[], SkillCatalog]:
    """Read only SDK-supplied skill roots, never application-global config.

    Relative SDK paths are resolved against the caller's current directory at
    construction. All parsed skills are enabled unless the caller explicitly
    supplies an extensions file. Per-agent visibility remains tool-owned.
    """
    snapshot = config.model_copy(deep=True)
    root_values = [snapshot.path] if snapshot.path else snapshot.directories
    if not root_values:
        raise ValueError("SDK skill discovery requires an explicit skills_config.path or directories")
    roots = tuple(Path(value).expanduser().resolve() for value in root_values)
    extensions_path = Path(snapshot.extensions_file).expanduser().resolve() if snapshot.extensions_file else None

    def catalog_provider() -> SkillCatalog:
        if not snapshot.enabled:
            return catalog_from_signature(())
        from deerflow.skills.parser import parse_skill_file

        extensions = None
        if extensions_path is not None:
            from deerflow.config.extensions_config import ExtensionsConfig

            extensions = ExtensionsConfig.from_file(str(extensions_path))
        skills = {}
        for root in roots:
            for category in ("public", "custom"):
                category_path = root / category
                if not category_path.is_dir():
                    continue
                for skill_file in sorted(category_path.rglob("SKILL.md")):
                    if not skill_file.resolve().is_relative_to(root):
                        continue
                    relative = skill_file.parent.relative_to(category_path)
                    if any(part.startswith(".") for part in relative.parts):
                        continue
                    skill = parse_skill_file(skill_file, category=category, relative_path=relative)
                    if skill is not None and (extensions is None or extensions.is_skill_enabled(skill.name, category)):
                        skills[skill.name] = skill
        return catalog_from_signature(tuple(
            (skill.name, skill.description, skill.category, skill.get_container_file_path(snapshot.container_path))
            for skill in sorted(skills.values(), key=lambda value: value.name)
        ))

    return catalog_provider


def build_describe_skill_tool(
    available_skills: set[str] | None = None,
    *,
    catalog_provider: Callable[[], SkillCatalog] | None = None,
) -> BaseTool:
    allowed = frozenset(available_skills) if available_skills is not None else None

    def describe_skill(query: str) -> str:
        """Find skill metadata by task intent or exact skill names, then read its location.

        Args:
            query: Task keywords, a skill name, select:name1,name2, or +name intent.
        """
        # Resolve the current cached snapshot on each call, so disable/evolution
        # changes are visible without mutating another run's scope.
        if catalog_provider is None:
            from deerflow.agents.lead_agent.prompt import get_skill_discovery_catalog

            catalog = get_skill_discovery_catalog(allowed)
        else:
            supplied = catalog_provider()
            # A provider is an input source, not an authorization override.
            catalog = SkillCatalog(tuple(entry for entry in supplied.entries if allowed is None or entry.name in allowed))
        matches = catalog.search(query)
        return json.dumps({"skills": [entry.as_dict() for entry in matches]}, ensure_ascii=False)

    return StructuredTool.from_function(
        func=describe_skill,
        name="describe_skill",
        description="Discover skills by literal task intent. Use select:name1,name2 for explicitly requested skills; then read the returned SKILL.md location. Only this agent's enabled skills are visible.",
        parse_docstring=True,
    )
