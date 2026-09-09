"""Unit tests for the uploads middleware and the ``list_uploaded_files`` tool.

The core regression these guard against: the ``<uploaded_files>`` block used
to be written back into persisted state with the same message id (so
``add_messages`` overwrote it in place), which meant every model call still
re-read every historical file's outline and, worse, a long thread accumulated
one near-duplicate block per turn in the checkpoint. The fix injects the block
through ``wrap_model_call`` (transient, per model call, never persisted) and
lists historical uploads by name only.
"""

from __future__ import annotations

import importlib
from pathlib import Path
from types import SimpleNamespace

from langchain.agents.middleware.types import ModelRequest
from langchain_core.messages import AIMessage, HumanMessage

from deerflow.agents.image_inputs import INPUT_IMAGES_KEY
from deerflow.agents.middlewares.uploads_middleware import UploadsMiddleware
from deerflow.config.paths import Paths
from deerflow.tools.builtins.list_uploaded_files_tool import list_uploaded_files_tool

# ``deerflow.tools.builtins.__init__`` re-exports ``list_uploaded_files_tool``
# (the StructuredTool instance) under the same name as this submodule, which
# shadows the submodule on the package object. Importing it this way -- rather
# than through ``monkeypatch.setattr("deerflow.tools.builtins.list_uploaded_files_tool.get_paths", ...)``
# -- guarantees we patch the actual module, not the tool instance.
_tool_module = importlib.import_module("deerflow.tools.builtins.list_uploaded_files_tool")

THREAD_ID = "thread-1"

_HEADING_MD = "# Introduction\n\nSome text.\n\n## Details\n\nMore text.\n"


def _runtime(thread_id: str | None = THREAD_ID, *, state: dict | None = None) -> SimpleNamespace:
    context = {"thread_id": thread_id} if thread_id else {}
    return SimpleNamespace(context=context, config={}, state=state or {})


def _middleware(tmp_path: Path) -> UploadsMiddleware:
    return UploadsMiddleware(base_dir=str(tmp_path))


def _uploads_dir(tmp_path: Path) -> Path:
    d = Paths(str(tmp_path)).sandbox_uploads_dir(THREAD_ID)
    d.mkdir(parents=True, exist_ok=True)
    return d


def _human_with_file(text: str, filename: str, size: int, *, msg_id: str) -> HumanMessage:
    return HumanMessage(
        content=text,
        id=msg_id,
        additional_kwargs={"files": [{"filename": filename, "size": size, "status": "done"}]},
    )


def _request(messages: list, runtime: SimpleNamespace) -> ModelRequest:
    return ModelRequest(model=None, messages=messages, runtime=runtime)


def _block_text(content) -> str:
    text = content if isinstance(content, str) else content[0]["text"]
    assert "<uploaded_files>" in text, text
    return text


# --- ① repeated model calls never accumulate blocks in persisted state -----


def test_wrap_model_call_injection_is_transient_and_never_accumulates(tmp_path):
    uploads = _uploads_dir(tmp_path)
    (uploads / "report.pdf").write_bytes(b"%PDF-1.4 fake")

    middleware = _middleware(tmp_path)
    turn1 = _human_with_file("please look at this", "report.pdf", 1024, msg_id="h1")
    messages = [turn1]

    seen_message_lists = []

    def handler(request: ModelRequest):
        seen_message_lists.append(request.messages)
        return AIMessage(content="ok")

    # Simulate the model being invoked several times against the same
    # underlying (persisted) message list -- e.g. a retry loop, or several
    # conversation turns that never wrote a block back into `messages`.
    for _ in range(3):
        middleware.wrap_model_call(_request(messages, _runtime()), handler)

    # The list that would be checkpointed was never mutated: still one
    # message, content untouched, no block leaked into it.
    assert messages == [turn1]
    assert messages[0].content == "please look at this"
    assert "<uploaded_files>" not in messages[0].content

    # Every model call still saw exactly one block -- never stacked/duplicated.
    assert len(seen_message_lists) == 3
    for patched in seen_message_lists:
        assert len(patched) == 1
        text = _block_text(patched[0].content)
        assert text.count("<uploaded_files>") == 1
        assert text.count("</uploaded_files>") == 1


def test_before_agent_stores_state_only_never_writes_a_block(tmp_path):
    uploads = _uploads_dir(tmp_path)
    (uploads / "report.pdf").write_bytes(b"%PDF-1.4 fake")

    middleware = _middleware(tmp_path)
    turn1 = _human_with_file("please look at this", "report.pdf", 1024, msg_id="h1")
    state = {"messages": [turn1]}

    result = middleware.before_agent(state, _runtime())

    assert result is not None
    files = result["uploaded_files"]
    assert len(files) == 1
    assert files[0]["filename"] == "report.pdf"
    assert files[0]["size"] == 1024
    assert files[0]["path"] == "/mnt/user-data/uploads/report.pdf"
    # The message itself is untouched -- nothing gets persisted into history.
    assert state["messages"][0].content == "please look at this"


