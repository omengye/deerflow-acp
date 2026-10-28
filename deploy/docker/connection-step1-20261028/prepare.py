"""Verify the deployed baseline and create reproducible source release artifacts."""
import argparse
import gzip
import hashlib
import io
import json
from pathlib import Path
import subprocess
import tarfile
import tempfile


def digest(data):
    return hashlib.sha256(data).hexdigest()


def archive(path, files):
    with path.open('wb') as output, gzip.GzipFile(fileobj=output, mode='wb', mtime=0, filename='') as compressed:
        with tarfile.open(fileobj=compressed, mode='w') as tar:
            for name, data in sorted(files.items()):
                entry = tarfile.TarInfo(name)
                entry.size = len(data)
                entry.mode = 0o644
                tar.addfile(entry, io.BytesIO(data))


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--repository', type=Path, required=True)
    parser.add_argument('--baseline-copies', type=Path, required=True)
    parser.add_argument('--output', type=Path, required=True)
    args = parser.parse_args()
    task = Path(__file__).resolve().parent
    manifest = json.loads((task / 'base-source-manifest.json').read_text())
    baseline, current = {}, {}
    changed = []
    for name, expected in manifest['files'].items():
        if not (name.startswith('desktop-app/') or name.startswith('deploy/docker/Cargo.server')):
            continue
        source = args.repository / name
        before = args.baseline_copies / name
        original = (before if before.is_file() else source).read_bytes()
        normalized = original.replace(b'\r\n', b'\n')
        variants = (original, normalized, normalized.replace(b'\n', b'\r\n'))
        if not any(digest(value) == expected['sha256'] for value in variants):
            raise RuntimeError('deployed baseline mismatch: ' + name)
        baseline[name] = normalized
        current[name] = source.read_bytes().replace(b'\r\n', b'\n')
        if current[name] != normalized:
            changed.append(name)
    # Prove that the release contains exactly the committed task patch.
    with tempfile.TemporaryDirectory(prefix='deerflow-step1-patch-') as directory:
        root = Path(directory)
        for name, data in baseline.items():
            path = root / name
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_bytes(data)
        subprocess.run(['git', '-c', 'core.autocrlf=false', 'apply', str(task / 'step1-server.patch')], cwd=root, check=True)
        for name, data in current.items():
            if (root / name).read_bytes().replace(b'\r\n', b'\n') != data:
                raise RuntimeError('release differs from task patch: ' + name)
    args.output.mkdir(parents=True, exist_ok=True)
    archive(args.output / 'baseline-source.tar.gz', baseline)
    current['Dockerfile'] = (task / 'Dockerfile').read_bytes()
    archive(args.output / 'build-source.tar.gz', current)
    report = {
        'base_image_id': 'sha256:d89395c9629b359a3c8a8688f9a0f0e2145ff32a4c67f30a99b5226c045ab26f',
        'baseline_files_verified': len(baseline),
        'changed_files': changed,
        'patch_sha256': digest((task / 'step1-server.patch').read_bytes()),
        'baseline_archive_sha256': digest((args.output / 'baseline-source.tar.gz').read_bytes()),
        'build_archive_sha256': digest((args.output / 'build-source.tar.gz').read_bytes()),
        'source_newlines': 'LF (verified original baseline accepts LF or CRLF)',
    }
    (args.output / 'source-verification.json').write_text(json.dumps(report, indent=2) + '\n')
    print(json.dumps(report))


if __name__ == '__main__':
    main()
