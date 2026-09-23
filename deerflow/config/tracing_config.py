import os
import threading
from typing import Any

from pydantic import BaseModel, Field

_config_lock = threading.Lock()


class LangSmithTracingConfig(BaseModel):
    """Configuration for LangSmith tracing."""

    enabled: bool = Field(...)
    api_key: str | None = Field(...)
    project: str = Field(...)
    endpoint: str = Field(...)

    @property
    def is_configured(self) -> bool:
        return self.enabled and bool(self.api_key)

    def validate_enabled(self) -> None:
        if self.enabled and not self.api_key:
            raise ValueError("LangSmith tracing is enabled but LANGSMITH_API_KEY (or LANGCHAIN_API_KEY) is not set.")


class LangfuseTracingConfig(BaseModel):
    """Configuration for Langfuse tracing."""

    enabled: bool = Field(...)
    public_key: str | None = Field(...)
    secret_key: str | None = Field(...)
    host: str = Field(...)

    @property
    def is_configured(self) -> bool:
        return self.enabled and bool(self.public_key) and bool(self.secret_key)

    def validate_enabled(self) -> None:
        if not self.enabled:
            return
        missing: list[str] = []
        if not self.public_key:
            missing.append("LANGFUSE_PUBLIC_KEY")
        if not self.secret_key:
            missing.append("LANGFUSE_SECRET_KEY")
        if missing:
            raise ValueError(f"Langfuse tracing is enabled but required settings are missing: {', '.join(missing)}")


class TracingConfig(BaseModel):
    """Tracing configuration for supported providers."""

    langsmith: LangSmithTracingConfig = Field(...)
    langfuse: LangfuseTracingConfig = Field(...)

    @property
    def is_configured(self) -> bool:
        return bool(self.enabled_providers)

    @property
    def explicitly_enabled_providers(self) -> list[str]:
        enabled: list[str] = []
        if self.langsmith.enabled:
            enabled.append("langsmith")
        if self.langfuse.enabled:
            enabled.append("langfuse")
        return enabled

    @property
    def enabled_providers(self) -> list[str]:
        enabled: list[str] = []
        if self.langsmith.is_configured:
            enabled.append("langsmith")
        if self.langfuse.is_configured:
            enabled.append("langfuse")
        return enabled

    def validate_enabled(self) -> None:
        self.langsmith.validate_enabled()
        self.langfuse.validate_enabled()


_tracing_config: TracingConfig | None = None
_config_generation = 0


_TRUTHY_VALUES = {"1", "true", "yes", "on"}


def _env_flag_preferred(*names: str) -> bool:
    """Return the boolean value of the first env var that is present and non-empty."""
    for name in names:
        value = os.environ.get(name)
        if value is not None and value.strip():
            return value.strip().lower() in _TRUTHY_VALUES
    return False


def _first_env_value(*names: str) -> str | None:
    """Return the first non-empty environment value from candidate names."""
    for name in names:
        value = os.environ.get(name)
        if value and value.strip():
            return value.strip()
    return None


def _config_section() -> dict[str, Any]:
    """Return the tracing section from config.yaml, if it is available."""
    try:
        from deerflow.config.app_config import get_app_config

        raw = getattr(get_app_config(), "tracing", None)
    except Exception:
        return {}
    if not isinstance(raw, dict):
        return {}
    return raw


def _config_provider(section: dict[str, Any], name: str) -> dict[str, Any]:
    raw = section.get(name) or {}
    return raw if isinstance(raw, dict) else {}


def _flag_from_config(provider: dict[str, Any], key: str, *env_names: str) -> bool:
    value = provider.get(key)
    if value is None:
        return _env_flag_preferred(*env_names)
    if isinstance(value, bool):
        return value
    return str(value).strip().lower() in _TRUTHY_VALUES


def _value_from_config(provider: dict[str, Any], key: str, *env_names: str, default: str | None = None) -> str | None:
    value = provider.get(key)
    if value is None:
        return _first_env_value(*env_names) or default
    if isinstance(value, str):
        if value.startswith("$"):
            return _first_env_value(value[1:], *env_names) or default
        if value.strip():
            return value.strip()
        return default
    return str(value)


def get_tracing_config() -> TracingConfig:
    """Get the current tracing configuration from config.yaml with env fallback."""
    global _tracing_config
    while True:
        with _config_lock:
            if _tracing_config is not None:
                return _tracing_config
            generation = _config_generation

        # AppConfig loading can invalidate this cache. Never hold the tracing
        # lock while calling it, including on the first configuration read.
        section = _config_section()
        with _config_lock:
            # A reload while the lock was released makes this snapshot stale.
            if generation != _config_generation:
                continue
            if _tracing_config is not None:
                return _tracing_config
            langsmith = _config_provider(section, "langsmith")
            langfuse = _config_provider(section, "langfuse")
            _tracing_config = TracingConfig(
                langsmith=LangSmithTracingConfig(
                    enabled=_flag_from_config(langsmith, "enabled", "LANGSMITH_TRACING", "LANGCHAIN_TRACING_V2", "LANGCHAIN_TRACING"),
                    api_key=_value_from_config(langsmith, "api_key", "LANGSMITH_API_KEY", "LANGCHAIN_API_KEY"),
                    project=_value_from_config(langsmith, "project", "LANGSMITH_PROJECT", "LANGCHAIN_PROJECT", default="deer-flow") or "deer-flow",
                    endpoint=_value_from_config(langsmith, "endpoint", "LANGSMITH_ENDPOINT", "LANGCHAIN_ENDPOINT", default="https://api.smith.langchain.com") or "https://api.smith.langchain.com",
                ),
                langfuse=LangfuseTracingConfig(
                    enabled=_flag_from_config(langfuse, "enabled", "LANGFUSE_TRACING"),
                    public_key=_value_from_config(langfuse, "public_key", "LANGFUSE_PUBLIC_KEY"),
                    secret_key=_value_from_config(langfuse, "secret_key", "LANGFUSE_SECRET_KEY"),
                    host=_value_from_config(langfuse, "host", "LANGFUSE_BASE_URL", default="https://cloud.langfuse.com") or "https://cloud.langfuse.com",
                ),
            )
            return _tracing_config


def reset_tracing_config() -> None:
    """Reset the cached tracing config instance. Useful for tests/reloads."""
    global _tracing_config, _config_generation
    with _config_lock:
        _tracing_config = None
        _config_generation += 1


def get_enabled_tracing_providers() -> list[str]:
    """Return the configured tracing providers that are enabled and complete."""
    return get_tracing_config().enabled_providers


def get_explicitly_enabled_tracing_providers() -> list[str]:
    """Return tracing providers explicitly enabled by config, even if incomplete."""
    return get_tracing_config().explicitly_enabled_providers


def validate_enabled_tracing_providers() -> None:
    """Validate that any explicitly enabled providers are fully configured."""
    get_tracing_config().validate_enabled()


def is_tracing_enabled() -> bool:
    """Check if any tracing provider is enabled and fully configured."""
    return get_tracing_config().is_configured
