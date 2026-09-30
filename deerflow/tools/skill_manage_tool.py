"""Tool for submitting reviewable custom Skill evolution proposals."""

from __future__ import annotations

from langchain.tools import ToolRuntime, tool

from deerflow.agents.thread_state import AgentContext, ThreadState
from deerflow.tools.sync import make_sync_tool_wrapper


def _get_thread_id(runtime: ToolRuntime[AgentContext, ThreadState] | None) -> str | None:
    if runtime is None:
        return None
    if runtime.context and runtime.context.get("thread_id"):
        return runtime.context.get("thread_id")
    return runtime.config.get("configurable", {}).get("thread_id")


async def _skill_manage_impl(
    runtime: ToolRuntime[AgentContext, ThreadState],
    action: str,
    name: str,
    content: str | None = None,
    path: str | None = None,
    find: str | None = None,
    replace: str | None = None,
    expected_count: int | None = None,
    reason: str | None = None,
) -> str:
    """Submit a review proposal for a custom Skill.

    Args:
        action: One of create, patch, edit, delete, write_file, remove_file.
        name: Skill name in hyphen-case.
        content: New file content for create, edit, or write_file.
        path: Supporting file path for write_file or remove_file.
        find: Existing text to replace for patch.
        replace: Replacement text for patch.
        expected_count: Optional expected number of replacements for patch.
        reason: Short explanation of the reusable improvement and supporting evidence.
    """
    from deerflow.skills.evolution import SkillEvolutionService

    proposal = await SkillEvolutionService().create_proposal(
        action=action,
        name=name,
        content=content,
        path=path,
        find=find,
        replace=replace,
        expected_count=expected_count,
        reason=reason,
        thread_id=_get_thread_id(runtime),
    )
    return (
        f"Created Skill proposal '{proposal.id}' for '{proposal.skill_name}'. "
        f"Status: {proposal.status}. Risk: {proposal.risk}. "
        "No active Skill files were changed; Admin review is required."
    )


@tool("skill_manage", parse_docstring=True)
async def skill_manage_tool(
    runtime: ToolRuntime[AgentContext, ThreadState],
    action: str,
    name: str,
    content: str | None = None,
    path: str | None = None,
    find: str | None = None,
    replace: str | None = None,
    expected_count: int | None = None,
    reason: str | None = None,
) -> str:
    """Submit a review proposal for a custom Skill without publishing it.

    Args:
        action: One of create, patch, edit, delete, write_file, remove_file.
        name: Skill name in hyphen-case.
        content: New file content for create, edit, or write_file.
        path: Supporting file path for write_file or remove_file.
        find: Existing text to replace for patch.
        replace: Replacement text for patch.
        expected_count: Optional expected number of replacements for patch.
        reason: Short explanation of why this is a reusable improvement.
    """
    return await _skill_manage_impl(
        runtime=runtime,
        action=action,
        name=name,
        content=content,
        path=path,
        find=find,
        replace=replace,
        expected_count=expected_count,
        reason=reason,
    )


skill_manage_tool.func = make_sync_tool_wrapper(_skill_manage_impl, "skill_manage")