def test_before_agent_clears_previous_run_upload_snapshot(tmp_path):
    middleware = _middleware(tmp_path)
    state = {
        "messages": [HumanMessage(content="next turn", id="h2")],
        "uploaded_files": [{"filename": "previous.pdf"}],
    }

    assert middleware.before_agent(state, _runtime()) == {"uploaded_files": []}


def test_before_agent_locates_human_message_when_tail_is_tool_message(tmp_path):
    """After an interrupted-then-resumed turn, state can end in a ``ToolMessage``
    rather than the human turn that started it. ``before_agent`` must locate the
    same message ``_build_injected_messages`` would (most recent HumanMessage,
    not ``messages[-1]``), or ``uploaded_files`` silently stops being recorded.
    """
    from langchain_core.messages import ToolMessage

    uploads = _uploads_dir(tmp_path)
    (uploads / "report.pdf").write_bytes(b"%PDF-1.4 fake")

    middleware = _middleware(tmp_path)
    human = _human_with_file("please look at this", "report.pdf", 1024, msg_id="h1")
    state = {
        "messages": [
            human,
            AIMessage(
                content="",
                tool_calls=[{"name": "read_file", "args": {}, "id": "c1", "type": "tool_call"}],
            ),
            ToolMessage(content="file contents", tool_call_id="c1"),
        ]
    }

    result = middleware.before_agent(state, _runtime())

    assert result is not None
    files = result["uploaded_files"]
    assert len(files) == 1
    assert files[0]["filename"] == "report.pdf"


def test_wrap_model_call_strips_stale_block_from_multimodal_content(tmp_path):
    """Old ``before_agent`` implementations could inject the block into an
    image+text message just as readily as a plain-text one. If the list branch
    of the stripping logic didn't inspect text blocks, that stale block would
    be re-sent to the model on every request forever -- the same accumulation
    bug this middleware exists to fix, just scoped to multimodal messages.
    """
    uploads = _uploads_dir(tmp_path)
    (uploads / "old.pdf").write_bytes(b"%PDF-1.4 fake")

    middleware = _middleware(tmp_path)
    image_block = {"type": "image_url", "image_url": {"url": "data:image/png;base64,AAAA"}}
    stale_text_block = {
        "type": "text",
        "text": "<uploaded_files>\nSTALE HISTORICAL BLOCK\n</uploaded_files>\n\nWhat's in this picture?",
    }
    human = HumanMessage(content=[stale_text_block, image_block], id="h1")
    messages = [human]

    seen_message_lists = []

    def handler(request: ModelRequest):
        seen_message_lists.append(request.messages)
        return AIMessage(content="ok")

    middleware.wrap_model_call(_request(messages, _runtime()), handler)

    assert len(seen_message_lists) == 1
    patched_content = seen_message_lists[0][0].content
    assert isinstance(patched_content, list)

    text_blocks = [b for b in patched_content if isinstance(b, dict) and b.get("type") == "text"]
    full_text = "\n".join(b["text"] for b in text_blocks)
    # The stale block's own text is gone; only the freshly-built block remains.
    assert "STALE HISTORICAL BLOCK" not in full_text
    assert full_text.count("<uploaded_files>") == 1
    assert "old.pdf" in full_text
    assert "What's in this picture?" in full_text

    # The image block survives untouched (HumanMessage's pydantic validation
    # rebuilds dicts on construction, so compare content rather than identity).
    assert image_block in patched_content

    # The caller's list and message are never mutated in place.
    assert messages == [human]
    assert messages[0].content[0] == stale_text_block


# --- ② .md sibling of a converted upload is skipped from historical listing


def test_md_sibling_of_converted_file_is_skipped_but_standalone_md_is_listed(tmp_path):
    uploads = _uploads_dir(tmp_path)
    (uploads / "report.pdf").write_bytes(b"%PDF-1.4 fake")
    (uploads / "report.md").write_text(_HEADING_MD, encoding="utf-8")
    (uploads / "standalone.md").write_text("# Notes\n", encoding="utf-8")

    middleware = _middleware(tmp_path)
    # Nothing new uploaded this turn -- everything on disk is historical.
    last_message = HumanMessage(content="what's in my files?", id="h1")

    new_files, historical_names = middleware._collect_files(last_message, _runtime())

    assert new_files == []
    assert historical_names == ["report.pdf", "standalone.md"]
    assert "report.md" not in historical_names


# --- ③ historical files are named only, never re-outlined -------------------


