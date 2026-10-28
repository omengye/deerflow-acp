"""Run frozen Linux tests without production volumes, network, or credentials."""
import argparse
import hashlib
import json
from pathlib import Path
import re
import subprocess
import time
import uuid


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--root', type=Path, default=Path('/home/gen/deerflow-deploy/skill-learning-recovery-20261028'))
    parser.add_argument('--image', default='deerflow-server:skill-learning-recovery-20261028')
    args = parser.parse_args()
    root = args.root.resolve(strict=True)
    report_path = root / 'linux-tests.json'
    report_path.write_text(json.dumps({'passed': False, 'state': 'started'}) + '\n')
    plan = json.loads((root / 'test-plan.json').read_text())
    manifest = json.loads((root / 'build-manifest.json').read_text())
    assert manifest['completed'], 'build_incomplete'
    binaries = {Path(item['path']).name: item['sha256'] for item in manifest['artifacts'] if not item['test']}
    for artifact in manifest['artifacts']:
        path = (root / artifact['path']).resolve(strict=True)
        assert path.is_relative_to(root), 'artifact_outside_release'
        assert hashlib.sha256(path.read_bytes()).hexdigest() == artifact['sha256'], 'artifact_hash_mismatch'
        path.chmod(0o755)
    logs = root / 'linux-test-logs'
    logs.mkdir(exist_ok=True)
    results = []
    image_id = subprocess.check_output(['docker', 'image', 'inspect', '--format', '{{.Id}}', args.image], text=True).strip()
    for index, case in enumerate(plan['tests']):
        name = f"{index + 1:02}-{case['binary']}"
        container = 'deerflow-learning-test-' + uuid.uuid4().hex[:12]
        working_directory = case.get('working_directory', '')
        assert not working_directory or working_directory == 'test-fixtures/go-harness/internal/desktopconfig'
        command = ['docker', 'run', '--rm', '--name', container, '--network', 'none', '--user', '1000:1000',
                   '--cap-drop', 'ALL', '--security-opt', 'no-new-privileges:true',
                   '--memory', '2g', '--cpus', '2', '--pids-limit', '256',
                   '--read-only', '--tmpfs', '/tmp:rw,nosuid,size=512m',
                   '-v', f'{root / "tests"}:/release/tests:ro',
                   '-v', f'{root / "test-fixtures"}:/release/test-fixtures:ro',
                   '-w', '/release/' + working_directory if working_directory else '/tmp',
                   '--entrypoint', '/release/tests/' + case['binary'], image_id,
                   *plan['common_args'], '-test.run=' + case['run'], '-test.timeout=' + case['timeout']]
        started = time.monotonic()
        try:
            process = subprocess.run(command, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True,
                                     timeout=int(case['timeout'].removesuffix('s')) + 60)
            output, exit_code = process.stdout, process.returncode
        except subprocess.TimeoutExpired as error:
            output = error.stdout or ''
            if isinstance(output, bytes):
                output = output.decode('utf-8', errors='replace')
            output += '\nacceptance_container_timeout\n'
            exit_code = 124
        finally:
            subprocess.run(['docker', 'rm', '-f', container], stdout=subprocess.DEVNULL,
                           stderr=subprocess.DEVNULL, timeout=30)
        (logs / (name + '.log')).write_text(output)
        passed = len(re.findall(r'^\s*--- PASS:', output, re.MULTILINE))
        failed = len(re.findall(r'^\s*--- FAIL:', output, re.MULTILINE))
        result = {'name': name, 'exit_code': exit_code, 'passed_cases': passed,
                  'failed_cases': failed, 'seconds': round(time.monotonic() - started, 2),
                  'filter': case['run'], 'log': str(logs / (name + '.log'))}
        result['passed'] = exit_code == 0 and passed > 0 and failed == 0
        results.append(result)
        report = {'passed': len(results) == len(plan['tests']) and all(x['passed'] for x in results),
                  'image_id': image_id, 'build_id': manifest['build_id'], 'binaries': binaries, 'results': results}
        report_path.write_text(json.dumps(report, indent=2) + '\n')
        print(json.dumps(result), flush=True)
        if not result['passed']:
            raise SystemExit(1)


if __name__ == '__main__':
    main()
