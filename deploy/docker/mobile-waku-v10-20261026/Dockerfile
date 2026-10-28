# syntax=docker/dockerfile:1.7
# Build from the repository/source-snapshot root with -f pointing at this file.
# Preserve the deployed skills/music layers, Go .real binary and wrapper.
ARG BASE_IMAGE=deerflow-server:music-opencli-20261024
FROM rust:1.96-bookworm AS rust-builder

RUN apt-get update \
    && apt-get install --yes --no-install-recommends build-essential clang cmake pkg-config \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /src
COPY desktop-app/crates/waku-client ./desktop-app/crates/waku-client
COPY desktop-app/crates/waku-core ./desktop-app/crates/waku-core
COPY desktop-app/crates/waku-daemon ./desktop-app/crates/waku-daemon
COPY desktop-app/crates/waku-protocol ./desktop-app/crates/waku-protocol
COPY desktop-app/locales ./desktop-app/locales
COPY desktop-app/db/migrations ./desktop-app/db/migrations
COPY desktop-app/resources/deerflow/default-config.yaml ./desktop-app/resources/deerflow/default-config.yaml
COPY deploy/docker/Cargo.server.toml ./desktop-app/Cargo.toml
COPY deploy/docker/Cargo.server.lock ./desktop-app/Cargo.lock
COPY deploy/docker/healthcheck ./deploy/docker/healthcheck

ARG CARGO_BUILD_JOBS=4
ENV CARGO_BUILD_JOBS=${CARGO_BUILD_JOBS}

# Run the v10 wire and server transport/replay tests before producing deployable binaries.
RUN --mount=type=cache,id=deerflow-server-cargo-registry,target=/usr/local/cargo/registry \
    --mount=type=cache,id=deerflow-mobile-v10-daemon-target,target=/src/desktop-app/target \
    cargo test --locked --manifest-path desktop-app/Cargo.toml --package waku-protocol --lib \
    && cargo test --locked --manifest-path desktop-app/Cargo.toml --package waku-core --lib server:: \
    && cargo build --locked --release --manifest-path desktop-app/Cargo.toml --package waku-daemon --bin waku-daemon \
    && mkdir -p /out \
    && cp desktop-app/target/release/waku-daemon /out/waku-daemon

RUN --mount=type=cache,id=deerflow-server-cargo-registry,target=/usr/local/cargo/registry \
    --mount=type=cache,id=deerflow-mobile-v10-healthcheck-target,target=/src/deploy/docker/healthcheck/target \
    cargo build --locked --release --manifest-path deploy/docker/healthcheck/Cargo.toml \
    && cp deploy/docker/healthcheck/target/release/deerflow-server-healthcheck /out/deerflow-server-healthcheck

FROM ${BASE_IMAGE}

ARG SOURCE_REVISION=working-tree
LABEL cc.sprwhisp.deerflow.rollout="mobile-waku-v10-20261026" \
      cc.sprwhisp.deerflow.waku-protocol="10" \
      cc.sprwhisp.deerflow.source-revision="${SOURCE_REVISION}"

USER root
COPY --from=rust-builder --chmod=0755 /out/waku-daemon /opt/deerflow/bin/waku-daemon
COPY --from=rust-builder --chmod=0755 /out/deerflow-server-healthcheck /opt/deerflow/bin/deerflow-server-healthcheck
COPY --chmod=0755 deploy/docker/mobile-waku-v10-20261026/entrypoint.sh /opt/deerflow/bin/deerflow-server-entrypoint

# Fail the build if the selected base does not contain the expected existing runtime.
RUN test -x /opt/deerflow/bin/deerflow-acpd.real \
    && test -x /opt/deerflow/bin/deerflow-acp \
    && test -x /opt/deerflow-host-tools/opencli-proxy \
    && test -x /opt/deerflow-skill-python/bin/python \
    && /opt/deerflow-skill-python/bin/python -c "import paramiko, requests"

USER deerflow:deerflow

# ENTRYPOINT path, CMD, ENV, WORKDIR and HEALTHCHECK are intentionally inherited.
# The live Compose deployment also bind-mounts its own Go wrapper; retain that mount.
