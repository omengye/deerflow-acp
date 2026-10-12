# Desktop session reasoning effort

DeerFlow Desktop uses ACP v1 session configuration options to select reasoning
effort independently of the model. Rebuild the desktop, Waku daemon, Go ACP
daemon and configuration tool together; existing executables are not updated
by a source edit.

## Configure supported levels

In DeerFlow settings, select a model and fill **支持的思考强度** with one level
per line. Save and apply the configuration, then refresh the model catalog or
open a new session. The equivalent YAML is:

```yaml
models:
  - name: reasoning-model
    provider: openai
    model: your-model-id
    api_key: $MODEL_API_KEY
    reasoning_efforts: [low, medium, high]
```

Only declare levels actually supported by that model and endpoint. The current
OpenAI-compatible adapter accepts `low`, `medium`, `high`, `xhigh`, `max`. These are adapter limits, not automatic
assertions of remote model capability. Claude's thinking switch and token
budget are separate parameters and are not exposed as effort levels by this
implementation.

Omitted, null or empty `reasoning_efforts` hides the selector for that model.
The desktop always offers **Default** in addition to configured levels; Default
omits the provider parameter, it does not mean disabled thinking. No levels are
inferred from model names or old `supports_reasoning_effort` flags.

## Session behavior

- Changes are accepted only while the session is idle, with no waiting execution.
  The next model request uses the selected effort; an existing request is unchanged.
- Model and effort changes wait for the ACP response before further submission.
  The UI follows the confirmed server selection, including rejected changes.
- Switching models refreshes the available options. An incompatible remembered
  level falls back to Default; models without capabilities send no effort parameter.
- The Go database saves the selected effort and restores it on session load.
  Old databases migrate with an unset effort without changing existing settings.
- Main-agent calls and its tracked auxiliary calls use the selected effort.
  Child agents continue to use their own role recipes, without inheriting this override.
- Existing token-budget caps still apply. Selecting higher effort never raises
  a request's reserved output-token cap.

## ACP contract

`session/new`, `session/load` and `session/resume` expose a select option with
`id: reasoning_effort`, `category: thought_level`, `currentValue`, and `options`.
Clients send `session/set_config_option` with `configId: reasoning_effort` and a
selected string value. Replies and `config_option_update` carry the updated
configuration. `default` is represented internally as an unset effort.

For catalog discovery, the model option includes optional
`_meta.deerflow.reasoningEfforts`, a mapping from public model choice IDs to
configured levels. It contains no provider routes or credentials. Standard ACP
clients can ignore it and use the current session's `thought_level` option;
the desktop uses it to avoid probing each model with a separate session.

The desktop retains compatibility with older servers advertising only
`thinking_enabled` as a select or boolean. It does not send unadvertised effort
options to those servers.

## Focused checks

Run only the feature's tests rather than a whole-workspace regression:

```powershell
cd go-harness
go test -p 2 ./internal/acp/agent ./internal/desktopconfig ./internal/engine/eino ./internal/runtime -run '^TestSessionReasoning' -timeout 120s
cd ../desktop-app
cargo test --locked -p waku-core -p waku-protocol --lib session_reasoning
cargo check --locked -p waku --bin waku
```

The provider-wire tests use local HTTP fixtures, not paid model requests.