def test_historical_files_are_named_only_without_outline_extraction(tmp_path):
    uploads = _uploads_dir(tmp_path)
    (uploads / "old.pdf").write_bytes(b"%PDF-1.4 fake")
    (uploads / "old.md").write_text(_HEADING_MD, encoding="utf-8")

    middleware = _middleware(tmp_path)
    last_message = HumanMessage(content="anything new?", id="h1")

    new_files, historical_names = middleware._collect_files(last_message, _runtime())

    assert new_files == []
    assert historical_names == ["old.pdf"]

    message = middleware._create_files_message(new_files, historical_names)
    assert "Introduction" not in message  # heading from old.md never surfaced
    assert "Document outline" not in message
    assert "old.pdf" in message
    assert "list_uploaded_files" in message


# --- ④ historical listing is capped with a truncation hint ------------------


def test_historical_names_are_capped_with_truncation_hint(tmp_path):
    middleware = _middleware(tmp_path)
    names = [f"file{i}.txt" for i in range(35)]

    message = middleware._create_files_message([], names)

    for name in names[:30]:
        assert name in message
    for name in names[30:]:
        assert name not in message
    assert "... and 5 more" in message
    assert "list_uploaded_files" in message


# --- ⑤ current-turn uploads still get full outline injection ---------------


def test_current_turn_upload_gets_detailed_outline_injection(tmp_path):
    uploads = _uploads_dir(tmp_path)
    (uploads / "new.pdf").write_bytes(b"%PDF-1.4 fake")
    (uploads / "new.md").write_text(_HEADING_MD, encoding="utf-8")

    middleware = _middleware(tmp_path)
    last_message = _human_with_file("check this doc", "new.pdf", 2048, msg_id="h1")

    patched = middleware._build_injected_messages([last_message], _runtime())
    assert patched is not None
    text = _block_text(patched[-1].content)

    assert "new.pdf" in text
    assert "Document outline" in text
    assert "L1: Introduction" in text
    assert "L5: Details" in text
    assert "Path: /mnt/user-data/uploads/new.pdf" in text


# --- ⑥ list_uploaded_files tool ---------------------------------------------


def _patch_tool_paths(monkeypatch, tmp_path: Path) -> None:
    monkeypatch.setattr(_tool_module, "get_paths", lambda: Paths(str(tmp_path)))


def test_list_uploaded_files_tool_lists_all_files_without_filename(tmp_path, monkeypatch):
    _patch_tool_paths(monkeypatch, tmp_path)
    uploads = _uploads_dir(tmp_path)
    (uploads / "a.pdf").write_bytes(b"%PDF-1.4 fake")
    (uploads / "a.md").write_text(_HEADING_MD, encoding="utf-8")
    (uploads / "b.txt").write_text("hello", encoding="utf-8")

    result = list_uploaded_files_tool.func(_runtime(), filename=None)

    import json

    payload = json.loads(result)
    names = {f["filename"] for f in payload["files"]}
    assert names == {"a.pdf", "a.md", "b.txt"}
    assert payload["count"] == 3
    # A bare listing call is metadata only -- no outline extraction runs for
    # any file, so it never re-pays the outline cost for irrelevant files.
    for entry in payload["files"]:
        assert "outline" not in entry
        assert "begins_with" not in entry
    assert "filename" in payload["hint"]


def test_list_uploaded_files_excludes_current_run_snapshot(tmp_path, monkeypatch):
    _patch_tool_paths(monkeypatch, tmp_path)
    uploads = _uploads_dir(tmp_path)
    (uploads / "current.pdf").write_bytes(b"current")
    (uploads / "historical.pdf").write_bytes(b"historical")
    runtime = _runtime(state={"uploaded_files": [{"filename": "current.pdf"}]})

    import json

    payload = json.loads(list_uploaded_files_tool.func(runtime, filename=None))
    assert [entry["filename"] for entry in payload["files"]] == ["historical.pdf"]


def test_list_uploaded_files_tool_returns_outline_for_specific_filename(tmp_path, monkeypatch):
    _patch_tool_paths(monkeypatch, tmp_path)
    uploads = _uploads_dir(tmp_path)
    (uploads / "a.pdf").write_bytes(b"%PDF-1.4 fake")
    (uploads / "a.md").write_text(_HEADING_MD, encoding="utf-8")

    result = list_uploaded_files_tool.func(_runtime(), filename="a.pdf")

    import json

    payload = json.loads(result)
    assert payload["file"]["filename"] == "a.pdf"
    titles = [entry["title"] for entry in payload["file"]["outline"]]
    assert titles == ["Introduction", "Details"]


