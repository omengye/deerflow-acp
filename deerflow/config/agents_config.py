"""Configuration and loaders for custom agents."""

import logging
import re
import unicodedata
from typing import Annotated, Any

import yaml
from pydantic import (
    BaseModel,
    BeforeValidator,
    StringConstraints,
    ValidationError,
    field_validator,
)

from deerflow.config.paths import get_paths

logger = logging.getLogger(__name__)

SOUL_FILENAME = "SOUL.md"
AGENT_NAME_PATTERN = re.compile(r"^[A-Za-z0-9-]+$")


def _validate_display_name(value: object) -> object:
    """Reject labels that can alter logs/UI layout or render invisibly."""
    if isinstance(value, str):
        if re.search(
            r"[\x00-\x1f\x7f-\x9f\u00ad\u061c\u200b\u200e-\u200f"
            r"\u2028-\u202e\u2060-\u2069\ufeff]",
            value,
        ):
            raise ValueError(
                "Display name must not contain control characters or invisible formatting controls"
            )
        if value.strip() and all(
            unicodedata.category(character)[0] in {"C", "M", "Z"}
            for character in value
        ):
            raise ValueError("Display name must contain visible text")
    return value


AgentDisplayName = Annotated[
    str,
    StringConstraints(strip_whitespace=True, max_length=100),
    BeforeValidator(_validate_display_name),
]

_frozen_catalog: dict[str, "AgentConfig"] | None = None
_frozen_souls: dict[str | None, str | None] | None = None


def freeze_agent_catalog() -> None:
    """Pin the portable daemon's profiles until its next explicit restart."""
    global _frozen_catalog, _frozen_souls
    catalog = {agent.name: agent for agent in list_custom_agents()}
    souls = {name: load_agent_soul(name) for name in [None, *catalog]}
    _frozen_catalog = catalog
    _frozen_souls = souls


def validate_agent_name(name: str | None) -> str | None:
    """Validate a custom agent name before using it in filesystem paths."""
    if name is None:
        return None
    if not isinstance(name, str):
        raise ValueError("Invalid agent name. Expected a string or None.")
    if not AGENT_NAME_PATTERN.fullmatch(name):
        raise ValueError(f"Invalid agent name '{name}'. Must match pattern: {AGENT_NAME_PATTERN.pattern}")
    return name


class AgentConfig(BaseModel):
    """Configuration for a custom agent."""

    name: str
    display_name: AgentDisplayName | None = None
    description: str = ""
    model: str | None = None
    tool_groups: list[str] | None = None
    # skills controls which skills are loaded into the agent's prompt:
    # - None (or omitted): load all enabled skills (default fallback behavior)
    # - [] (explicit empty list): disable all skills
    # - ["skill1", "skill2"]: load only the specified skills
    skills: list[str] | None = None
    # Stateless custom agents can opt out of every memory read/write path.
    memory_enabled: bool = True

    @field_validator("display_name")
    @classmethod
    def _blank_display_name_to_none(cls, value: str | None) -> str | None:
        return value or None


def load_agent_config(name: str | None) -> AgentConfig | None:
    """Load the custom or default agent's config from its directory.

    Args:
        name: The agent name.

    Returns:
        AgentConfig instance.

    Raises:
        FileNotFoundError: If the agent directory or config.yaml does not exist.
        ValueError: If config.yaml cannot be parsed.
    """

    if name is None:
        return None

    name = validate_agent_name(name)
    if _frozen_catalog is not None:
        if name not in _frozen_catalog:
            raise FileNotFoundError(f"Agent {name!r} is not in the active configuration; apply saved settings first")
        return _frozen_catalog[name].model_copy(deep=True)
    agent_dir = get_paths().agent_dir(name)
    config_file = agent_dir / "config.yaml"

    if not agent_dir.exists():
        raise FileNotFoundError(f"Agent directory not found: {agent_dir}")

    if not config_file.exists():
        raise FileNotFoundError(f"Agent config not found: {config_file}")

    try:
        with open(config_file, encoding="utf-8") as f:
            data: dict[str, Any] = yaml.safe_load(f) or {}
    except yaml.YAMLError as e:
        raise ValueError(f"Failed to parse agent config {config_file}: {e}") from e

    # Ensure name is set from directory name if not in file
    if "name" not in data:
        data["name"] = name

    # Strip unknown fields before passing to Pydantic (e.g. legacy prompt_file)
    known_fields = set(AgentConfig.model_fields.keys())
    data = {k: v for k, v in data.items() if k in known_fields}

    try:
        return AgentConfig(**data)
    except ValidationError as original_error:
        # A stale/invalid label must not hide an otherwise healthy agent. Drop
        # only that presentation field; validation of every identity/runtime
        # field still runs and remains authoritative.
        if "display_name" not in data:
            raise
        without_display_name = dict(data)
        without_display_name.pop("display_name", None)
        try:
            return AgentConfig(**without_display_name)
        except ValidationError:
            raise original_error


def load_agent_soul(agent_name: str | None) -> str | None:
    """Read the SOUL.md file for a custom agent, if it exists.

    SOUL.md defines the agent's personality, values, and behavioral guardrails.
    It is injected into the lead agent's system prompt as additional context.

    Args:
        agent_name: The name of the agent or None for the default agent.

    Returns:
        The SOUL.md content as a string, or None if the file does not exist.
    """
    if _frozen_souls is not None:
        return _frozen_souls.get(agent_name)
    agent_dir = get_paths().agent_dir(agent_name) if agent_name else get_paths().base_dir
    soul_path = agent_dir / SOUL_FILENAME
    if not soul_path.exists():
        return None
    content = soul_path.read_text(encoding="utf-8").strip()
    return content or None


def list_custom_agents() -> list[AgentConfig]:
    """Scan the agents directory and return all valid custom agents.

    Returns:
        List of AgentConfig for each valid agent directory found.
    """
    if _frozen_catalog is not None:
        return [agent.model_copy(deep=True) for agent in _frozen_catalog.values()]
    agents_dir = get_paths().agents_dir

    if not agents_dir.exists():
        return []

    agents: list[AgentConfig] = []

    for entry in sorted(agents_dir.iterdir()):
        if not entry.is_dir():
            continue

        config_file = entry / "config.yaml"
        if not config_file.exists():
            logger.debug(f"Skipping {entry.name}: no config.yaml")
            continue

        try:
            agent_cfg = load_agent_config(entry.name)
            agents.append(agent_cfg)
        except Exception as e:
            logger.warning(f"Skipping agent '{entry.name}': {e}")

    return agents
