"""Dynamic loader for user-declared middlewares from ExtensionsConfig.

Lets deployments extend the lead agent's middleware chain without forking
deerflow-api: declare middleware classes in extensions_config.json under
`middlewares` (format: "module.path:ClassName") and they are instantiated
and appended automatically by `_build_middlewares()`. See
config.example.yaml for the documented format.
"""

import logging

from langchain.agents.middleware import AgentMiddleware

from deerflow.config.extensions_config import ExtensionsConfig, get_extensions_config
from deerflow.reflection import resolve_class

logger = logging.getLogger(__name__)


def load_configured_middlewares(extensions_config: ExtensionsConfig | None = None) -> list[AgentMiddleware]:
    """Instantiate the middlewares declared in `ExtensionsConfig.middlewares`.

    Each entry must resolve to an `AgentMiddleware` subclass with a
    no-argument constructor. A misconfigured entry (bad path, import error,
    wrong type, constructor failure) is logged and skipped so it cannot take
    down lead agent creation.

    Args:
        extensions_config: Extensions config to read from. Defaults to the
            process-wide cached config.

    Returns:
        Instantiated middlewares, in declared order.
    """
    config = extensions_config if extensions_config is not None else get_extensions_config()
    middlewares: list[AgentMiddleware] = []
    for declaration in config.middlewares:
        middleware_path = declaration if isinstance(declaration, str) else declaration.class_path
        kwargs = {} if isinstance(declaration, str) else declaration.kwargs
        try:
            middleware_class = resolve_class(middleware_path, AgentMiddleware)
            middlewares.append(middleware_class(**kwargs))
        except Exception as e:
            logger.warning(f"Failed to load configured middleware {middleware_path!r}: {e}", exc_info=True)
    return middlewares
