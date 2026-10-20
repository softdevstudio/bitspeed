#!/usr/bin/env python3
"""Build either independent tool after exact-commit CI and native gates."""
import argparse
import hashlib
import json
import os
from pathlib import Path, PurePosixPath
import re
import shutil
import subprocess
import tarfile
import tempfile
import urllib.error
import urllib.parse
import urllib.request
import zipfile

ROOT = Path(__file__).resolve().parents[1]
REPO = 'softdevstudio/bitspeed'
PRODUCT = 'inspect'
TARGETS = [('linux', 'amd64'), ('linux', 'arm64'), ('darwin', 'amd64'), ('darwin', 'arm64'), ('windows', 'amd64')]
NATIVE = [('linux', 'amd64'), ('darwin', 'arm64'), ('windows', 'amd64')]


def command(*args, **kwargs):
    return subprocess.check_output(args, cwd=ROOT, text=True, **kwargs).strip()


def version():
    matches = re.findall(r'^const Version = "([^"]+)"$', (ROOT / PRODUCT / 'version.go').read_text(), re.M)
    if len(matches) != 1 or not re.fullmatch(r'\d+\.\d+\.\d+(?:-dev)?', matches[0]):
        raise ValueError('invalid product version constant')
    return matches[0]


def valid_sha(sha):
    if not re.fullmatch(r'[0-9a-f]{40}', sha):
        raise ValueError('invalid source SHA')
    return sha


def archive_name(product, goos, arch):
    if (goos, arch) not in TARGETS or product not in ('inspect', 'transfer'):
        raise ValueError('unsupported archive target')
    return f'bitspeed-{product}-{goos}-{arch}.' + ('zip' if goos == 'windows' else 'tar.gz')


def reference_docs():
    return ('recording-v1.md', 'explanations.md', 'comparisons.md', 'releases.md') if PRODUCT == 'inspect' else ('sender-spool.md', 'receiver-staging.md', 'transfer-v1.md', 'finalization.md', 'transfer-releases.md')


def package(goos, arch, output, allow_dirty=False):
    name = archive_name(PRODUCT, goos, arch)
    sha = valid_sha(command('git', 'rev-parse', 'HEAD'))
    dirty = bool(command('git', 'status', '--porcelain'))
    if dirty and not allow_dirty:
        raise ValueError('packaging requires a clean checkout')
    env = dict(os.environ, GOOS=goos, GOARCH=arch, CGO_ENABLED='0', GOTOOLCHAIN='local')
    goversion = command('go', 'env', 'GOVERSION', env=env)
    required = re.search(r'^go ([\d.]+)$', (ROOT / 'go.mod').read_text(), re.M).group(1)
    if goversion != 'go' + required:
        raise ValueError('packaging toolchain must match go.mod exactly')
    goroot = Path(command('go', 'env', 'GOROOT', env=env))
    output = Path(output).resolve()
    output.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory() as temp:
        stage = Path(temp)
        binary = stage / ('bitspeed-' + PRODUCT + ('.exe' if goos == 'windows' else ''))
        subprocess.run(['go', 'build', '-trimpath', '-buildvcs=true', '-ldflags=-s -w', '-o', str(binary), './cmd/bitspeed-' + PRODUCT], cwd=ROOT, env=env, check=True)
        binary.chmod(0o755)
        for filename in ('LICENSE', 'NOTICE', 'README.md', 'THIRD_PARTY_LICENSES.md'):
            shutil.copyfile(ROOT / filename, stage / filename)
        (stage / 'docs').mkdir()
        for filename in reference_docs():
            shutil.copyfile(ROOT / 'docs' / filename, stage / 'docs' / filename)
        # Preserve toolchain license/notice paths, including bundled standard-library vendors.
        for path in goroot.rglob('*'):
            if path.is_file() and (path.name.startswith(('LICENSE', 'NOTICE', 'PATENTS')) or path == goroot / 'VERSION'):
                dest = stage / 'third_party/go' / path.relative_to(goroot)
                dest.parent.mkdir(parents=True, exist_ok=True)
                shutil.copyfile(path, dest)
        info = dict(product='bitspeed-' + PRODUCT, version=version(), source_sha=sha, dirty=dirty,
                    go_version=goversion, goos=goos, goarch=arch, cgo_enabled=False,
                    native_smoke_required=[goos, arch] in [list(t) for t in NATIVE],
                    operational_support='unsupported' if PRODUCT == 'transfer' and goos == 'windows' else 'see included platform documentation',
                    native_smoke_scope='version/help/unsupported-operation checks only' if PRODUCT == 'transfer' and goos == 'windows' else 'product operations on native targets')
        (stage / 'BUILD_INFO.json').write_text(json.dumps(info, indent=2) + '\n')
        files = sorted(p for p in stage.rglob('*') if p.is_file())
        if goos == 'windows':
            with zipfile.ZipFile(output / name, 'w', compression=zipfile.ZIP_DEFLATED) as archive:
                for path in files:
                    archive.write(path, path.relative_to(stage).as_posix())
        else:
            with tarfile.open(output / name, 'w:gz') as archive:
                for path in files:
                    archive.add(path, arcname=path.relative_to(stage).as_posix(), recursive=False)
    print(name)


