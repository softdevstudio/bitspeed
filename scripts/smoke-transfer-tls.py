#!/usr/bin/env python3
"""Exercise the packaged TLS listener using a locally trusted certificate."""
from pathlib import Path
import http.client
import json
import os
import queue
import ssl
import subprocess
import sys
import tempfile
import threading
from urllib.parse import urlsplit

binary = str(Path(sys.argv[1]).resolve())
with tempfile.TemporaryDirectory() as directory:
    root = Path(directory)
    cert, key = root / 'cert.pem', root / 'key.pem'
    config = root / 'openssl.cnf'
    config.write_text('[req]\nprompt=no\ndistinguished_name=dn\nx509_extensions=v3\n[dn]\nCN=127.0.0.1\n[v3]\nsubjectAltName=IP:127.0.0.1\nbasicConstraints=critical,CA:TRUE\n')
    subprocess.run(['openssl', 'req', '-x509', '-newkey', 'rsa:2048', '-sha256', '-nodes', '-days', '1', '-config', str(config), '-keyout', str(key), '-out', str(cert)], check=True, capture_output=True, timeout=20)
    key.chmod(0o600)
    token = root / 'token'; token.write_text('0123456789abcdef' * 4 + '\n'); token.chmod(0o600)
    source = root / 'source'; source.write_bytes(b'TLS verified file')
    result = subprocess.run([binary, 'send', '--prepare-only', '--source=' + str(source), '--spool=' + str(root / 'spool')], capture_output=True, text=True, check=True, timeout=15)
    manifest = json.loads(result.stdout)['manifest']; identity = manifest['transfer_id']
    process = subprocess.Popen([binary, 'serve', '--root=' + str(root / 'receiver'), '--token-file=' + str(token), '--listen=127.0.0.1:0', '--tls-cert=' + str(cert), '--tls-key=' + str(key)], stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
    try:
        lines = queue.Queue(); threading.Thread(target=lambda: lines.put(process.stdout.readline()), daemon=True).start()
        url = lines.get(timeout=10).strip(); assert url.startswith('https://127.0.0.1:'), (url, process.poll(), process.stderr.read() if process.poll() is not None else 'still running')
        context = ssl.create_default_context(cafile=str(cert)); context.minimum_version = ssl.TLSVersion.TLSv1_2
        def request(method, path, data, kind):
            connection = http.client.HTTPSConnection(urlsplit(url).netloc, context=context, timeout=15)
            try:
                connection.request(method, path, data, {'Authorization': 'Bearer ' + token.read_text().strip(), 'Content-Type': kind})
                response = connection.getresponse(); saved = json.loads(response.read())
                assert response.status in (200, 201), saved
                return saved
            finally:
                connection.close()
        admitted = request('POST', '/v1/transfers', json.dumps(manifest).encode(), 'application/json')
        assert admitted['state'] == 'uploading'
        request('PUT', '/v1/transfers/' + identity + '/chunks/0', source.read_bytes(), 'application/octet-stream')
        status = request('POST', '/v1/transfers/' + identity + '/finalize', json.dumps({'manifest_sha256': admitted['manifest_sha256']}).encode(), 'application/json')
        assert status['state'] == 'completed'
        assert (root / 'receiver' / 'files' / status['receipt']['output_name']).read_bytes() == source.read_bytes()
    finally:
        process.kill(); _, errors = process.communicate(timeout=10)
        assert '0123456789abcdef' not in errors
print('PASS: executable HTTPS listener, certificate verification, authenticated upload and completion')
