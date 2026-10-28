#!/bin/sh
set -eu

# Keep the portable bridge's config/runtime arguments while enabling execution
# inside this Docker container. The immutable Go executable remains unchanged.
# skill_run checks the configured venv path before the backend canonicalizes it.
# The original deployment created its sessions beside config.yaml. Waku supplies
# a different DEERFLOW_GO_DATA_DIR on managed launches, so pin one explicit CLI
# value for every launch. Keep this separate from Waku's environment override.
data_dir="${DEERFLOW_PERSISTENT_DATA_DIR:-${DEER_FLOW_PORTABLE_ROOT:-/var/lib/deerflow}/user-data/config/go-harness-state}"
case "$data_dir" in
  /*) ;;
  *) echo 'DEERFLOW_PERSISTENT_DATA_DIR must be an absolute container path' >&2; exit 2 ;;
esac
exec /opt/deerflow/bin/deerflow-acpd.real "$@" \
  --data-dir="$data_dir" \
  --sandbox-provider=local \
  --sandbox-allow-shell=false \
  --sandbox-allow-command=/opt/deerflow-skill-python/bin/python \
  --sandbox-allow-command=/usr/bin/python3.11 \
  --sandbox-allow-command=/usr/bin/node \
  --sandbox-allow-command=/usr/bin/npm \
  --sandbox-allow-command=/usr/bin/npx \
  --sandbox-allow-command=/usr/bin/curl \
  --sandbox-allow-command=/usr/bin/git \
  --sandbox-allow-command=/bin/bash \
  --sandbox-allow-command=/usr/bin/tar \
  --sandbox-allow-command=/usr/bin/find \
  --sandbox-allow-command=/usr/bin/mktemp \
  --sandbox-env=DEERFLOW_SKILL_PYTHON=/opt/deerflow-skill-python/bin/python \
  --sandbox-env=PATH=/opt/deerflow-skill-python/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
  --sandbox-env=HOME=/home/deerflow \
  --sandbox-env=FANREN_RAG_CONFIG=/var/lib/deerflow/user-data/private/skills/fanren-rag/endpoint.json
