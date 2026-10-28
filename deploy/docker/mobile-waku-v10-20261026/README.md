# Mobile Waku v10 rollout helpers

This directory is separate from the existing deployment sources. The overlay replaces only
`waku-daemon`, `deerflow-server-healthcheck`, and the entrypoint script; it keeps the deployed
music/skills image, Go `.real` binary, ACP bridge, wrapper, Python/Node dependencies,
entrypoint path and health-check command. The entrypoint starts the native daemon before
Waku so management requests work immediately after restart. Cleanup runs on startup failure,
signals and Waku exit, and native processes do not inherit the Waku authentication token.
`BASE_IMAGE` is configurable. Do not substitute the bare server image for the
music/skills base: the live wrapper requires the `.real` binary and the installed skills.

## Prepare and build

1. Confirm all clients that connect to this daemon support v10. Do not blindly change a
   v9 client's numeric constant: it may need sequence/event/replay handling updates.
2. Record the existing image ID, Compose configuration, bind mounts and container health.
   Back up `.env`, `compose.yaml`, wrapper and data with private permissions. Never publish
   Token values in reports. Retain the old image for rollback.
3. Freeze a source snapshot containing the four Waku crates, locales, database migrations,
   `desktop-app/resources/deerflow/default-config.yaml`,
   server Cargo workspace/lock, health-check crate/lock, this Dockerfile and `entrypoint.sh`. The builder
   runs the protocol library and server transport/replay tests, then compiles both binaries.
4. Build from the repository/source-snapshot root with the pinned existing image as
   `BASE_IMAGE`, for example:

```sh
docker build --pull=false \
  --file deploy/docker/mobile-waku-v10-20261026/Dockerfile \
  --build-arg BASE_IMAGE=deerflow-server:music-opencli-20261024 \
  --build-arg SOURCE_REVISION='<revision-or-source-manifest-id>' \
  -t deerflow-server:mobile-waku-v10-20261026 .
```

If source review proves that matching Go or ACP changes are required, revise the overlay
explicitly before rollout. A new Go binary must replace `deerflow-acpd.real`, not the
bind-mounted wrapper path. Keep the existing portable root and native-history data path.

## Switch and verify

- Check active tasks before recreating the container. Stop/recreate ends live runtimes
  and their replay journals even though persisted conversation data remains.
- Change only the image and required port publication. Preserve existing Compose fields,
  volumes and the LAN publication, and add `100.80.205.120:18787:18787` for the Caddy host.
- Run `python3 rollout.py --check`, then `python3 rollout.py --apply` from the remote
  rollout directory. The script checks native active operations, stops FoloToy, backs up
  data while both services are stopped, and recreates DeerFlow followed by the matching
  FoloToy v10 adapter. Other Docker services are not recreated.
- Verify the LAN endpoint, Tailscale endpoint, then public `wss://deerflow.sprwhisp.cc/v1`.
  Run the script with the Token already in its environment:

```sh
python3 verify_wss.py --endpoint wss://deerflow.sprwhisp.cc/v1 \
  --inspect-session --reconnect-cycles 2 --abrupt-first-disconnect
```

The probe authenticates, reads settings and the catalog, optionally hydrates and attaches
an existing DeerFlow session, and validates bounded replay pages when a runtime exists.
It never starts a provider, submits a prompt, writes task state, cancels, approves, or
deletes. A replay result of `skipped: no_live_runtime` is expected after restart and does
not prove live-stream replay. `complete: false` means the configured page cap was reached.
Only counts and controlled result codes are emitted; TLS verification stays enabled.

- Confirm the original skill dependencies, `.real` executable, wrapper mount, host-tools
  bridge and voice service health remain intact. Do not infer an end-to-end voice task
  succeeded solely from container health or this read-only probe.

## Rollback

Run `python3 rollout.py --rollback` in the remote rollout directory. It restores both
DeerFlow and FoloToy configurations, the FoloToy source module and both retained images,
so the v9 clients and server remain compatible. Preserve data and workspace mounts. Do
not automatically restore a data backup over data written since the rollout; assess that separately.
Check original protocol health and existing clients after rollback. A successful image
rollback cannot recover an already terminated in-memory runtime/replay journal.

The original Compose file publishes only the LAN address. A full rollback therefore
removes the Tailscale publication used by the public Caddy endpoint. The original v9
entrypoint also starts the native daemon lazily. To restore management immediately
after rollback, start the retained native runtime explicitly:

```sh
docker exec deerflow-deploy-deerflow-1 env -u WAKU_DAEMON_TOKEN \
  /var/lib/deerflow/deerflow-acp \
  --config /var/lib/deerflow/user-data/config/config.yaml \
  --daemon /var/lib/deerflow/deerflow-acpd \
  --runtime-dir /var/lib/deerflow/user-data/runtime/acp-go --start-daemon
```

## Completed deployment

The mobile endpoint is `wss://deerflow.sprwhisp.cc/v1`, protocol 10. Use the existing
daemon Token and server workspace `/workspaces` in the Android app settings. TLS
validation, authentication rejection, catalog/history hydration, abrupt transport
disconnect followed by reconnection, and native `proposal.list` after cold startup
passed. The deployed FoloToy client also authenticated and read settings successfully.
The original Go/ACP/skill binaries, native data directory and host OpenCLI bridge
were preserved. Public verification is recorded in `reports/public-wss.json`.

The read-only verification had no live task runtime: live-event replay was skipped.
This does not constitute a physical-phone weak-network test or a complete voice/model
task test. Protocol/server replay and app resilience tests provide separate coverage.

Remote rollback records are in `/home/gen/deerflow-deploy/mobile-waku-v10-20261026`.
Keep `rollout.py`, `preflight.json`, `deployment.json`, all of `backup/`, and both
original image tags. The final `deployment.json.targetImageIds` must match the actual
running images. Source archives, test copies, build logs and dedicated mobile build
caches may be removed after validation; production data and shared caches are retained.
