#!/usr/bin/env python3
"""Check the built CLI and fixture together, including partial-failure output."""
import json
import queue
import signal
import subprocess
import sys
import threading
import time

binary = sys.argv[1]
fixture = subprocess.Popen([binary, 'fixture'], stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
lines = queue.Queue()
threading.Thread(target=lambda: lines.put(fixture.stdout.readline()), daemon=True).start()
try:
    base = lines.get(timeout=10).strip()
    assert base.startswith('http://127.0.0.1:'), 'missing fixture URL'
    for path, outcome in [('/empty', 'complete'), ('/redirect', 'complete'), ('/disconnect', 'failed')]:
        run = subprocess.run([binary, 'record', '--samples=1', base + path], capture_output=True, text=True, timeout=10)
        assert run.returncode == (0 if outcome == 'complete' else 1), run.stderr
        recording = json.loads(run.stdout)
        assert recording['samples'][0]['outcome'] == outcome
        if path == '/redirect':
            assert len(recording['samples'][0]['hops']) == 2
        if path == '/disconnect':
            hop = recording['samples'][0]['hops'][0]
            assert hop['body_bytes'] == 5 and not hop['body_complete']
    bad = subprocess.run([binary, 'record', 'https://user:private@example.org/'], capture_output=True, text=True, timeout=10)
    assert bad.returncode == 1 and not bad.stdout and 'private' not in bad.stderr
    cancelled = subprocess.Popen([binary, 'record', '--samples=3', base + '/timeout'], stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
    try:
        time.sleep(0.2)
        cancelled.send_signal(signal.SIGTERM)
        stdout, stderr = cancelled.communicate(timeout=10)
        assert cancelled.returncode == 1, stderr
        recording = json.loads(stdout)
        assert recording['samples'][0]['outcome'] == 'cancelled'
        assert recording['samples'][0]['hops'][0]['error_code'] == 'cancelled'
    finally:
        if cancelled.poll() is None:
            cancelled.kill()
            cancelled.communicate()
    print('PASS: executable recording, redirects, truncated body, redaction, SIGTERM cancellation')
finally:
    fixture.terminate()
    try:
        fixture.communicate(timeout=10)
    except subprocess.TimeoutExpired:
        fixture.kill()
        fixture.communicate()
