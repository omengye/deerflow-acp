# Session thinking mode

The native desktop composer exposes thinking mode independently of reasoning
effort for DeerFlow sessions. The menu offers `default`, `on` and `off` when
the daemon advertises them. Busy sessions and pending option changes disable
the controls. A failed change restores the daemon-confirmed selection.

`off` preserves the selected effort and does not disable its selector. Independent
`enable_thinking` and `thinking_type` contracts continue sending the selected
effort, including while thinking is off. Only the shared `reasoning_effort`
contract replaces the request effort with `none` while off, without changing
the saved level. Re-enabling uses the selected level.
`default` removes the explicit thinking override; it does **not** disable thinking.
This is a model execution setting, not a transcript visibility preference.

## Model configuration

No capability is inferred from the model name, provider name or URL. Configure
the real endpoint contract under Settings > Models > Advanced, or in YAML:

```yaml
models:
  - name: example
    provider: openai
    model: example-model
    base_url: https://example.invalid/v1
    api_key: $EXAMPLE_API_KEY
    reasoning_efforts: [low, medium, high]
    thinking_parameter: enable_thinking
```

Supported `thinking_parameter` values:

| Value | On | Off |
| --- | --- | --- |
| empty/omitted | No switch advertised | No switch advertised |
| `unsupported` | Explicitly declared not switchable | No switch advertised |
| `enable_thinking` | `enable_thinking: true` | `enable_thinking: false` |
| `thinking_type` | `thinking: {type: enabled}` | `thinking: {type: disabled}` |
| `reasoning_effort` | Explicitly selected effort; default is rejected | `reasoning_effort: none` |
| `claude` | Native thinking enabled with a token budget | Native thinking disabled |

The `enable_thinking`, `thinking_type` and `reasoning_effort` contracts require an OpenAI-compatible provider.
`reasoning_effort` requires a nonempty `reasoning_efforts` list; selecting this
contract explicitly asserts the endpoint accepts `none`. `claude` requires a
Claude/Anthropic provider. Its optional `thinking_budget_tokens` defaults to
1024, must be at least 1024, and must remain below the effective output limit.
Runtime budget exhaustion fails rather than silently disabling thinking.
The `claude` contract is specifically the legacy manual-thinking API, not
adaptive thinking. Claude effort (`output_config.effort`) is not yet integrated
by this application; this does not mean the Claude API lacks effort support.
Switching back to provider defaults removes overrides, never picks the first
allowlisted level. For a shared-parameter endpoint, select a concrete level
before enabling thinking, or return thinking to default before resetting effort.

Provider adapters still own their remote capability checks. An unsupported
endpoint may reject the request; the daemon never treats omission of an effort
parameter as proof that thinking was disabled. Do not configure a switch for
models that always reason or endpoints that ignore these fields.

## Protocol and persistence

The daemon uses existing ACP v1 configuration calls, with no custom RPC:

```json
{"jsonrpc":"2.0","id":1,"method":"session/set_config_option","params":{"sessionId":"SESSION","configId":"thinking_enabled","value":"off"}}
```

`thinking_enabled` is a DeerFlow-defined option ID, not an ACP-standard field.
The option uses the standard `select` mechanism with `default`, `on` and `off`.
The separate `reasoning_effort` option retains category `thought_level`.
Responses and `config_option_update` notifications include both settings.
Optional model metadata `_meta.deerflow.thinkingModes` maps model IDs to
capability booleans; the live session option is authoritative.
`_meta.deerflow.thinkingAvailability` distinguishes unconfigured capabilities
from explicitly unsupported switches; older daemons without it remain unknown.

The native desktop carries `thinkingMode` in Waku daemon IPC, then translates
it to ACP `thinking_enabled`. ACP v1 Bridge forwards the request unchanged.
New clients can read legacy boolean/on-off switches, but never conflate their
values with reasoning levels. An old daemon without the capability cannot
silently acknowledge an explicit on/off request.

SQLite adds `harness_session_configs.thinking_mode`; old rows default to an
unset override. Session restart preserves both settings. Switching to a model
without the capability clears the override. Durable execution identity includes
the override, so a suspended execution cannot resume with changed parameters.
Background and role agents retain their own recipes; the main session switch
does not override subagent thinking configuration.

Web/mobile UI changes are outside this feature; generated shared protocol
bindings include the optional session field for compatibility.
