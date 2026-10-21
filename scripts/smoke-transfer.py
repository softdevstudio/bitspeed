#!/usr/bin/env python3
"""Exercise development preparation, staging, durable delivery and restart."""
from pathlib import Path
import subprocess
import json
import os
import sys
import tempfile
import queue
import threading
import hashlib

binary = str(Path(sys.argv[1]).resolve())
version = subprocess.run([binary, '--version'], capture_output=True, text=True, check=True, timeout=10)
expected_version = sys.argv[2] if len(sys.argv) > 2 else __import__('re').search(r'const Version = "([^"]+)"', (Path(__file__).resolve().parents[1] / 'transfer/version.go').read_text()).group(1)
assert version.stdout == 'bitspeed-transfer ' + expected_version + '\n'
with tempfile.TemporaryDirectory() as directory:
    canary = Path(directory) / 'source'
    canary.write_bytes(b'unchanged')
    for command in ('serve', 'send', 'status', 'clean'):
        help_result = subprocess.run([binary, command, '--help'], capture_output=True, text=True, timeout=10)
        assert help_result.returncode == 0 and help_result.stdout
        result = subprocess.run([binary, command, str(canary), '--token=SECRET'], cwd=directory, capture_output=True, text=True, timeout=10)
        assert result.returncode == 1 and not result.stdout
        assert result.stderr and 'SECRET' not in result.stderr
        assert canary.read_bytes() == b'unchanged'
        assert list(Path(directory).iterdir()) == [canary]
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
        assert base.startswith('http://127.0.0.1:'), (base, receiver.poll(), receiver.stderr.read() if receiver.poll() is not None else 'still running')
        uploaded = subprocess.run([binary, 'send', '--upload-only', '--spool=' + str(spool), '--reuse=' + identity, '--url=' + base, '--token-file=' + str(token_file)], capture_output=True, text=True, check=True, timeout=20)
        staged = json.loads(uploaded.stdout)
        assert 'receiver has 0 of 1 chunks verified; sending 1 (9 bytes)' in uploaded.stderr and 'sent 1 of 1 chunks, 9/9 bytes (100%)' in uploaded.stderr
        assert '0123456789abcdef' not in uploaded.stderr and str(spool) not in uploaded.stderr
        assert staged['uploaded'] and not staged['transferred']
        assert staged['status']['state'] == 'uploading' and staged['status']['receipt'] is None
        assert staged['status']['verified_bytes'] == 9 and not staged['status']['missing_chunks']
        status = subprocess.run([binary, 'status', '--url=' + base, '--id=' + identity, '--token-file=' + str(token_file)], capture_output=True, text=True, check=True, timeout=10)
        assert json.loads(status.stdout) == staged['status']
        assert not list((receiver_root / 'files').iterdir())
        assert (receiver_root / 'sessions' / identity / 'chunks' / '0').read_bytes() == b'unchanged'
        send_args = [binary, 'send', '--spool=' + str(spool), '--reuse=' + identity, '--url=' + base, '--token-file=' + str(token_file)]
        completed = subprocess.run(send_args, capture_output=True, text=True, check=True, timeout=20)
        delivered = json.loads(completed.stdout)
        assert 'receiver has 1 of 1 chunks verified; sending 0 (0 bytes)' in completed.stderr and 'all 1 chunks verified; receiver is checking and publishing the file' in completed.stderr
        assert delivered['transferred'] and delivered['status']['state'] == 'completed'
        receipt = delivered['status']['receipt']
        output = receiver_root / 'files' / receipt['output_name']
        assert output.read_bytes() == b'unchanged'
        assert hashlib.sha256(output.read_bytes()).hexdigest() == receipt['file_sha256']
        assert json.loads((spool / identity / 'receipt.json').read_text()) == receipt

    finally:
        receiver.terminate()
        try:
            _, diagnostics = receiver.communicate(timeout=15)
        except subprocess.TimeoutExpired:
            receiver.kill()
            _, diagnostics = receiver.communicate()
        assert '0123456789abcdef' not in diagnostics
    # With the receiver stopped, clean removes the completed session's chunks only.
    chunk = receiver_root / 'sessions' / identity / 'chunks' / '0'
    dry = json.loads(subprocess.run([binary, 'clean', '--root=' + str(receiver_root), '--dry-run'], capture_output=True, text=True, check=True, timeout=20).stdout)
    assert dry['dry_run'] and dry['chunks_removed'] == 1 and dry['bytes_freed'] == 9 and chunk.exists()
    cleaned = json.loads(subprocess.run([binary, 'clean', '--root=' + str(receiver_root)], capture_output=True, text=True, check=True, timeout=20).stdout)
    assert not cleaned['dry_run'] and cleaned['chunks_removed'] == 1 and cleaned['sessions'][0]['cleaned'] and not chunk.exists()
    assert output.read_bytes() == b'unchanged'
    # Start a new receiver and a new sender process with the saved identity.
    receiver = subprocess.Popen([binary, 'serve', '--listen=127.0.0.1:0', '--root=' + str(receiver_root), '--token-file=' + str(token_file)], stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
    try:
        lines = queue.Queue()
        threading.Thread(target=lambda: lines.put(receiver.stdout.readline()), daemon=True).start()
        base = lines.get(timeout=10).strip()
        repeat_args = [binary, 'send', '--spool=' + str(spool), '--reuse=' + identity, '--url=' + base, '--token-file=' + str(token_file)]
        repeated = subprocess.run(repeat_args, capture_output=True, text=True, check=True, timeout=20)
        assert json.loads(repeated.stdout)['status']['receipt'] == receipt
        assert len(list((receiver_root / 'files').iterdir())) == 1
        output.write_bytes(b'damaged!!')
        rejected = subprocess.run(repeat_args, capture_output=True, text=True, timeout=20)
        assert rejected.returncode == 1 and not rejected.stdout
        status = subprocess.run([binary, 'status', '--url=' + base, '--id=' + identity, '--token-file=' + str(token_file)], capture_output=True, text=True, check=True, timeout=10)
        damaged = json.loads(status.stdout)
        assert damaged['state'] == 'failed' and damaged['error_code'] == 'storage_corrupt' and damaged['receipt'] is None
    finally:
        receiver.terminate()
        try:
            _, diagnostics = receiver.communicate(timeout=15)
        except subprocess.TimeoutExpired:
            receiver.kill()
            _, diagnostics = receiver.communicate()
        assert '0123456789abcdef' not in diagnostics


print('PASS: Transfer preparation, staging, verified publication, receipt cache, offline clean, progress output, process restart, repeat delivery and damaged-output rejection')
