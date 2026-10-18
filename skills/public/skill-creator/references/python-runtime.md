# Python skill resources

## Package source, not an environment

Include required `.py` source files and text dependency declarations such as `requirements.txt` or `pyproject.toml`. Keep the user's Python implementation when it suits the requested task; the Go host does not prohibit Python skill resources.

Do not package `.venv`, `site-packages`, `__pycache__`, bytecode, wheels, interpreter binaries, credentials or generated installation output. Path traversal, links, permission overrides and size limits still apply. Dependency declarations are reviewable input, not permission to install or execute packages.

Document the Python version, supported platforms, entry script, arguments and required packages. The optional `skill-runtime.json` declaration uses this shape:

```json
{
  "version": 1,
  "python": {
    "min_version": "3.10",
    "packages": ["paramiko"],
    "entrypoints": ["scripts/diagnose.py"]
  }
}
```

`packages` contains distribution names, not arbitrary import expressions or installer arguments. Declare package requirements in the appropriate text dependency file too. The host checks distribution metadata without importing declared packages or installing them. Without this optional manifest, `.py` resources can still run, but undeclared dependencies are not established as ready.

Use only supported declaration fields. Invented `meta.json` keys do not enable dependency checks. The host selects its configured Python environment; a candidate must not specify an absolute interpreter path. An operator's configured `DEERFLOW_SKILL_PYTHON` environment override is not a skill-controlled setting.

## Check the session snapshot

Use `skill_run` with `action: "check"` and the skill's `name` for the session-selected snapshot. Inspect the actual schema for any additional constraints. Read the returned version and readiness diagnostics. A `skill://` URI or `<SKILL_DIR>` placeholder is not an executable filesystem path.

The host binds checks and execution to an immutable skill snapshot. Do not substitute a mutable installation directory or an externally edited script when the session is pinned to another version. After changing the package, obtain new approval and verify which version the next turn selects.

A readiness check is not behavioral testing and does not authorize execution. If Python or a dependency is missing, report the exact unready condition. Do not automatically run `pip install`, create environments, modify global Python or install dependencies on skill load. Dependency preparation is a separate authorized operation with a disclosed installation plan.

## Run only the authorized operation

Use `skill_run` with `action: "run"`, `name`, package-relative `path`, explicit `args` and an appropriate `timeout_seconds`. The host checks readiness before running, materializes the pinned package and selects the configured interpreter. Follow the existing execution-approval policy; a skill's manual publication is not command authorization. Do not execute in plan mode.

Local execution, PowerShell and WSL do not become security sandboxes through snapshotting or hash checks. Use an actually configured isolation backend when required for untrusted execution. Do not claim isolation that the host has not supplied.

Record the returned version, environment and process result. Distinguish missing runtime, denied execution, nonzero exit, timeout and successful completion. A zero exit code proves process completion, not that every remote operation or output was correct; inspect the expected artifacts. Never retry an externally mutating operation solely because its response was lost.
