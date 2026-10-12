# Claude model adapter patch

Upstream: `github.com/cloudwego/eino-ext/components/model/claude v0.1.25`.
Source: https://github.com/cloudwego/eino-ext/tree/components/model/claude/v0.1.25/components/model/claude
License: Apache-2.0, retained in `LICENSE` and the source headers.

This directory contains the upstream module manifest and its four production
source files (`claude.go`, `message_extra.go`, `option.go`, `utils.go`). The first
behavioral modification gives `ChatModel.Stream`'s message concatenation error a
goroutine-local variable. Upstream writes the method's named return variable
from the stream goroutine while the caller is returning, which races.
`gofmt` also reordered two imports. The module cache is not modified.

On 2026-10-02, an opt-in `Config.DisableRetries` was added. It sends the SDK's
`option.WithMaxRetries(0)` on model requests, so skill maintenance can charge
and retry each provider request through its durable budget. The default retains
upstream retries; ordinary conversation models keep that default. Maintenance
provider fixtures exercise HTTP 503 failures for OpenAI and Claude.

Original `claude.go` SHA256:
`3D6DF9037EBB0506E55EE8C71BA0833DC39B32BDC0C2C050939F2AAD9DC07E25`.
Module sum: `h1:ZMJZmIWv3KZXPoVzYRiQB4/n9TktqnTBzu0nrxKv+G4=`.
go.mod sum: `h1:2N/8/BccxDYp8ilOcucPSzigX9zm4SVFSlbE8zdvdSU=`.

On 2026-10-01, v0.1.26 still contains the same race and additionally requires
`CacheWriteTokens`, unavailable in the project's Eino v0.10.0-alpha.35.
Remove this local replacement after a compatible upstream release fixes the
race and provides retry control, and passes the provider fixtures and role regressions. No upstream issue
or message has been sent.

Regression from `go-harness` (uses local HTTP provider fixtures):

```sh
go test -race ./internal/engine/eino ./internal/agents -run 'TestController|TestConfiguredRole|TestRoleProvider|TestRoleThinking|TestCatalog' -count=1 -timeout 180s
```
