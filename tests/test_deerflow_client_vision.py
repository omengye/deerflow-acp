from types import SimpleNamespace
from unittest.mock import patch

from langchain_core.messages import HumanMessage

from deerflow.agents.image_inputs import INPUT_IMAGES_KEY
from deerflow.client import DeerFlowClient


def _client() -> DeerFlowClient:
    client = object.__new__(DeerFlowClient)
    client._agent = None
    client._agent_config_key = None
    client._checkpointer = object()
    client._effective_checkpointer = None
    client._model_name = None
    client._thinking_enabled = True
    client._subagent_enabled = False
    client._plan_mode = False
    client._max_concurrent_subagents = 3
    client._agent_name = None
    client._available_skills = None
    client._middlewares = []
    client._recursion_limit = 200
    return client


def _app_config(*, supports_vision: bool):
    first_model = SimpleNamespace(name="first-model", supports_vision=False)
    model = SimpleNamespace(name="vision-model", supports_vision=supports_vision)
    by_name = {candidate.name: candidate for candidate in (first_model, model)}
    return SimpleNamespace(
        models=[first_model, model],
        default_model="vision-model",
        subagents=SimpleNamespace(enabled=True),
        get_model_config=by_name.get,
        get_default_model_name=lambda: "vision-model",
    )


def test_deerflow_client_uses_default_model_name_for_vision_middleware() -> None:
    client = _client()
    config = {
        "configurable": {
            "model_name": None,
            "thinking_enabled": True,
            "subagent_enabled": False,
            "is_plan_mode": False,
            "max_concurrent_subagents": 3,
        }
    }

    with (
        patch("deerflow.client.get_app_config", return_value=_app_config(supports_vision=True)),
        patch.object(DeerFlowClient, "_get_memory_signature", return_value=None),
        patch("deerflow.client.create_chat_model", return_value=object()) as create_model,
        patch.object(DeerFlowClient, "_get_tools", return_value=[]) as get_tools,
        patch("deerflow.client._build_middlewares", return_value=[]) as build_middlewares,
        patch("deerflow.client.apply_prompt_template", return_value="system"),
        patch("deerflow.client.create_agent", return_value=object()),
    ):
        client._ensure_agent(config)

    create_model.assert_called_once()
    assert create_model.call_args.kwargs["name"] == "vision-model"
    assert get_tools.call_args.kwargs["model_name"] == "vision-model"
    assert build_middlewares.call_args.kwargs["model_name"] == "vision-model"


def test_deerflow_client_rebuilds_agent_when_supports_vision_changes() -> None:
    client = _client()
    config = {
        "configurable": {
            "model_name": None,
            "thinking_enabled": True,
            "subagent_enabled": False,
            "is_plan_mode": False,
            "max_concurrent_subagents": 3,
        }
    }

    with (
        patch(
            "deerflow.client.get_app_config",
            side_effect=[
                _app_config(supports_vision=False),
                _app_config(supports_vision=True),
            ],
        ),
        patch.object(DeerFlowClient, "_get_memory_signature", return_value=None),
        patch("deerflow.client.create_chat_model", return_value=object()),
        patch.object(DeerFlowClient, "_get_tools", return_value=[]),
        patch("deerflow.client._build_middlewares", return_value=[]),
        patch("deerflow.client.apply_prompt_template", return_value="system"),
        patch("deerflow.client.create_agent", return_value=object()) as create_agent,
    ):
        client._ensure_agent(config)
        client._ensure_agent(config)

    assert create_agent.call_count == 2


def test_deerflow_client_rebuilds_agent_when_calendar_date_changes() -> None:
    client = _client()
    config = {
        "configurable": {
            "model_name": None,
            "thinking_enabled": True,
            "subagent_enabled": False,
            "is_plan_mode": False,
            "max_concurrent_subagents": 3,
        }
    }

    with (
        patch("deerflow.client.get_current_date", side_effect=["2042-03-04, Tuesday", "2042-03-05, Wednesday"]),
        patch("deerflow.client.get_app_config", return_value=_app_config(supports_vision=False)),
        patch.object(DeerFlowClient, "_get_memory_signature", return_value=None),
        patch("deerflow.client.create_chat_model", return_value=object()),
        patch.object(DeerFlowClient, "_get_tools", return_value=[]),
        patch("deerflow.client._build_middlewares", return_value=[]),
        patch("deerflow.client.apply_prompt_template", return_value="system") as apply_prompt,
        patch("deerflow.client.create_agent", return_value=object()) as create_agent,
    ):
        client._ensure_agent(config)
        client._ensure_agent(config)

    assert create_agent.call_count == 2
    assert [call.kwargs["current_date"] for call in apply_prompt.call_args_list] == [
        "2042-03-04, Tuesday",
        "2042-03-05, Wednesday",
    ]


