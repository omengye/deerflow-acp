import copy
import uuid
from collections.abc import Sequence
from functools import cache
from typing import Annotated, Any, NotRequired, TypedDict, cast, get_type_hints

from langchain.agents import AgentState
from langchain_core.messages import (
    AnyMessage,
    BaseMessageChunk,
    RemoveMessage,
    convert_to_messages,
    message_chunk_to_message,
)
from langgraph.channels import DeltaChannel
from langgraph.graph.message import REMOVE_ALL_MESSAGES

from deerflow.agents.goal_state import GoalState
from deerflow.config.checkpointer_config import (
    DEFAULT_CHECKPOINT_SNAPSHOT_FREQUENCY,
    CheckpointChannelMode,
)


class SandboxState(TypedDict):
    sandbox_id: NotRequired[str | None]
    skills_revision: NotRequired[str]
    skills_path: NotRequired[str]
    available_skills: NotRequired[list[str] | None]
    workspace_path: NotRequired[str | None]


class AgentContext(TypedDict, total=False):
    """Runtime context schema passed to LangGraph ``create_agent``.

    Declared so Pydantic does not warn when middlewares (e.g. sandbox tools)
    mutate ``runtime.context`` to add ``sandbox_id`` alongside ``thread_id``.
    """

    thread_id: NotRequired[str]
    run_id: NotRequired[str]
    sandbox_id: NotRequired[str]
    agent_name: NotRequired[str]
    user_id: NotRequired[str]
    workspace_path: NotRequired[str]
    available_skills: NotRequired[list[str] | None]
    stop_reason: NotRequired[str]


class ThreadDataState(TypedDict):
    thread_id: NotRequired[str]
    workspace_path: NotRequired[str | None]
    workspace_path_managed: NotRequired[bool]
    uploads_path: NotRequired[str | None]
    outputs_path: NotRequired[str | None]


class ViewedImageData(TypedDict):
    # ``base64`` is optional only for backward compatibility with checkpoints
    # created before image payloads were made model-call-ephemeral.
    base64: NotRequired[str]
    mime_type: str
    size: NotRequired[int]
    sha256: NotRequired[str]
    source_sandbox_id: NotRequired[str]


def merge_artifacts(existing: list[str] | None, new: list[str] | None) -> list[str]:
    """Reducer for artifacts list - merges and deduplicates artifacts."""
    if existing is None:
        return new or []
    if new is None:
        return existing
    # Use dict.fromkeys to deduplicate while preserving order
    return list(dict.fromkeys(existing + new))


def merge_viewed_images(existing: dict[str, ViewedImageData] | None, new: dict[str, ViewedImageData] | None) -> dict[str, ViewedImageData]:
    """Reducer for viewed_images dict - merges image dictionaries.

    Special case: If new is an empty dict {}, it clears the existing images.
    This allows middlewares to clear the viewed_images state after processing.
    """
    if existing is None:
        return new or {}
    if new is None:
        return existing
    # Special case: empty dict means clear all viewed images
    if len(new) == 0:
        return {}
    # Merge dictionaries, new values override existing ones for same keys
    return {**existing, **new}


def merge_goal(
    existing: GoalState | None,
    new: GoalState | None,
) -> GoalState | None:
    """Preserve an active goal when a graph node does not update it."""
    if new is None:
        return existing
    return new


class ThreadState(AgentState):
    sandbox: NotRequired[SandboxState | None]
    thread_data: NotRequired[ThreadDataState | None]
    title: NotRequired[str | None]
    artifacts: Annotated[list[str], merge_artifacts]
    uploaded_files: NotRequired[list[dict] | None]
    viewed_images: Annotated[dict[str, ViewedImageData], merge_viewed_images]  # image_path -> {base64, mime_type}
    goal: Annotated[GoalState | None, merge_goal]
    thread_directories_created: NotRequired[bool]


def _normalize_messages(value: Any) -> list[AnyMessage]:
    values = value if isinstance(value, list) else [value]
    messages = [
        message_chunk_to_message(cast(BaseMessageChunk, message))
        for message in convert_to_messages(values)
    ]
    for message in messages:
        if message.id is None:
            message.id = str(uuid.uuid4())
    return messages


