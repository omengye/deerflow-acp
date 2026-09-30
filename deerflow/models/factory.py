import asyncio
import logging
import os
import ssl
import weakref

from langchain.chat_models import BaseChatModel
from langchain_core.callbacks import BaseCallbackManager

from deerflow.config import get_app_config
from deerflow.reflection import resolve_class
from deerflow.tracing import build_tracing_callbacks

logger = logging.getLogger(__name__)


def _verified_ssl_context_for_http_clients() -> ssl.SSLContext:
    """Use LangChain's already-loaded certifi context when trust rules match.

    Importing BaseChatOpenAI creates a verified certifi context at module load.
    HTTPX uses the same certifi bundle when neither SSL_CERT_FILE nor
    SSL_CERT_DIR is set, so loading it again adds seconds on Windows. Keep
    HTTPX's own path for explicit certificate overrides and future LangChain
    versions that do not expose a suitable context.
    """
    import httpx

    if not (os.getenv("SSL_CERT_FILE") or os.getenv("SSL_CERT_DIR")):
        try:
            from langchain_openai.chat_models import base as openai_base

            context = getattr(openai_base, "global_ssl_context", None)
            if (
                isinstance(context, ssl.SSLContext)
                and context.verify_mode == ssl.CERT_REQUIRED
                and context.check_hostname
            ):
                return context
        except ImportError:
            pass
    return httpx.create_ssl_context(verify=True, trust_env=True)


def _build_no_keepalive_async_client():
    """Build an httpx.AsyncClient with keep-alive disabled.

    Used as the ``http_async_client`` for long-lived BaseChatOpenAI instances
    on hot paths (lead agent, shared summarization middleware) so the
    openai/httpx connection pool never retains SSL transports across LLM
    calls — eliminating the residual `RuntimeError: Event loop is closed`
    risk if any code path ever schedules a model call from a foreign loop.

    ``timeout=None`` defers timeout enforcement to the openai SDK / chunk
    timeout middleware, matching langchain_openai's default behaviour
    when it builds its own httpx client.
    """
    import httpx

    # AsyncClient builds one transport per configured proxy. Passing verify=True
    # would load the same CA bundle separately for each transport on startup.
    # Use HTTPX's own trust_env rules once, then share that context within this
    # client so SSL_CERT_FILE, SSL_CERT_DIR, and proxy settings keep their meaning.
    ssl_context = _verified_ssl_context_for_http_clients()
    return httpx.AsyncClient(
        verify=ssl_context,
        limits=httpx.Limits(max_keepalive_connections=0),
        timeout=httpx.Timeout(timeout=None),
    )


def _build_no_keepalive_http_clients():
    """Build sync and async HTTP clients using one verified SSL context.

    ACP constructs both OpenAI clients even though its lead agent invokes the
    model asynchronously. Sharing the context avoids loading the CA bundle a
    second time during cold graph construction. Neither client retains idle
    connections, so replacing a cached graph cannot strand open transports.
    """
    import httpx

    ssl_context = _verified_ssl_context_for_http_clients()
    limits = httpx.Limits(max_keepalive_connections=0)
    sync_client = httpx.Client(
        verify=ssl_context,
        limits=limits,
        timeout=httpx.Timeout(timeout=None),
    )
    try:
        async_client = httpx.AsyncClient(
            verify=ssl_context,
            limits=limits,
            timeout=httpx.Timeout(timeout=None),
        )
    except BaseException:
        sync_client.close()
        raise
    return sync_client, async_client


def _close_unattached_http_clients(sync_client, async_client) -> None:
    """Release factory-owned clients if model construction fails."""
    if sync_client is not None:
        try:
            sync_client.close()
        except Exception:
            logger.debug("Failed to close unattached sync HTTP client", exc_info=True)
    if async_client is not None:
        try:
            loop = asyncio.get_running_loop()
        except RuntimeError:
            try:
                asyncio.run(async_client.aclose())
            except Exception:
                logger.debug("Failed to close unattached async HTTP client", exc_info=True)
        else:
            loop.create_task(async_client.aclose())


