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
