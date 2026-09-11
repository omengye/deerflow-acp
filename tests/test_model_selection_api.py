import unittest
from types import SimpleNamespace
from unittest.mock import AsyncMock, Mock, patch

from fastapi import HTTPException

from app.routers import chat, models
from app.schemas import AguiRunAgentInput


class ModelSelectionApiTests(unittest.IsolatedAsyncioTestCase):
    def request_body(self, model="model-a"):
        body = {"threadId": "thread-1", "runId": "run-1", "messages": [
            {"id": "user-1", "role": "user", "content": "hello"}
        ]}
        if model is not None:
            body["modelName"] = model
        return AguiRunAgentInput.model_validate(body)

    def manager(self, record=None):
        manager = Mock()
        manager.run_manager.get.return_value = record
        manager.get_client.return_value.get_model.return_value = {"name": "model-a"}
        manager.get_async_client = AsyncMock(return_value=SimpleNamespace(agent_name="lead_agent"))
        manager.start_client_stream_run = AsyncMock(return_value=SimpleNamespace())
        return manager

    async def test_unknown_model_rejected_before_creating_run(self):
        manager = self.manager()
        manager.get_client.return_value.get_model.return_value = None
        with patch.object(chat, "get_client_manager", return_value=manager):
            with self.assertRaises(HTTPException) as caught:
                await chat.chat_agui(SimpleNamespace(headers={}), self.request_body("removed"))
        self.assertEqual(caught.exception.status_code, 400)
        self.assertEqual(caught.exception.detail["code"], "MODEL_NOT_FOUND")
        manager.start_client_stream_run.assert_not_awaited()

    async def test_selected_model_passed_to_new_run(self):
        manager = self.manager()
        with patch.object(chat, "get_client_manager", return_value=manager):
            await chat.chat_agui(SimpleNamespace(headers={}), self.request_body())
        self.assertEqual(manager.start_client_stream_run.call_args.kwargs["kwargs"], {"model_name": "model-a"})

    async def test_default_does_not_override_backend_selection(self):
        manager = self.manager()
        with patch.object(chat, "get_client_manager", return_value=manager):
            await chat.chat_agui(SimpleNamespace(headers={}), self.request_body(None))
        manager.get_client.assert_not_called()
        self.assertEqual(manager.start_client_stream_run.call_args.kwargs["kwargs"], {})

    async def test_reconnect_keeps_original_model_without_catalog_validation(self):
        record = SimpleNamespace(thread_id="thread-1", metadata={}, kwargs={"model_name": "original"})
        manager = self.manager(record)
        with patch.object(chat, "get_client_manager", return_value=manager):
            await chat.chat_agui(SimpleNamespace(headers={"last-event-id": "1-0"}), self.request_body("new"))
        manager.get_client.assert_not_called()
        manager.start_client_stream_run.assert_not_awaited()
        manager.get_async_client.assert_awaited_once_with(model_name="original")

    async def test_expired_reconnect_does_not_create_run_or_validate_new_model(self):
        manager = self.manager()
        with patch.object(chat, "get_client_manager", return_value=manager):
            with self.assertRaises(HTTPException) as caught:
                await chat.chat_agui(SimpleNamespace(headers={"last-event-id": "1-0"}), self.request_body("new"))
        self.assertEqual(caught.exception.status_code, 410)
        manager.get_client.assert_not_called()
        manager.start_client_stream_run.assert_not_awaited()

    async def test_model_display_names_fall_back_to_name(self):
        manager = self.manager()
        manager.get_client.return_value.list_models.return_value = {"models": [
            {"name": "null-label", "display_name": None},
            {"name": "empty-label", "display_name": ""},
            {"name": "missing-label"},
            {"name": "named", "display_name": "Friendly", "supports_vision": True},
        ]}
        with patch.object(models, "get_client_manager", return_value=manager):
            response = await models.list_models()
        self.assertEqual([model.display_name for model in response.models], ["null-label", "empty-label", "missing-label", "Friendly"])
        self.assertTrue(response.models[-1].supports_vision)
