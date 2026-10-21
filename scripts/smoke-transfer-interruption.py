#!/usr/bin/env python3
"""Kill actual sender/receiver executables at controlled HTTP boundaries."""
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
import hashlib
import http.client
import json
import os
import queue
import subprocess
import sys
import tempfile
import threading
from urllib.parse import urlsplit

binary = str(Path(sys.argv[1]).resolve())


def run(*args):
    return json.loads(subprocess.run([binary, *args], capture_output=True, text=True, check=True, timeout=30).stdout)


def stop(process):
    if process.poll() is None:
        process.kill()
    _, diagnostic = process.communicate(timeout=10)
    assert '0123456789abcdef' not in diagnostic


def receiver(root, token):
    process = subprocess.Popen([binary, 'serve', '--root=' + str(root), '--token-file=' + str(token), '--listen=127.0.0.1:0'], stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
    lines = queue.Queue()
    threading.Thread(target=lambda: lines.put(process.stdout.readline()), daemon=True).start()
    try:
        url = lines.get(timeout=10).strip()
        assert url.startswith('http://127.0.0.1:'), 'receiver did not start'
        return process, url
    except BaseException:
        stop(process)
        raise


for point in ('partial chunk', 'chunk response', 'final response'):
    with tempfile.TemporaryDirectory() as directory:
        base = Path(directory)
        source = base / 'source'
        data = bytes(range(256)) * 8192 + b'last partial bytes'
        source.write_bytes(data)
        digest = hashlib.sha256(data).hexdigest()
        spool, root = base / 'spool', base / 'receiver'
        token = base / 'token'
        token.write_text('0123456789abcdef' * 4 + '\n'); token.chmod(0o600)
        prepared = run('send', '--prepare-only', '--source=' + str(source), '--spool=' + str(spool), '--chunk-size=65536')
        identity = prepared['manifest']['transfer_id']
        source.unlink()
        server, url = receiver(root, token)
        reached, release = threading.Event(), threading.Event()
        consumed = threading.Event()

        class Proxy(BaseHTTPRequestHandler):
            def log_message(self, *args):
                pass

            def do_POST(self):
                self.forward()

            def do_PUT(self):
                self.forward()

            def do_GET(self):
                self.forward()

            def forward(self):
                connection = http.client.HTTPConnection(urlsplit(url).netloc, timeout=15)
                length = int(self.headers.get('Content-Length', '0'))
                headers = {key: self.headers[key] for key in ('Authorization', 'Content-Type') if key in self.headers}
                headers['Content-Length'] = str(length)
                chunk = self.command == 'PUT'
                final = self.path.endswith('/finalize')
                held = not consumed.is_set() and ((chunk and point != 'final response') or (final and point == 'final response'))
                if held:
                    consumed.set()
                try:
                    if held and point == 'partial chunk':
                        connection.putrequest(self.command, self.path)
                        for key, value in headers.items():
                            connection.putheader(key, value)
                        connection.endheaders()
                        connection.send(self.rfile.read(length // 2))
                        reached.set(); release.wait(20)
                        return
                    body = self.rfile.read(length) if length else None
                    connection.request(self.command, self.path, body, headers)
                    response = connection.getresponse()
                    saved = response.read(2 * 1024 * 1024 + 1)
                    assert len(saved) <= 2 * 1024 * 1024
                    if held:
                        assert response.status == 200
                        reached.set(); release.wait(20)
                        return
                    self.send_response(response.status)
                    self.send_header('Content-Type', 'application/json')
                    self.send_header('Content-Length', str(len(saved)))
                    self.end_headers(); self.wfile.write(saved)
                except (OSError, http.client.HTTPException):
                    pass
                finally:
                    connection.close()

        proxy = ThreadingHTTPServer(('127.0.0.1', 0), Proxy)
        threading.Thread(target=proxy.serve_forever, daemon=True).start()
        sender = subprocess.Popen([binary, 'send', '--spool=' + str(spool), '--reuse=' + identity, '--url=http://127.0.0.1:' + str(proxy.server_port), '--token-file=' + str(token)], stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
        try:
            assert reached.wait(20), 'sender did not reach interruption boundary'
            stop(sender); stop(server)
        finally:
            release.set()
            stop(sender); stop(server)
            proxy.shutdown(); proxy.server_close()
        chunk_zero = root / 'sessions' / identity / 'chunks' / '0'
        previous = chunk_zero.stat() if chunk_zero.exists() else None
        restarted, url = receiver(root, token)
        try:
            observed = run('status', '--url=' + url, '--token-file=' + str(token), '--id=' + identity)
            if point == 'partial chunk':
                assert observed['verified_chunks'] == 0
            elif point == 'chunk response':
                assert observed['verified_chunks'] == 1
            else:
                assert observed['state'] == 'completed'
            delivered = run('send', '--spool=' + str(spool), '--reuse=' + identity, '--url=' + url, '--token-file=' + str(token))
            assert delivered['transferred'] and delivered['status']['state'] == 'completed'
            receipt = delivered['status']['receipt']
            output = root / 'files' / receipt['output_name']
            assert hashlib.sha256(output.read_bytes()).hexdigest() == digest == receipt['file_sha256']
            assert len(list((root / 'files').iterdir())) == 1
            assert json.loads((spool / identity / 'receipt.json').read_text()) == receipt
            if previous:
                current = chunk_zero.stat()
                assert (current.st_ino, current.st_mtime_ns) == (previous.st_ino, previous.st_mtime_ns), 'resend replaced a verified chunk'
            repeated = run('send', '--spool=' + str(spool), '--reuse=' + identity, '--url=' + url, '--token-file=' + str(token))
            assert repeated['status']['receipt'] == receipt
        finally:
            stop(restarted)
        print('PASS: executable kill/restart/resume at ' + point)