def verify_archive(path, expected_version, sha, destination=None, require_clean=False):
    path = Path(path)
    matching = [(goos, arch) for goos, arch in TARGETS if path.name == archive_name(PRODUCT, goos, arch)]
    if len(matching) != 1:
        raise ValueError('unexpected archive filename')
    goos, arch = matching[0]
    with tempfile.TemporaryDirectory() as temp:
        stage = Path(temp)
        if path.suffix == '.zip':
            with zipfile.ZipFile(path) as archive:
                members = archive.infolist()
                names = [m.filename for m in members]
                if any((m.external_attr >> 16) & 0o170000 == 0o120000 for m in members):
                    raise ValueError('archive contains symlink')
                for name in names:
                    safe_member(name)
                if len(names) != len(set(names)):
                    raise ValueError('duplicate archive members')
                archive.extractall(stage)
        else:
            with tarfile.open(path) as archive:
                members = archive.getmembers()
                names = [m.name for m in members]
                for m in members:
                    safe_member(m.name)
                    if not m.isfile():
                        raise ValueError('archive contains non-file member')
                if len(names) != len(set(names)):
                    raise ValueError('duplicate archive members')
                archive.extractall(stage, filter='data')
        required = {'BUILD_INFO.json', 'LICENSE', 'NOTICE', 'README.md', 'THIRD_PARTY_LICENSES.md', 'third_party/go/LICENSE', 'third_party/go/PATENTS'}
        required.add('bitspeed-' + PRODUCT + ('.exe' if goos == 'windows' else ''))
        required.update('docs/' + name for name in reference_docs())
        if not required.issubset(set(names)):
            raise ValueError('required archive files missing')
        info = json.loads((stage / 'BUILD_INFO.json').read_text())
        if (info['product'], info['version'], info['source_sha'], info['goos'], info['goarch']) != ('bitspeed-' + PRODUCT, expected_version, sha, goos, arch):
            raise ValueError('archive provenance mismatch')
        if require_clean and info['dirty']:
            raise ValueError('dirty source archive cannot be released')
        if destination:
            dest = Path(destination)
            if dest.exists() and any(dest.iterdir()):
                raise ValueError('extraction destination must be empty')
            shutil.copytree(stage, dest, dirs_exist_ok=True)
        return info


def safe_member(name):
    path = PurePosixPath(name)
    if path.is_absolute() or '..' in path.parts or '\\' in name or ':' in name:
        raise ValueError('unsafe archive path')


def api(path, optional=False):
    if os.environ.get('GITHUB_REPOSITORY') != REPO:
        raise ValueError('release repository mismatch')
    request = urllib.request.Request('https://api.github.com/repos/' + REPO + '/' + path,
        headers={'Authorization': 'Bearer ' + os.environ['GH_TOKEN'], 'Accept': 'application/vnd.github+json'})
    try:
        with urllib.request.urlopen(request, timeout=30) as response:
            return json.load(response)
    except urllib.error.HTTPError as err:
        if err.code == 404 and optional:
            return None
        raise


def verify_ci(sha):
    run = os.environ['CI_RUN_ID']
    if not run.isdecimal():
        raise ValueError('invalid CI run id')
    result = api('actions/runs/' + run)
    if result['conclusion'] != 'success' or result['head_sha'] != sha or result['head_branch'] != 'main' or result['event'] != 'push' or result['repository']['full_name'] != REPO:
        raise ValueError('successful exact-commit main CI was not established')
    if api('git/ref/heads/main')['object']['sha'] != sha:
        raise ValueError('checked source is no longer main HEAD')


