ARG BASE_IMAGE=deerflow-server:single-user-memory-20261027
FROM registry-1.docker.io/library/rust:1.96-bookworm@sha256:a339861ae23e9abb272cea45dfafde21760d2ce6577a70f8a926153677902663 AS builder
WORKDIR /src/desktop-app
COPY desktop-app/ ./
COPY deploy/docker/Cargo.server.toml ./Cargo.toml
COPY deploy/docker/Cargo.server.lock ./Cargo.lock
ENV CARGO_BUILD_JOBS=4 CARGO_NET_OFFLINE=true
RUN cargo test --locked -p waku-protocol --lib \
    && cargo test --locked -p waku-core --lib server:: \
    && cargo test --locked -p waku-daemon \
    && cargo build --locked --release -p waku-daemon --bin waku-daemon

FROM ${BASE_IMAGE}
LABEL cc.sprwhisp.deerflow.rollout="connection-step1-20261028"
USER root
COPY --from=builder --chmod=0755 /src/desktop-app/target/release/waku-daemon /opt/deerflow/bin/waku-daemon
USER deerflow:deerflow
