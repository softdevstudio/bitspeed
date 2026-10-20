import io
import json
from pathlib import Path
import tarfile
import tempfile
import unittest
from unittest.mock import patch
import release_inspect as release


class ReleaseGuards(unittest.TestCase):
    def test_asset_names(self):
        self.assertEqual(len({release.archive_name('inspect', *t) for t in release.TARGETS}), 5)
        self.assertRaises(ValueError, release.archive_name, 'inspect', 'linux', 'unknown')

    def test_archive_paths(self):
        for name in ('../escape', '/absolute', 'a/../../b', 'C:/windows', 'a\\b'):
            with self.subTest(name=name):
                self.assertRaises(ValueError, release.safe_member, name)

    def test_checksums(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / 'SHA256SUMS'
            path.write_text('a' * 64 + '  archive.tar.gz\n')
            self.assertEqual(release.checksum_map(path)['archive.tar.gz'], 'a' * 64)
            path.write_text('a' * 64 + '  ../escape\n')
            self.assertRaises(ValueError, release.checksum_map, path)

    def make_archive(self, path, sha='a' * 40, dirty=False, unsafe=False):
        info = dict(product='bitspeed-inspect', version='0.1.0', source_sha=sha, dirty=dirty, goos='linux', goarch='amd64')
        files = {name: b'fixture' for name in ('bitspeed-inspect', 'LICENSE', 'NOTICE', 'README.md', 'THIRD_PARTY_LICENSES.md', 'third_party/go/LICENSE', 'third_party/go/PATENTS')}
        files['BUILD_INFO.json'] = json.dumps(info).encode()
        if unsafe:
            files['../escape'] = b'bad'
        with tarfile.open(path, 'w:gz') as archive:
            for name, data in files.items():
                member = tarfile.TarInfo(name); member.size = len(data)
                archive.addfile(member, io.BytesIO(data))

    def test_archive_provenance(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / release.archive_name('inspect', 'linux', 'amd64')
            self.make_archive(path)
            self.assertEqual(release.verify_archive(path, '0.1.0', 'a' * 40)['version'], '0.1.0')
            self.assertRaises(ValueError, release.verify_archive, path, '0.1.0', 'b' * 40)
            self.make_archive(path, dirty=True)
            self.assertRaises(ValueError, release.verify_archive, path, '0.1.0', 'a' * 40, require_clean=True)
            self.make_archive(path, unsafe=True)
            self.assertRaises(ValueError, release.verify_archive, path, '0.1.0', 'a' * 40)

    def test_exact_ci_and_main_gate(self):
        sha = 'a' * 40
        run = dict(conclusion='success', head_sha=sha, head_branch='main', event='push', repository=dict(full_name=release.REPO))
        with patch.dict('os.environ', {'CI_RUN_ID': '123'}):
            with patch.object(release, 'api', side_effect=[run, {'object': {'sha': sha}}]):
                release.verify_ci(sha)
            for field, value in [('conclusion', 'failure'), ('head_sha', 'b' * 40), ('event', 'pull_request'), ('head_branch', 'other')]:
                bad = dict(run); bad[field] = value
                with patch.object(release, 'api', return_value=bad):
                    self.assertRaises(ValueError, release.verify_ci, sha)
            with patch.object(release, 'api', side_effect=[run, {'object': {'sha': 'b' * 40}}]):
                self.assertRaises(ValueError, release.verify_ci, sha)

    def test_draft_discovery_and_pagination(self):
        draft = {'tag_name': 'inspect/v0.1.0', 'draft': True}
        first = [{'tag_name': 'other/v' + str(i)} for i in range(100)]
        with patch.object(release, 'api', side_effect=[first, [draft]]) as api:
            self.assertEqual(release.find_release('inspect/v0.1.0'), draft)
            self.assertEqual(api.call_args_list[1].args[0], 'releases?per_page=100&page=2')
        with patch.object(release, 'api', return_value=[]):
            self.assertIsNone(release.find_release('inspect/v9.0.0'))

    def test_publisher_rejects_missing_native_gate_before_upload(self):
        with patch.dict('os.environ', {'GITHUB_RUN_ID': '456'}), patch.object(release, 'verify_ci'), patch.object(release, 'command', return_value='a' * 40), patch.object(release, 'version', return_value='0.1.0'), patch.object(release, 'api', side_effect=[[], {'jobs': []}]), patch.object(release.subprocess, 'run') as run:
            self.assertRaisesRegex(ValueError, 'gates', release.publish, 'a' * 40, '.')
            run.assert_not_called()

    def test_publisher_draft_retry_and_corrupt_upload(self):
        import shutil
        sha = 'a' * 40
        jobs = [{'name': prefix + '-' + osname + '-' + arch, 'conclusion': 'success'} for prefix, targets in [('package', release.TARGETS), ('native', release.NATIVE)] for osname, arch in targets]
        for corrupt in (False, True):
            with self.subTest(corrupt=corrupt), tempfile.TemporaryDirectory() as directory:
                output = Path(directory)
                for target in release.TARGETS:
                    (output / release.archive_name('inspect', *target)).write_bytes(b'archive')
                calls = []
                def run(args, **kwargs):
                    calls.append(args)
                    if args[:3] == ['gh', 'release', 'download']:
                        destination = Path(args[args.index('--dir') + 1])
                        for path in output.iterdir():
                            shutil.copyfile(path, destination / path.name)
                        if corrupt:
                            (destination / 'SHA256SUMS').write_text('corruption')
                def api(path, **kwargs):
                    if path.startswith('actions/'):
                        return {'jobs': jobs}
                    if path.startswith('git/'):
                        return {'object': {'sha': sha}}
                    return [{'tag_name': 'inspect/v0.1.0', 'draft': True, 'assets': [{'name': p.name} for p in output.iterdir()]}]
                with patch.dict('os.environ', {'GITHUB_RUN_ID': '456', 'CI_RUN_ID': '123'}), patch.object(release, 'verify_ci'), patch.object(release, 'command', return_value=sha), patch.object(release, 'version', return_value='0.1.0'), patch.object(release, 'api', side_effect=api), patch.object(release, 'verify_archive'), patch.object(release, 'carry_transfer'), patch.object(release.subprocess, 'run', side_effect=run):
                    if corrupt:
                        self.assertRaisesRegex(ValueError, 'integrity', release.publish, sha, output)
                    else:
                        release.publish(sha, output)
                self.assertFalse(any(c[:3] == ['gh', 'release', 'create'] for c in calls))
                self.assertEqual(any(c[:3] == ['gh', 'release', 'edit'] for c in calls), not corrupt)

    def test_transfer_carry_requires_complete_verified_assets(self):
        names = {release.archive_name('transfer', *t) for t in release.TARGETS}
        with tempfile.TemporaryDirectory() as directory:
            output = Path(directory)
            with patch.object(release, 'api', return_value={'assets': [{'name': next(iter(names))}]}):
                self.assertRaisesRegex(ValueError, 'incomplete', release.carry_transfer, output)
            def download(args, **kwargs):
                destination = Path(args[args.index('--dir') + 1])
                for name in names:
                    (destination / name).write_bytes(b'verified transfer')
                (destination / 'SHA256SUMS').write_text(''.join(release.hash_file(destination / name) + '  ' + name + '\n' for name in sorted(names)))
            latest = {'tag_name': 'transfer/v0.1.0', 'assets': [{'name': n} for n in names | {'SHA256SUMS'}]}
            with patch.object(release, 'api', return_value=latest), patch.object(release.subprocess, 'run', side_effect=download):
                release.carry_transfer(output)
            self.assertEqual({p.name for p in output.iterdir()}, names)


if __name__ == '__main__':
    unittest.main()
