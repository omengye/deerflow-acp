"""Build-time download of a pinned, public ONNX model. No application data."""
import hashlib
import json
from pathlib import Path
import os
import urllib.request

REVISION = '46fbe35fd4374a00fee7de77dfddaeb6dd6a2c59'
MODEL_SHA256 = '1294ea4b6331115a353d81f96b85e8c8d7fdcc284453d5b2fab5b016230aad38'
ROOT = Path(os.environ.get('MEMORY_MODEL_DIRECTORY', '/opt/deerflow-memory/model'))
EXPECTED = {
 'config.json':'9088751d39abbf86ec3d19ffca92ad62ad19075f7e59712e6c71217fa125d1d3',
 'model_optimized.onnx':MODEL_SHA256,
 'special_tokens_map.json':'b6d346be366a7d1d48332dbc9fdf3bf8960b5d879522b7799ddba59e76237ee3',
 'tokenizer_config.json':'e6f3b96db926a37d4039995fbf5ad17de158dfb8f6343d607e4dbaad18d75f5a',
 'tokenizer.json':'48cea5d44424912a6fd1ea647bf4fe50b55ab8b1e5879c3275f80e339e8fae26',
 'vocab.txt':'45bbac6b341c319adc98a532532882e91a9cefc0329aa57bac9ae761c27b291c',
}
ROOT.mkdir(parents=True, exist_ok=True)
manifest = {}
for name in ('config.json', 'model_optimized.onnx', 'special_tokens_map.json',
             'tokenizer.json', 'tokenizer_config.json', 'vocab.txt'):
    url = 'https://huggingface.co/Qdrant/bge-small-zh-v1.5/resolve/' + REVISION + '/' + name
    if not (ROOT / name).exists():
        with urllib.request.urlopen(url, timeout=120) as response, (ROOT / name).open('wb') as output:
            while block := response.read(1024 * 1024):
                output.write(block)
    manifest[name] = hashlib.sha256((ROOT / name).read_bytes()).hexdigest()
    assert manifest[name] == EXPECTED[name], 'pinned file checksum mismatch: ' + name
assert manifest['model_optimized.onnx'] == MODEL_SHA256, 'model checksum mismatch'
(ROOT / 'manifest.json').write_text(json.dumps({'revision': REVISION, 'sha256': manifest}, indent=2))
print(json.dumps({'model_downloaded': True, 'revision': REVISION}))
