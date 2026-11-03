"""Build and test isolated Linux artifacts; production keeps running."""
import hashlib
import json
from pathlib import Path
import subprocess
import tarfile

TASK = Path('/home/gen/deerflow-deploy/storage-binding-20261103')
CONTAINER = 'deerflow-build-storage-20261103'
BASE = 'sha256:90e46bb01bd79c2d79d330190e419b798417e630f21f606079d9a5defa210cf9'


def digest(path):
    with path.open('rb') as handle:
        return hashlib.file_digest(handle, 'sha256').hexdigest()


def main():
    manifest = json.loads((TASK / 'source-manifest.json').read_text())
    assert subprocess.check_output(['docker', 'inspect', '--format', '{{.Image}}',
        'deerflow-deploy-deerflow-1'], text=True).strip() == BASE
    build = TASK / 'build'
    source, binaries = build / 'source', build / 'bin'
    source.mkdir(parents=True, exist_ok=False)
    binaries.mkdir()
    for filename, target, key in [('source.tar.gz', source, 'source_archive_sha256'),
                                  ('go-artifacts.tar.gz', binaries, 'go_archive_sha256')]:
        path = TASK / filename
        assert digest(path) == manifest[key]
        with tarfile.open(path) as archive:
            archive.extractall(target, filter='data')
    for name, value in manifest['files'].items():
        assert digest(source / name) == value, name
    # Windows archive producers can lose POSIX mode bits. Restore the executable
    # paths recorded from git ls-tree of the pinned source commit.
    for name in manifest['executable_files']:
        assert name in manifest['files']
        (source / name).chmod(0o755)
    for name, value in manifest['go_artifacts'].items():
        assert digest(binaries / name) == value, name
    assert list((source / 'desktop-app/db/migrations').glob('*.sql')), 'missing embedded migrations'
    assert (source / 'desktop-app/locales').is_dir(), 'missing locale catalog'
    assert (source / 'docs/fixtures/skill-behavior-v2.json').is_file(), 'missing Go regression fixtures'
    subprocess.run(['docker', 'cp', 'deerflow-deploy-deerflow-1:/opt/deerflow-git',
                    str(build / 'git')], check=True, capture_output=True)
    script = '''#!/bin/bash
set -euo pipefail
export PATH=/build/git/bin:$PATH
cd /build/source/desktop-app
rustc --version
cargo test --locked --offline -p waku-protocol --lib
cargo test --locked --offline -p waku-core --lib deerflow_config::
cargo test --locked --offline -p waku-core --lib driver::acp::
cargo test --locked --offline -p waku-core --lib daemon::tests::deerflow_ready
cargo build --locked --offline --release -p waku-daemon --bin waku-daemon
cp target/release/waku-daemon /build/bin/waku-daemon
cd /build/source/bridge
cargo test --locked --offline
cargo build --locked --offline --release
cp target/release/deerflow-acp /build/bin/deerflow-acp
export DEERFLOW_TEST_BRIDGE=/build/bin/deerflow-acp
export DEERFLOW_TEST_GO_DAEMON=/build/bin/deerflow-acpd.real
export DEERFLOW_TEST_GO_STDIO=/build/bin/deerflow-acp-go
cd /build/source/go-harness/internal/runtimepaths
/build/bin/paths.test -test.v -test.timeout=2m
cd ../launch
/build/bin/launch.test -test.v -test.timeout=3m
cd ../desktopconfig
/build/bin/desktopconfig.test -test.v -test.timeout=5m
cd ../../cmd/deerflow-acpd-go
/build/bin/daemon.test -test.v -test.timeout=5m
sha256sum /build/bin/waku-daemon /build/bin/deerflow-acp /build/bin/deerflow-acpd.real /build/bin/deerflow-acp-go
'''
    (build / 'run.sh').write_text(script)
    builder = TASK / 'builder'
    builder.mkdir()
    (builder / 'Dockerfile').write_text(
        'FROM registry-1.docker.io/library/rust:1.96-bookworm@sha256:a339861ae23e9abb272cea45dfafde21760d2ce6577a70f8a926153677902663\n'
        'RUN groupadd --gid 1000 builduser && useradd --uid 1000 --gid 1000 --create-home --shell /bin/bash builduser\n')
    with (TASK / 'builder.log').open('w') as log:
        subprocess.run(['docker', 'build', '--network', 'none', '--pull=false', '-t',
                        'deerflow-builder:storage-20261103', str(builder)],
                       stdout=log, stderr=subprocess.STDOUT, check=True)
    identifier = subprocess.check_output(['docker', 'run', '-d', '--name', CONTAINER,
        '--network', 'none', '--user', '1000:1000', '--cpus', '3', '--memory', '5g',
        '--pids-limit', '512', '-e', 'CARGO_BUILD_JOBS=3', '-e', 'CARGO_HOME=/tmp/cargo',
        '-v', str(build) + ':/build', 'deerflow-builder:storage-20261103',
        '/bin/bash', '/build/run.sh'], text=True).strip()
    print(json.dumps({'build_started': True, 'container_id': identifier, 'production_modified': False}))


if __name__ == '__main__':
    main()
