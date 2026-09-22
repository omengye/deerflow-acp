"""Opt-in redaction of common sensitive text at supported model boundaries."""

from pydantic import BaseModel, ConfigDict


class PiiRedactionConfig(BaseModel):
    enabled: bool = False
    email: bool = True
    phone: bool = True
    bank_card: bool = True
    chinese_id: bool = True
    api_key: bool = True

    model_config = ConfigDict(extra="forbid")
