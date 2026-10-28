# DeerFlow Server container

This pilot runs the authenticated Waku WebSocket control daemon and Go DeerFlow
runtime in one container. The Windows desktop can connect directly over the LAN;
normal desktop connections do not require SSH.

## Build

Run from the repository root:

```sh
docker build \
  --file deploy/docker/Dockerfile.server \
  --tag deerflow-server:pilot-20261024 \
  --build-arg DEERFLOW_BUILD_ID="$(git rev-parse --short HEAD)" .
```

The build defaults to GOPROXY=https://goproxy.cn,direct. Override that build
argument where another proxy is appropriate. Cargo.server.toml and
Cargo.server.lock provide a server-only Rust workspace without GPUI dependencies.
Rust dependencies and targets use BuildKit cache mounts; CARGO_BUILD_JOBS
defaults to four.

## Run and LAN address

Copy compose.yaml and .env.example into a private deployment directory, rename
the environment example to .env, and generate a random WAKU_DAEMON_TOKEN.
Use mode 0600 for .env. Create data, logs, and the workspace directory with
ownership matching DEERFLOW_UID:GID.

For this Gentoo deployment, .env contains DEERFLOW_BIND_ADDRESS=192.168.1.190.
The endpoint is ws://192.168.1.190:18787/v1. Compose defaults to 127.0.0.1 if the
bind address is omitted, so other deployments remain local until configured.

```sh
cd /home/gen/deerflow-deploy
docker compose config --quiet
docker compose up -d --wait
docker compose ps
```

WS is intended for the trusted LAN in this pilot. Use a TLS reverse proxy and WSS
when an encrypted connection is needed. The token grants control of this service.

## Desktop settings

Open Settings → Server connection (设置 → 服务器连接), select Remote server
(远程服务器), and enter the endpoint and Token. Test connection checks protocol
compatibility, authentication and settings RPC. Save and restart desktop
(保存并重启桌面) validates the connection before saving and switching. Stop
active tasks before switching; closing a remote desktop keeps the server running.

The Token input is masked. Windows saves credentials with current-user DPAPI
encryption in user-data/desktop/server-connection.json; Unix uses a mode-0600 file.
These preferences stay on the desktop and never go through server settings RPC.
Saved UI preferences take precedence over the legacy WAKU_DAEMON_ADDRESS and
WAKU_DAEMON_TOKEN environment launcher. If the saved server is unavailable, the
desktop opens local settings with an explicit connection error so it can be fixed.

The optional PowerShell launcher also connects directly, asks for a masked Token,
and saves the same encrypted preferences; it does not invoke SSH:

```powershell
.\deploy\docker\start-desktop-remote.ps1 `
  -DesktopPath '.\dist\desktop\DeerFlow-Desktop-Go-20261010-103944\deerflow-desktop.exe' `
  -ServerAddress 'ws://192.168.1.190:18787'
```

Configure the model and API key in the connected desktop's DeerFlow settings.
The initial image contains a placeholder key. Health checks do not make model
requests. Select server paths such as /workspaces when opening projects.

## Persistent data and maintenance

The deployment directory is /home/gen/deerflow-deploy. Persistent configuration,
databases and Go runtime logs are under data/user-data. Host project files are
under /home/gen/deerflow-workspaces and mounted as /workspaces. The initial
configuration disables Bash and host tools. Docker stdout/stderr logs rotate at
10 MiB per file, with three files retained.

To update, build or load a new image, change DEERFLOW_IMAGE and run
docker compose up -d. docker compose down leaves bind-mounted data intact.
After verification, remove only this deployment's temporary source archives,
superseded images and specific build-cache records. Keep .env, data and workspaces.
