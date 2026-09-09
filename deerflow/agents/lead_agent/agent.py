import logging
from collections.abc import Mapping
from typing import Any, NotRequired, TypedDict, cast

from langchain.agents import create_agent
from langchain.agents.middleware import AgentMiddleware
from langchain.agents.middleware.summarization import ContextSize as SummarizationContextSize
from langchain.chat_models import BaseChatModel
from langchain_core.runnables import RunnableConfig

from deerflow.agents.lead_agent.prompt import apply_prompt_template
from deerflow.agents.memory.summarization_hook import memory_flush_hook
from deerflow.agents.middlewares.clarification_middleware import ClarificationMiddleware
from deerflow.agents.middlewares.configured_extensions import load_configured_middlewares
from deerflow.agents.middlewares.loop_detection_middleware import LoopDetectionMiddleware, calibrate_loop_detection
from deerflow.agents.middlewares.memory_middleware import MemoryMiddleware
from deerflow.agents.middlewares.runtime_headers_middleware import RuntimeHeadersMiddleware
from deerflow.agents.middlewares.evolution_signal_middleware import EvolutionSignalMiddleware
from deerflow.agents.middlewares.subagent_limit_middleware import SubagentLimitMiddleware, clamp_subagent_limit
from deerflow.agents.middlewares.summarization_middleware import BeforeSummarizationHook, DeerFlowSummarizationMiddleware
from deerflow.agents.middlewares.title_middleware import TitleMiddleware
from deerflow.agents.middlewares.todo_middleware import TodoMiddleware
from deerflow.agents.middlewares.token_usage_middleware import TokenUsageMiddleware
from deerflow.agents.middlewares.tool_error_handling_middleware import build_lead_runtime_middlewares
from deerflow.agents.middlewares.view_image_middleware import ViewImageMiddleware
from deerflow.agents.thread_state import (
    AgentContext,
    get_thread_state_schema,
    normalize_middleware_state_schemas,
)
from deerflow.config.agents_config import load_agent_config, validate_agent_name
from deerflow.config.app_config import get_app_config
from deerflow.config.memory_config import get_memory_config
from deerflow.config.summarization_config import DEFAULT_KEEP, get_summarization_config
from deerflow.models import create_chat_model

logger = logging.getLogger(__name__)


class _SummarizationMiddlewareKwargs(TypedDict):
    model: BaseChatModel
    trigger: SummarizationContextSize | list[SummarizationContextSize] | None
    keep: SummarizationContextSize
    trim_tokens_to_summarize: NotRequired[int]
    summary_prompt: NotRequired[str]


def _get_runtime_config(config: RunnableConfig) -> dict:
    """Merge legacy configurable options with LangGraph runtime context."""
    cfg = dict(config.get("configurable", {}) or {})
    context = config.get("context", {}) or {}
    if isinstance(context, dict):
        cfg.update(context)
    return cfg


def _resolve_model_name(requested_model_name: str | None = None, fallback_model_name: str | None = None) -> str:
    """Resolve a runtime model name safely, falling back through agent/default config."""
    app_config = get_app_config()
    default_model_name = app_config.get_default_model_name()

    if requested_model_name and app_config.get_model_config(requested_model_name):
        return requested_model_name
    if fallback_model_name and app_config.get_model_config(fallback_model_name):
        if requested_model_name and requested_model_name != fallback_model_name:
            logger.warning(f"Model '{requested_model_name}' not found in config; fallback to agent model '{fallback_model_name}'.")
        return fallback_model_name

    if requested_model_name and requested_model_name != default_model_name:
        logger.warning(f"Model '{requested_model_name}' not found in config; fallback to default model '{default_model_name}'.")
    if fallback_model_name and fallback_model_name != default_model_name:
        logger.warning(f"Agent model '{fallback_model_name}' not found in config; fallback to default model '{default_model_name}'.")
    return default_model_name


def _model_profile_max_input_tokens(model: Any) -> int | None:
    profile = getattr(model, "profile", None)
    if not isinstance(profile, Mapping):
        return None
    value = profile.get("max_input_tokens")
    return value if isinstance(value, int) and value > 0 else None