def test_list_uploaded_files_tool_missing_filename_returns_clear_error(tmp_path, monkeypatch):
    _patch_tool_paths(monkeypatch, tmp_path)
    _uploads_dir(tmp_path)

    result = list_uploaded_files_tool.func(_runtime(), filename="missing.pdf")

    import json

    payload = json.loads(result)
    assert "error" in payload
    assert "missing.pdf" in payload["error"]


def test_list_uploaded_files_tool_rejects_path_traversal_filename(tmp_path, monkeypatch):
    _patch_tool_paths(monkeypatch, tmp_path)
    _uploads_dir(tmp_path)

    result = list_uploaded_files_tool.func(_runtime(), filename="../../etc/passwd")

    import json

    payload = json.loads(result)
    assert "error" in payload


# --- ⑦ missing/empty uploads directory never raises -------------------------


def test_list_uploaded_files_tool_handles_missing_uploads_dir(tmp_path, monkeypatch):
    _patch_tool_paths(monkeypatch, tmp_path)
    # Note: uploads dir intentionally not created.

    result = list_uploaded_files_tool.func(_runtime(), filename=None)

    import json

    payload = json.loads(result)
    assert payload == {"files": [], "count": 0}


def test_list_uploaded_files_tool_no_active_thread_returns_error(tmp_path, monkeypatch):
    _patch_tool_paths(monkeypatch, tmp_path)

    result = list_uploaded_files_tool.func(_runtime(thread_id=None), filename=None)

    import json

    payload = json.loads(result)
    assert "error" in payload


def test_injection_targets_last_human_message_across_tool_calls(tmp_path):
    """Mid-turn the model is re-invoked with tool results appended.

    The block must follow the most recent *human* message rather than the last
    message, or file context silently disappears for every model call after the
    first tool result arrives.
    """
    from langchain_core.messages import ToolMessage

    uploads = _uploads_dir(tmp_path)
    (uploads / "report.pdf").write_bytes(b"%PDF-1.4 fake")

    middleware = _middleware(tmp_path)
    human = _human_with_file("check this doc", "report.pdf", 2048, msg_id="h1")
    messages = [
        human,
        AIMessage(
            content="",
            tool_calls=[{"name": "read_file", "args": {}, "id": "c1", "type": "tool_call"}],
        ),
        ToolMessage(content="file contents", tool_call_id="c1"),
    ]

    patched = middleware._build_injected_messages(messages, _runtime())

    assert patched is not None
    text = _block_text(patched[0].content)
    assert "report.pdf" in text
    # Trailing tool result is preserved untouched.
    assert patched[-1].content == "file contents"
    # The caller's list is not mutated in place.
    assert messages[0] is human


def test_outline_injection_surfaces_converted_markdown_path(tmp_path):
    """Outline line numbers index the ``.md``, so that path must be shown.

    The ``.md`` sibling is hidden from the historical listing, so without this
    the model would be handed line numbers and only the binary ``.pdf`` path
    they do not apply to.
    """
    uploads = _uploads_dir(tmp_path)
    (uploads / "deck.pdf").write_bytes(b"%PDF-1.4 fake")
    (uploads / "deck.md").write_text(_HEADING_MD, encoding="utf-8")

    middleware = _middleware(tmp_path)
    last_message = _human_with_file("summarise", "deck.pdf", 4096, msg_id="h1")

    patched = middleware._build_injected_messages([last_message], _runtime())
    assert patched is not None
    text = _block_text(patched[-1].content)

    assert "/mnt/user-data/uploads/deck.md" in text
    assert "L1: Introduction" in text


def test_wrap_model_call_no_uploads_dir_and_no_new_files_is_a_no_op(tmp_path):
    # Uploads dir never created -- thread with no attachments at all.
    middleware = _middleware(tmp_path)
    turn1 = HumanMessage(content="hello", id="h1")
    messages = [turn1]

    def handler(request: ModelRequest):
        return AIMessage(content=request.messages[0].content)

    response = middleware.wrap_model_call(_request(messages, _runtime()), handler)

    assert response.content == "hello"
    assert messages == [turn1]


def test_direct_input_image_is_not_duplicated_as_a_historical_upload(tmp_path):
    uploads = _uploads_dir(tmp_path)
    filename = "acp-image-1.png"
    image_data = b"\x89PNG\r\n\x1a\nimage"
    (uploads / filename).write_bytes(image_data)
    message = HumanMessage(
        content="inspect image",
        additional_kwargs={
            INPUT_IMAGES_KEY: [
                {
                    "name": "screen.png",
                    "mime_type": "image/png",
                    "virtual_path": f"/mnt/user-data/uploads/{filename}",
                    "size": len(image_data),
                    "sha256": "a" * 64,
                }
            ]
        },
    )

    assert _middleware(tmp_path)._build_injected_messages([message], _runtime()) is None
