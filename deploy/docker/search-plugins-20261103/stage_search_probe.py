"""Synthetic HTTP providers and Waku search RPCs in an isolated network namespace."""
import json
import os
import sys
import threading
import time
import uuid
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import urlsplit, parse_qs

PORT = 39743
counts = {name: 0 for name in ('brave', 'deepseek', 'searxng')}
errors = []
lock = threading.Lock()

class Handler(BaseHTTPRequestHandler):
    def log_message(self, *_):
        pass

    def send_json(self, value):
        body = json.dumps(value).encode()
        self.send_response(200)
        self.send_header('Content-Type', 'application/json')
        self.send_header('Content-Length', str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def search(self, name):
        expected = {'brave': ('X-Subscription-Token', 'stage-brave'),
                    'deepseek': ('Authorization', 'Bearer stage-deepseek'),
                    'searxng': ('Authorization', '')}
        header, value = expected[name]
        if self.headers.get(header, '') != value:
            with lock:
                errors.append('unexpected provider credential: ' + name)
        if name == 'searxng' and parse_qs(urlsplit(self.path).query).get('format') != ['json']:
            with lock:
                errors.append('SearXNG JSON format missing')
        with lock:
            counts[name] += 1
        result = {'title': name + ' fixture', 'url': 'https://example.test/' + name,
                  'content': 'fixture result', 'summary': 'fixture result',
                  'description': 'fixture result'}
        self.send_json({'web': {'results': [result]}} if name == 'brave' else {'results': [result]})

    def do_GET(self):
        name = {'/res/v1/web/search': 'brave', '/search': 'searxng'}.get(urlsplit(self.path).path)
        if name:
            self.search(name)
        else:
            self.send_error(404)

    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers.get('Content-Length', '0'))))
        path = urlsplit(self.path).path
        if path == '/v1/tools/search':
            self.search('deepseek')
            return
        if path != '/v1/chat/completions':
            self.send_error(404)
            return
        messages = body.get('messages', [])
        delta, finish = {'role': 'assistant', 'content': 'fixture done'}, 'stop'
        if not messages or messages[-1].get('role') != 'tool':
            delta = {'role': 'assistant', 'tool_calls': [{'index': 0, 'id': 'stage-' + uuid.uuid4().hex,
                'type': 'function', 'function': {'name': 'web_search',
                'arguments': json.dumps({'query': 'isolated provider routing'})}}]}
            finish = 'tool_calls'
        self.send_response(200)
        self.send_header('Content-Type', 'text/event-stream')
        self.end_headers()
        chunk = {'id': 'fixture', 'object': 'chat.completion.chunk',
                 'choices': [{'index': 0, 'delta': delta, 'finish_reason': finish}]}
        self.wfile.write(('data: ' + json.dumps(chunk) + '\n\ndata: [DONE]\n\n').encode())
        self.wfile.flush()

class RPC:
    def __init__(self):
        try:
            import websocket
        except ModuleNotFoundError:
            from websockets.sync.client import connect
            self.ws = connect('ws://127.0.0.1:18787/v1', open_timeout=30,
                              close_timeout=5, compression=None, max_size=16 * 1024 * 1024)
            self.receive = lambda: self.ws.recv(timeout=30)
        else:
            self.ws = websocket.create_connection('ws://127.0.0.1:18787/v1', timeout=30,
                                                  suppress_origin=True, http_no_proxy=['*'])
            self.receive = self.ws.recv
        self.ws.send(json.dumps({'type': 'hello', 'protocolVersion': 12,
            'token': os.environ['WAKU_DAEMON_TOKEN'], 'clientId': str(uuid.uuid4()), 'resumeFrom': []}))
        hello = json.loads(self.receive())
        if hello.get('type') != 'hello' or hello.get('protocolVersion') != 12:
            raise RuntimeError('protocol handshake failed')

    def call(self, operation, fields=None):
        identifier = str(uuid.uuid4())
        self.ws.send(json.dumps({'type': 'request', 'requestId': identifier,
            'sessionId': str(uuid.UUID(int=0)), 'runtimeId': str(uuid.UUID(int=0)),
            'command': {'type': 'deerFlow', 'operation': operation, 'input': fields or {}}}))
        deadline = time.monotonic() + 40
        while time.monotonic() < deadline:
            reply = json.loads(self.receive())
            if reply.get('type') == 'response' and reply.get('requestId') == identifier:
                if reply['outcome']['status'] != 'ok':
                    raise RuntimeError('search RPC failed: ' + operation)
                return reply['outcome']['payload']['data']
        raise TimeoutError('search RPC timed out')


def main():
    server = ThreadingHTTPServer(('127.0.0.1', PORT), Handler)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    rpc = RPC()
    print(json.dumps({'ready': True}), flush=True)
    try:
        for line in sys.stdin:
            request = json.loads(line)
            action = request['action']
            if action == 'configure':
                state = rpc.call('search-status')
                for name in ('brave', 'deepseek', 'searxng'):
                    fields = {'provider': name, 'expected_revision': state['revision'],
                              'base_url': 'http://127.0.0.1:' + str(PORT)}
                    if name != 'searxng':
                        fields['api_key'] = 'stage-' + name
                    state = rpc.call('search-configure', fields)
                result = {'configured': [p['name'] for p in state['providers'] if p['configured']]}
            elif action == 'select':
                state = rpc.call('search-select', {'provider': request['provider']})
                result = {'default_provider': state['default_provider']}
            elif action == 'inspect':
                with lock:
                    result = {'counts': dict(counts), 'errors': list(errors)}
            elif action == 'close':
                break
            else:
                raise RuntimeError('unknown fixture action')
            print(json.dumps(result), flush=True)
    finally:
        rpc.ws.close()
        server.shutdown()

if __name__ == '__main__':
    main()
