# Optional model-bound sensitive-text redaction

`pii_redaction.enabled` defaults to `false`. Enabling it filters temporary
messages sent to the main model, native subagents, title generation, and both
primary and fallback conversation-summary models. The `email`, `phone`,
`bank_card`, `chinese_id`, and `api_key` switches default to `true` within that
opt-in feature. Cards use a Luhn check; Chinese IDs require a valid birth date
and checksum. Mobile matching handles Chinese text without whitespace. API
keys cover known provider prefixes, common labeled keys, and bearer tokens.

The middleware replaces matching text with fixed `[REDACTED_*]` markers. It
keeps existing markers, stores no reversible mapping, and copies the model
request through `ModelRequest.override`. Replayed tool arguments and assistant
reasoning text are filtered in that copy too. Actual tool execution arguments,
stored user/tool messages, message IDs, receipt metadata, and artifacts remain
unchanged. This includes tools returning `Command` updates, whose resulting
messages pass through the same model boundary on the next call.

Knowledge-source artifacts and generated source files remain original evidence
for the user. Their model-visible message text is redacted; the evidence itself
is not rewritten. Enabling this option does not anonymize stored conversations
or source downloads. It does not cover memory extraction, arbitrary extension
model calls, or sensitive content inside images/audio/opaque provider payloads.
Pattern detection is best effort and can miss uncommon formats or redact a
non-sensitive number that passes a supported validation rule.

The pure SDK factory accepts `pii_redaction=PiiRedactionConfig(enabled=True)`
without reading YAML. Its automatic chain configures the model boundary and
known DeerFlow title/summary middleware. A caller taking over the entire
`middleware` list must install `PiiRedactionMiddleware` and configure auxiliary
callers explicitly. Standalone `TitleMiddleware` and
`DeerFlowSummarizationMiddleware` accept the same `pii_redaction` argument.

When the SDK factory adds a native `task` tool, an explicit PII config is bound
to that tool and forwarded to its child model. Per-call metadata cannot disable
the bound setting. Explicit SDK skill catalogs and allowlists are likewise
carried by the native task tool; a missing explicit catalog fails closed instead
of loading application-global skills with matching names. SDK callers own the
mapping from their explicit skill locations to their execution environment;
the child does not mount application-global skill bodies as a substitute.