def _drop_unusable_fraction_clauses(
    model: Any,
    trigger: SummarizationContextSize | list[SummarizationContextSize] | None,
    keep: SummarizationContextSize,
) -> tuple[
    SummarizationContextSize | list[SummarizationContextSize] | None,
    SummarizationContextSize,
    bool,
]:
    clauses = list(trigger) if isinstance(trigger, list) else ([] if trigger is None else [trigger])
    has_fraction_trigger = any(
        isinstance(clause, tuple) and clause[0] == "fraction"
        for clause in clauses
    )
    keep_is_fraction = isinstance(keep, tuple) and keep[0] == "fraction"
    if not (has_fraction_trigger or keep_is_fraction):
        return trigger, keep, True
    if _model_profile_max_input_tokens(model) is not None:
        return trigger, keep, True

    surviving = [
        clause
        for clause in clauses
        if not (isinstance(clause, tuple) and clause[0] == "fraction")
    ]
    dropped = [clause for clause in clauses if clause not in surviving]
    if dropped:
        logger.warning(
            "Dropped summarization fraction trigger clause(s) %s because the "
            "summary model exposes no context window; declare context_window or "
            "use an absolute token/message threshold.",
            dropped,
        )

    effective_keep = keep
    if keep_is_fraction:
        effective_keep = cast("SummarizationContextSize", DEFAULT_KEEP)
        logger.warning(
            "Summarization fraction keep %s is unusable without a model context "
            "window; falling back to %s.",
            keep,
            effective_keep,
        )

    if not surviving:
        return None, effective_keep, not clauses
    effective_trigger = surviving if isinstance(trigger, list) else surviving[0]
    return effective_trigger, effective_keep, True


def _create_summarization_middleware(run_model_name: str | None = None) -> DeerFlowSummarizationMiddleware | None:
    """Create and configure the summarization middleware from config.

    Args:
        run_model_name: The model resolved for this run (see `make_lead_agent`).
            Used as the summarization model when no explicit
            `summarization.model_name` is configured, so a per-run model
            choice (and its provider) is what actually summarizes that run's
            history instead of unconditionally landing on `models[0]`.
    """
    config = get_summarization_config()

    if not config.enabled:
        return None

    # Prepare trigger parameter
    trigger: SummarizationContextSize | list[SummarizationContextSize] | None = None
    if config.trigger is not None:
        if isinstance(config.trigger, list):
            trigger = [cast("SummarizationContextSize", item.to_tuple()) for item in config.trigger]
        else:
            trigger = cast("SummarizationContextSize", config.trigger.to_tuple())

    # Prepare keep parameter
    keep = cast("SummarizationContextSize", config.keep.to_tuple())

    app_config = get_app_config()
    default_model_name = app_config.get_default_model_name()

    # Resolve which model summarizes this agent's turns:
    #   1. An explicit config.model_name always wins, provided it names a
    #      configured model. `admin.py` validates writes through the API, but
    #      config.yaml can still be hand-edited with a stale/typo'd name, and
    #      that must degrade like every other tier here instead of hard-
    #      failing lead-agent construction (create_chat_model raises on an
    #      unknown name).
    #   2. Otherwise follow the current run's model, provided it resolves to a
    #      configured model — `client.py`'s model_name may not have been
    #      validated yet, so don't trust it blindly.
    #   3. Fall back to the configured default model if neither applies.
    valid_run_model_name = run_model_name if run_model_name and app_config.get_model_config(run_model_name) else None
    if run_model_name and valid_run_model_name is None:
        logger.warning(f"Run model '{run_model_name}' not found in config; summarization falls back to the default model.")

    valid_config_model_name = config.model_name if config.model_name and app_config.get_model_config(config.model_name) else None
    if config.model_name and valid_config_model_name is None:
        fallback_description = f"run model '{valid_run_model_name}'" if valid_run_model_name else f"default model '{default_model_name}'"
        logger.warning(f"Configured summarization model '{config.model_name}' not found in config; falling back to {fallback_description}.")

    primary_model_name = valid_config_model_name or valid_run_model_name

    # The summarization middleware is built once per lead-agent instance and
    # reused for every summarisation turn — same long-lived-pool risk as the
    # lead agent itself, so opt out of keep-alive consistently.
    model = create_chat_model(name=primary_model_name, thinking_enabled=False, disable_keepalive=True)

    trigger, keep, has_usable_trigger = _drop_unusable_fraction_clauses(
        model,
        trigger,
        keep,
    )
    if not has_usable_trigger:
        logger.warning(
            "Every configured summarization trigger is fraction-based, but the "
            "summary model exposes no context window; auto-compaction will not "
            "fire for this agent build."
        )

    # Second-chance model for when the primary summarization model errors out
    # (see DeerFlowSummarizationMiddleware's tiered fallback). Only meaningful
    # when it names a different, valid model than the primary one — lazily
    # constructed inside the middleware, and only on first failure.
    effective_primary_model_name = primary_model_name or default_model_name
    fallback_model_name = valid_run_model_name if valid_run_model_name and valid_run_model_name != effective_primary_model_name else None

    # Prepare kwargs
    kwargs: _SummarizationMiddlewareKwargs = {
        "model": model,
        "trigger": trigger,
        "keep": keep,
    }

    if config.trim_tokens_to_summarize is not None:
        kwargs["trim_tokens_to_summarize"] = config.trim_tokens_to_summarize

    if config.summary_prompt is not None:
        kwargs["summary_prompt"] = config.summary_prompt

    hooks: list[BeforeSummarizationHook] = []
    if get_memory_config().enabled:
        hooks.append(memory_flush_hook)

    # The logic below relies on two assumptions holding true: this factory is
    # the sole entry point for DeerFlowSummarizationMiddleware, and the runtime
    # config is not expected to change after startup.
    try:
        skills_container_path = app_config.skills.container_path or "/mnt/skills"
    except Exception:
        logger.exception("Failed to resolve skills container path; falling back to default")
        skills_container_path = "/mnt/skills"

    return DeerFlowSummarizationMiddleware(
        **kwargs,
        skills_container_path=skills_container_path,
        skill_file_read_tool_names=config.skill_file_read_tool_names,
        before_summarization=hooks,
        preserve_recent_skill_count=config.preserve_recent_skill_count,
        preserve_recent_skill_tokens=config.preserve_recent_skill_tokens,
        preserve_recent_skill_tokens_per_skill=config.preserve_recent_skill_tokens_per_skill,
        fallback_model_name=fallback_model_name,
    )


