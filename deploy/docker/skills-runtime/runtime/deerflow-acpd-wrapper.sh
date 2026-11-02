#!/bin/sh
set -eu

# Keep the portable bridge's config/runtime arguments while enabling execution
# inside this Docker container. The immutable Go executable remains unchanged.
# skill_run checks the configured venv path before the backend canonicalizes it.
# The native launcher owns runtime-data.json. Preserve a legacy explicit setting
# as a request that it must validate, never as a separate default database.
if [ -n "${DEERFLOW_PERSISTENT_DATA_DIR:-}" ]; then
  case "$DEERFLOW_PERSISTENT_DATA_DIR" in
    /*) set -- "$@" "--data-dir=$DEERFLOW_PERSISTENT_DATA_DIR" ;;
    *) echo 'DEERFLOW_PERSISTENT_DATA_DIR must be an absolute container path' >&2; exit 2 ;;
  esac
fi
# Refuse an older image that cannot enforce the binding. Validate before the
# actual launch, including paths supplied by an older host or mounted wrapper.
/opt/deerflow/bin/deerflow-acpd.real "$@" --check-paths >/dev/null
exec /opt/deerflow/bin/deerflow-acpd.real "$@" \
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
