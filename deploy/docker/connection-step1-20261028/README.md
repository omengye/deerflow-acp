# Mobile connection recovery release (2026-10-28)

This release changes the Waku daemon's authentication budget from 5 to 20 seconds.
`WAKU_AUTHENTICATION_TIMEOUT_SECONDS` or `--authentication-timeout-seconds` can set
1–60 seconds (the CLI takes precedence). Invalid values fail startup. The absolute
deadline, authentication quotas, token validation and protocol version 10 remain
enforced. Existing v10 desktop and mobile clients remain wire-compatible.

The Android companion release is 2.1.2, with foreground retry wakeups, stable-link
backoff reset, ordered network callbacks and bounded read-only artifact retries.
There are no memory, database-schema, model, skill or public-port changes.

## Source provenance

The deployed v10 source baseline includes earlier changes that are not committed
in the current backend checkout. This task preserves that working tree rather
than committing unrelated changes. `step1-server.patch` contains only this task's
four source-file changes; it applies to the verified deployment baseline, **not
directly to Git HEAD**. The same changes are present in the working source files.

`base-source-manifest.json` is the existing deployment's source checksum inventory.
`prepare.py` verifies 125 build inputs against it (allowing Windows line endings),
then proves the patched snapshot equals exactly the task patch. It creates:

- `baseline-source.tar.gz`: durable, normalized deployment source baseline;
- `build-source.tar.gz`: patched source and this release's Dockerfile;
- `source-verification.json`: archive hashes, patch hash and changed-file list.

Release source artifacts are retained in `dist/connection-step1-20261028` locally
and `/home/gen/deerflow-deploy/connection-step1-20261028` on Gentoo. They are release
inputs, not disposable build directories. The base image is pinned by verification
to `sha256:d89395c9629b359a3c8a8688f9a0f0e2145ff32a4c67f30a99b5226c045ab26f`.
The builder image is pinned by digest in the Dockerfile. Only `waku-daemon` is
replaced in the runtime image; inherited entrypoint, Go runtime, health check,
memory services and bind mounts are preserved.

## Rebuild and deployment

Extract `build-source.tar.gz` into a dedicated build directory. Copy
`deploy/docker/Cargo.server.toml` and `.lock` to `desktop-app/Cargo.toml` and
`Cargo.lock`. From `desktop-app`, use `cargo vendor --locked --versioned-dirs
--respect-source-config vendor` to obtain dependencies verified against the lock
file, and save its emitted source configuration as `.cargo/config.toml`.
The Docker build runs offline with these vendored dependencies. Temporary vendor
contents and extracted build directories can be removed after validation.

Verify the base image ID before `docker build -t
deerflow-server:connection-step1-20261028 .`. The build runs protocol, server
transport/authentication and daemon argument tests before producing the release
binary. Registry access in the initial build environment was slow; vendoring
avoids network dependence during compilation and tests.

On Gentoo, `python3 -B release.py smoke` validates an isolated loopback-only
container using a random temporary token. It checks normal authentication,
6-second and 10-second delayed Hello, invalid credentials and expiry beyond the
20-second deadline. Test containers do not mount production data.

`python3 -B release.py apply` verifies the tested image, requires an idle runtime,
drains work, changes only `DEERFLOW_IMAGE`, recreates the service, then verifies
health and read-only v10 RPCs over direct and public endpoints. It resumes work
after verification. Rollback restores the previous image configuration; it never
restores an old copy of business data. The private `previous.env` backup remains
on the server for rollback and must not be committed or printed.

Reports contain counts, elapsed times and image IDs, never tokens or chat content.
Physical Android radio switching and vendor battery-management behavior still
require on-device validation; JVM timing tests are not a substitute for those.

## Completed validation

The production image is `sha256:0a39622576b914ba64d73794288315b824b48bbdcfcb22d7687c8799872a6514`.
All 124 Rust protocol/server/daemon tests and 19 release boundary tests passed.
Isolated tests accepted 6-second and 10-second delayed Hello, rejected a wrong
token and closed an unauthenticated connection beyond 20 seconds. Production
verification repeated the delayed Hello and settings/catalog reads through
`wss://deerflow.sprwhisp.cc/v1`; the service is healthy and resumed accepting work.
The inherited Go binaries, entrypoint, health checker and production configuration
retain their prior hashes. See `reports/` for measurements and cleanup records.

The first deployment check used a loopback address absent from the existing port
bindings and automatically rolled back. The direct probe was corrected to
`100.80.205.120:18787`; redeployment then passed. Existing port bindings are unchanged.
