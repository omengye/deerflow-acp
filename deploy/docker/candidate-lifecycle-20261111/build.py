"""Verify the uploaded artifacts and run Linux regressions in the deployed base."""
import hashlib
import json
from pathlib import Path
import shlex
import subprocess
import tarfile

ROOT = Path('/home/gen/deerflow-deploy')
TASK = Path(__file__).resolve().parent


def digest(path):
    with path.open('rb') as stream:
        return hashlib.file_digest(stream, 'sha256').hexdigest()


def output(args):
    return subprocess.check_output(args, text=True).strip()


assert TASK.parent == ROOT and TASK.resolve() == TASK
manifest = json.loads((TASK / 'source-manifest.json').read_text())
spec = json.loads((TASK / 'deployment-spec.json').read_text())
assert output(['docker', 'inspect', '--format', '{{.Image}}', 'deerflow-deploy-deerflow-1']) == spec['base_image_id']
build = TASK / 'build'
source = build / 'source'
source.mkdir(parents=True, exist_ok=False)
for name, destination, key in [('source.tar.gz', source, 'source_archive_sha256'), ('go-artifacts.tar.gz', build, 'go_archive_sha256')]:
    assert digest(TASK / name) == manifest[key], 'uploaded archive differs'
    with tarfile.open(TASK / name) as contents:
        contents.extractall(destination, filter='data')
for directory, key in [('bin', 'binaries'), ('tests', 'go_tests')]:
    for name, expected in manifest[key].items():
        path = build / directory / name
        assert digest(path) == expected, 'uploaded executable differs'
        path.chmod(0o755)
commands = []
for name, plan in manifest['linux_test_plan'].items():
    binary = '/build/tests/' + name + '.test'
    commands += ['echo ' + shlex.quote('LINUX_PACKAGE_BEGIN ' + name),
                 'cd ' + shlex.quote('/build/source/go-harness/' + plan['package'].removeprefix('./')),
                 shlex.quote(binary) + ' ' + shlex.quote('-test.list=' + plan['run']) + ' > /build/tests/' + name + '.inventory',
                 shlex.quote(binary) + ' -test.v -test.parallel=2 -test.timeout=10m ' + shlex.quote('-test.run=' + plan['run']),
                 'echo ' + shlex.quote('LINUX_PACKAGE_END ' + name)]
(build / 'run.sh').write_text('''#!/bin/bash
set -euo pipefail
export GOMAXPROCS=2
export DEERFLOW_TEST_BRIDGE=/opt/deerflow/bin/deerflow-acp
export DEERFLOW_TEST_GO_DAEMON=/build/bin/deerflow-acpd.real
export DEERFLOW_TEST_GO_STDIO=/build/bin/deerflow-acp-go
unset DEER_FLOW_PORTABLE_ROOT WAKU_HEALTH_ADDRESS
RUN_TESTS
sha256sum /build/bin/*
echo COMMITTED_LINUX_VALIDATION_COMPLETE
'''.replace('RUN_TESTS', '\n'.join(commands)))
builder = 'deerflow-build-' + manifest['commit'][:12]
identifier = output(['docker', 'run', '-d', '--pull=never', '--name', builder,
                     '--network', 'none', '--user', '1000:1000', '--cpus', '2', '--memory', '3g', '--pids-limit', '512',
                     '--cap-drop', 'ALL', '--security-opt', 'no-new-privileges:true',
                     '-v', str(build) + ':/build', '--entrypoint', '/bin/bash', spec['builder_image_id'], '/build/run.sh'])
report = {'passed': True, 'builder': builder, 'container_id': identifier, 'commit': manifest['commit'], 'production_modified': False}
(TASK / 'builder-report.json').write_text(json.dumps(report, indent=2) + '\n')
print(json.dumps(report))
