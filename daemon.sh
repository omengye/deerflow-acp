#!/usr/bin/env bash
# Manage the Eino Go ACP daemon; no dependency installation or HTTP service.
set -euo pipefail
script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
cd -- "$script_dir"

usage() {
    printf 'Usage: %s {start|stop|restart|status} [--config PATH] [--bridge PATH] [--daemon PATH] [--runtime-dir PATH]\n' "$0"
}
action="${1:-status}"
if [ "$#" -gt 0 ]; then shift; fi
case "$action" in
    start|stop|restart|status) ;;
    -h|--help) usage; exit 0 ;;
    *) usage >&2; exit 2 ;;
esac

config="${DEER_FLOW_CONFIG_PATH:-$script_dir/config.yaml}"
bridge="${DEER_FLOW_ACP_BRIDGE:-$script_dir/bridge/target/release/deerflow-acp}"
runtime_dir="${DEER_FLOW_ACP_RUNTIME_DIR:-$script_dir/user-data/runtime/acp-go}"
daemon=""
while [ "$#" -gt 0 ]; do
    case "$1" in
        --config|--bridge|--daemon|--runtime-dir)
            if [ "$#" -lt 2 ] || [ -z "$2" ]; then
                printf '%s requires a path\n' "$1" >&2
                exit 2
            fi
            case "$1" in
                --config) config="$2" ;;
                --bridge) bridge="$2" ;;
                --daemon) daemon="$2" ;;
                --runtime-dir) runtime_dir="$2" ;;
            esac
            shift 2
            ;;
        --python|--python=*)
            printf '%s\n' '--python has been removed; use --daemon PATH with the Eino Go executable.' >&2
            exit 2
            ;;
        *) printf 'Unknown option: %s\n' "$1" >&2; usage >&2; exit 2 ;;
    esac
done
# Resolve the selected Bridge as a path, never a name searched through PATH.
case "$bridge" in /*) ;; *) bridge="$script_dir/$bridge" ;; esac
if [ ! -x "$bridge" ] || [ ! -f "$bridge" ]; then
    printf 'Native ACP Bridge is missing or not executable: %s\n' "$bridge" >&2
    exit 1
fi
if [ ! -f "$config" ]; then
    printf 'DeerFlow config is missing: %s\n' "$config" >&2
    exit 1
fi
args=(--config "$config" --runtime-dir "$runtime_dir")
if [ -n "$daemon" ]; then args+=(--daemon "$daemon"); fi

stop_daemon() {
    local output code
    if output="$("$bridge" "${args[@]}" --stop-daemon 2>&1)"; then
        printf '%s\n' "$output"
    else
        code="$?"
        if [[ "$output" == *"DeerFlow ACP daemon is not running"* ]]; then
            printf '%s\n' 'DeerFlow ACP daemon is already stopped.'
        else
            printf '%s\n' "$output" >&2
            return "$code"
        fi
    fi
}
case "$action" in
    start) exec "$bridge" "${args[@]}" --start-daemon ;;
    status) exec "$bridge" "${args[@]}" --status ;;
    stop) stop_daemon ;;
    restart) stop_daemon; exec "$bridge" "${args[@]}" --start-daemon ;;
esac
