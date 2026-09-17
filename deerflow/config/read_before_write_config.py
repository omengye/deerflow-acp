"""Configuration for the deterministic read-before-write file gate."""

from pydantic import BaseModel, Field


class ReadBeforeWriteConfig(BaseModel):
    enabled: bool = Field(
        default=True,
        description=(
            "Block modifications of existing files until their current version "
            "has been read in the retained conversation history"
        ),
    )
    elide_blocked_payloads: bool = Field(
        default=True,
        description=(
            "Replace large payloads from gate-blocked write calls in the "
            "model-bound request while preserving checkpoint history."
        ),
    )
    elide_min_chars: int = Field(
        default=2000,
        ge=0,
        description="Minimum payload character count eligible for elision.",
    )