def test_deerflow_client_rebuilds_agent_when_pii_policy_changes() -> None:
    from deerflow.config.pii_redaction_config import PiiRedactionConfig

    client = _client()
    app_config = _app_config(supports_vision=False)
    app_config.pii_redaction = PiiRedactionConfig()
    with (
        patch("deerflow.client.get_app_config", return_value=app_config),
        patch.object(DeerFlowClient, "_get_memory_signature", return_value=None),
        patch.object(DeerFlowClient, "_get_skill_catalog_version", return_value=0),
        patch("deerflow.client.create_chat_model", return_value=object()),
        patch.object(DeerFlowClient, "_get_tools", return_value=[]),
        patch("deerflow.client._build_middlewares", return_value=[]),
        patch("deerflow.client.apply_prompt_template", return_value="system"),
        patch("deerflow.client.create_agent", return_value=object()) as create_agent,
    ):
        client._ensure_agent({})
        client._ensure_agent({})
        assert create_agent.call_count == 1
        app_config.pii_redaction.enabled = True
        client._ensure_agent({})
        assert create_agent.call_count == 2
        app_config.pii_redaction.email = False
        client._ensure_agent({})
        client._ensure_agent({})
        assert create_agent.call_count == 3


def test_deerflow_client_explicit_model_overrides_configured_default() -> None:
    client = _client()
    client._model_name = "first-model"
    config = {
        "configurable": {
            "model_name": "first-model",
            "thinking_enabled": True,
            "subagent_enabled": False,
            "is_plan_mode": False,
            "max_concurrent_subagents": 3,
        }
    }

    with (
        patch("deerflow.client.get_app_config", return_value=_app_config(supports_vision=True)),
        patch.object(DeerFlowClient, "_get_memory_signature", return_value=None),
        patch("deerflow.client.create_chat_model", return_value=object()) as create_model,
        patch.object(DeerFlowClient, "_get_tools", return_value=[]),
        patch("deerflow.client._build_middlewares", return_value=[]),
        patch("deerflow.client.apply_prompt_template", return_value="system"),
        patch("deerflow.client.create_agent", return_value=object()),
    ):
        client._ensure_agent(config)

    assert create_model.call_args.kwargs["name"] == "first-model"


def test_deerflow_client_adds_client_mcp_tools_without_overriding_existing() -> None:
    client = _client()
    existing = SimpleNamespace(name="deerflow_tool")
    client._additional_mcp_tools = [
        SimpleNamespace(name="deerflow_tool"),
        SimpleNamespace(name="codeg_read_file"),
    ]

    with patch("deerflow.tools.get_available_tools", return_value=[existing]):
        tools = client._get_tools(model_name=None, subagent_enabled=False)

    assert tools == [existing, client._additional_mcp_tools[1]]


def test_deerflow_client_checkpoints_only_direct_image_metadata() -> None:
    client = _client()
    client._agent = object()
    metadata = {
        "name": "screen.png",
        "mime_type": "image/png",
        "virtual_path": "/mnt/user-data/uploads/screen.png",
        "size": 12,
        "sha256": "a" * 64,
    }
    config = {"configurable": {"thread_id": "thread-1"}}

    with (
        patch.object(DeerFlowClient, "_get_runnable_config", return_value=config),
        patch.object(DeerFlowClient, "_ensure_agent"),
    ):
        _, state, _ = client._prepare_stream_invocation(
            "Inspect this image",
            "thread-1",
            input_images=[metadata],
        )

    message = state["messages"][0]
    assert message.content == "Inspect this image"
    assert message.additional_kwargs == {INPUT_IMAGES_KEY: [metadata]}
    assert "base64" not in str(message.additional_kwargs)
    assert DeerFlowClient._serialize_message(message)["input_images"] == [metadata]


def test_deerflow_client_preserves_hidden_goal_continuation_metadata() -> None:
    client = _client()
    client._agent = object()
    continuation = HumanMessage(
        content="continue",
        additional_kwargs={
            "hide_from_ui": True,
            "deerflow_goal_continuation": True,
        },
    )
    config = {"configurable": {"thread_id": "thread-1"}}

    with (
        patch.object(DeerFlowClient, "_get_runnable_config", return_value=config),
        patch.object(DeerFlowClient, "_ensure_agent"),
    ):
        _, state, _ = client._prepare_stream_invocation(
            continuation,
            "thread-1",
        )

    message = state["messages"][0]
    assert message is not continuation
    assert message.content == "continue"
    assert message.additional_kwargs == continuation.additional_kwargs
