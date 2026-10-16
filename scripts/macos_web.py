"""Seal and verify the browser application's macOS distribution, then test it."""
import argparse
from pathlib import Path
import plistlib
import shutil
import subprocess
import sys
import tempfile
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
        info.update(CFBundleIdentifier='local.maildesk.web', CFBundleVersion='6',
                    CFBundleInfoDictionaryVersion='6.0', CFBundleIconFile='MailDesk.icns', LSUIElement=True)
        info_path.write_bytes(plistlib.dumps(info))
        icon = contents / 'Resources/MailDesk.icns'
        icon.parent.mkdir(exist_ok=True)
        shutil.copyfile(ROOT / 'assets/maildesk.icns', icon)
        pkg_info = contents / 'PkgInfo'
        pkg_info.write_bytes(b'APPL????')
        executable.chmod(0o755)
        launcher = package / 'Установить MailDesk.command'
        launcher.chmod(0o755)
        installer = contents / 'Resources/Install.command'
        shutil.copyfile(ROOT / 'scripts/macos_web_install.command', installer)
        installer.chmod(0o755)
        subprocess.run(['codesign', '--force', '--sign', '-', '--timestamp=none',
                        '--identifier', info['CFBundleIdentifier'], str(app)], check=True)
        subprocess.run(['codesign', '--verify', '--deep', '--strict', '--verbose=2', str(app)], check=True)
        # Explicit publication list: never archive arbitrary adjacent files.
        files = [info_path, executable, icon, installer, pkg_info, contents / '_CodeSignature/CodeResources',
                 launcher, package / 'ИНСТРУКЦИЯ.md', package / 'Пример.csv']
        output = folder / archive.name
        with zipfile.ZipFile(output, 'w', zipfile.ZIP_DEFLATED) as z:
            for file in sorted(files):
                z.write(file, str(Path(package.name) / file.relative_to(package)))
        shutil.copyfile(output, archive)

if __name__ == '__main__':
    parser = argparse.ArgumentParser()
    parser.add_argument('archive', type=Path)
    parser.add_argument('--smoke', action='store_true')
    parser.add_argument('--report', type=Path)
    args = parser.parse_args()
    if args.smoke:
        from macos_web_check import smoke
        smoke(args.archive.resolve(), args.report)
    else:
        seal_archive(args.archive.resolve())
