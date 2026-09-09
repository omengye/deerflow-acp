import asyncio
import unittest
import tempfile
import importlib
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import patch

from fastapi import FastAPI, HTTPException
from fastapi.testclient import TestClient

import app.config as app_config
from app.config import settings, validate_api_exposure
from app.dependencies import ClientManager
from app.middleware import ApiKeyAuthMiddleware
from deerflow.config.app_config import AppConfig, get_app_config, reset_app_config
from deerflow.config.tracing_config import get_tracing_config, reset_tracing_config
from deerflow.runtime import ConflictError, DisconnectMode, END_SENTINEL, RunStatus


class ProductionControlsTests(unittest.IsolatedAsyncioTestCase):
    def _auth_test_client(self) -> TestClient:
        app = FastAPI()
        app.add_middleware(ApiKeyAuthMiddleware)

        @app.get("/api/chat")
        async def protected():
            return {"ok": True}

        @app.post("/v1/chat/completions")
        async def openai_compatible():
            return {"ok": True}

        @app.get("/health")
        async def public():
            return {"status": "ok"}

        @app.get("/health/ready")
        async def ready():
            return {"status": "ok", "checks": {"storage": {"ok": True}}}

        return TestClient(app)

    def _mounted_auth_test_client(self, mount_path: str = "/prefix") -> TestClient:
        child = FastAPI()
        child.add_middleware(ApiKeyAuthMiddleware)

        @child.get("/api/chat")
        async def protected():
            return {"ok": True}

        @child.get("/health")
        async def public():
            return {"status": "ok"}

        parent = FastAPI()
        parent.mount(mount_path, child)
        return TestClient(parent)

    async def test_api_auth_rejects_missing_bearer_token(self) -> None:
        original_enabled = settings.auth_enabled
        original_keys = list(settings.api_keys)
        settings.auth_enabled = True
        settings.api_keys = ["secret"]
        try:
            client = self._auth_test_client()
            response = client.get("/api/chat")
            self.assertEqual(response.status_code, 401)
            self.assertEqual(response.json(), {"detail": "Unauthorized"})
        finally:
            settings.auth_enabled = original_enabled
            settings.api_keys = original_keys

    async def test_auth_protects_v1_docs_openapi_and_readiness(self) -> None:
        original_enabled = settings.auth_enabled
        original_keys = list(settings.api_keys)
        settings.auth_enabled = True
        settings.api_keys = ["secret"]
        try:
            client = self._auth_test_client()
            protected_paths = [
                "/api/chat",
                "/v1/chat/completions",
                "/docs",
                "/docs/oauth2-redirect",
                "/openapi.json",
                "/redoc",
                "/health/ready",
            ]
            for path in protected_paths:
                method = client.post if path == "/v1/chat/completions" else client.get
                kwargs = {"json": {"messages": [{"role": "user", "content": "hi"}]}} if path == "/v1/chat/completions" else {}
                with self.subTest(path=path):
                    unauthorized = method(path, **kwargs)
                    authorized = method(path, headers={"Authorization": "Bearer secret"}, **kwargs)
                    self.assertEqual(unauthorized.status_code, 401)
                    self.assertEqual(unauthorized.headers["www-authenticate"], "Bearer")
                    self.assertEqual(authorized.status_code, 200)
        finally:
            settings.auth_enabled = original_enabled
            settings.api_keys = original_keys

    async def test_auth_keeps_basic_health_public(self) -> None:
        original_enabled = settings.auth_enabled
        original_keys = list(settings.api_keys)
        settings.auth_enabled = True
        settings.api_keys = ["secret"]
        try:
            client = self._auth_test_client()
            public = client.get("/health")
            self.assertEqual(public.status_code, 200)
        finally:
            settings.auth_enabled = original_enabled
            settings.api_keys = original_keys

    async def test_auth_uses_router_path_when_app_is_mounted(self) -> None:
        original_enabled = settings.auth_enabled
        original_keys = list(settings.api_keys)
        settings.auth_enabled = True
        settings.api_keys = ["secret"]
        try:
            client = self._mounted_auth_test_client()

            unauthorized = client.get("/prefix/api/chat")
            authorized = client.get(
                "/prefix/api/chat",
                headers={"Authorization": "Bearer secret"},
            )
            public = client.get("/prefix/health")

            self.assertEqual(unauthorized.status_code, 401)
            self.assertEqual(authorized.status_code, 200)
            self.assertEqual(public.status_code, 200)
        finally:
            settings.auth_enabled = original_enabled
            settings.api_keys = original_keys

    async def test_admin_ui_is_only_served_when_auth_enabled(self) -> None:
        from app import _admin_ui_file_response

        original_enabled = settings.auth_enabled
        try:
            settings.auth_enabled = False
            with self.assertRaises(HTTPException) as disabled:
                _admin_ui_file_response()
            self.assertEqual(disabled.exception.status_code, 404)

            settings.auth_enabled = True
            response = _admin_ui_file_response()
            self.assertTrue(str(response.path).endswith("admin-ui\\index.html") or str(response.path).endswith("admin-ui/index.html"))
        finally:
            settings.auth_enabled = original_enabled

    async def test_management_ui_redirects_bare_management_path_to_slash(self) -> None:
        from app import management_redirect

        original_enabled = settings.auth_enabled
        try:
            settings.auth_enabled = True
            response = management_redirect()
            self.assertEqual(response.status_code, 307)
            self.assertEqual(response.headers["location"], "/management/")
        finally:
            settings.auth_enabled = original_enabled

    async def test_legacy_admin_ui_routes_are_not_registered(self) -> None:
        from app import app as main_app

        route_paths = {getattr(route, "path", "") for route in main_app.routes}
        self.assertNotIn("/admin", route_paths)
        self.assertNotIn("/admin/", route_paths)

    async def test_admin_ui_rejects_path_traversal(self) -> None:
        from app import _admin_ui_file_response

        original_enabled = settings.auth_enabled
        try:
            settings.auth_enabled = True
            with self.assertRaises(HTTPException) as traversal:
                _admin_ui_file_response("../config.yaml")
            self.assertEqual(traversal.exception.status_code, 404)
        finally:
            settings.auth_enabled = original_enabled

    async def test_skills_api_preserves_skill_category(self) -> None:
        from app.routers import skills as skills_router

        client = SimpleNamespace(
            list_skills=lambda enabled_only=False: {
                "skills": [
                    {
                        "name": "builtin",
                        "description": "Built-in skill",
                        "category": "public",
                        "enabled": True,
                    },
                    {
                        "name": "custom-one",
                        "description": "Custom skill",
                        "category": "custom",
                        "enabled": False,
                    },
                ]
            }
        )
        manager = SimpleNamespace(get_client=lambda: client)

        with patch.object(skills_router, "get_client_manager", return_value=manager):
            response = await skills_router.list_skills()

        categories = {skill.name: skill.category for skill in response.skills}
        self.assertEqual(categories, {"builtin": "public", "custom-one": "custom"})

    async def test_run_manager_rejects_second_inflight_run_on_same_thread(self) -> None:
        manager = ClientManager()
        await manager.run_manager.create_or_reject("thread-1", multitask_strategy="reject")

        with self.assertRaises(ConflictError):
            await manager.run_manager.create_or_reject("thread-1", multitask_strategy="reject")

    async def test_run_manager_idempotency_reuses_same_run_and_rejects_payload_conflict(self) -> None:
        manager = ClientManager()
        first = await manager.run_manager.create_or_reject(
            "thread-1",
            idempotency_actor="actor-a",
            idempotency_key="request-1",
            payload_fingerprint="payload-a",
        )
        duplicate = await manager.run_manager.create_or_reject(
            "thread-1",
            idempotency_actor="actor-a",
            idempotency_key="request-1",
            payload_fingerprint="payload-a",
        )
        self.assertIs(first, duplicate)

        with self.assertRaisesRegex(ConflictError, "different request payload"):
            await manager.run_manager.create_or_reject(
                "thread-1",
                idempotency_actor="actor-a",
                idempotency_key="request-1",
                payload_fingerprint="payload-b",
            )

    async def test_run_manager_idempotency_scope_and_cleanup(self) -> None:
        manager = ClientManager()
        first = await manager.run_manager.create_or_reject(
            "thread-1",
            idempotency_actor="actor-a",
            idempotency_key="same-key",
            payload_fingerprint="payload",
        )
        other_thread = await manager.run_manager.create_or_reject(
            "thread-2",
            idempotency_actor="actor-a",
            idempotency_key="same-key",
            payload_fingerprint="payload",
        )
        self.assertNotEqual(first.run_id, other_thread.run_id)

        await manager.run_manager.cleanup(first.run_id, delay=0)
        replacement = await manager.run_manager.create_or_reject(
            "thread-1",
            idempotency_actor="actor-a",
            idempotency_key="same-key",
            payload_fingerprint="payload",
        )
        self.assertNotEqual(first.run_id, replacement.run_id)

    async def test_cancel_run_marks_inflight_run_interrupted(self) -> None:
        manager = ClientManager()
        record = await manager.run_manager.create_or_reject("thread-1")
        await manager.run_manager.set_status(record.run_id, RunStatus.running)

        cancelled = await manager.cancel_run(record.run_id)

        self.assertTrue(cancelled)
        self.assertEqual(record.status, RunStatus.interrupted)

    async def test_disconnected_continue_run_expires_replay_after_grace_period(self) -> None:
        manager = ClientManager()
        manager._reconnect_grace_seconds = 0.01
        record = await manager.run_manager.create_or_reject(
            "thread-1",
            run_id="run-1",
            on_disconnect=DisconnectMode.continue_,
        )
        await manager.run_manager.set_status(record.run_id, RunStatus.running)
        await manager.stream_bridge.publish(record.run_id, "messages-tuple", {"content": "before"})

        await manager.attach_run_stream(record.run_id)
        await manager.detach_run_stream(record.run_id)
        await asyncio.sleep(0.05)

        self.assertTrue(record.metadata["replay_expired"])
        self.assertEqual(record.metadata["replay_expired_reason"], "disconnect_timeout")
        await manager.stream_bridge.publish(record.run_id, "messages-tuple", {"content": "after"})
        stream = manager.stream_bridge.subscribe(record.run_id, heartbeat_interval=0.001)
        self.assertIs(await anext(stream), END_SENTINEL)

    async def test_completed_replay_and_run_metadata_use_separate_retention_windows(self) -> None:
        manager = ClientManager()
        manager._completed_replay_seconds = 0.01
        manager._run_metadata_retention_seconds = 10
        record = await manager.run_manager.create_or_reject("thread-1", run_id="run-1")
        await manager.run_manager.set_status(record.run_id, RunStatus.success)
        await manager.stream_bridge.publish(record.run_id, "messages-tuple", {"content": "done"})
        await manager.stream_bridge.publish_end(record.run_id)

        manager._schedule_completed_run_retention(record.run_id)
        await asyncio.sleep(0.03)

        self.assertIsNotNone(manager.run_manager.get(record.run_id))
        self.assertTrue(record.metadata["replay_expired"])
        self.assertEqual(record.metadata["replay_expired_reason"], "completed_retention")

        metadata_handle = manager._metadata_cleanup_handles.pop(record.run_id)
        metadata_handle.cancel()
        manager._on_run_metadata_elapsed(record.run_id)
        await asyncio.sleep(0.01)
        self.assertIsNone(manager.run_manager.get(record.run_id))

    async def test_sqlite_checkpointer_failure_does_not_fallback_by_default(self) -> None:
        original_type = settings.checkpointer_type
        original_path = settings.checkpointer_path
        original_fallback = settings.allow_memory_fallback
        settings.checkpointer_type = "sqlite"
        settings.checkpointer_path = "/tmp/deerflow-api-test/checkpoints.db"
        settings.allow_memory_fallback = False

        class _Conn:
            def close(self) -> None:
                pass

        try:
            manager = ClientManager()
            with patch("app.dependencies.sqlite3.connect", return_value=_Conn()):
                with patch("langgraph.checkpoint.sqlite.SqliteSaver", side_effect=RuntimeError("sqlite unavailable")):
                    with self.assertRaises(RuntimeError):
                        manager._get_checkpointer()
        finally:
            settings.checkpointer_type = original_type
            settings.checkpointer_path = original_path
            settings.allow_memory_fallback = original_fallback

    async def test_settings_prefer_config_yaml_api_section_over_env(self) -> None:
        original_api_config = dict(app_config._API_CONFIG)
        try:
            app_config._API_CONFIG = {
                "host": "127.0.0.1",
                "port": 9001,
                "plan_mode": False,
                "subagent_enabled": False,
                "max_concurrent_subagents": 4,
                "cors_allow_origins": ["https://example.test"],
                "api_keys": ["from-config"],
                "auth_enabled": True,
                "allow_insecure_remote": True,
                "chat_request_timeout": 12.5,
                "max_upload_size_mb": 8,
                "max_uploads_per_request": 2,
                "allowed_upload_extensions": ["md", ".txt"],
                "allow_memory_fallback": True,
            }
            with patch.dict(
                "os.environ",
                {
                    "HOST": "0.0.0.0",
                    "PORT": "1234",
                    "DEER_FLOW_PLAN_MODE": "true",
                    "DEER_FLOW_API_KEYS": "from-env",
                },
                clear=False,
            ):
                loaded = app_config.Settings()

            self.assertEqual(loaded.host, "127.0.0.1")
            self.assertEqual(loaded.port, 9001)
            self.assertFalse(loaded.plan_mode)
            self.assertFalse(loaded.subagent_enabled)
            self.assertEqual(loaded.max_concurrent_subagents, 4)
            self.assertEqual(loaded.cors_allow_origins, ["https://example.test"])
            self.assertEqual(loaded.api_keys, ["from-config"])
            self.assertTrue(loaded.auth_enabled)
            self.assertTrue(loaded.allow_insecure_remote)
            self.assertEqual(loaded.chat_request_timeout, 12.5)
            self.assertEqual(loaded.max_upload_size_mb, 8)
            self.assertEqual(loaded.max_uploads_per_request, 2)
            self.assertEqual(loaded.allowed_upload_extensions, ["md", ".txt"])
            self.assertTrue(loaded.allow_memory_fallback)
        finally:
            app_config._API_CONFIG = original_api_config

    async def test_settings_fallback_to_environment_when_api_section_omits_value(self) -> None:
        original_api_config = dict(app_config._API_CONFIG)
        try:
            app_config._API_CONFIG = {}
            with patch.dict(
                "os.environ",
                {
                    "HOST": "127.0.0.2",
                    "PORT": "9010",
                    "DEER_FLOW_CORS_ORIGINS": "https://one.test, https://two.test",
                    "DEER_FLOW_AUTH_ENABLED": "true",
                    "DEER_FLOW_API_KEYS": "a,b",
                },
                clear=False,
            ):
                loaded = app_config.Settings()

            self.assertEqual(loaded.host, "127.0.0.2")
            self.assertEqual(loaded.port, 9010)
            self.assertEqual(loaded.cors_allow_origins, ["https://one.test", "https://two.test"])
            self.assertTrue(loaded.auth_enabled)
            self.assertEqual(loaded.api_keys, ["a", "b"])
        finally:
            app_config._API_CONFIG = original_api_config

    async def test_scheduler_recursion_limit_defaults_to_interactive_limit(self) -> None:
        original_api_config = dict(app_config._API_CONFIG)
        try:
            app_config._API_CONFIG = {"recursion_limit": 321}
            with patch.dict("os.environ", {}, clear=True):
                inherited = app_config.Settings()
            self.assertEqual(inherited.recursion_limit, 321)
            self.assertEqual(inherited.scheduler_recursion_limit, 321)

            app_config._API_CONFIG["scheduler_recursion_limit"] = 480
            with patch.dict("os.environ", {}, clear=True):
                overridden = app_config.Settings()
            self.assertEqual(overridden.recursion_limit, 321)
            self.assertEqual(overridden.scheduler_recursion_limit, 480)
        finally:
            app_config._API_CONFIG = original_api_config

    async def test_settings_support_legacy_and_short_subagent_env_names(self) -> None:
        original_api_config = dict(app_config._API_CONFIG)
        try:
            app_config._API_CONFIG = {}
            with patch.dict("os.environ", {"DEER_FLOW_MAX_CONCURRENT_SUBAGENTS": "4", "MAX_CONCURRENT_SUBAGENTS": "2"}, clear=False):
                legacy = app_config.Settings()
            self.assertEqual(legacy.max_concurrent_subagents, 4)

            with patch.dict("os.environ", {"MAX_CONCURRENT_SUBAGENTS": "2"}, clear=True):
                short = app_config.Settings()
            self.assertEqual(short.max_concurrent_subagents, 2)
        finally:
            app_config._API_CONFIG = original_api_config

    async def test_unauthenticated_remote_bind_fails_closed(self) -> None:
        exposed = app_config.Settings(
            host="0.0.0.0",
            auth_enabled=False,
            api_keys=[],
            allow_insecure_remote=False,
        )

        with self.assertRaisesRegex(RuntimeError, "Refusing to bind"):
            validate_api_exposure(exposed)

    async def test_loopback_or_explicit_remote_opt_in_is_allowed(self) -> None:
        for host in ("127.0.0.1", "::1", "localhost"):
            validate_api_exposure(
                app_config.Settings(
                    host=host,
                    auth_enabled=False,
                    api_keys=[],
                    allow_insecure_remote=False,
                )
            )
        validate_api_exposure(
            app_config.Settings(
                host="0.0.0.0",
                auth_enabled=False,
                api_keys=[],
                allow_insecure_remote=True,
            )
        )

    async def test_app_config_resolves_braced_env_variables_with_defaults(self) -> None:
        config = {
            "plain": "x",
            "classic": "$API_TOKEN",
            "braced": "${TZ}",
            "defaulted": "${SANDBOX_LANG:-C.UTF-8}",
            "nested": {"items": ["${MISSING_WITH_DEFAULT:-fallback}"]},
        }

        with patch.dict("os.environ", {"API_TOKEN": "secret", "TZ": "Asia/Shanghai"}, clear=True):
            resolved = AppConfig.resolve_env_variables(config)

        self.assertEqual(resolved["plain"], "x")
        self.assertEqual(resolved["classic"], "secret")
        self.assertEqual(resolved["braced"], "Asia/Shanghai")
        self.assertEqual(resolved["defaulted"], "C.UTF-8")
        self.assertEqual(resolved["nested"]["items"], ["fallback"])

    async def test_app_config_rejects_missing_braced_env_variable_without_default(self) -> None:
        with patch.dict("os.environ", {}, clear=True):
            with self.assertRaisesRegex(ValueError, "Environment variable TZ not found"):
                AppConfig.resolve_env_variables("${TZ}")

    async def test_settings_custom_config_path_is_single_runtime_source(self) -> None:
        original_env = dict(app_config.os.environ)
        try:
            with tempfile.TemporaryDirectory() as tmpdir:
                config_path = Path(tmpdir) / "custom-config.yaml"
                config_path.write_text(
                    """
config_version: 11
api:
  config_path: ./wrong-config.yaml
  host: 127.0.0.9
  port: 8123
  max_concurrent_subagents: 4
sandbox:
  use: deerflow.sandbox.local:LocalSandboxProvider
models: []
tools: []
tool_groups: []
tracing:
  langfuse:
    enabled: true
    public_key: file-public
    secret_key: file-secret
    host: https://file-langfuse.test
""".strip(),
                    encoding="utf-8",
                )

                with patch.dict("os.environ", {"DEER_FLOW_CONFIG_PATH": str(config_path)}, clear=True):
                    reloaded_app_config = importlib.reload(app_config)
                    reset_app_config()
                    reset_tracing_config()

                    loaded_settings = reloaded_app_config.Settings()
                    runtime_config = get_app_config()
                    tracing = get_tracing_config()

                self.assertEqual(loaded_settings.config_path, str(config_path))
                self.assertEqual(loaded_settings.host, "127.0.0.9")
                self.assertEqual(loaded_settings.port, 8123)
                self.assertEqual(runtime_config.sandbox.use, "deerflow.sandbox.local:LocalSandboxProvider")
                self.assertTrue(tracing.langfuse.enabled)
                self.assertEqual(tracing.langfuse.public_key, "file-public")
                self.assertEqual(tracing.langfuse.secret_key, "file-secret")
                self.assertEqual(tracing.langfuse.host, "https://file-langfuse.test")
        finally:
            app_config.os.environ.clear()
            app_config.os.environ.update(original_env)
            importlib.reload(app_config)
            reset_tracing_config()
            reset_app_config()

    async def test_tracing_config_reads_from_config_yaml(self) -> None:
        try:
            reset_app_config()
            reset_tracing_config()
            config = get_app_config()
            extra = config.model_extra or {}
            tracing_section = dict(extra.get("tracing") or {})
            tracing_section["langfuse"] = {
                "enabled": True,
                "public_key": "public-from-config",
                "secret_key": "secret-from-config",
                "host": "https://langfuse.test",
            }
            extra["tracing"] = tracing_section

            loaded = get_tracing_config()

            self.assertTrue(loaded.langfuse.enabled)
            self.assertEqual(loaded.langfuse.public_key, "public-from-config")
            self.assertEqual(loaded.langfuse.secret_key, "secret-from-config")
            self.assertEqual(loaded.langfuse.host, "https://langfuse.test")
        finally:
            reset_tracing_config()
            reset_app_config()


if __name__ == "__main__":
    unittest.main()
