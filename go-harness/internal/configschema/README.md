# Shared native settings contract

`Describe` supplies `config_schema` in configuration snapshots and local
`daemon.status`, `daemon.drain`, and `daemon.resume` responses. The initial
`deerflow-settings-v2` contract covers **runtime, memory, models, subagents,
native tool parameters, local sandbox authorization, skill enablement and
manual skill candidate review**. The current contract also includes bounded
generation, behavior evaluation, discovery, auto-patch observation and the
daemon's selected effective-setting projection; their consumers are implemented.
`host_settings` advertises host-only `mcp.host_recipes` support and restart scope
without exposing recipe values or allowing model/desktop recipe editing.

Each descriptor includes a draft JSON pointer, native YAML path, scalar/list
type, nullability, default, enum/range/character or UTF-8 byte limit, `restart` apply scope,
supported operations, and a SHA-256 identity. The schema also has an identity.
These are metadata hashes, independent of configuration values and credentials.
Change the version when changing semantics that cannot be expressed in these
descriptors. Waku accepts known versions/types and intersects field descriptors
with the live daemon; a stopped service uses the selected config tool's schema.
Capability refresh never replaces the draft. Missing or incompatible metadata
makes the scoped fields read-only and prevents saving that document.

Tool fields use `/tools/*/...` descriptors with `for_tools` applicability and
per-tool result bounds. Drafts reject irrelevant parameters and class paths.
Native launch validates the consumed parameters against the same definitions;
unknown original fields remain migration input, with path-only diagnostics in
the settings snapshot. Tool groups are retained as original metadata and never
used as a Go permission gate. The native forms support fixed tool names,
adding/removing configurations and explicit credential clearing. Removing a
file/command parameter entry does not revoke the tool; use the tool policy.
API keys and proxy userinfo are hidden, retained only for the same original tool
identity, and never copied into a newly added replacement. Ordinary proxy URLs
and environment references remain editable. OpenCLI `allowed_sites: null`
keeps the builtin adapter list; `[]` grants no adapters (top-level discovery is
still available). Numeric zero retains each native tool's existing default.

Sandbox forms expose `provider: local`, `allow_host_bash` and `allow_host_tools`;
the obsolete `use` and opaque advanced editor are removed. Original unconsumed
sandbox data stays on disk until explicit migration and is never re-created from
snapshot defaults. Existing nonlocal providers require an explicit draft
conversion. Concrete CLI-selected sandboxes and applied-state reporting still
need their separate capability contract.

Model fields use `/models/*/...` descriptors; the desktop matches these to
array indices. `clear_api_key` is an edit operation with no native YAML path.
Provider choices are openai/ark/claude and the effective `context_window` is
explicit. Original-name and credential-presence metadata is validated but not
written as model parameters. Literal credentials stay hidden; backend changes
clear old credentials and provider-specific extensions. Unrelated saves retain
original unknown data on the same backend. Model probes use the selected native
provider, including Ark.

The two role maps have `agent_map`, wildcard `agent_role`, nested model settings,
and per-field descriptors. Sparse role definitions do not acquire defaults when
viewed; section defaults include direct children only. Scalar, list and byte
limits apply to both draft validation and native launch. Role maps also use
`agents.DecodeConfig` and `SubagentsConfig.ValidateModelOptions`, including all
selectable inherited model providers. They are not opaque passthrough objects.
Unknown role keys, embedded credentials, missing required custom instructions,
cycles and unsupported generation options are rejected before writing. The
desktop supplies separate global controls and per-role native fields, with
builtin/custom selection, custom create/rename/delete and builtin override reset.
Rename updates every delegate reference atomically in the draft; referenced
roles cannot be deleted until those references are removed. Field capabilities
are intersected at every nested level; collection edits require the complete
role contract. Unknown original role data remains visible as migration diagnostics.
Optional numeric and thinking fields preserve null/inheritance, explicit zero
where supported, and false. Tool/skill lists preserve null versus empty; the bash
builtin's absent tool override retains its native bash-only default. Role prompt
limits count UTF-8 bytes, matching the native role validator. Old main-Agent
directories are no longer scanned, initialized,
edited, backed up or replaced by settings save/rollback; existing directories
remain available for future explicit import.

