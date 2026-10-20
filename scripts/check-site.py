#!/usr/bin/env python3
"""Optional deployed-site smoke check, enabled by BITSPEED_CHECK_SITE=1."""
from concurrent.futures import ThreadPoolExecutor
from subprocess import run

paths = ['/', '/about/', '/contact/', '/download/', '/quick-start/', '/reference/', '/inspect/', '/transfer/', '/bitspeed-transfer-resumes-interrupted-file-transfers-from-verified-chunks/', '/images/inspect-evidence.svg', '/images/ukue-terminal.png']

def check(path):
    result = run(['curl', '--silent', '--show-error', '--max-time', '40', '--max-filesize', '2097152', '--write-out', '\\n%{http_code}', 'https://bitspeed.org' + path], capture_output=True, text=True, timeout=45)
    assert result.returncode == 0, (path, result.stderr)
    body, status_text = result.stdout.rsplit('\n', 1)
    status = int(status_text)
    assert status == (404 if path.endswith('ukue-terminal.png') else 200), (path, status)
    assert 'ukue' not in body.lower(), path
    if path == '/':
        assert 'Find the Slow Interval' in body
        assert body.count('href="/contact/"') + body.count('href=/contact/') >= 2
    if path == '/contact/':
        assert 'info@bitspeed.org' in body
    if path == '/download/':
        assert 'bitspeed-inspect-linux-amd64.tar.gz' in body and 'bitspeed-transfer-linux-amd64.tar.gz' in body and 'Cross-built only' in body
        assert 'delivery unsupported' in body and 'bitspeed-transfer-verification.json' in body
    if path in ('/transfer/', '/bitspeed-transfer-resumes-interrupted-file-transfers-from-verified-chunks/'):
        assert 'Transfer 0.1.0' in body and 'Windows' in body
    return f'PASS live {status} https://bitspeed.org{path}'

with ThreadPoolExecutor(max_workers=5) as pool:
    for result in pool.map(check, paths):
        print(result)