def _create_todo_list_middleware(is_plan_mode: bool) -> TodoMiddleware | None:
    """Create and configure the TodoList middleware.

    Args:
        is_plan_mode: Whether to enable plan mode with TodoList middleware.

    Returns:
        TodoMiddleware instance if plan mode is enabled, None otherwise.
    """
    if not is_plan_mode:
        return None

    # Custom prompts matching DeerFlow's style
    system_prompt = """
<todo_list_system>
You have access to the `write_todos` tool to help you manage and track complex multi-step objectives.

**CRITICAL RULES:**
- Mark todos as completed IMMEDIATELY after finishing each step - do NOT batch completions
- Keep EXACTLY ONE task as `in_progress` at any time (unless tasks can run in parallel)
- Update the todo list in REAL-TIME as you work - this gives users visibility into your progress
- DO NOT use this tool for simple tasks (< 3 steps) - just complete them directly

**When to Use:**
This tool is designed for complex objectives that require systematic tracking:
- Complex multi-step tasks requiring 3+ distinct steps
- Non-trivial tasks needing careful planning and execution
- User explicitly requests a todo list
- User provides multiple tasks (numbered or comma-separated list)
- The plan may need revisions based on intermediate results

**When NOT to Use:**
- Single, straightforward tasks
- Trivial tasks (< 3 steps)
- Purely conversational or informational requests
- Simple tool calls where the approach is obvious

**Best Practices:**
- Break down complex tasks into smaller, actionable steps
- Use clear, descriptive task names
- Remove tasks that become irrelevant
- Add new tasks discovered during implementation
- Don't be afraid to revise the todo list as you learn more

**Task Management:**
Writing todos takes time and tokens - use it when helpful for managing complex problems, not for simple requests.
</todo_list_system>
"""

    tool_description = """Use this tool to create and manage a structured task list for complex work sessions.

**IMPORTANT: Only use this tool for complex tasks (3+ steps). For simple requests, just do the work directly.**

## When to Use

Use this tool in these scenarios:
1. **Complex multi-step tasks**: When a task requires 3 or more distinct steps or actions
2. **Non-trivial tasks**: Tasks requiring careful planning or multiple operations
3. **User explicitly requests todo list**: When the user directly asks you to track tasks
4. **Multiple tasks**: When users provide a list of things to be done
5. **Dynamic planning**: When the plan may need updates based on intermediate results

## When NOT to Use

Skip this tool when:
1. The task is straightforward and takes less than 3 steps
2. The task is trivial and tracking provides no benefit
3. The task is purely conversational or informational
4. It's clear what needs to be done and you can just do it

## How to Use

1. **Starting a task**: Mark it as `in_progress` BEFORE beginning work
2. **Completing a task**: Mark it as `completed` IMMEDIATELY after finishing
3. **Updating the list**: Add new tasks, remove irrelevant ones, or update descriptions as needed
4. **Multiple updates**: You can make several updates at once (e.g., complete one task and start the next)

## Task States

- `pending`: Task not yet started
- `in_progress`: Currently working on (can have multiple if tasks run in parallel)
- `completed`: Task finished successfully

## Task Completion Requirements

**CRITICAL: Only mark a task as completed when you have FULLY accomplished it.**

Never mark a task as completed if:
- There are unresolved issues or errors
- Work is partial or incomplete
- You encountered blockers preventing completion
- You couldn't find necessary resources or dependencies
- Quality standards haven't been met

If blocked, keep the task as `in_progress` and create a new task describing what needs to be resolved.

## Best Practices

- Create specific, actionable items
- Break complex tasks into smaller, manageable steps
- Use clear, descriptive task names
- Update task status in real-time as you work
- Mark tasks complete IMMEDIATELY after finishing (don't batch completions)
- Remove tasks that are no longer relevant
- **IMPORTANT**: When you write the todo list, mark your first task(s) as `in_progress` immediately
- **IMPORTANT**: Unless all tasks are completed, always have at least one task `in_progress` to show progress

Being proactive with task management demonstrates thoroughness and ensures all requirements are completed successfully.

**Remember**: If you only need a few tool calls to complete a task and it's clear what to do, it's better to just do the task directly and NOT use this tool at all.
"""

    return TodoMiddleware(system_prompt=system_prompt, tool_description=tool_description)


