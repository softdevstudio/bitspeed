#!/usr/bin/env python3
"""The unreleased Transfer skeleton must never report a successful transfer."""
from pathlib import Path
import subprocess
import json
import os
import sys
import tempfile

binary = str(Path(sys.argv[1]).resolve())
version = subprocess.run([binary, '--version'], capture_output=True, text=True, check=True, timeout=10)
assert version.stdout == 'bitspeed-transfer 0.0.0-dev\n'
with tempfile.TemporaryDirectory() as directory:
    canary = Path(directory) / 'source'
    canary.write_bytes(b'unchanged')
    for command in ('serve', 'send', 'status'):
        help_result = subprocess.run([binary, command, '--help'], capture_output=True, text=True, timeout=10)
        assert help_result.returncode == 0 and 'Not implemented' in help_result.stdout
        result = subprocess.run([binary, command, str(canary), '--token=SECRET'], cwd=directory, capture_output=True, text=True, timeout=10)
        assert result.returncode == 1 and not result.stdout
        assert 'not implemented' in result.stderr and 'SECRET' not in result.stderr
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
print('PASS: local snapshot prepare/reuse and Transfer skeleton version/help; unimplemented operations fail closed')
