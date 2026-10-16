"""Package a native GPUI executable. Only explicitly listed files enter the ZIP."""
import argparse
import hashlib
import plistlib
from pathlib import Path
import shutil
import zipfile

ROOT = Path(__file__).resolve().parents[1]


def package(binary, label, output):
    folder = output / f'MailDesk-Native-{label}'
    folder.mkdir(parents=True, exist_ok=True)
    files = []
    if label.startswith('macOS'):
        contents = folder / 'MailDesk.app' / 'Contents'
        executable = contents / 'MacOS' / 'MailDesk'
        resources = contents / 'Resources'
        resources.mkdir(parents=True, exist_ok=True)
        info = {'CFBundleExecutable': 'MailDesk', 'CFBundleIdentifier': 'local.maildesk.app',
                'CFBundleName': 'MailDesk', 'CFBundleDisplayName': 'MailDesk',
                'CFBundleShortVersionString': '2.0.0', 'CFBundleVersion': '4',
                'CFBundlePackageType': 'APPL', 'CFBundleIconFile': 'MailDesk.icns',
                'LSMinimumSystemVersion': '12.0', 'NSHighResolutionCapable': True}
        (contents / 'Info.plist').write_bytes(plistlib.dumps(info))
        shutil.copyfile(ROOT / 'assets/maildesk.icns', resources / 'MailDesk.icns')
        files += [contents / 'Info.plist', resources / 'MailDesk.icns']
    else:
        executable = folder / ('MailDesk.exe' if label.startswith('Windows') else 'maildesk')
    executable.parent.mkdir(parents=True, exist_ok=True)
    shutil.copyfile(binary, executable)
    executable.chmod(0o755)
    shutil.copyfile(ROOT / 'NATIVE.md', folder / 'ИНСТРУКЦИЯ.md')
    shutil.copyfile(ROOT / 'assets/maildesk.png', folder / 'maildesk.png')
    example = folder / 'Пример.csv'
    example.write_text('Company,Email,Overview\nКомпания без описания,,\nКомпания с описанием,,Производство вентиляционных установок\n', encoding='utf-8-sig')
    files += [executable, folder / 'ИНСТРУКЦИЯ.md', folder / 'maildesk.png', example]
    if label.startswith('Linux'):
        desktop = folder / 'MailDesk.desktop'
        desktop.write_text('[Desktop Entry]\nType=Application\nName=MailDesk\nExec=maildesk\nIcon=local.maildesk.app\nCategories=Office;Email;\nTerminal=false\n', encoding='utf-8')
        files.append(desktop)
    archive = output / f'{folder.name}.zip'
    with zipfile.ZipFile(archive, 'w', zipfile.ZIP_DEFLATED) as z:
        for file in sorted(files):
            z.write(file, str(Path(folder.name) / file.relative_to(folder)))
    print(archive.name)
    return archive


def checksums(output):
    archives = sorted(output.glob('MailDesk-*.zip'))
    (output / 'SHA256SUMS.txt').write_text(''.join(f'{hashlib.sha256(p.read_bytes()).hexdigest()}  {p.name}\n' for p in archives))


if __name__ == '__main__':
    parser = argparse.ArgumentParser()
    parser.add_argument('--binary', type=Path)
    parser.add_argument('--label')
    parser.add_argument('--output', type=Path, default=ROOT / 'dist')
    parser.add_argument('--checksums-only', action='store_true')
    args = parser.parse_args()
    args.output.mkdir(parents=True, exist_ok=True)
    if not args.checksums_only:
        if not args.binary or not args.label:
            parser.error('--binary and --label are required')
        package(args.binary, args.label, args.output)
    checksums(args.output)
