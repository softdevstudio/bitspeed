#!/usr/bin/env python3
"""The unreleased Transfer skeleton must never report a successful transfer."""
from pathlib import Path
import subprocess
import json
import os
import sys
import tempfile
import queue
import threading

binary = str(Path(sys.argv[1]).resolve())
version = subprocess.run([binary, '--version'], capture_output=True, text=True, check=True, timeout=10)
assert version.stdout == 'bitspeed-transfer 0.0.0-dev\n'
with tempfile.TemporaryDirectory() as directory:
    canary = Path(directory) / 'source'
    canary.write_bytes(b'unchanged')
    for command in ('serve', 'send', 'status'):
        help_result = subprocess.run([binary, command, '--help'], capture_output=True, text=True, timeout=10)
        assert help_result.returncode == 0 and help_result.stdout
        result = subprocess.run([binary, command, str(canary), '--token=SECRET'], cwd=directory, capture_output=True, text=True, timeout=10)
        assert result.returncode == 1 and not result.stdout
        assert result.stderr and 'SECRET' not in result.stderr
        assert canary.read_bytes() == b'unchanged'
        assert list(Path(directory).iterdir()) == [canary]
    if os.name != 'nt':
        spool = Path(directory) / 'spool'
        prepared = subprocess.run([binary, 'send', '--prepare-only', '--source=' + str(canary), '--spool=' + str(spool)], capture_output=True, text=True, check=True, timeout=10)
        result = json.loads(prepared.stdout)
        assert result['prepared'] and not result['transferred']
        identity = result['manifest']['transfer_id']
        canary.unlink()
        reused = subprocess.run([binary, 'send', '--prepare-only', '--spool=' + str(spool), '--reuse=' + identity], capture_output=True, text=True, check=True, timeout=10)
        assert json.loads(reused.stdout) == result
        token_file = Path(directory) / 'token'
        token_file.write_text('0123456789abcdef' * 4 + '\n')
        token_file.chmod(0o600)
        receiver_root = Path(directory) / 'receiver'
        receiver = subprocess.Popen([binary, 'serve', '--listen=127.0.0.1:0', '--root=' + str(receiver_root), '--token-file=' + str(token_file)], stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
        try:
            lines = queue.Queue()
            threading.Thread(target=lambda: lines.put(receiver.stdout.readline()), daemon=True).start()
            base = lines.get(timeout=10).strip()
            assert base.startswith('http://127.0.0.1:'), base
            uploaded = subprocess.run([binary, 'send', '--upload-only', '--spool=' + str(spool), '--reuse=' + identity, '--url=' + base, '--token-file=' + str(token_file)], capture_output=True, text=True, check=True, timeout=20)
            staged = json.loads(uploaded.stdout)
            assert staged['uploaded'] and not staged['transferred']
            assert staged['status']['state'] == 'uploading' and staged['status']['receipt'] is None
            assert staged['status']['verified_bytes'] == 9 and not staged['status']['missing_chunks']
            status = subprocess.run([binary, 'status', '--url=' + base, '--id=' + identity, '--token-file=' + str(token_file)], capture_output=True, text=True, check=True, timeout=10)
            assert json.loads(status.stdout) == staged['status']
            assert not list((receiver_root / 'files').iterdir())
            assert (receiver_root / 'sessions' / identity / 'chunks' / '0').read_bytes() == b'unchanged'
        finally:
            receiver.terminate()
            try:
                _, diagnostics = receiver.communicate(timeout=15)
            except subprocess.TimeoutExpired:
                receiver.kill()
                _, diagnostics = receiver.communicate()
            assert '0123456789abcdef' not in diagnostics

print('PASS: Transfer prepare/reuse, authenticated chunk staging and status, no destination publication; invalid operations fail closed')
