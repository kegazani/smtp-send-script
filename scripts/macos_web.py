"""Seal and verify the browser application's macOS distribution, then test it."""
import argparse
import json
import os
from pathlib import Path
import plistlib
import shutil
import signal
import subprocess
import sys
import tempfile
import time
import urllib.request
import zipfile

ROOT = Path(__file__).resolve().parents[1]


def seal_archive(archive):
    if sys.platform != 'darwin':
        raise RuntimeError('Sealing a macOS app requires macOS codesign')
    with tempfile.TemporaryDirectory(prefix='maildesk-web-seal-') as tmp:
        folder = Path(tmp)
        subprocess.run(['ditto', '-x', '-k', str(archive), str(folder)], check=True)
        package = folder / archive.stem
        app = package / 'MailDesk.app'
        contents = app / 'Contents'
        executable = contents / 'MacOS/MailDesk'
        info_path = contents / 'Info.plist'
        info = plistlib.loads(info_path.read_bytes())
        # Distinct application identities keep Launch Services from confusing
        # the background web server with the native GPUI window.
        info.update(CFBundleIdentifier='local.maildesk.web', CFBundleVersion='4',
                    CFBundleInfoDictionaryVersion='6.0', CFBundleIconFile='MailDesk.icns', LSUIElement=True)
        info_path.write_bytes(plistlib.dumps(info))
        icon = contents / 'Resources/MailDesk.icns'
        icon.parent.mkdir(exist_ok=True)
        shutil.copyfile(ROOT / 'assets/maildesk.icns', icon)
        pkg_info = contents / 'PkgInfo'
        pkg_info.write_bytes(b'APPL????')
        executable.chmod(0o755)
        launcher = package / 'Запустить MailDesk.command'
        launcher.chmod(0o755)
        subprocess.run(['codesign', '--force', '--sign', '-', '--timestamp=none',
                        '--identifier', info['CFBundleIdentifier'], str(app)], check=True)
        subprocess.run(['codesign', '--verify', '--deep', '--strict', '--verbose=2', str(app)], check=True)
        # Explicit publication list: never archive arbitrary adjacent files.
        files = [info_path, executable, icon, pkg_info, contents / '_CodeSignature/CodeResources',
                 launcher, package / 'ИНСТРУКЦИЯ.md', package / 'Пример.csv']
        output = folder / archive.name
        with zipfile.ZipFile(output, 'w', zipfile.ZIP_DEFLATED) as z:
            for file in sorted(files):
                z.write(file, str(Path(package.name) / file.relative_to(package)))
        shutil.copyfile(output, archive)


def check_server(command, profile, env, client):
    instance = origin = headers = None
    log_path = profile.parent / (profile.name + '.log')
    with log_path.open('w') as log:
        process = subprocess.Popen(command + ['--no-browser', '--port', '0',
            '--data-dir', str(profile), '--test-recipient', 'ddkritsin@gmail.com'],
            env=env, stdin=subprocess.PIPE, stdout=log, stderr=log, start_new_session=True)
        # Accept the scoped first-launch prompt in this disposable CI fixture.
        process.stdin.write(b'\n')
        process.stdin.close()
        try:
            deadline = time.monotonic() + 20
            while time.monotonic() < deadline:
                if process.poll() is not None:
                    raise RuntimeError(f'Web app exited before becoming ready ({process.returncode})')
                try:
                    instance = json.loads((profile / 'instance.json').read_text())
                    origin = f'http://127.0.0.1:{instance["port"]}'
                    headers = {'Cookie': 'maildesk=' + instance['session'], 'Origin': origin}
                    request = urllib.request.Request(origin + '/api/state', headers=headers)
                    state = json.load(client.open(request, timeout=2))
                    assert state['project']['leads'] == []
                    assert state['testRecipient'] == 'ddkritsin@gmail.com'
                    break
                except (OSError, ValueError):
                    time.sleep(0.2)
            else:
                raise RuntimeError('Web app did not serve an authenticated state request')
        except Exception:
            log.flush()
            print(log_path.read_text(), flush=True)
            raise
        finally:
            if instance and origin and headers:
                request = urllib.request.Request(origin + '/api/quit', data=b'{}',
                    headers={**headers, 'Content-Type': 'application/json'}, method='POST')
                try:
                    client.open(request, timeout=3).close()
                except OSError:
                    pass
            try:
                process.wait(timeout=5)
            except subprocess.TimeoutExpired:
                os.killpg(process.pid, signal.SIGTERM)
                process.wait(timeout=5)


