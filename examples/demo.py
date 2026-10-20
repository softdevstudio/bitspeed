#!/usr/bin/env python3
"""Generate controlled loopback recordings; never use these as a network benchmark."""
from pathlib import Path
import queue
import subprocess
import sys
import threading
from urllib.parse import urlsplit

binary = str(Path(sys.argv[1]).resolve())
output = Path(sys.argv[2])
output.mkdir(parents=True, exist_ok=True)
address = '127.0.0.1:0'
for name, delay in [('before', '200ms'), ('after', '20ms')]:
    fixture = subprocess.Popen([binary, 'fixture', '--listen=' + address, '--header-delay=' + delay], stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
    try:
        lines = queue.Queue()
        threading.Thread(target=lambda: lines.put(fixture.stdout.readline()), daemon=True).start()
        base = lines.get(timeout=10).strip()
        if not base.startswith('http://127.0.0.1:'):
            raise RuntimeError('fixture did not start')
        address = urlsplit(base).netloc
        recording = subprocess.run([binary, 'record', '--samples=3', '--interval=0s', '--timeout=5s', base + '/headers'], capture_output=True, text=True, check=True, timeout=20)
        (output / (name + '.json')).write_text(recording.stdout)
    finally:
        fixture.terminate()
        try:
            fixture.communicate(timeout=10)
        except subprocess.TimeoutExpired:
            fixture.kill()
            fixture.communicate()
for command, inputs, filename in [('explain', ['before.json'], 'explanation.txt'), ('compare', ['before.json', 'after.json'], 'comparison.txt')]:
    report = subprocess.run([binary, command, *[str(output / p) for p in inputs]], capture_output=True, text=True, check=True, timeout=10)
    (output / filename).write_text(report.stdout)
print('Saved loopback recordings and reports to', output)
