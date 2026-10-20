#!/usr/bin/env python3
"""Check the built CLI and fixture together, including partial-failure output."""
import json
import os
import queue
import signal
import subprocess
import sys
import threading
import tempfile
from pathlib import Path
import time

binary = sys.argv[1]
fixture = subprocess.Popen([binary, 'fixture'], stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
lines = queue.Queue()
threading.Thread(target=lambda: lines.put(fixture.stdout.readline()), daemon=True).start()
try:
    base = lines.get(timeout=10).strip()
    assert base.startswith('http://127.0.0.1:'), 'missing fixture URL'
    saved_recording = None
    for path, outcome in [('/empty', 'complete'), ('/redirect', 'complete'), ('/disconnect', 'failed')]:
        run = subprocess.run([binary, 'record', '--samples=1', base + path], capture_output=True, text=True, timeout=10)
        assert run.returncode == (0 if outcome == 'complete' else 1), run.stderr
        if saved_recording is None:
            saved_recording = run.stdout
        recording = json.loads(run.stdout)
        assert recording['samples'][0]['outcome'] == outcome
        explanation = subprocess.run([binary, 'explain', '--format=json', '-'], input=run.stdout, capture_output=True, text=True, timeout=10)
        assert explanation.returncode == 0, explanation.stderr
        report = json.loads(explanation.stdout)
        assert report['schema_version'] == 1 and len(report['source_sha256']) == 64
        assert report['samples'][0]['hops'][0]['calculations']
        terminal = subprocess.run([binary, 'explain', '-'], input=run.stdout, capture_output=True, text=True, timeout=10)
        assert terminal.returncode == 0 and 'Calculated' in terminal.stdout
        if path == '/redirect':
            assert len(recording['samples'][0]['hops']) == 2
        if path == '/disconnect':
            hop = recording['samples'][0]['hops'][0]
            assert hop['body_bytes'] == 5 and not hop['body_complete']
    with tempfile.TemporaryDirectory() as directory:
        baseline = Path(directory) / 'before.json'
        baseline.write_text(saved_recording)
        comparison = subprocess.run([binary, 'compare', '--format=json', str(baseline), '-'], input=saved_recording, capture_output=True, text=True, timeout=10)
        assert comparison.returncode == 0, comparison.stderr
        compared = json.loads(comparison.stdout)
        assert compared['baseline']['recorded'] == 1 and compared['candidate']['recorded'] == 1
        assert compared['hops'][0]['calculations']
    invalid = subprocess.run([binary, 'explain', '-'], input='{}', capture_output=True, text=True, timeout=10)
    assert invalid.returncode == 1 and not invalid.stdout
    bad = subprocess.run([binary, 'record', 'https://user:private@example.org/'], capture_output=True, text=True, timeout=10)
    assert bad.returncode == 1 and not bad.stdout and 'private' not in bad.stderr
    timed = subprocess.run([binary, 'record', '--samples=1', '--timeout=100ms', base + '/timeout'], capture_output=True, text=True, timeout=10)
    assert timed.returncode == 1 and json.loads(timed.stdout)['samples'][0]['hops'][0]['error_code'] == 'timeout'
    if os.name != 'nt':
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
    print('PASS: executable recording, explanations, comparison, redirects, truncated body, redaction, timeouts and native cancellation checks')
finally:
    fixture.terminate()
    try:
        fixture.communicate(timeout=10)
    except subprocess.TimeoutExpired:
        fixture.kill()
        fixture.communicate()
