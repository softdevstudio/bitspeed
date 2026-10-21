#!/usr/bin/env python3
"""Record a real kill-and-resume delivery with a released Transfer binary.

Usage: demo-transfer-resume.py BINARY OUTPUT_DIR

Prepares a 64 MiB file in 1 MiB chunks, sends it through a local proxy that
slows each chunk upload so the kill lands part way, kills the sender and the
receiver, restarts the receiver, asks it what it can prove, resumes and checks
the delivered file's SHA-256. Every command's output is saved to OUTPUT_DIR.
"""
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
import hashlib
import http.client
import json
import platform
import queue
import random
import subprocess
import sys
import tempfile
import threading
import time
from urllib.parse import urlsplit

binary, out = str(Path(sys.argv[1]).resolve()), Path(sys.argv[2])
out.mkdir(parents=True, exist_ok=True)
log = []


def note(text):
    log.append(text)
    print(text, flush=True)


def run(*args, timeout=120):
    result = subprocess.run([binary, *args], capture_output=True, text=True, timeout=timeout)
    if result.returncode != 0:
        raise SystemExit(f'{args[0]} failed: {result.stderr.strip()}')
    return result


def serve(root, token):
    process = subprocess.Popen([binary, 'serve', '--root=' + str(root), '--token-file=' + str(token), '--listen=127.0.0.1:0'], stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
    lines = queue.Queue()
    threading.Thread(target=lambda: lines.put(process.stdout.readline()), daemon=True).start()
    url = lines.get(timeout=20).strip()
    assert url.startswith('http://127.0.0.1:'), (url, process.wait(timeout=10), process.stderr.read())
    return process, url


def kill(process):
    if process.poll() is None:
        process.kill()
    process.wait(timeout=20)


version = run('--version').stdout.strip()
(out / 'environment.txt').write_text(f'{version}\n{platform.platform()}\n{platform.machine()}\nUTC {time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())}\n')
note(version + ' on ' + platform.platform())

with tempfile.TemporaryDirectory() as directory:
    base = Path(directory)
    source = base / 'video.mov'
    data = random.Random(2026).randbytes(64 << 20)
    source.write_bytes(data)
    digest = hashlib.sha256(data).hexdigest()
    token = base / 'token'
    token.write_text(random.Random(7).randbytes(32).hex() + '\n')
    token.chmod(0o600)
    spool, root = base / 'sender', base / 'receiver'

    prepared = run('send', '--prepare-only', '--source=' + str(source), '--spool=' + str(spool), '--chunk-size=1048576')
    manifest = json.loads(prepared.stdout)['manifest']
    identity = manifest['transfer_id']
    source.unlink()
    note(f'prepared 64 MiB as {len(manifest["chunk_sha256"])} chunks; original deleted')

    receiver, url = serve(root, token)

    # Slow each chunk upload a little so the kill lands part way through.
    class Slow(BaseHTTPRequestHandler):
        def log_message(self, *args):
            pass

        def forward(self):
            length = int(self.headers.get('Content-Length', '0'))
            body = self.rfile.read(length) if length else None
            if self.command == 'PUT':
                time.sleep(0.15)
            headers = {k: self.headers[k] for k in ('Authorization', 'Content-Type') if k in self.headers}
            headers['Content-Length'] = str(length)
            connection = http.client.HTTPConnection(urlsplit(url).netloc, timeout=30)
            try:
                connection.request(self.command, self.path, body, headers)
                response = connection.getresponse()
                payload = response.read()
                self.send_response(response.status)
                self.send_header('Content-Type', 'application/json')
                self.send_header('Content-Length', str(len(payload)))
                self.end_headers()
                self.wfile.write(payload)
            except (OSError, http.client.HTTPException):
                pass
            finally:
                connection.close()

        do_GET = do_POST = do_PUT = forward

    proxy = ThreadingHTTPServer(('127.0.0.1', 0), Slow)
    threading.Thread(target=proxy.serve_forever, daemon=True).start()
    sender = subprocess.Popen([binary, 'send', '--spool=' + str(spool), '--reuse=' + identity, '--url=http://127.0.0.1:' + str(proxy.server_port), '--token-file=' + str(token)], stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
    first = []
    for line in sender.stderr:
        first.append(line.rstrip('\n'))
        if line.startswith('sent 16 of 64'):
            break
    kill(sender)
    kill(receiver)
    proxy.shutdown()
    proxy.server_close()
    first.append('[sender and receiver killed]')
    (out / 'run1-stderr.txt').write_text('\n'.join(first) + '\n')
    note('run 1 last line before the kill: ' + first[-2])

    receiver, url = serve(root, token)
    status = json.loads(run('status', '--url=' + url, '--token-file=' + str(token), '--id=' + identity).stdout)
    (out / 'status-after-restart.json').write_text(json.dumps(status, indent=2) + '\n')
    note(f'after restart the receiver proves {status["verified_chunks"]} of 64 chunks; missing {len(status["missing_chunks"])}')

    second = run('send', '--spool=' + str(spool), '--reuse=' + identity, '--url=' + url, '--token-file=' + str(token))
    (out / 'run2-stderr.txt').write_text(second.stderr)
    result = json.loads(second.stdout)
    (out / 'run2-result.json').write_text(json.dumps(result, indent=2) + '\n')
    note('run 2 first line: ' + second.stderr.splitlines()[0])
    receipt = result['status']['receipt']
    delivered = root / 'files' / receipt['output_name']
    actual = hashlib.sha256(delivered.read_bytes()).hexdigest()
    assert result['transferred'] and actual == digest == receipt['file_sha256']
    assert len(list((root / 'files').iterdir())) == 1
    note(f'transferred true; delivered SHA-256 {actual} matches the original')
    kill(receiver)

    cleaned = json.loads(run('clean', '--root=' + str(root)).stdout)
    (out / 'clean.json').write_text(json.dumps(cleaned, indent=2) + '\n')
    note(f'clean removed {cleaned["chunks_removed"]} chunks, {cleaned["bytes_freed"]} bytes')

    summary = dict(version=version, platform=platform.platform(), file_bytes=len(data), chunks=64, chunk_bytes=1 << 20,
                   killed_after=first[-2], verified_after_restart=status['verified_chunks'], missing_after_restart=len(status['missing_chunks']),
                   resumed_first_line=second.stderr.splitlines()[0], transferred=result['transferred'], sha256=actual,
                   clean_chunks_removed=cleaned['chunks_removed'], clean_bytes_freed=cleaned['bytes_freed'])
    (out / 'summary.json').write_text(json.dumps(summary, indent=2) + '\n')
(out / 'log.txt').write_text('\n'.join(log) + '\n')