`ValidateDraft` rejects unknown, missing, and malformed scoped fields. Native
launch validates present known fields through the same definitions; retained
unknown YAML is migration input. Native YAML null remains unset. Integer fields
reject fractions, durations use 0.001–86400 seconds, and prompt length counts
Unicode characters. Root tool names share native trimming/deduplication; role
tool lists use the existing stricter exact-name/duplicate checks. Cross-field
memory validation also runs after explicit launch
overrides: global scope is permitted only while memory is disabled, and enabled
tool mode requires retrieval. The UI offers workspace/session only.

The snapshot no longer fabricates Python storage classes, old memory paths,
unsupported extraction parameters, global thinking/plan defaults, custom main
agent selection, or legacy goal continuation settings. Memory saves update
only enabled/mode/injection/retrieval. They preserve original unknown values
and secrets until explicit migration, and convert the consumed
`backend_config.retrieval_enabled` alias to the canonical root field so it
cannot shadow an edit. A new native memory section receives no legacy defaults.

## Canonical subagent controls and explicit migration

Native files use `config_version: eino-harness-v1`, `subagents.enabled` (default
false) and `subagents.max_concurrent` (default 2, range 1–64). The settings schema
changed to v2 because the former runtime gate and concurrency inputs are removed.
Mixed native/legacy controls are rejected, including null legacy keys. Explicit
`--disable-subagents` wins over native YAML; sessions and tool policy may further
restrict the resulting host capability.

Files without a version or with recognized numeric versions 1–16 retain the
previous two-gate behavior: absent local gate is false, absent role gate is true,
and a false role gate cannot be reopened by CLI. `ProjectSubagentControls` reports
their AND and preserves their concurrency limit. The snapshot exposes this
projection plus a migration report. It never rewrites the source. `validate` can
inspect the proposed native draft; `save` requires `config_migration_ack` matching
the reviewed original revision. That acknowledgement is an edit intent, with no
native YAML path. The ordinary revision guard also rejects changed files.

The UI explicitly explains that native CLI precedence can override the former
role hard gate. Without explicit flags the converted default remains unchanged.
Saving backs up exact original files, sets the native version and deletes the
two old local keys. It clears the acknowledgement and migration-required state
on success. Unknown formats, malformed values and ambiguous mixed keys require
correction instead of fallback. Remaining historical fields and Python class
aliases are outside this scoped migration and remain pending cleanup.

The daemon's bounded `subagent_settings` report uses a separate version and the
configuration revision loaded at startup. It includes configured/default and
effective host enablement, CLI/YAML/legacy/default source, the task tool-policy
restriction and concurrent limit/source. Waku displays it read-only, without
substituting draft values when stopped or incompatible. Existing sessions and
task snapshots may be more restrictive than the current defaults for new runs.

Automatic discovery/publication schemas and native forms are now implemented.
Historical Agent import produces a draft; evolution history is a read-only
archive. Actual paths and selected CLI/effective values have native consumers.
These changes do not replace real desktop and complete P0/P6/P7 acceptance.

## Skill review and source configuration

Native review fields cover enablement, review-only mode, builtin overlays, two
explicit configured model names, mandatory fail-closed security and nested
`review_limits`. Numeric descriptors include `zero_is_default`: zero retains
60 seconds / 131072 bytes / 1024 output tokens; otherwise the ranges are
1–300 / 1024–2097152 / 128–8192. The security field carries `must_be_true`.
These rules are part of the field hash, shared with native validation. Newly
edited invalid values are rejected even while disabled. Unchanged dormant
legacy values can remain during unrelated saves; enabling validates all fields.