def merge_message_writes(
    state: list[AnyMessage], writes: Sequence[Any]
) -> list[AnyMessage]:
    """Fold DeltaChannel writes with ``add_messages`` replacement semantics."""
    messages: list[AnyMessage | None] = _normalize_messages(state)
    positions = {cast(str, message.id): index for index, message in enumerate(messages)}

    for raw_write in writes:
        if raw_write is None:
            raise ValueError("messages delta writes cannot contain None")
        write = _normalize_messages(raw_write)
        remove_all = next(
            (
                index
                for index, message in enumerate(write)
                if isinstance(message, RemoveMessage)
                and message.id == REMOVE_ALL_MESSAGES
            ),
            None,
        )
        if remove_all is not None:
            messages = list(write[remove_all + 1 :])
            positions = {
                cast(str, message.id): index
                for index, message in enumerate(messages)
            }
            continue

        for message in write:
            message_id = cast(str, message.id)
            previous = positions.get(message_id)
            if isinstance(message, RemoveMessage):
                if previous is None:
                    raise ValueError(
                        f"Attempting to delete a message with an ID that doesn't exist ({message_id!r})"
                    )
                messages[previous] = None
                positions.pop(message_id, None)
            elif previous is None:
                positions[message_id] = len(messages)
                messages.append(message)
            else:
                messages[previous] = message

    return [message for message in messages if message is not None]


def delta_messages_field(
    snapshot_frequency: int = DEFAULT_CHECKPOINT_SNAPSHOT_FREQUENCY,
) -> Any:
    return Annotated[
        list[AnyMessage],
        DeltaChannel(
            merge_message_writes,
            snapshot_frequency=snapshot_frequency,
        ),
    ]


class DeltaThreadState(ThreadState):
    messages: delta_messages_field()


def get_thread_state_schema(
    mode: CheckpointChannelMode,
    snapshot_frequency: int | None = None,
) -> type:
    if mode == "full":
        return ThreadState
    frequency = snapshot_frequency or DEFAULT_CHECKPOINT_SNAPSHOT_FREQUENCY
    return _delta_thread_state_schema(frequency)


@cache
def _delta_thread_state_schema(snapshot_frequency: int) -> type:
    if snapshot_frequency == DEFAULT_CHECKPOINT_SNAPSHOT_FREQUENCY:
        return DeltaThreadState
    annotations = get_type_hints(ThreadState, include_extras=True)
    annotations["messages"] = delta_messages_field(snapshot_frequency)
    return TypedDict(
        f"DeltaThreadState_f{snapshot_frequency}",
        annotations,
        total=getattr(ThreadState, "__total__", True),
    )


@cache
def _adapt_state_schema_for_delta(schema: type, snapshot_frequency: int) -> type:
    annotations = get_type_hints(schema, include_extras=True)
    annotations["messages"] = delta_messages_field(snapshot_frequency)
    return TypedDict(
        f"Delta{schema.__module__.replace('.', '_')}_{schema.__name__}_f{snapshot_frequency}",
        annotations,
        total=getattr(schema, "__total__", True),
    )


def adapt_state_schema_for_mode(
    schema: type,
    mode: CheckpointChannelMode,
    snapshot_frequency: int | None = None,
) -> type:
    if mode == "full":
        return schema
    return _adapt_state_schema_for_delta(
        schema,
        snapshot_frequency or DEFAULT_CHECKPOINT_SNAPSHOT_FREQUENCY,
    )


def normalize_middleware_state_schemas(
    middleware: Sequence[Any],
    mode: CheckpointChannelMode,
    snapshot_frequency: int | None = None,
) -> list[Any]:
    """Make middleware-contributed ``messages`` channels mode-consistent."""
    if mode == "full":
        return list(middleware)
    normalized: list[Any] = []
    frequency = snapshot_frequency or DEFAULT_CHECKPOINT_SNAPSHOT_FREQUENCY
    for item in middleware:
        schema = getattr(item, "state_schema", None)
        if schema is None:
            normalized.append(item)
            continue
        adapted = copy.copy(item)
        adapted.state_schema = adapt_state_schema_for_mode(schema, mode, frequency)
        normalized.append(adapted)
    return normalized