def smoke(archive):
    if sys.platform != 'darwin':
        raise RuntimeError('Run this launch check on macOS')
    with tempfile.TemporaryDirectory(prefix='maildesk-web-launch-') as tmp:
        # Spaces exercise Terminal drag-and-drop quoting as well.
        folder = Path(tmp) / 'Downloaded MailDesk'
        folder.mkdir()
        subprocess.run(['ditto', '-x', '-k', str(archive), str(folder)], check=True)
        app = folder / archive.stem / 'MailDesk.app'
        launcher = app.parent / 'Запустить MailDesk.command'
        binary = app / 'Contents/MacOS/MailDesk'
        subprocess.run(['codesign', '--verify', '--deep', '--strict', '--verbose=2', str(app)], check=True)
        assert os.access(binary, os.X_OK)
        env = os.environ.copy()
        for key in ['SMTP_USERNAME', 'SMTP_PASSWORD', 'SMTP_FROM_EMAIL', 'LLM_API_KEY']:
            env.pop(key, None)
        client = urllib.request.build_opener(urllib.request.ProxyHandler({}))
        check_server(['open', '-n', '-W', str(app), '--args'], folder / 'launch-services-profile', env, client)
        print('::notice::macOS web ZIP extracted, bundle signature verified, Launch Services opened the server and authenticated local state was received.')

        quarantine = '0083;{:x};MailDesk CI;{}'.format(int(time.time()), '00000000-0000-4000-8000-000000000001')
        for path in (app, binary, launcher):
            subprocess.run(['xattr', '-w', 'com.apple.quarantine', quarantine, str(path)], check=True)
        check_server(['/bin/sh', str(launcher)], folder / 'downloaded-profile', env, client)
        for path in (app, binary):
            assert subprocess.run(['xattr', '-p', 'com.apple.quarantine', str(path)], capture_output=True).returncode != 0
        subprocess.run(['codesign', '--verify', '--deep', '--strict', str(app)], check=True)
        print('::notice::Downloaded-file quarantine fixture: Terminal launcher verified the bundle, removed only its quarantine, and served authenticated local state.')

        # Corrupt a sealed resource. The launcher must not remove quarantine,
        # re-sign the bundle, create a profile, or execute the server.
        icon = app / 'Contents/Resources/MailDesk.icns'
        icon.write_bytes(icon.read_bytes() + b'corrupted-ci-fixture')
        subprocess.run(['xattr', '-w', 'com.apple.quarantine', quarantine, str(app)], check=True)
        rejected_profile = folder / 'rejected-profile'
        rejected = subprocess.run(['/bin/sh', str(launcher), '--no-browser', '--data-dir',
            str(rejected_profile), '--test-recipient', 'ddkritsin@gmail.com'],
            input='\n', text=True, env=env, capture_output=True, timeout=10)
        assert rejected.returncode != 0, rejected.stdout
        assert not rejected_profile.exists()
        assert subprocess.run(['xattr', '-p', 'com.apple.quarantine', str(app)], capture_output=True).returncode == 0
        print('::notice::Modified bundle rejected before quarantine removal or server startup.')


if __name__ == '__main__':
    parser = argparse.ArgumentParser()
    parser.add_argument('archive', type=Path)
    parser.add_argument('--smoke', action='store_true')
    args = parser.parse_args()
    if args.smoke:
        smoke(args.archive.resolve())
    else:
        seal_archive(args.archive.resolve())
