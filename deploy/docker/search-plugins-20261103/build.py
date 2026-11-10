"""Start a bounded offline Linux build, without mounting production data."""
import hashlib
import json
from pathlib import Path
import subprocess
import tarfile

ROOT = Path('/home/gen/deerflow-deploy')
TASK = ROOT / 'search-plugins-20261103-204617'
BUILDER = 'deerflow-build-search-20261103-204617'
PREVIOUS = ROOT / 'storage-binding-20261103'


def digest(path):
    with path.open('rb') as stream:
        return hashlib.file_digest(stream, 'sha256').hexdigest()


def main():
    manifest = json.loads((TASK / 'source-manifest.json').read_text())
    for name, expected in manifest.get('deployment_scripts', {}).items():
        assert digest(TASK / name) == expected, 'deployment script checksum changed'
    assert manifest['go_toolchain'].startswith('go version go1.26.8 '), 'use the project-pinned Go compiler and GOROOT'
    spec = json.loads((TASK / 'deployment-spec.json').read_text())
    current = subprocess.check_output(['docker', 'inspect', '--format', '{{.Image}}',
                                      'deerflow-deploy-deerflow-1'], text=True).strip()
    assert current == spec['base_image_id'], 'production image changed'
    build = TASK / 'build'
    source, binary = build / 'source', build / 'bin'
    source.mkdir(parents=True, exist_ok=False)
    binary.mkdir()
    tests = build / 'tests'; tests.mkdir()
    for filename, destination, key in [('source.tar.gz', source, 'source_archive_sha256'),
                                      ('go-artifacts.tar.gz', build, 'go_archive_sha256')]:
        archive = TASK / filename
        assert digest(archive) == manifest[key], 'archive checksum mismatch'
        with tarfile.open(archive) as contents:
            contents.extractall(destination, filter='data')
    for name, value in manifest['files'].items():
        assert digest(source / name) == value, name
    for name in manifest['executable_files']:
        assert name in manifest['files']
        (source / name).chmod(0o755)
    for name, value in manifest['go_artifacts'].items():
        assert digest(binary / name) == value, name
    for name, value in manifest['go_tests'].items():
        assert digest(tests / name) == value, name
    previous = json.loads((PREVIOUS / 'source-manifest.json').read_text())
    for name in ('desktop-app/Cargo.lock', 'bridge/Cargo.lock'):
        assert manifest['files'][name] == previous['files'][name], 'offline dependency lock changed'
    old_source = PREVIOUS / 'build/source'
    assert (old_source / 'vendor').is_dir(), 'verified offline dependencies unavailable'
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
for package in websearch desktopconfig configschema tools launch runtimepaths; do
  testname=$package
  if [ "$package" = runtimepaths ]; then testname=paths; fi
  cd /build/source/go-harness/internal/$package
  /build/tests/$testname.test -test.v -test.timeout=10m
done
cd /build/source/go-harness/harness
/build/tests/harness.test -test.v -test.timeout=10m
cd /build/source/go-harness/cmd/deerflow-acpd-go
/build/tests/daemon.test -test.v -test.timeout=10m
cd /build/source/go-harness/internal/runtime
/build/tests/runtime.test -test.v -test.run='TestAutomaticSessionUpdates|TestAutomaticExecutionStarted' -test.count=3 -test.timeout=5m
cd /build/source/go-harness
/build/tests/root.test -test.v -test.run='^TestSDKAutomaticNotification(AdmissionFailureIsAtomicAndRetryable|RestartPolicyAndIndependentAck)$' -test.count=5 -test.timeout=3m
/build/tests/root.test -test.v -test.timeout=15m
sha256sum /build/bin/*
'''
    (build / 'run.sh').write_text(script)
    builder_image = subprocess.check_output(['docker', 'image', 'inspect', '--format', '{{.Id}}',
        'deerflow-builder:storage-20261103'], text=True).strip()
    assert builder_image == spec['builder_image_id'], 'builder image changed'
    command = ['docker', 'run', '-d', '--pull=never', '--name', BUILDER, '--network', 'none',
        '--user', '1000:1000', '--cpus', '3', '--memory', '5g', '--pids-limit', '512',
        '-e', 'CARGO_BUILD_JOBS=3', '-e', 'CARGO_HOME=/tmp/cargo', '-v', str(build) + ':/build',
        '-v', str(PREVIOUS / 'build/git') + ':/build/git:ro',
        '-v', str(old_source / 'vendor') + ':/build/source/vendor:ro',
        '-v', str(old_source / 'desktop-app/target') + ':/build/source/desktop-app/target',
        '-v', str(old_source / 'bridge/target') + ':/build/source/bridge/target',
        builder_image, '/bin/bash', '/build/run.sh']
    identifier = subprocess.check_output(command, text=True).strip()
    print(json.dumps({'build_started': True, 'container_id': identifier, 'production_modified': False}))


if __name__ == '__main__':
    main()
