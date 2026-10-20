#!/usr/bin/env python3
"""Verify released tag/latest bytes and run downloaded native executables."""
import argparse
from concurrent.futures import ThreadPoolExecutor
import json
from pathlib import Path
import platform
import subprocess
import sys
import release_inspect as release

parser = argparse.ArgumentParser()
parser.add_argument('--metadata', required=True)
parser.add_argument('--output', required=True)
parser.add_argument('--downloads', required=True)
args = parser.parse_args()
metadata = json.loads(Path(args.metadata).read_text())
sha, version = release.valid_sha(metadata['source_sha']), metadata['version']
assert metadata['tag_name'] == 'transfer/v' + version and not metadata['draft']
assert metadata['tag_sha'] == sha
output, downloads = Path(args.output), Path(args.downloads)
output.mkdir(parents=True, exist_ok=True); downloads.mkdir(parents=True, exist_ok=True)
expected = {release.archive_name(p, *t) for p in ('inspect', 'transfer') for t in release.TARGETS}
expected |= {'bitspeed-inspect-verification.json', 'bitspeed-transfer-verification.json', 'SHA256SUMS'}
assets = {a['name']: a['url'] for a in metadata['assets']}
assert set(assets) == expected, 'public asset set mismatch'


def fetch(url, path):
    assert url.startswith('https://github.com/' + release.REPO + '/releases/'), 'unexpected download origin'
    subprocess.run(['curl', '--fail', '--silent', '--show-error', '--location', '--max-time', '60', '--retry', '4', '--retry-all-errors', '--retry-delay', '2', '--output', str(path), url], check=True, capture_output=True)


fetch(assets['SHA256SUMS'], downloads / 'SHA256SUMS')
checks = release.checksum_map(downloads / 'SHA256SUMS')
assert set(checks) == expected - {'SHA256SUMS'}
previous = release.checksum_map(release.ROOT / 'test/results/session-five-release/SHA256SUMS')
for name, digest in previous.items():
    assert checks[name] == digest, 'Inspect bytes changed: ' + name


def verify(name):
    tagged, latest = downloads / name, downloads / ('latest-' + name)
    fetch(assets[name], tagged)
    fetch('https://github.com/' + release.REPO + '/releases/latest/download/' + name, latest)
    assert release.hash_file(tagged) == release.hash_file(latest), 'latest differs: ' + name
    if name != 'SHA256SUMS':
        assert release.hash_file(tagged) == checks[name], 'checksum mismatch: ' + name
    return 'PASS tag/latest checksum: ' + name


lines = []
with ThreadPoolExecutor(max_workers=4) as pool:
    lines.extend(pool.map(verify, sorted(expected)))
release.PRODUCT = 'transfer'
for target in release.TARGETS:
    info = release.verify_archive(downloads / release.archive_name('transfer', *target), version, sha, require_clean=True)
    if target[0] == 'windows':
        assert info['operational_support'] == 'unsupported'
verification = json.loads((downloads / 'bitspeed-transfer-verification.json').read_text())
assert verification['source_sha'] == sha and verification['version'] == version
assert verification['operational_native_targets'] == ['linux/amd64', 'darwin/arm64']
assert verification['cross_build_only_targets'] == ['linux/arm64', 'darwin/amd64']
assert 'windows/amd64' in verification['limited_native_targets']
native = {('Linux', 'x86_64'): ('linux', 'amd64'), ('Darwin', 'arm64'): ('darwin', 'arm64')}.get((platform.system(), platform.machine()))
if native:
    destination = downloads / 'native-transfer'
    release.verify_archive(downloads / release.archive_name('transfer', *native), version, sha, destination, require_clean=True)
    binary = destination / 'bitspeed-transfer'
    for script in ('smoke-transfer.py', 'smoke-transfer-interruption.py', 'smoke-transfer-tls.py'):
        command = [sys.executable, str(release.ROOT / 'scripts' / script), str(binary)]
        if script == 'smoke-transfer.py':
            command.append(version)
        result = subprocess.run(command, check=True, capture_output=True, text=True, timeout=90)
        lines.extend(result.stdout.strip().splitlines())
    release.PRODUCT = 'inspect'
    prior = json.loads((downloads / 'bitspeed-inspect-verification.json').read_text())
    destination = downloads / 'native-inspect'
    release.verify_archive(downloads / release.archive_name('inspect', *native), prior['version'], prior['source_sha'], destination, require_clean=True)
    result = subprocess.run([sys.executable, str(release.ROOT / 'scripts/smoke-inspect.py'), str(destination / 'bitspeed-inspect')], check=True, capture_output=True, text=True, timeout=90)
    lines.extend(result.stdout.strip().splitlines())
lines.append('PASS public Transfer ' + version + ' source ' + sha + '; Inspect archives and verification metadata unchanged')
(output / 'SHA256SUMS').write_bytes((downloads / 'SHA256SUMS').read_bytes())
(output / 'verification.json').write_text(json.dumps(verification, indent=2) + '\n')
(output / 'checks.log').write_text('\n'.join(lines) + '\n')
print('\n'.join(lines))
