"""Cross-build standalone distributions; does not include user data or secrets."""
from concurrent.futures import ThreadPoolExecutor
import hashlib
import os
from pathlib import Path
import plistlib
import shutil
import stat
import subprocess
import zipfile

ROOT=Path(__file__).resolve().parents[1]
DIST=ROOT/'dist'
TARGETS=[('windows','amd64','Windows-x64'),('windows','arm64','Windows-ARM64'),('darwin','arm64','macOS-AppleSilicon'),('darwin','amd64','macOS-Intel'),('linux','amd64','Linux-x64')]


def build(target):
    system,arch,label=target
    folder=DIST/f'MailDesk-{label}'
    folder.mkdir(parents=True,exist_ok=True)
    if system=='darwin':
        contents=folder/'MailDesk.app'/'Contents'
        binary=contents/'MacOS'/'MailDesk'
        binary.parent.mkdir(parents=True,exist_ok=True)
        info={'CFBundleExecutable':'MailDesk','CFBundleIdentifier':'local.maildesk.app','CFBundleName':'MailDesk','CFBundleDisplayName':'MailDesk','CFBundleShortVersionString':'1.0.0','CFBundleVersion':'1','CFBundlePackageType':'APPL','LSMinimumSystemVersion':'12.0'}
        (contents/'Info.plist').write_bytes(plistlib.dumps(info))
        launcher=folder/'Запустить MailDesk.command'
        launcher.write_text('#!/bin/sh\ncd "$(dirname "$0")"\nexec ./MailDesk.app/Contents/MacOS/MailDesk\n')
        launcher.chmod(0o755)
    else:
        binary=folder/('MailDesk.exe' if system=='windows' else 'maildesk')
    environment=dict(os.environ,GOOS=system,GOARCH=arch,CGO_ENABLED='0',GOMAXPROCS='3')
    subprocess.run([os.environ.get('MAILDESK_GO','go'),'build','-p','3','-trimpath','-ldflags=-s -w','-o',str(binary),'.'],cwd=ROOT,env=environment,check=True)
    binary.chmod(0o755)
    shutil.copyfile(ROOT/'README.md',folder/'ИНСТРУКЦИЯ.md')
    (folder/'Пример.csv').write_text('Company,Email,Overview\nКомпания без описания,,\nКомпания с описанием,,Производство вентиляционного оборудования\n',encoding='utf-8-sig')
    archive=DIST/f'MailDesk-{label}.zip'
    with zipfile.ZipFile(archive,'w',zipfile.ZIP_DEFLATED) as z:
        for file in sorted(folder.rglob('*')):
            if file.is_file():z.write(file,Path(folder.name)/file.relative_to(folder))
    print(f'Built {archive.name}',flush=True)
    return archive


def main():
    DIST.mkdir(exist_ok=True)
    with ThreadPoolExecutor(max_workers=2) as pool:
        archives=list(pool.map(build,TARGETS))
    (DIST/'SHA256SUMS.txt').write_text(''.join(f'{hashlib.sha256(p.read_bytes()).hexdigest()}  {p.name}\n' for p in archives))


if __name__=='__main__':main()