# ThreadDataMiddleware must be before SandboxMiddleware to ensure thread_id is available
# UploadsMiddleware should be after ThreadDataMiddleware to access thread_id
# DanglingToolCallMiddleware patches missing ToolMessages before model sees the history
# SummarizationMiddleware should be early to reduce context before other processing
# TodoListMiddleware should be before ClarificationMiddleware to allow todo management
# TitleMiddleware generates title after first exchange
# MemoryMiddleware queues conversation for memory update (after TitleMiddleware)
# EvolutionSignalMiddleware observes only the latest completed turn (after MemoryMiddleware)
# ViewImageMiddleware should be before ClarificationMiddleware to inject image details before LLM
# ToolErrorHandlingMiddleware should be before ClarificationMiddleware to convert tool exceptions to ToolMessages
# ClarificationMiddleware should be last to intercept clarification requests after model calls
def _build_middlewares(
    config: RunnableConfig,
    model_name: str | None,
    agent_name: str | None = None,
    custom_middlewares: list[AgentMiddleware] | None = None,
    recursion_limit: int | None = None,
) -> list[AgentMiddleware[Any, Any, Any]]:
    """Build middleware chain based on runtime configuration.

    Args:
        config: Runtime configuration containing configurable options like is_plan_mode.
        agent_name: If provided, MemoryMiddleware will use per-agent memory storage.
        custom_middlewares: Optional list of custom middlewares to inject into the chain.
        recursion_limit: Graph recursion limit this agent runs under; used to
            keep LoopDetectionMiddleware's per-run backstop in lock-step.

    Returns:
        List of middleware instances.
    """
    middlewares: list[AgentMiddleware[Any, Any, Any]] = list(build_lead_runtime_middlewares(lazy_init=True))

    # Add summarization middleware if enabled
    summarization_middleware = _create_summarization_middleware(run_model_name=model_name)
    if summarization_middleware is not None:
        middlewares.append(summarization_middleware)

    # Add TodoList middleware if plan mode is enabled
    cfg = _get_runtime_config(config)
    is_plan_mode = cfg.get("is_plan_mode", False)
    todo_list_middleware = _create_todo_list_middleware(is_plan_mode)
    if todo_list_middleware is not None:
        middlewares.append(todo_list_middleware)

    # Add TokenUsageMiddleware when token_usage tracking is enabled
    if get_app_config().token_usage.enabled:
        middlewares.append(TokenUsageMiddleware())

    # Add TitleMiddleware
    middlewares.append(TitleMiddleware())

    # Add MemoryMiddleware (after TitleMiddleware)
    middlewares.append(MemoryMiddleware(agent_name=agent_name))

    # Collect lightweight improvement signals and probation outcomes. Candidate
    # generation runs later on the single background worker, never in-response.
    if get_app_config().skill_evolution.enabled:
        middlewares.append(EvolutionSignalMiddleware())

    # Add ViewImageMiddleware only if the current model supports vision.
    # Use the resolved runtime model_name from make_lead_agent to avoid stale config values.
    app_config = get_app_config()
    model_config = app_config.get_model_config(model_name) if model_name else None
    if model_config is not None and model_config.runtime_headers:
        middlewares.append(RuntimeHeadersMiddleware(model_config.runtime_headers))
    if model_config is not None and model_config.supports_vision:
        middlewares.append(ViewImageMiddleware())

    # Add DeferredToolFilterMiddleware to hide deferred tool schemas from model binding
    if app_config.tool_search.enabled:
        from deerflow.agents.middlewares.deferred_tool_filter_middleware import DeferredToolFilterMiddleware

        middlewares.append(DeferredToolFilterMiddleware())

    # Add SubagentLimitMiddleware to truncate excess parallel task calls
    subagent_enabled = bool(cfg.get("subagent_enabled", False)) and getattr(get_app_config().subagents, "enabled", True)
    if subagent_enabled:
        max_concurrent_subagents = clamp_subagent_limit(cfg.get("max_concurrent_subagents", 3))
        middlewares.append(SubagentLimitMiddleware(max_concurrent=max_concurrent_subagents))

    # LoopDetectionMiddleware — detect and break repetitive tool call loops
    middlewares.append(LoopDetectionMiddleware())

    # LangChain executes after_model hooks in reverse middleware order, so the
    # safety repair sees raw provider output before loop detection does.
    from deerflow.agents.middlewares.finish_reason_middleware import build_finish_reason_middlewares

    middlewares.extend(build_finish_reason_middlewares())

    # Inject custom middlewares before ClarificationMiddleware
    if custom_middlewares:
        middlewares.extend(custom_middlewares)

    # Inject middlewares declared via ExtensionsConfig.middlewares
    # ("module.path:ClassName"), letting deployments extend the chain without
    # forking the codebase.
    middlewares.extend(load_configured_middlewares())

    # ClarificationMiddleware should always be last
    middlewares.append(ClarificationMiddleware())

    # Calibrate the loop-detection backstop now the full chain (and thus the
    # per-turn graph cost) is known, keeping it below the recursion limit.
    calibrate_loop_detection(middlewares, recursion_limit)
    return middlewares


