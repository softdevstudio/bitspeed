#!/usr/bin/env python3
"""Time one send with --parallel=1, 4 and 8 through a loopback proxy that adds
a fixed delay to every request, standing in for a long round trip.

Usage: demo-parallel-latency.py BINARY OUTPUT_DIR [DELAY_MS]
Writes results.json and log.txt. The numbers describe this setup only:
loopback, one machine, a simulated delay per request, not a real WAN.
"""
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from urllib.parse import urlsplit
import hashlib
import http.client
import json
import os
import platform
import queue
import subprocess
import sys
import tempfile
import threading
import time

binary = str(Path(sys.argv[1]).resolve())
out = Path(sys.argv[2])
delay = int(sys.argv[3]) / 1000 if len(sys.argv) > 3 else 0.1
out.mkdir(parents=True, exist_ok=True)
log = []


def note(line):
    print(line)
    log.append(line)


def run(*args, timeout=300):
    result = subprocess.run([binary, *args], capture_output=True, text=True, timeout=timeout)
    assert result.returncode == 0, (args, result.stderr)
    return result


version = run('--version').stdout.strip()
with tempfile.TemporaryDirectory() as directory:
    base = Path(directory)
    size, chunk = 32 << 20, 1 << 20
    data = os.urandom(size)
    digest = hashlib.sha256(data).hexdigest()
    source = base / 'source.bin'
    source.write_bytes(data)
    token = base / 'token'
    token.write_text('0123456789abcdef' * 4 + '\n')
    token.chmod(0o600)
    root = base / 'receiver'
    receiver = subprocess.Popen([binary, 'serve', '--root=' + str(root), '--token-file=' + str(token), '--listen=127.0.0.1:0', '--max-reserved-bytes=' + str(1 << 30)], stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
    lines = queue.Queue()
    threading.Thread(target=lambda: lines.put(receiver.stdout.readline()), daemon=True).start()
    url = lines.get(timeout=10).strip()

    class Delay(BaseHTTPRequestHandler):
        def log_message(self, *args):
            pass

        def forward(self):
            length = int(self.headers.get('Content-Length', '0'))
            body = self.rfile.read(length) if length else None
            time.sleep(delay)
            headers = {k: self.headers[k] for k in ('Authorization', 'Content-Type') if k in self.headers}
            headers['Content-Length'] = str(length)
            connection = http.client.HTTPConnection(urlsplit(url).netloc, timeout=60)
            try:
                connection.request(self.command, self.path, body, headers)
                response = connection.getresponse()
                payload = response.read()
                self.send_response(response.status)
                self.send_header('Content-Type', 'application/json')
                self.send_header('Content-Length', str(len(payload)))
                self.end_headers()
                self.wfile.write(payload)
            finally:
                connection.close()

        do_GET = do_POST = do_PUT = forward

    proxy = ThreadingHTTPServer(('127.0.0.1', 0), Delay)
    proxy.daemon_threads = True
    threading.Thread(target=proxy.serve_forever, daemon=True).start()
    results = []
    try:
        for parallel in (1, 4, 8):
            spool = base / ('spool-%d' % parallel)
            prepared = json.loads(run('send', '--prepare-only', '--source=' + str(source), '--spool=' + str(spool), '--chunk-size=' + str(chunk)).stdout)
            identity = prepared['manifest']['transfer_id']
            start = time.monotonic()
            sent = run('send', '--spool=' + str(spool), '--reuse=' + identity, '--url=http://127.0.0.1:%d' % proxy.server_port, '--token-file=' + str(token), '--parallel=%d' % parallel)
            seconds = time.monotonic() - start
            receipt = json.loads(sent.stdout)['status']['receipt']
            assert receipt['file_sha256'] == digest
            assert hashlib.sha256((root / 'files' / receipt['output_name']).read_bytes()).hexdigest() == digest
            results.append({'parallel': parallel, 'seconds': round(seconds, 2)})
            note('--parallel=%d: %.2f s, delivered SHA-256 matches' % (parallel, seconds))
    finally:
        proxy.shutdown()
        receiver.terminate()
        receiver.communicate(timeout=15)
    summary = {'version': version, 'platform': platform.platform(), 'file_bytes': size, 'chunk_bytes': chunk, 'chunks': size // chunk,
               'added_delay_ms_per_request': int(delay * 1000), 'network': 'loopback through a local proxy that waits before forwarding each request',
               'runs': results}
    (out / 'results.json').write_text(json.dumps(summary, indent=2) + '\n')
    (out / 'log.txt').write_text('\n'.join(log) + '\n')
