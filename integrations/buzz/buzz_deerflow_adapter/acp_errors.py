"""Prompt failures that must not automatically rerun potentially executed tools."""


class ACPPromptError(RuntimeError):
    """An accepted ACP prompt ended with an error or cancellation."""


class ACPPromptTimeoutError(TimeoutError):
    """A submitted ACP prompt timed out and may already have executed tools."""