Operator generation adds `generation_model_name`, nested `generation_settings`
and `maintenance_budget` descriptors. `generation.enabled` defaults to false;
the model reference alone does not authorize work. Its timeout/input/output
fields accept 0 for 60 seconds / 131072 bytes / 8192 tokens, otherwise
1–300 / 1024–2097152 / 128–32768. Budget fields are positive and bounded:
daily calls 1–100000, daily tokens 1–1000000000, job seconds 1–3600, workers 1–16.
They never interpret 0 as unlimited. YAML activation requires all budget fields;
templates and the native draft display bounded defaults (30 / 100000 / 300 / 1).
Limits are checked before YAML integer conversion can truncate fractions.
The source grant is the existing candidate destination set (at most 32 sources).
Global evolution disable and generation disable both remove the worker option
at launch, without deleting history. Waku renames the generation model reference
alongside the two reviewer names and checks every nested descriptor's hash.

See [generation configuration and actual consumption](../acp/skill-evolution.md#yaml-and-waku-generation-settings).
`generation_settings_test.go` exercises save -> launch -> actual Eino requests:
distinct model routes/credentials, output limits, input bounds, per-call timeout,
job deadline, daily call/token limits and disabled-worker history recovery.

Snapshots expose supported values and path-only conversion diagnostics, never
unknown evolution values. `skill_evolution_migration_ack` must match the current
configuration revision before removing unsupported legacy settings. Conversion
requires review mode and fail-closed security; it preserves enablement, selected
models and unrelated configuration, backs up the original file, and clears its
acknowledgement after save. Invalid/stale acknowledgements fail before writes.
The Waku conversion button preserves invalid text drafts and participates in
the existing edit-generation guard. Native fields replace the opaque JSON editor;
nested controls require compatible parent and child descriptors from the daemon.

`skills_enabled` maps to `skills.enabled`. The obsolete filesystem `skills`
array is no longer produced by snapshots or accepted by saves. Settings save
does not rewrite extension-file bytes or Registry enablement; use `skill.*`
management operations for published skills. YAML managed sources and export
paths are resolved by the same read-only source checks used at launch. The
native source form uses `/skill_sources` with import_path, export_root,
nullable include_global and managed source definitions. These map to native
`skills.path`, `skills.export_root`, `skills.include_global` and
`skills.managed_sources`; no additional runtime configuration is created. See
`internal/launch/skills.md` for the schema and scope rules. Existing import-source
IDs remain stable for checkpoints and published histories.

Source collections have a bounded catalog descriptor, per-source object and
individual id/root/scope/workspace descriptors. Waku checks each parent and
field contract; collection edits require the complete source contract. The
native editor supports selection/add/remove and typed fields, with no JSON
passthrough or render-time I/O. Selecting a source never edits its configuration.
Identifier/root/scope changes can revoke access to old pinned tasks; removing
a declaration preserves database history and source files. IDs use the same
name validation as the Registry, including the reserved legacy import IDs.

Snapshots retain relative paths and distinguish null/global inheritance from
explicit booleans. Unknown root skills fields are path-only diagnostics and
stay in the original YAML during unrelated saves. Malformed source shapes and
unrecognized nested keys fail before exposing their contents. Unchanged source
representations are retained, including absent optional fields; edited sources
are validated without writing. Dormant unchanged invalid identities can remain
while skills are off, but enabling or editing requires the current contract.
New directory edits are checked even while skills are off. Native launch repeats
the checks; desktop source validation never starts a model or a runtime.

Integration coverage lives in `internal/desktopconfig/settings_contract_test.go`
and `internal/desktopconfig/evolution_settings_test.go`,
`internal/desktopconfig/skill_sources_test.go`,
`internal/desktopconfig/skill_source_settings_test.go` and
`cmd/deerflow-acpd-go/main_test.go`: save → native consumption, rejection
without writes, preservation of original data, stale schema, Unicode and time
boundaries, alias conversion, and schema advertisement over the real local
daemon protocol. Rust tests exercise stopped/live capability intersection,
unknown versions, malformed fields, and draft preservation. These tests do not
replace desktop interaction acceptance or a no-Python release check.
