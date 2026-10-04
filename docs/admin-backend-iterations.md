> 历史记录：本文描述旧 Python 实现或当时的桌面设计，命令和能力声明不作为当前分支的使用说明。原文与源码参见[固定历史提交](https://github.com/omengye/deerflow-acp/tree/9c29fcaac991f2d9f1e7af68250b12f6b3ec07d9)。当前 Eino Go 入口与验收范围见[项目说明](../README.md)。

# Admin Backend Iteration Plan

## Background

The current `/management/` UI is a static management surface served by FastAPI only when API authentication is enabled. It can already call existing public API endpoints for read-only or limited write operations:

- `GET /api/models`
- `GET /api/skills`
- `POST /api/skills/{name}/enable`
- `POST /api/skills/{name}/disable`
- `GET /api/mcp/config`
- `PUT /api/mcp/config`
- `GET /health/ready`

The remaining gap is a dedicated Admin API layer. The Admin UI should not write raw YAML or expose resolved secrets directly. Backend work should therefore be split into two iterations:

1. Establish a secure Admin API foundation and complete model/config write workflows.
2. Add custom skill file management and broader runtime operations.

## Implementation Status

- Iteration 1 is implemented: Admin capability/config endpoints, model config writes, explicit config reload, UI integration, and tests.
- Iteration 2 is implemented: custom skill CRUD/history/supporting files, allowlisted runtime patching, MCP enable/disable/test, UI integration, and tests.
- Remaining ideas are outside this two-iteration scope: role-based admin auth, comment-preserving YAML edits, full sandbox/tracing/Feishu configuration management, and operator-approved live MCP tool discovery.

## Shared Design Principles

- All `/api/admin/*` endpoints are protected by the existing Bearer token middleware.
- Admin responses must redact secrets by default. Fields such as `api_key`, `secret`, `token`, `password`, `authorization`, `headers`, and `env` values should not return raw values unless a future explicit privileged export flow is designed.
- Config writes must be atomic: write to a temporary file in the same directory, validate by loading the resulting config, then replace the original.
- Config write APIs should preserve unrelated config sections and comments where practical. If comment preservation is not feasible in the first pass, the API must document that it rewrites normalized YAML and should keep the write surface narrow.
- After any config-changing operation, the backend must invalidate runtime caches that can hold stale models, skills, MCP tools, prompts, or clients.
- Public skill directories are read-only through Admin APIs. Custom skill writes are limited to `skills/custom/`.

## Iteration 1: Admin Foundation And Model Config

### Goal

Make `/management/` a real authenticated management panel for service identity, safe config inspection, model configuration writes, and config reloads.

### Deliverables

- Add `app/routers/admin.py` and mount it under `/api`.
- Add Admin capability discovery for the UI.
- Add redacted config/status reads.
- Add model configuration create/update/delete support.
- Add explicit config reload with cache invalidation.
- Update Admin UI model page from "copy YAML" to "save model" once endpoints are available.

### Backend API Design

#### `GET /api/admin/me`

Purpose: let the UI confirm it is using an authenticated Admin API and discover supported capabilities.

Response shape:

```json
{
  "authenticated": true,
  "auth": {
    "type": "bearer",
    "auth_enabled": true
  },
  "capabilities": {
    "config_read": true,
    "config_reload": true,
    "models_write": true,
    "custom_skills_write": false,
    "runtime_write": false
  }
}
```

Notes:

- Do not return token values or API key identifiers.
- In this project there is no user/role model yet, so the token represents the admin principal.

#### `GET /api/admin/config`

Purpose: return a redacted operational view of the active configuration.

Response shape:

```json
{
  "config_path": "D:/Tools/deerflow-api/config.yaml",
  "config_version": 12,
  "mtime": "2026-07-07T10:00:00+08:00",
  "api": {
    "host": "0.0.0.0",
    "port": 8000,
    "auth_enabled": true,
    "checkpointer_type": "sqlite",
    "checkpointer_path": "./data/checkpoints.db",
    "model_name": null,
    "thinking_enabled": true,
    "subagent_enabled": true,
    "plan_mode": true,
    "max_concurrent_subagents": 4,
    "chat_request_timeout": 600
  },
  "models": [
    {
      "name": "qwen3.6-plus",
      "display_name": "Qwen 3.6 Plus",
      "use": "deerflow.models.patched_dashscope:PatchedDashScopeChatOpenAI",
      "model": "deepseek-v4-pro",
      "api_key": {
        "redacted": true,
        "source": "literal"
      },
      "base_url": "https://dashscope.aliyuncs.com/compatible-mode/v1",
      "supports_thinking": true,
      "supports_vision": true
    }
  ],
  "default_model": "qwen3.6-plus",
  "paths": {
    "skills_root": "D:/Tools/deerflow-api/skills",
    "extensions_config": "D:/Tools/deerflow-api/extensions_config.json",
    "data_dir": "./data"
  }
}
```

Notes:

- This endpoint should show the file-backed config, not a fully secret-resolved object.
- If a value is a placeholder such as `$DASHSCOPE_API_KEY`, return that placeholder and mark `source: "env_ref"`.
- If a value is a literal secret, return only redaction metadata.

#### `PUT /api/admin/models`

Purpose: replace the model list and optionally the default model in `config.yaml`.

Request shape:

```json
{
  "models": [
    {
      "name": "qwen3.6-plus",
      "display_name": "Qwen 3.6 Plus",
      "use": "deerflow.models.patched_dashscope:PatchedDashScopeChatOpenAI",
      "model": "deepseek-v4-pro",
      "api_key": "$DASHSCOPE_API_KEY",
      "base_url": "https://dashscope.aliyuncs.com/compatible-mode/v1",
      "supports_thinking": true,
      "supports_vision": true,
      "when_thinking_enabled": {
        "extra_body": {
          "enable_thinking": true
        }
      }
    }
  ],
  "default_model": "qwen3.6-plus",
  "reload": true
}
```

Response shape:

```json
{
  "success": true,
  "models": [
    {
      "name": "qwen3.6-plus",
      "display_name": "Qwen 3.6 Plus",
      "supports_thinking": true,
      "supports_vision": true
    }
  ],
  "default_model": "qwen3.6-plus",
  "reloaded": true
}
```

Validation:

- Model names must be unique and non-empty.
- `default_model`, when provided, must match one of the model names.
- Each item must validate against `deerflow.config.model_config.ModelConfig`.
- `use` should be a non-empty import path string.
- Secret-like fields should accept either environment variable placeholders or literal values. The response never echoes literal secrets.
- Reject an empty model list unless an explicit future "disable model runtime" design exists.

Implementation notes:

- In the first iteration, replace the full `models` array instead of supporting fine-grained patch semantics. This is simpler and avoids merge ambiguity.
- Keep `GET /api/models` unchanged for public/client compatibility.
- If `reload=true`, call the same reload path as `POST /api/admin/config/reload`.

#### `POST /api/admin/config/reload`

Purpose: explicitly reload config and clear stale runtime state.

Request shape:

```json
{
  "include_extensions": true,
  "reset_clients": true
}
```

Response shape:

```json
{
  "success": true,
  "config_version": 12,
  "models_count": 1,
  "extensions_reloaded": true,
  "clients_reset": true
}
```

Required cache invalidation:

- `reload_app_config(settings.config_path)`
- `reload_extensions_config()` when requested
- Reset MCP tools cache
- Clear `ClientManager._client_map`
- Clear `ClientManager._async_client_map`
- Reset existing cached agents where reachable

Do not close active runs in iteration 1. If active runs exist, reload affects new runs only. Return a warning field when active threads are present.

### Frontend Changes

- On login, call `GET /api/admin/me` after token validation and store capabilities in state.
- Runtime view should use `GET /api/admin/config` instead of static explanatory text.
- Model draft panel should become a real save form when `models_write=true`.
- After model save, call reload or rely on `reload=true`, then refresh `/api/models`.
- Secret fields should display as "configured" or "env ref", not raw values.

### Tests

- Admin routes reject unauthenticated requests when auth is enabled.
- `GET /api/admin/config` redacts literal secrets.
- `PUT /api/admin/models` validates duplicate names, missing default model, malformed model item, and empty list.
- Model write preserves unrelated top-level config sections.
- Reload clears client maps and reloads app config.
- Existing `/api/models` behavior remains unchanged.

### Acceptance Criteria

- Admin UI can display config status and save model changes without manual YAML copy.
- No Admin response returns literal API keys or configured secrets.
- A saved model appears in `GET /api/models` after reload.
- Existing production control tests still pass.

## Iteration 2: Custom Skills And Runtime Operations

### Goal

Complete the Admin UI as an operational management surface by adding custom skill file management, safer MCP/runtime operations, and richer service status.

### Deliverables

- Add custom skill CRUD endpoints backed by `deerflow.skills.manager`.
- Add skill content read, write, delete, history, and supporting file management.
- Add runtime config read/write for selected safe API fields.
- Add MCP server-level operations and connection test hooks.
- Update the Admin UI Skills and Runtime pages to use these endpoints.

### Backend API Design

#### `GET /api/admin/skills/custom`

Purpose: list custom skills with editable metadata.

Response shape:

```json
{
  "skills": [
    {
      "name": "market-research",
      "description": "Collect and summarize market information",
      "enabled": true,
      "path": "skills/custom/market-research/SKILL.md",
      "updated_at": "2026-07-07T10:00:00+08:00"
    }
  ]
}
```

#### `GET /api/admin/skills/custom/{name}`

Purpose: read a custom skill's `SKILL.md` content and supporting file index.

Response shape:

```json
{
  "name": "market-research",
  "content": "---\nname: market-research\ndescription: ...\n---\n",
  "enabled": true,
  "files": [
    "references/sources.md",
    "templates/report.md"
  ]
}
```

#### `PUT /api/admin/skills/custom/{name}`

Purpose: create or replace a custom skill's `SKILL.md`.

Request shape:

```json
{
  "content": "---\nname: market-research\ndescription: ...\n---\n",
  "enabled": true,
  "reload": true
}
```

Validation and safety:

- Validate `name` with `validate_skill_name`.
- Validate frontmatter with `validate_skill_markdown_content`.
- Reject attempts to overwrite public skills through this endpoint.
- Run `scan_skill_content` or a deterministic local fallback policy before writes.
- Write with `atomic_write`.
- Append a history record.
- Refresh skills prompt cache and reload extensions state when needed.

#### `DELETE /api/admin/skills/custom/{name}`

Purpose: delete a custom skill directory.

Rules:

- Only delete under `skills/custom/{name}`.
- Refuse if the path is not a custom skill directory.
- Append history before deletion.
- Refresh skills prompt cache.

#### `GET /api/admin/skills/custom/{name}/history`

Purpose: inspect Admin/agent changes for a custom skill.

Response shape:

```json
{
  "name": "market-research",
  "history": [
    {
      "ts": "2026-07-07T10:00:00+00:00",
      "action": "edit",
      "author": "admin",
      "file_path": "SKILL.md"
    }
  ]
}
```

Do not return full previous/new content by default in the list response. Add a later detail endpoint if diff inspection is needed.

#### `PUT /api/admin/skills/custom/{name}/files/{path}`

Purpose: write supporting files under allowed skill subdirectories.

Allowed subdirectories:

- `references/`
- `templates/`
- `scripts/`
- `assets/`

Rules:

- Reuse `ensure_safe_support_path`.
- For `scripts/`, require strict security scan approval.
- Keep file size limits to prevent accidental large writes.

#### `DELETE /api/admin/skills/custom/{name}/files/{path}`

Purpose: remove a supporting file.

Rules:

- Reuse `ensure_safe_support_path`.
- Append history with the removed file path.

### Runtime Config Operations

Add selected safe write APIs only after the iteration 1 config writer exists.

#### `PATCH /api/admin/runtime`

Purpose: update safe `api` section fields without editing the whole config.

Initial writable fields:

- `model_name`
- `thinking_enabled`
- `subagent_enabled`
- `plan_mode`
- `max_concurrent_subagents`
- `chat_request_timeout`
- `max_upload_size_mb`
- `max_uploads_per_request`
- `allowed_upload_extensions`
- `scheduler_enabled`
- `scheduler_poll_interval_seconds`
- `scheduler_timezone`

Non-goals for this iteration:

- Editing `api_keys`
- Editing Feishu credentials
- Editing tracing credentials
- Editing sandbox provider and mounts
- Editing arbitrary YAML paths

Response should indicate whether each changed field is hot-applied or requires service restart.

### MCP Enhancements

The current `GET/PUT /api/mcp/config` supports full JSON read/write. Iteration 2 should add admin-friendly operations:

- `POST /api/admin/mcp/{name}/enable`
- `POST /api/admin/mcp/{name}/disable`
- `POST /api/admin/mcp/{name}/test`

The test endpoint should validate config shape and attempt a lightweight initialization/list-tools operation with a timeout. It must not leak headers, env values, OAuth client secrets, or tokens.

### Frontend Changes

- Skills page:
  - Open custom skill editor.
  - Create/edit/delete custom skill.
  - Show validation errors inline.
  - Show history metadata.
  - Keep public skills read-only except for enable/disable.
- Runtime page:
  - Replace static text with editable controls for safe runtime fields.
  - Show "requires reload" or "requires restart" badges per field.
- MCP page:
  - Add per-server enable/disable/test actions.
  - Keep full JSON editor as advanced mode.

### Tests

- Custom skill create/edit/delete happy path.
- Reject invalid skill names and frontmatter mismatch.
- Reject path traversal in supporting files.
- Reject public skill overwrite/delete.
- History is appended on create/edit/delete/supporting file writes.
- Skill reload makes changes visible through `GET /api/skills`.
- Runtime patch only accepts allowlisted fields.
- MCP test redacts secrets in both success and error responses.

### Acceptance Criteria

- Admin UI can create and edit custom skills without filesystem access.
- Public skills cannot be modified through custom skill endpoints.
- Runtime page displays real config state and can update safe operational fields.
- MCP page supports both advanced JSON editing and safer per-server actions.
- Secret material remains redacted in all Admin responses and errors.

## Suggested Implementation Order

1. Add reusable config read/write/redaction helpers.
2. Add `admin.py` router with `me`, `config`, `models`, and `reload`.
3. Add cache invalidation method to `ClientManager`.
4. Update tests for iteration 1.
5. Update Admin UI to use iteration 1 endpoints.
6. Add custom skill HTTP wrapper around existing skill manager utilities.
7. Add runtime patch and MCP admin operations.
8. Update tests and UI for iteration 2.

## Open Decisions

- Whether model writes should preserve YAML comments. A narrow writer that only updates `models` and `default_model` may be enough for iteration 1.
- Whether skill security scanning should call an LLM in Admin flows or use a deterministic local policy first. For predictable Admin UX, prefer deterministic checks plus optional scan warnings.
- Whether runtime config changes should hot-apply to `settings` in process. Iteration 2 can start by writing config and reporting reload/restart requirements.
- Whether future Admin APIs need roles. Current Bearer token auth is enough for this two-iteration plan.
