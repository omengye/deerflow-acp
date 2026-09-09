from __future__ import annotations

from types import SimpleNamespace

from langchain_core.messages import AIMessage, HumanMessage

from deerflow.agents.middlewares.title_middleware import TitleMiddleware


def _state(user_content: str):
    return {
        "messages": [
            HumanMessage(content=user_content),
            AIMessage(content="I can inspect the report."),
        ]
    }


def test_title_prompt_ignores_legacy_uploaded_files_context() -> None:
    middleware = TitleMiddleware()
    state = _state(
        "<uploaded_files>\n- confidential-report.pdf\n</uploaded_files>\n\n"
        "Summarize the report"
    )

    prompt, user_message = middleware._build_title_prompt(state)

    assert user_message == "Summarize the report"
    assert "confidential-report.pdf" not in prompt


async def test_attachment_only_title_skips_title_model(monkeypatch) -> None:
    middleware = TitleMiddleware()
    state = _state("<uploaded_files>\n- report.pdf\n</uploaded_files>\n")
    monkeypatch.setattr(
        "deerflow.agents.middlewares.title_middleware.create_chat_model",
        lambda **_kwargs: (_ for _ in ()).throw(
            AssertionError("attachment-only title must not call a model")
        ),
    )

    assert await middleware._agenerate_title_result(state) == {
        "title": "New Conversation"
    }


async def test_title_model_inherits_thread_runtime_header(monkeypatch) -> None:
    class _Model:
        def __init__(self) -> None:
            self._deerflow_runtime_headers = {"x-opencode-session": "thread_id"}
            self.bound: dict | None = None

        def bind(self, **kwargs):
            self.bound = kwargs
            return self

        async def ainvoke(self, *_args, **_kwargs):
            return AIMessage(content="A useful title")

    model = _Model()
    monkeypatch.setattr(
        "deerflow.agents.middlewares.title_middleware.create_chat_model",
        lambda **_kwargs: model,
    )
    runtime = SimpleNamespace(
        context={"thread_id": "title-thread"},
        config={"configurable": {"thread_id": "title-thread"}},
    )

    assert await TitleMiddleware()._agenerate_title_result(
        _state("Summarize the report"),
        runtime,
    ) == {"title": "A useful title"}
    assert model.bound == {
        "extra_headers": {"x-opencode-session": "title-thread"}
    }
