# Daemon lifecycle diagnostics

`deerflow-acpd-go` writes JSON Lines without requiring an inherited terminal or
stderr handle. Desktop launches set `DEERFLOW_GO_LOG_DIR` to `user-data/logs`.
Standalone launches accept `--log-dir`; the fallback is `runtime-dir/logs`, then
the explicitly supplied data directory's logs, then the default Go data directory.

Each process creates `deerflow-acpd-<pid>-<unique>.jsonl` and retains at most two
2 MiB rotations (`.1`, `.2`) for that file. Separate starts own separate files;
this does not impose a global retention policy on logs from earlier starts.
Log creation or write failure does not abort an otherwise usable daemon.

Records contain UTC time, process ID, host-defined lifecycle phase, severity,
and a known error category. Phases identify configuration loading, harness open,
endpoint startup, readiness, session retention cleanup, and shutdown. They do
not contain raw errors, model/tool input/output, configuration, endpoint tokens,
or environment values. Configuration validation still provides its specific
field diagnostics through the configuration service; these logs locate the
failed startup stage rather than duplicating that potentially sensitive text.

`daemon.status`, `daemon.drain`, and `daemon.resume` report `diagnostics_log`
with availability, file path and rotation bounds. Waku's overview displays the
active file or its launch directory if startup has not completed. Render code
reads cached status and does not probe the filesystem.