def find_release(tag):
    # The tag endpoint omits drafts. List authenticated releases so retries
    # discover the draft created by this workflow before it is published.
    page = 1
    while True:
        releases = api(f'releases?per_page=100&page={page}')
        for item in releases:
            if item['tag_name'] == tag:
                return item
        if len(releases) < 100:
            return None
        page += 1


def plan(sha):
    valid_sha(sha)
    if command('git', 'rev-parse', 'HEAD') != sha:
        raise ValueError('checkout is not checked source')
    verify_ci(sha)
    v = version()
    release = find_release(PRODUCT + '/v' + v)
    published = release is not None and not release['draft']
    result = dict(sha=sha, version=v, build=str(not published).lower(), publish=str(not published and not v.endswith('-dev')).lower())
    with open(os.environ['GITHUB_OUTPUT'], 'a') as output:
        for key, value in result.items():
            output.write(key + '=' + value + '\n')
    print(json.dumps(result))


def hash_file(path):
    digest = hashlib.sha256()
    with open(path, 'rb') as stream:
        for block in iter(lambda: stream.read(1 << 20), b''):
            digest.update(block)
    return digest.hexdigest()


def checksum_map(path):
    out = {}
    for line in Path(path).read_text().splitlines():
        digest, name = line.split('  ', 1)
        if not re.fullmatch(r'[0-9a-f]{64}', digest) or name != Path(name).name or name in out:
            raise ValueError('invalid checksum manifest')
        out[name] = digest
    return out


def carry_other(output):
    other = 'transfer' if PRODUCT == 'inspect' else 'inspect'
    latest = api('releases/latest', optional=True)
    if latest is None:
        return
    allowed = {archive_name(other, *target) for target in TARGETS}
    names = {a['name'] for a in latest['assets']}
    present = names & allowed
    if not present:
        return
    if present != allowed or 'SHA256SUMS' not in names:
        raise ValueError('previous other-product release assets are incomplete')
    with tempfile.TemporaryDirectory() as temp:
        args = ['gh', 'release', 'download', latest['tag_name'], '--repo', REPO, '--dir', temp]
        for name in sorted(allowed | {'SHA256SUMS'}):
            args.extend(['--pattern', name])
        subprocess.run(args, check=True)
        checks = checksum_map(Path(temp) / 'SHA256SUMS')
        for name in sorted(allowed):
            if checks.get(name) != hash_file(Path(temp) / name):
                raise ValueError('previous other-product asset checksum mismatch')
            shutil.copyfile(Path(temp) / name, output / name)


