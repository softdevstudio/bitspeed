#!/usr/bin/env python3
"""Send a folder with parallel chunks, then unpack it while the receiver runs."""
from pathlib import Path
import hashlib
import json
import queue
import subprocess
import sys
import tempfile
import threading

binary = str(Path(sys.argv[1]).resolve())


def listing(root):
    out = {}
    for path in sorted(root.rglob('*')):
        rel = path.relative_to(root).as_posix()
        out[rel] = 'dir' if path.is_dir() else hashlib.sha256(path.read_bytes()).hexdigest()
    return out


with tempfile.TemporaryDirectory() as directory:
    base_dir = Path(directory)
    source = base_dir / 'source'
    files = {
        'README.txt': b'folder smoke\n',
        'img/2026/frame.raw': bytes(range(256)) * 1200,
        'img/café.txt': b'unicode name',
        'docs/empty': b'',
        'docs/nested/deep/notes.md': b'# notes\n' * 5000,
    }
    for name, data in files.items():
        path = source / name
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_bytes(data)
    (source / 'empty-folder').mkdir()
    expected = listing(source)
    spool = base_dir / 'spool'
    prepared = subprocess.run([binary, 'send', '--prepare-only', '--source=' + str(source), '--spool=' + str(spool), '--chunk-size=65536'], capture_output=True, text=True, check=True, timeout=30)
    result = json.loads(prepared.stdout)
    assert result['prepared'] and not result['transferred']
    assert result['tree'] == {'files': 5, 'folders': 6, 'bytes': sum(len(v) for v in files.values())}, result['tree']
    identity = result['manifest']['transfer_id']
    chunks = len(result['manifest']['chunk_sha256'])
    assert chunks > 4, chunks
    token_file = base_dir / 'token'
    token_file.write_text('0123456789abcdef' * 4 + '\n')
    token_file.chmod(0o600)
    root = base_dir / 'receiver'
    receiver = subprocess.Popen([binary, 'serve', '--listen=127.0.0.1:0', '--root=' + str(root), '--token-file=' + str(token_file)], stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
    try:
        lines = queue.Queue()
        threading.Thread(target=lambda: lines.put(receiver.stdout.readline()), daemon=True).start()
        url = lines.get(timeout=10).strip()
        assert url.startswith('http://127.0.0.1:'), url
        sent = subprocess.run([binary, 'send', '--spool=' + str(spool), '--reuse=' + identity, '--url=' + url, '--token-file=' + str(token_file), '--parallel=4'], capture_output=True, text=True, check=True, timeout=60)
        delivered = json.loads(sent.stdout)
        assert delivered['transferred'] and delivered['status']['state'] == 'completed'
        assert 'sent %d of %d chunks' % (chunks, chunks) in sent.stderr, sent.stderr
        # Unpack while serve is still running.
        out = base_dir / 'restored'
        unpacked = subprocess.run([binary, 'unpack', '--root=' + str(root), '--id=' + identity, '--out=' + str(out)], capture_output=True, text=True, check=True, timeout=30)
        report = json.loads(unpacked.stdout)
        assert report['unpacked'] and report['files'] == 5 and report['folders'] == 6, report
        assert listing(out) == expected
        again = subprocess.run([binary, 'unpack', '--root=' + str(root), '--id=' + identity, '--out=' + str(out)], capture_output=True, text=True, timeout=30)
        assert again.returncode == 1 and not again.stdout and 'already exists' in again.stderr
        assert listing(out) == expected
        inside = subprocess.run([binary, 'unpack', '--root=' + str(root), '--id=' + identity, '--out=' + str(root / 'files' / 'x')], capture_output=True, text=True, timeout=30)
        assert inside.returncode == 1 and 'outside the receiver root' in inside.stderr
    finally:
        receiver.terminate()
        try:
            _, diagnostics = receiver.communicate(timeout=15)
        except subprocess.TimeoutExpired:
            receiver.kill()
            _, diagnostics = receiver.communicate()
        assert '0123456789abcdef' not in diagnostics
    # A plain file transfer is not a folder.
    single = base_dir / 'single'
    single.write_bytes(b'one file')
    plain = json.loads(subprocess.run([binary, 'send', '--prepare-only', '--source=' + str(single), '--spool=' + str(spool)], capture_output=True, text=True, check=True, timeout=10).stdout)
    assert 'tree' not in plain

print('PASS: Transfer folder snapshot, parallel send, unpack beside a running receiver, refused re-unpack and output inside the root')