def _deep_merge_dicts(base: dict | None, override: dict) -> dict:
    """Recursively merge two dictionaries without mutating the inputs."""
    merged = dict(base or {})
    for key, value in override.items():
        if isinstance(value, dict) and isinstance(merged.get(key), dict):
            merged[key] = _deep_merge_dicts(merged[key], value)
        else:
            merged[key] = value
    return merged


def _vllm_disable_chat_template_kwargs(chat_template_kwargs: dict) -> dict:
    """Build the disable payload for vLLM/Qwen chat template kwargs."""
    disable_kwargs: dict[str, bool] = {}
    if "thinking" in chat_template_kwargs:
        disable_kwargs["thinking"] = False
    if "enable_thinking" in chat_template_kwargs:
        disable_kwargs["enable_thinking"] = False
    return disable_kwargs


def _enable_stream_usage_by_default(model_use_path: str, model_settings_from_config: dict) -> None:
    """Enable stream usage for OpenAI-compatible models unless explicitly configured.

    LangChain only auto-enables ``stream_usage`` for OpenAI models when no custom
    base URL or client is configured. DeerFlow frequently uses OpenAI-compatible
    gateways, so token usage tracking would otherwise stay empty and the
    TokenUsageMiddleware would have nothing to log.
    """
    if model_use_path != "langchain_openai:ChatOpenAI":
        return
    if "stream_usage" in model_settings_from_config:
        return
    if "base_url" in model_settings_from_config or "openai_api_base" in model_settings_from_config:
        model_settings_from_config["stream_usage"] = True