def publish(sha, output):
    valid_sha(sha)
    verify_ci(sha)
    if command('git', 'rev-parse', 'HEAD') != sha:
        raise ValueError('publisher checkout is not checked source')
    v = version()
    if not re.fullmatch(r'\d+\.\d+\.\d+', v):
        raise ValueError('only stable product versions can publish')
    tag = PRODUCT + '/v' + v
    existing = find_release(tag)
    if existing and not existing['draft']:
        raise ValueError('published releases are immutable')
    run = os.environ['GITHUB_RUN_ID']
    if not run.isdecimal():
        raise ValueError('invalid release run id')
    jobs = api('actions/runs/' + run + '/jobs?per_page=100')['jobs']
    needed = {'package-' + goos + '-' + arch for goos, arch in TARGETS} | {'native-' + goos + '-' + arch for goos, arch in NATIVE}
    succeeded = {job['name'] for job in jobs if job['conclusion'] == 'success'}
    if not needed.issubset(succeeded):
        raise ValueError('required package/native gates did not succeed')
    output = Path(output).resolve()
    for target in TARGETS:
        verify_archive(output / archive_name(PRODUCT, *target), v, sha, require_clean=True)
    carry_other(output)
    verification = dict(product='bitspeed-' + PRODUCT, version=v, source_sha=sha, ci_run_id=int(os.environ['CI_RUN_ID']), release_run_id=int(run),
        native_smoke_targets=[goos + '/' + arch for goos, arch in NATIVE],
        cross_build_only_targets=[goos + '/' + arch for goos, arch in TARGETS if (goos, arch) not in NATIVE],
        operational_native_targets=['linux/amd64', 'darwin/arm64'] if PRODUCT == 'transfer' else [goos + '/' + arch for goos, arch in NATIVE],
        limited_native_targets={'windows/amd64': 'version/help/unsupported-operation checks only'} if PRODUCT == 'transfer' else {})
    (output / ('bitspeed-' + PRODUCT + '-verification.json')).write_text(json.dumps(verification, indent=2) + '\n')
    assets = sorted(path for path in output.iterdir() if path.is_file() and path.name != 'SHA256SUMS')
    allowed = {archive_name(p, *t) for p in ('inspect', 'transfer') for t in TARGETS} | {'bitspeed-' + PRODUCT + '-verification.json'}
    if any(path.name not in allowed for path in assets):
        raise ValueError('unexpected release asset')
    (output / 'SHA256SUMS').write_text(''.join(hash_file(path) + '  ' + path.name + '\n' for path in assets))
    ref = api('git/ref/tags/' + urllib.parse.quote(tag, safe='/'), optional=True)
    if ref is not None and ref['object']['sha'] != sha:
        raise ValueError('existing tag source mismatch')
    if ref is None:
        subprocess.run(['gh', 'api', 'repos/' + REPO + '/git/refs', '-f', 'ref=refs/tags/' + tag, '-f', 'sha=' + sha], check=True, stdout=subprocess.DEVNULL)
    if existing is None:
        subprocess.run(['gh', 'release', 'create', tag, '--repo', REPO, '--verify-tag', '--draft', '--title', 'BitSpeed ' + PRODUCT.title() + ' ' + v, '--notes', ('HTTP timing recording, explanations and comparisons.' if PRODUCT == 'inspect' else 'Verified resumable one-file delivery on supported Linux/macOS local filesystems. Windows operations are unsupported; its archive checks help/version and fail-closed behavior only. Linux arm64/macOS amd64 are cross-built without native runtime coverage.') + ' See included documentation and verification metadata.'], check=True)
    subprocess.run(['gh', 'release', 'upload', tag, '--repo', REPO, '--clobber', *map(str, assets), str(output / 'SHA256SUMS')], check=True)
    uploaded = find_release(tag)
    expected_names = {path.name for path in assets} | {'SHA256SUMS'}
    if uploaded is None or not uploaded['draft'] or {asset['name'] for asset in uploaded['assets']} != expected_names:
        raise ValueError('draft asset set mismatch')
    with tempfile.TemporaryDirectory() as temp:
        subprocess.run(['gh', 'release', 'download', tag, '--repo', REPO, '--dir', temp], check=True)
        for name in expected_names:
            if hash_file(Path(temp) / name) != hash_file(output / name):
                raise ValueError('uploaded asset integrity mismatch')
    verify_ci(sha)
    subprocess.run(['gh', 'release', 'edit', tag, '--repo', REPO, '--draft=false', '--latest'], check=True)
    print('Published ' + tag)


if __name__ == '__main__':
    parser = argparse.ArgumentParser()
    parser.add_argument('--product', choices=('inspect', 'transfer'), default='inspect')
    sub = parser.add_subparsers(dest='action', required=True)
    p = sub.add_parser('package'); p.add_argument('--goos', required=True); p.add_argument('--goarch', required=True); p.add_argument('--output', required=True); p.add_argument('--allow-dirty', action='store_true')
    p = sub.add_parser('verify'); p.add_argument('archive'); p.add_argument('--version', required=True); p.add_argument('--sha', required=True); p.add_argument('--extract'); p.add_argument('--require-clean', action='store_true')
    for action in ('plan', 'publish'):
        p = sub.add_parser(action); p.add_argument('--sha', required=True)
        if action == 'publish': p.add_argument('--output', required=True)
    args = parser.parse_args()
    PRODUCT = args.product
    if args.action == 'package': package(args.goos, args.goarch, args.output, args.allow_dirty)
    elif args.action == 'verify': print(json.dumps(verify_archive(args.archive, args.version, args.sha, args.extract, args.require_clean)))
    elif args.action == 'plan': plan(args.sha)
    else: publish(args.sha, args.output)
