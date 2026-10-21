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
    with tempfile.TemporaryDirectory() as scratch:
        body = Path(scratch) / 'body.json'
        body.write_text('{"token":"SECRET-BODY"}')
        posted = subprocess.run([binary, 'record', '--samples=2', '--method=POST', '--body-file=' + str(body), '--content-type=application/json', '--connection=warm', base + '/empty'], capture_output=True, text=True, timeout=20)
        assert posted.returncode == 0, posted.stderr
        warm = json.loads(posted.stdout)
        assert warm['schema_version'] == 2 and warm['request']['method'] == 'POST' and warm['request']['connection_policy'] == 'warm' and 'SECRET-BODY' not in posted.stdout
        assert warm['samples'][1]['hops'][0]['connection_reused'] is True and warm['samples'][1]['hops'][0]['method'] == 'POST'
        warm_path = Path(scratch) / 'warm.json'
        warm_path.write_text(posted.stdout)
        plain = subprocess.run([binary, 'record', '--samples=2', base + '/empty'], capture_output=True, text=True, timeout=20)
        assert plain.returncode == 0 and json.loads(plain.stdout)['schema_version'] == 1
        mixed = subprocess.run([binary, 'compare', '--format=json', str(warm_path), '-'], input=plain.stdout, capture_output=True, text=True, timeout=10)
        rules = {c['rule_id'] for c in json.loads(mixed.stdout)['checks'] if c['severity'] == 'blocking'}
        assert mixed.returncode == 0 and {'request_setting_method_v1', 'request_setting_connection_policy_v1'} <= rules, rules
        rejected = subprocess.run([binary, 'record', '--body-file=' + str(body), base + '/empty'], capture_output=True, text=True, timeout=10)
        assert rejected.returncode == 1 and not rejected.stdout
    saved = Path(__file__).resolve().parents[1] / 'examples' / 'recordings'
    before, after = str(saved / 'before.json'), str(saved / 'after.json')
    public = Path(__file__).resolve().parents[1] / 'examples' / 'public' / 'results' / 'github-ubuntu'
    site_a, site_b = str(public / 'example-com.json'), str(public / 'bitspeed-home.json')
    for args, code in ((['--fail-if-slower=first_byte_wait', before, after], 0), (['--fail-if-slower=first_byte_wait', after, before], 3), (['--fail-if-slower=first_byte_wait', site_a, site_b], 4), (['--fail-if-slower=first_byte_wait', '--allow-undecided', site_a, site_b], 0), (['--fail-if-slower=observed_payload_rate', before, after], 1)):
        gated = subprocess.run([binary, 'compare', *args], capture_output=True, text=True, timeout=10)
        assert gated.returncode == code, (args, gated.returncode, gated.stderr)
        assert (code == 1) == (gated.stdout == ''), args
    print('PASS: executable recording, explanations, comparison, compare gate exit codes, POST bodies, warm connections, redirects, truncated body, redaction, timeouts and native cancellation checks')
finally:
    fixture.terminate()
    try:
        fixture.communicate(timeout=10)
    except subprocess.TimeoutExpired:
        fixture.kill()
        fixture.communicate()
