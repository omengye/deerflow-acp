"""Schemas for API requests/responses."""
from typing import Any, Literal, Optional
from pydantic import BaseModel, Field
from deerflow.community.ragflow.scope import KnowledgeScope


# --- Chat ---
class ChatRequest(BaseModel):
    message: str = Field(min_length=1, max_length=200_000)
    thread_id: Optional[str] = Field(default=None, max_length=128)
    model_name: Optional[str] = None
    thinking_enabled: Optional[bool] = None
    subagent_enabled: Optional[bool] = None
    plan_mode: Optional[bool] = None
    max_concurrent_subagents: Optional[int] = Field(default=None, ge=2, le=4)
    multitask_strategy: Optional[Literal["reject", "interrupt", "rollback"]] = None
    on_disconnect: Optional[Literal["cancel", "continue"]] = None
    knowledge_scope: KnowledgeScope | None = None


class AguiMessage(BaseModel):
    id: str
    role: str
    content: Optional[str] = None
    name: Optional[str] = None
    tool_call_id: Optional[str] = Field(default=None, alias="toolCallId")


class AguiRunAgentInput(BaseModel):
    thread_id: str = Field(alias="threadId")
    run_id: str = Field(alias="runId")
    parent_run_id: Optional[str] = Field(default=None, alias="parentRunId")
    state: dict[str, Any] = Field(default_factory=dict)
    messages: list[AguiMessage] = Field(default_factory=list)
    tools: list[dict[str, Any]] = Field(default_factory=list)
    context: list[dict[str, Any]] = Field(default_factory=list)
    forwarded_props: dict[str, Any] = Field(default_factory=dict, alias="forwardedProps")
    model_name: Optional[str] = Field(default=None, alias="modelName")
    agent_name: Optional[str] = Field(default=None, alias="agentName")
    thinking_enabled: Optional[bool] = Field(default=None, alias="thinkingEnabled")
    subagent_enabled: Optional[bool] = Field(default=None, alias="subagentEnabled")
    plan_mode: Optional[bool] = Field(default=None, alias="planMode")
    max_concurrent_subagents: Optional[int] = Field(default=None, ge=2, le=4, alias="maxConcurrentSubagents")
    multitask_strategy: Optional[Literal["reject", "interrupt", "rollback"]] = Field(default=None, alias="multitaskStrategy")
    on_disconnect: Optional[Literal["cancel", "continue"]] = Field(default=None, alias="onDisconnect")
    knowledge_scope: KnowledgeScope | None = Field(default=None, alias="knowledgeScope")


# --- Threads ---
class ThreadResponse(BaseModel):
    thread_id: str
    title: Optional[str] = None
    created_at: Optional[str] = None


class ThreadDetail(BaseModel):
    thread_id: str
    messages: list[dict[str, Any]]
    artifacts: list[Any] = Field(default_factory=list)
    title: Optional[str] = None
    status: Literal["idle", "running"] = "idle"


class ThreadStatusResponse(BaseModel):
    thread_id: str
    status: Literal["idle", "running"]
    title: Optional[str] = None


# --- Models ---
class ModelInfo(BaseModel):
    name: str
    display_name: str
    supports_thinking: bool
    supports_vision: bool


class ModelListResponse(BaseModel):
    models: list[ModelInfo]


# --- Skills ---
class SkillInfo(BaseModel):
    name: str
    display_name: str
    description: str
    category: Literal["public", "custom"] = "public"
    enabled: bool


class SkillListResponse(BaseModel):
    skills: list[SkillInfo]


# --- MCP ---
class MCPConfigResponse(BaseModel):
    mcp_servers: dict[str, Any]
