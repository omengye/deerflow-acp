#!/usr/bin/env bash
# Stop the selected Eino Go ACP daemon through authenticated IPC.
set -euo pipefail
script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
exec "$script_dir/daemon.sh" stop "$@"