def create_chat_model(
    name: str | None = None,
    thinking_enabled: bool = False,
    *,
    disable_keepalive: bool = False,
    share_ssl_context_for_http_clients: bool = False,
    **kwargs,
) -> BaseChatModel:
    """Create a chat model instance from the config.

    Args:
        name: The name of the model to create. If None, the configured default model will be used.
        thinking_enabled: Enable thinking-mode settings declared in the model config.
        disable_keepalive: Inject an httpx.AsyncClient with keep-alive disabled
            for BaseChatOpenAI subclasses. Opt in for long-lived shared models on
            hot paths (lead agent, shared summarization middleware) so the
            openai connection pool never carries SSL transports across LLM
            calls. Adds a TCP+TLS handshake per request; do not enable for
            short-lived per-call models — they already drain on close via
            ``aclose_chat_model``.
        share_ssl_context_for_http_clients: When ``disable_keepalive`` is true,
            also inject a sync client sharing the same verified SSL context.
            Intended for ACP's long-lived lead model on cold startup.

    Returns:
        A chat model instance.
    """
    config = get_app_config()
    if name is None:
        name = config.get_default_model_name()
    model_config = config.get_model_config(name)
    if model_config is None:
        raise ValueError(f"Model {name} not found in config") from None
    model_class = resolve_class(model_config.use, BaseChatModel)
    model_settings_from_config = model_config.model_dump(
        exclude_none=True,
        exclude={
            "use",
            "name",
            "display_name",
            "description",
            "supports_thinking",
            "supports_reasoning_effort",
            "when_thinking_enabled",
            "when_thinking_disabled",
            "thinking",
            "supports_vision",
            "context_window",
            "runtime_headers",
            "request_admission",
        },
    )
    # Compute effective when_thinking_enabled by merging in the `thinking` shortcut field.
    # The `thinking` shortcut is equivalent to setting when_thinking_enabled["thinking"].
    has_thinking_settings = (model_config.when_thinking_enabled is not None) or (model_config.thinking is not None)
    effective_wte: dict = dict(model_config.when_thinking_enabled) if model_config.when_thinking_enabled else {}
    if model_config.thinking is not None:
        merged_thinking = {**(effective_wte.get("thinking") or {}), **model_config.thinking}
        effective_wte = {**effective_wte, "thinking": merged_thinking}
    # Thinking mode requires model support. This used to raise ValueError here,
    # but per-agent subagent overrides (config.yaml subagents.agents.*.thinking_enabled)
    # can now request thinking without knowing in advance which model the agent
    # will resolve to -- hard-failing chat model construction for that is too
    # strict. Warn and degrade to non-thinking instead, mirroring the lead
    # agent's existing fallback (see agents/lead_agent/agent.py).
    if thinking_enabled and not model_config.supports_thinking:
        logger.warning(f"Thinking mode is enabled but model '{name}' does not support it; falling back to non-thinking mode.")
        thinking_enabled = False
    if thinking_enabled and has_thinking_settings and effective_wte:
        model_settings_from_config.update(effective_wte)
    if not thinking_enabled:
        if model_config.when_thinking_disabled is not None:
            # User-provided disable settings take full precedence
            model_settings_from_config.update(model_config.when_thinking_disabled)
        elif has_thinking_settings and effective_wte.get("extra_body", {}).get("thinking", {}).get("type"):
            # OpenAI-compatible gateway: thinking is nested under extra_body
            model_settings_from_config["extra_body"] = _deep_merge_dicts(
                model_settings_from_config.get("extra_body"),
                {"thinking": {"type": "disabled"}},
            )
            model_settings_from_config["reasoning_effort"] = "minimal"
        elif has_thinking_settings and (disable_chat_template_kwargs := _vllm_disable_chat_template_kwargs(effective_wte.get("extra_body", {}).get("chat_template_kwargs") or {})):
            # vLLM uses chat template kwargs to switch thinking on/off.
            model_settings_from_config["extra_body"] = _deep_merge_dicts(
                model_settings_from_config.get("extra_body"),
                {"chat_template_kwargs": disable_chat_template_kwargs},
            )
        elif has_thinking_settings and effective_wte.get("thinking", {}).get("type"):
            # Native langchain_anthropic: thinking is a direct constructor parameter
            model_settings_from_config["thinking"] = {"type": "disabled"}
    if not model_config.supports_reasoning_effort:
        kwargs.pop("reasoning_effort", None)
        model_settings_from_config.pop("reasoning_effort", None)

    _enable_stream_usage_by_default(model_config.use, model_settings_from_config)

    # For Codex Responses API models: map thinking mode to reasoning_effort
    from deerflow.models.openai_codex_provider import CodexChatModel

    if issubclass(model_class, CodexChatModel):
        # The ChatGPT Codex endpoint currently rejects max_tokens/max_output_tokens.
        # A per-agent override supplies max_tokens via kwargs (not
        # model_settings_from_config), and kwargs wins in the
        # `{**model_settings_from_config, **kwargs}` merge below -- so it must
        # be popped from both places, or an override would still reach the
        # Codex endpoint and get rejected with a 400.
        model_settings_from_config.pop("max_tokens", None)
        if kwargs.pop("max_tokens", None) is not None:
            logger.warning(f"Model '{name}' is a Codex Responses API model, which does not support max_tokens; ignoring per-agent max_tokens override.")

        # Use explicit reasoning_effort from frontend if provided (low/medium/high)
        explicit_effort = kwargs.pop("reasoning_effort", None)
        if not thinking_enabled:
            model_settings_from_config["reasoning_effort"] = "none"
        elif explicit_effort and explicit_effort in ("low", "medium", "high", "xhigh"):
            model_settings_from_config["reasoning_effort"] = explicit_effort
        elif "reasoning_effort" not in model_settings_from_config:
            model_settings_from_config["reasoning_effort"] = "medium"

    # For MindIE models: enforce conservative retry defaults.
    # Timeout normalization is handled inside MindIEChatModel itself.
    if getattr(model_class, "__name__", "") == "MindIEChatModel":
        # Enforce max_retries constraint to prevent cascading timeouts.
        model_settings_from_config["max_retries"] = model_settings_from_config.get("max_retries", 1)

    translate_context_window = (
        bool(model_config.context_window)
        and "profile" not in kwargs
        and "profile" not in model_settings_from_config
    )

    if model_config.request_admission is not None:
        from deerflow.models.request_admission import get_request_admission

        if "rate_limiter" in kwargs or "rate_limiter" in model_settings_from_config:
            raise ValueError(
                "request_admission cannot be combined with a custom rate_limiter"
            )
        model_settings_from_config["rate_limiter"] = get_request_admission(
            name,
            model_config.request_admission,
        )

        # SDK-internal retries bypass BaseChatModel's rate-limiter hook. Keep
        # retries at the agent middleware layer so every attempt is admitted.
        model_fields = getattr(model_class, "model_fields", {})
        if "max_retries" in model_fields:
            kwargs.pop("max_retries", None)
            model_settings_from_config["max_retries"] = 0
        if "retry_max_attempts" in model_fields:
            # DeerFlow's Claude/Codex providers implement a second retry loop
            # outside the SDK.  One attempt is the minimum those models accept
            # and disables that loop so every retry re-enters admission.
            kwargs.pop("retry_max_attempts", None)
            model_settings_from_config["retry_max_attempts"] = 1

    owned_sync_client = None
    owned_async_client = None
    if disable_keepalive:
        try:
            from langchain_openai.chat_models.base import BaseChatOpenAI

            # LangChain rejects an explicit openai_proxy combined with either
            # custom HTTP client. Honor that setting and caller-supplied clients.
            openai_proxy = kwargs.get("openai_proxy", model_settings_from_config.get("openai_proxy"))
            if (
                issubclass(model_class, BaseChatOpenAI)
                and not openai_proxy
                and "http_async_client" not in model_settings_from_config
                and "http_async_client" not in kwargs
            ):
                if (
                    share_ssl_context_for_http_clients
                    and "http_client" not in model_settings_from_config
                    and "http_client" not in kwargs
                ):
                    owned_sync_client, owned_async_client = _build_no_keepalive_http_clients()
                    model_settings_from_config["http_client"] = owned_sync_client
                else:
                    owned_async_client = _build_no_keepalive_async_client()
                model_settings_from_config["http_async_client"] = owned_async_client
        except Exception:
            logger.debug("Failed to inject no-keepalive http_async_client; falling back to default", exc_info=True)

    try:
        model_instance = model_class(**{**model_settings_from_config, **kwargs})
    except BaseException:
        _close_unattached_http_clients(owned_sync_client, owned_async_client)
        raise
    if owned_sync_client is not None:
        # A graph can be replaced while another turn still uses it. Closing at
        # model collection, rather than at reset_agent(), preserves that turn.
        # aclose_chat_model() may invoke this finalizer earlier when appropriate.
        finalizer = None
        try:
            finalizer = weakref.finalize(model_instance, owned_sync_client.close)
            finalizer.atexit = False
            object.__setattr__(model_instance, "_deerflow_owned_http_client_finalizer", finalizer)
        except BaseException:
            if finalizer is not None:
                finalizer.detach()
            _close_unattached_http_clients(owned_sync_client, owned_async_client)
            raise
    # Direct model consumers such as summarization do not pass through the
    # agent middleware chain.  Keep only the declarative source mapping on the
    # instance; actual values are resolved per invocation, never shared here.
    object.__setattr__(model_instance, "_deerflow_runtime_headers", dict(model_config.runtime_headers))

    if translate_context_window:
        inferred_profile = getattr(model_instance, "profile", None)
        model_instance.profile = {
            **(inferred_profile or {}),
            "max_input_tokens": model_config.context_window,
        }

    callbacks = build_tracing_callbacks()
    if callbacks:
        existing_callbacks = model_instance.callbacks
        if isinstance(existing_callbacks, BaseCallbackManager):
            for callback in callbacks:
                existing_callbacks.add_handler(callback)
        else:
            model_instance.callbacks = [*(existing_callbacks or []), *callbacks]
        logger.debug(f"Tracing attached to model '{name}' with providers={len(callbacks)}")
    return model_instance
