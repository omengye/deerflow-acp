"""Build a small credential-free deployment payload from reviewed source files."""
import argparse
import hashlib
import io
import json
from pathlib import Path
import tarfile


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--output', required=True, type=Path)
    args = parser.parse_args()
    root = Path(__file__).resolve().parents[3]
    repair = root / 'deploy/docker/repairs-20261024'
    files = {
        'runtime/deerflow-acpd-wrapper.sh': root / 'deploy/docker/skills-runtime/runtime/deerflow-acpd-wrapper.sh',
        'runtime/install-runtime-override.py': root / 'deploy/docker/skills-runtime/install-runtime-override.py',
        'folotoy/install-folotoy-clock.py': repair / 'install-folotoy-clock.py',
        'folotoy/Dockerfile.folotoy-clock': repair / 'Dockerfile.folotoy-clock',
        'folotoy/deerflow.py': root.parent / 'folo_server_src/src/folotoy-decompiled/src/sdk/llm_sdk/deerflow.py',
        'deploy-repair.py': repair / 'deploy-repair.py',
    }
    packages = {
        'skills/public/weather-cma': root / 'skills/public/weather-cma',
        'skills/custom/huaweicloud-linux': root / 'deploy/docker/skill-updates/huaweicloud-linux',
        'skills/custom/huaweicloud-sandbox': root / 'deploy/docker/skill-updates/huaweicloud-sandbox',
    }
    for prefix, directory in packages.items():
        for source in directory.rglob('*'):
            if source.is_file() and '__pycache__' not in source.parts:
                files[prefix + '/' + source.relative_to(directory).as_posix()] = source
    content = {}
    for name, source in sorted(files.items()):
        if source.is_symlink() or not source.is_file():
            raise RuntimeError('Missing/nonregular payload source: ' + str(source))
        data = source.read_bytes()
        if b'-----BEGIN OPENSSH PRIVATE KEY-----' in data or b'-----BEGIN RSA PRIVATE KEY-----' in data:
            raise RuntimeError('A credential was found in payload source: ' + name)
        content[name] = data
    manifest = {'credential_values_included': False, 'files': {
        name: hashlib.sha256(data).hexdigest() for name, data in content.items()}}
    content['manifest.json'] = (json.dumps(manifest, indent=2) + '\n').encode()
    args.output.parent.mkdir(parents=True, exist_ok=True)
    with tarfile.open(args.output, 'w:gz') as archive:
        for name, data in content.items():
            info = tarfile.TarInfo(name)
            info.size = len(data)
            info.mode = 0o644
            archive.addfile(info, io.BytesIO(data))
    print(json.dumps({'output': str(args.output), 'bytes': args.output.stat().st_size,
                      'files': len(content), 'sha256': hashlib.sha256(args.output.read_bytes()).hexdigest()}))


if __name__ == '__main__':
    main()
