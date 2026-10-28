#!/usr/bin/env bash
set -euo pipefail

readonly image_root=/opt/deerflow
readonly portable_root="${DEER_FLOW_PORTABLE_ROOT:-/var/lib/deerflow}"
readonly runtime_dir="$portable_root/user-data/runtime/acp-go"
readonly config_path="$portable_root/user-data/config/config.yaml"

if [[ -z "${WAKU_DAEMON_TOKEN:-}" ]]; then
  echo "WAKU_DAEMON_TOKEN is required" >&2
  exit 2
fi

install -d -m 0700 \
  "$portable_root" \
  "$portable_root/resources" \
  "$portable_root/user-data" \
  "$runtime_dir"

for binary in deerflow-acp deerflow-acpd deerflow-config-go deerflow-control-go deerflow-skill-go; do
  install -m 0755 "$image_root/bin/$binary" "$portable_root/$binary"
done
install -m 0644 "$image_root/resources/default-config.yaml" \
  "$portable_root/resources/default-config.yaml"

"$portable_root/deerflow-config-go" \
  --config "$config_path" \
  --user-data "$portable_root/user-data" \
  --resources "$portable_root/resources" \
  init >/dev/null

shutdown() {
  trap - TERM INT
  "$portable_root/deerflow-acp" \
    --config "$config_path" \
    --daemon "$portable_root/deerflow-acpd" \
    --runtime-dir "$runtime_dir" \
    --stop-daemon >/dev/null 2>&1 || true
  if kill -0 "$waku_pid" 2>/dev/null; then
    kill -TERM "$waku_pid" 2>/dev/null || true
  fi
  wait "$waku_pid" 2>/dev/null || true
  exit 0
}

trap shutdown TERM INT

"$image_root/bin/waku-daemon" \
  --bind "0.0.0.0:${DEERFLOW_SERVER_PORT:-18787}" \
  --allow-non-loopback &
waku_pid=$!
set +e
wait "$waku_pid"
waku_status=$?
set -e

"$portable_root/deerflow-acp" \
  --config "$config_path" \
  --daemon "$portable_root/deerflow-acpd" \
  --runtime-dir "$runtime_dir" \
  --stop-daemon >/dev/null 2>&1 || true
exit "$waku_status"
