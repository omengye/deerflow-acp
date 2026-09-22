"""Immutable construction-time policy carried by an SDK-owned native task tool."""

from collections.abc import Callable
from dataclasses import dataclass

from deerflow.config.pii_redaction_config import PiiRedactionConfig
from deerflow.skills.catalog import SkillCatalog


@dataclass(frozen=True)
class SdkTaskPolicy:
    skills_explicit: bool = False
    skill_catalog_provider: Callable[[], SkillCatalog] | None = None
    available_skills: frozenset[str] | None = None
    skill_discovery_enabled: bool = False
    pii_redaction: PiiRedactionConfig | None = None
