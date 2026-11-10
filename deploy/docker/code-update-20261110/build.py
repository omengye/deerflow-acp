"""Build the committed Rust server and run Linux regressions in an offline container."""
import hashlib
import json
from pathlib import Path
import re
import shlex
import shutil
import subprocess
import tarfile

ROOT = Path('/home/gen/deerflow-deploy')
TASK = Path(__file__).resolve().parent
CACHE = ROOT / 'storage-binding-20261103/build/source'

def digest(path):
    with path.open('rb') as stream:
        return hashlib.file_digest(stream, 'sha256').hexdigest()

def output(args):
    return subprocess.check_output(args, text=True).strip()

def main():
    assert TASK.parent == ROOT
    manifest = json.loads((TASK / 'source-manifest.json').read_text())
    spec = json.loads((TASK / 'deployment-spec.json').read_text())
    commit = manifest['commit']
    assert re.fullmatch('[a-f0-9]{40}', commit)
    assert output(['docker', 'inspect', '--format', '{{.Image}}', 'deerflow-deploy-deerflow-1']) == spec['base_image_id']
    assert output(['docker', 'image', 'inspect', '--format', '{{.Id}}', spec['builder_image_id']]) == spec['builder_image_id']
    build = TASK / 'build'
    source = build / 'source'
    source.mkdir(parents=True, exist_ok=False)
    for name, destination, key in [('source.tar.gz', source, 'source_archive_sha256'),
                                   ('go-artifacts.tar.gz', build, 'go_archive_sha256')]:
        assert digest(TASK / name) == manifest[key], 'archive checksum mismatch'
        with tarfile.open(TASK / name) as contents:
            contents.extractall(destination, filter='data')
    for directory, key in [('bin', 'binaries'), ('tests', 'go_tests')]:
        for name, expected in manifest[key].items():
            path = build / directory / name
            assert digest(path) == expected, 'Go artifact checksum mismatch'
            path.chmod(0o755)
    assert (CACHE / 'vendor').is_dir(), 'offline Rust dependencies missing'
    assert (ROOT / 'storage-binding-20261103/build/git/bin/git').is_file(), 'offline Git helper missing'
    assert digest(source / 'deploy/docker/Cargo.server.lock') == digest(CACHE / 'desktop-app/Cargo.lock')
    assert digest(source / 'bridge/Cargo.lock') == digest(CACHE / 'bridge/Cargo.lock')
    shutil.copy2(source / 'deploy/docker/Cargo.server.toml', source / 'desktop-app/Cargo.toml')
    shutil.copy2(source / 'deploy/docker/Cargo.server.lock', source / 'desktop-app/Cargo.lock')
    (source / '.cargo').mkdir(exist_ok=True)
    (source / '.cargo/config.toml').write_text('[source.crates-io]\nreplace-with = "vendored-sources"\n[source.vendored-sources]\ndirectory = "vendor"\n')
    (build / 'test-plan.json').write_text(json.dumps(manifest['linux_test_plan']))
    test_commands = []
    for name, plan in manifest['linux_test_plan'].items():
        binary = '/build/tests/' + name + '.test'
        selected = plan['run'] or '^(Test|Example|Fuzz)'
        package = '/build/source/go-harness/' + plan['package'].removeprefix('./')
        test_commands += [
            'echo ' + shlex.quote('LINUX_PACKAGE_BEGIN ' + name),
            'cd ' + shlex.quote(package),
            shlex.quote(binary) + ' ' + shlex.quote('-test.list=' + selected) + ' > ' + shlex.quote('/build/tests/' + name + '.inventory'),
            shlex.quote(binary) + ' -test.v -test.parallel=2 -test.timeout=12m ' + shlex.quote('-test.run=' + selected),
            'echo ' + shlex.quote('LINUX_PACKAGE_END ' + name),
        ]
    (build / 'run.sh').write_text('''#!/bin/bash
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
export GOMAXPROCS=2
RUN_GO_TESTS
sha256sum /build/bin/*
echo COMMITTED_LINUX_VALIDATION_COMPLETE
'''.replace('RUN_GO_TESTS', '\n'.join(test_commands)))
    builder = 'deerflow-build-' + commit[:12]
    identifier = output(['docker', 'run', '-d', '--pull=never', '--name', builder,
        '--network', 'none', '--user', '1000:1000', '--cpus', '3', '--memory', '5g', '--pids-limit', '512',
        '-e', 'CARGO_BUILD_JOBS=3', '-e', 'CARGO_HOME=/tmp/cargo',
        '-v', str(build) + ':/build',
        '-v', str(ROOT / 'storage-binding-20261103/build/git') + ':/build/git:ro',
        '-v', str(CACHE / 'vendor') + ':/build/source/vendor:ro',
        '-v', str(CACHE / 'desktop-app/target') + ':/build/source/desktop-app/target',
        '-v', str(CACHE / 'bridge/target') + ':/build/source/bridge/target',
        spec['builder_image_id'], '/bin/bash', '/build/run.sh'])
    print(json.dumps({'build_started': True, 'builder': builder, 'container_id': identifier,
                      'production_modified': False, 'commit': commit}))

if __name__ == '__main__':
    main()
