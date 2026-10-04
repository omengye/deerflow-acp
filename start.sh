#!/usr/bin/env bash
# Start the Eino Go ACP daemon through the native Bridge.
set -euo pipefail
script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
exec "$script_dir/daemon.sh" start "$@"
