"""Private, bounded embedding adapter. Never logs request text or vectors."""
from http.server import BaseHTTPRequestHandler, HTTPServer
import json
import math
import socket
from fastembed import TextEmbedding

IDENTITY = 'BAAI/bge-small-zh-v1.5@46fbe35'
model = TextEmbedding(model_name='BAAI/bge-small-zh-v1.5', threads=2,
                      specific_model_path='/opt/deerflow-memory/model', local_files_only=True)
# Startup health implies the pinned model can execute, not only that HTTP binds.
assert len(next(model.embed(['记忆检索健康检查']))) == 512

class Handler(BaseHTTPRequestHandler):
    def setup(self):
        super().setup()
        self.connection.settimeout(10)

    def log_message(self, *args):
        pass

    def reply(self, status, value):
        encoded = json.dumps(value, ensure_ascii=False, allow_nan=False).encode()
        self.send_response(status)
        self.send_header('Content-Type', 'application/json')
        self.send_header('Content-Length', str(len(encoded)))
        self.end_headers()
        self.wfile.write(encoded)

    def do_GET(self):
        self.reply(200 if self.path == '/health' else 404,
                   {'ok': self.path == '/health', 'model': IDENTITY})

    def do_POST(self):
        try:
            length = int(self.headers.get('Content-Length', '0'))
            if self.path != '/embeddings' or not 0 < length <= 64 * 1024:
                return self.reply(400, {'error': 'invalid_request'})
            data = json.loads(self.rfile.read(length))
            texts = data.get('input')
            if (data.get('model') != IDENTITY or not isinstance(texts, list) or
                    not 0 < len(texts) <= 32 or any(not isinstance(t, str) or not t or len(t.encode()) > 4096 for t in texts)):
                return self.reply(400, {'error': 'invalid_input'})
            vectors = [vector.tolist() for vector in model.embed(texts, batch_size=16)]
            assert all(len(v) == 512 and all(math.isfinite(x) for x in v) for v in vectors)
            self.reply(200, {'model': IDENTITY, 'vectors': vectors})
        except (BrokenPipeError, ConnectionResetError, socket.timeout):
            pass
        except Exception:
            self.reply(500, {'error': 'embedding_failed'})

HTTPServer(('0.0.0.0', 8080), Handler).serve_forever()
