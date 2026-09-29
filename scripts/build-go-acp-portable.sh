#!/usr/bin/env bash
set -euo pipefail

if [[ "$(uname -s)" != Linux || "$(uname -m)" != x86_64 ]]; then
  echo 'This package script targets Linux x86_64.' >&2
  exit 2
fi

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
repo_root="$(cd -- "$script_dir/.." && pwd -P)"
dist_root="$(realpath -m -- "$repo_root/dist")"
configuration="${1:-Release}"
output_arg="${2:-dist/go-acp/linux-x64}"
archive="${3:-no-archive}"
if [[ "$configuration" != Debug && "$configuration" != Release ]]; then
  echo 'Configuration must be Debug or Release.' >&2
  exit 2
fi
if [[ "$archive" != archive && "$archive" != no-archive ]]; then
  echo 'Third argument must be archive or no-archive.' >&2
  exit 2
fi
if [[ "$output_arg" = /* ]]; then
  output_root="$(realpath -m -- "$output_arg")"
else
  output_root="$(realpath -m -- "$repo_root/$output_arg")"
fi
case "$output_root" in
  "$dist_root"/*) ;;
  *) echo "OutputDirectory must be below $dist_root" >&2; exit 2 ;;
esac
if [[ -e "$output_root" ]]; then
  echo "OutputDirectory already exists: $output_root. Choose a new path." >&2
  exit 2
fi

profile="${configuration,,}"
cargo_args=(build --locked --manifest-path "$repo_root/bridge/Cargo.toml")
if [[ "$configuration" == Release ]]; then cargo_args+=(--release); fi
cargo "${cargo_args[@]}"

mkdir -p -- "$output_root"
cp -- "$repo_root/bridge/target/$profile/deerflow-acp" "$output_root/deerflow-acp"
(
  cd -- "$repo_root/go-harness"
  go build -trimpath -p=2 -o "$output_root/deerflow-acpd" ./cmd/deerflow-acpd-go
  go build -trimpath -p=2 -o "$output_root/deerflow-acp-go" ./cmd/deerflow-acp-go
)
cp -- "$repo_root/LICENSE" "$output_root/LICENSE.txt"
cp -- "$repo_root/docs/go-acp-portable.md" "$output_root/README.md"
for binary in deerflow-acp deerflow-acpd deerflow-acp-go; do
  if [[ ! -s "$output_root/$binary" || ! -x "$output_root/$binary" ]]; then
    echo "Package binary is missing, empty, or not executable: $binary" >&2
    exit 1
  fi
done

if [[ "$archive" == archive ]]; then
  archive_path="$output_root.tar.gz"
  if [[ -e "$archive_path" ]]; then
    echo "Archive already exists: $archive_path" >&2
    exit 2
  fi
  tar -czf "$archive_path" -C "$(dirname -- "$output_root")" "$(basename -- "$output_root")"
  echo "Go ACP archive: $archive_path"
fi
echo "Go ACP package: $output_root"
