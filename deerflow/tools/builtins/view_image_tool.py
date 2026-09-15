import hashlib
import mimetypes
from pathlib import Path
from typing import Annotated

from langchain.tools import InjectedToolCallId, ToolRuntime, tool
from langchain_core.messages import ToolMessage
from langgraph.types import Command

from deerflow.agents.image_inputs import (
    IMAGE_EXTENSION_TO_MIME,
    MAX_INPUT_IMAGE_BYTES,
    detect_image_mime,
)
from deerflow.agents.thread_state import AgentContext, ThreadDataState, ThreadState
from deerflow.config.paths import VIRTUAL_PATH_PREFIX

_ALLOWED_IMAGE_VIRTUAL_ROOTS = (
    f"{VIRTUAL_PATH_PREFIX}/workspace",
    f"{VIRTUAL_PATH_PREFIX}/uploads",
    f"{VIRTUAL_PATH_PREFIX}/outputs",
)
_ALLOWED_IMAGE_VIRTUAL_ROOTS_TEXT = ", ".join(_ALLOWED_IMAGE_VIRTUAL_ROOTS)
def _is_allowed_image_virtual_path(image_path: str) -> bool:
    return any(image_path == root or image_path.startswith(f"{root}/") for root in _ALLOWED_IMAGE_VIRTUAL_ROOTS)
def _sanitize_image_error(
    error: Exception,
    thread_data: ThreadDataState | None,
    runtime: ToolRuntime[AgentContext, ThreadState],
) -> str:
    from deerflow.sandbox.tools import mask_local_paths_in_output

    return mask_local_paths_in_output(f"{type(error).__name__}: {error}", thread_data, runtime)


@tool("view_image", parse_docstring=True)
def view_image_tool(
    runtime: ToolRuntime[AgentContext, ThreadState],
    image_path: str,
    tool_call_id: Annotated[str, InjectedToolCallId],
) -> Command:
    """Read an image file.

    Use this tool to read an image file and make it available for display.

    When to use the view_image tool:
    - When you need to view an image file.

    When NOT to use the view_image tool:
    - For non-image files (use present_files instead)
    - For multiple files at once (use present_files instead)

    Args:
        image_path: Absolute /mnt/user-data virtual path to the image file. Common formats supported: jpg, jpeg, png, webp, gif.
    """
    from deerflow.sandbox.exceptions import SandboxRuntimeError
    from deerflow.sandbox.tools import (
        get_thread_data,
        is_host_fs_sandbox,
        resolve_and_validate_user_data_path,
        sandbox_from_runtime,
        validate_local_tool_path,
    )

    thread_data = get_thread_data(runtime)

    if not _is_allowed_image_virtual_path(image_path):
        return Command(
            update={
                "messages": [
                    ToolMessage(
                        f"Error: Only image paths under {_ALLOWED_IMAGE_VIRTUAL_ROOTS_TEXT} are allowed",
                        tool_call_id=tool_call_id,
                    )
                ]
            },
        )

    try:
        validate_local_tool_path(image_path, thread_data, read_only=True)
        actual_path = resolve_and_validate_user_data_path(image_path, thread_data)
    except (PermissionError, SandboxRuntimeError) as e:
        return Command(
            update={"messages": [ToolMessage(f"Error: {str(e)}", tool_call_id=tool_call_id)]},
        )

    # Validate image extension
    expected_mime_type = IMAGE_EXTENSION_TO_MIME.get(Path(image_path).suffix.lower())
    if expected_mime_type is None:
        return Command(
            update={"messages": [ToolMessage(f"Error: Unsupported image format: {Path(image_path).suffix}. Supported formats: {', '.join(IMAGE_EXTENSION_TO_MIME)}", tool_call_id=tool_call_id)]},
        )

    # Detect MIME type from file extension
    mime_type, _ = mimetypes.guess_type(image_path)
    if mime_type is None:
        mime_type = expected_mime_type

    sandbox_state = runtime.state.get("sandbox") if runtime and runtime.state else None
    source_sandbox_id = (
        sandbox_state.get("sandbox_id") if isinstance(sandbox_state, dict) else None
    )

    # Mounted local/WSL sandboxes share the host filesystem. Other providers
    # are authoritative for their own files and may not have a host mirror.
    try:
        if isinstance(source_sandbox_id, str) and not is_host_fs_sandbox(runtime):
            image_data = sandbox_from_runtime(runtime).download_file(image_path)
        else:
            path = Path(actual_path)
            if not path.exists():
                raise FileNotFoundError(image_path)
            if not path.is_file():
                raise IsADirectoryError(image_path)
            image_data = path.read_bytes()
    except Exception as e:
        return Command(
            update={"messages": [ToolMessage(f"Error reading image file: {_sanitize_image_error(e, thread_data, runtime)}", tool_call_id=tool_call_id)]},
        )

    image_size = len(image_data)
    if image_size > MAX_INPUT_IMAGE_BYTES:
        return Command(
            update={"messages": [ToolMessage(f"Error: Image file is too large: {image_size} bytes. Maximum supported size is {MAX_INPUT_IMAGE_BYTES} bytes", tool_call_id=tool_call_id)]},
        )

    detected_mime_type = detect_image_mime(image_data)
    if detected_mime_type is None:
        return Command(
            update={"messages": [ToolMessage("Error: File contents do not match a supported image format", tool_call_id=tool_call_id)]},
        )
    if detected_mime_type != expected_mime_type:
        return Command(
            update={"messages": [ToolMessage(f"Error: Image contents are {detected_mime_type}, but file extension indicates {expected_mime_type}", tool_call_id=tool_call_id)]},
        )
    mime_type = detected_mime_type
    # Persist only lightweight metadata. ViewImageMiddleware reloads and
    # base64-encodes the image for the immediate model request, so binary image
    # payloads are never copied into every later checkpoint.
    metadata = {
        "mime_type": mime_type,
        "size": image_size,
        "sha256": hashlib.sha256(image_data).hexdigest(),
    }
    if isinstance(source_sandbox_id, str):
        metadata["source_sandbox_id"] = source_sandbox_id
    new_viewed_images = {image_path: metadata}

    return Command(
        update={"viewed_images": new_viewed_images, "messages": [ToolMessage("Successfully read image", tool_call_id=tool_call_id)]},
    )