def make_lead_agent(config: RunnableConfig):
    # Lazy import to avoid circular dependency
    from deerflow.tools import get_available_tools
    from deerflow.tools.builtins import setup_agent

    cfg = _get_runtime_config(config)

    thinking_enabled = cfg.get("thinking_enabled", True)
    reasoning_effort = cfg.get("reasoning_effort", None)
    requested_model_name: str | None = cfg.get("model_name") or cfg.get("model")
    is_plan_mode = cfg.get("is_plan_mode", False)
    subagent_enabled = bool(cfg.get("subagent_enabled", False)) and getattr(get_app_config().subagents, "enabled", True)
    max_concurrent_subagents = clamp_subagent_limit(cfg.get("max_concurrent_subagents", 3))
    is_bootstrap = cfg.get("is_bootstrap", False)
    agent_name = validate_agent_name(cfg.get("agent_name"))

    agent_config = load_agent_config(agent_name) if not is_bootstrap else None
    # Custom agent model from agent config (if any), or None to let _resolve_model_name pick the default
    agent_model_name = agent_config.model if agent_config and agent_config.model else None

    # Final model name resolution: request → agent config → global default, with fallback for unknown names
    model_name = _resolve_model_name(requested_model_name, agent_model_name)

    app_config = get_app_config()
    checkpointer_config = getattr(app_config, "checkpointer", None)
    checkpoint_mode = getattr(checkpointer_config, "channel_mode", "full")
    checkpoint_frequency = getattr(
        getattr(checkpointer_config, "delta", None), "snapshot_frequency", 10
    )
    from deerflow.runtime.checkpoint_mode import (
        freeze_checkpoint_channel_mode,
        freeze_checkpoint_snapshot_frequency,
    )

    freeze_checkpoint_channel_mode(checkpoint_mode)
    freeze_checkpoint_snapshot_frequency(checkpoint_frequency)
    model_config = app_config.get_model_config(model_name)

    if model_config is None:
        raise ValueError("No chat model could be resolved. Please configure at least one model in config.yaml or provide a valid 'model_name'/'model' in the request.")
    if thinking_enabled and not model_config.supports_thinking:
        logger.warning(f"Thinking mode is enabled but model '{model_name}' does not support it; fallback to non-thinking mode.")
        thinking_enabled = False

    logger.info(
        "Create Agent(%s) -> thinking_enabled: %s, reasoning_effort: %s, model_name: %s, is_plan_mode: %s, subagent_enabled: %s, max_concurrent_subagents: %s",
        agent_name or "default",
        thinking_enabled,
        reasoning_effort,
        model_name,
        is_plan_mode,
        subagent_enabled,
        max_concurrent_subagents,
    )

    # Inject run metadata for LangSmith trace tagging
    if "metadata" not in config:
        config["metadata"] = {}

    config["metadata"].update(
        {
            "agent_name": agent_name or "default",
            "model_name": model_name or "default",
            "thinking_enabled": thinking_enabled,
            "reasoning_effort": reasoning_effort,
            "is_plan_mode": is_plan_mode,
            "subagent_enabled": subagent_enabled,
            "tool_groups": agent_config.tool_groups if agent_config else None,
            "available_skills": ["bootstrap"] if is_bootstrap else (agent_config.skills if agent_config and agent_config.skills is not None else None),
        }
    )

    if is_bootstrap:
        # Special bootstrap agent with minimal prompt for initial custom agent creation flow
        middleware = normalize_middleware_state_schemas(
            _build_middlewares(config, model_name=model_name),
            checkpoint_mode,
            checkpoint_frequency,
        )
        return create_agent(
            model=create_chat_model(name=model_name, thinking_enabled=thinking_enabled),
            tools=get_available_tools(model_name=model_name, subagent_enabled=subagent_enabled) + [setup_agent],
            middleware=middleware,
            system_prompt=apply_prompt_template(subagent_enabled=subagent_enabled, max_concurrent_subagents=max_concurrent_subagents, available_skills=set(["bootstrap"])),
            state_schema=get_thread_state_schema(checkpoint_mode, checkpoint_frequency),
            context_schema=AgentContext,
        )

    # Default lead agent (unchanged behavior)
    middleware = normalize_middleware_state_schemas(
        _build_middlewares(config, model_name=model_name, agent_name=agent_name),
        checkpoint_mode,
        checkpoint_frequency,
    )
    return create_agent(
        model=create_chat_model(name=model_name, thinking_enabled=thinking_enabled, reasoning_effort=reasoning_effort),
        tools=get_available_tools(model_name=model_name, groups=agent_config.tool_groups if agent_config else None, subagent_enabled=subagent_enabled),
        middleware=middleware,
        system_prompt=apply_prompt_template(
            subagent_enabled=subagent_enabled, max_concurrent_subagents=max_concurrent_subagents, agent_name=agent_name, available_skills=set(agent_config.skills) if agent_config and agent_config.skills is not None else None
        ),
        state_schema=get_thread_state_schema(checkpoint_mode, checkpoint_frequency),
        context_schema=AgentContext,
    )
