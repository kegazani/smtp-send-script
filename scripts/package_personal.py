"""Prepare private local archives from generic ZIPs and an existing local .env.

No credentials are included in source or Git artifacts. Output lives outside Git.
"""
import hashlib
from pathlib import Path
import shutil
import stat
import tempfile
import zipfile

ROOT=Path(__file__).resolve().parents[1]
LOCAL=Path('/workspace/smtp-send-script-local')
READY=LOCAL/'ready'


def main():
    variables={}
    for line in (LOCAL/'.env').read_text().splitlines():
        key,sep,value=line.partition('=')
        if sep and key!='LEADS_CSV':variables[key]=value
    secrets=[variables[key].encode() for key in ('SMTP_PASSWORD','LLM_API_KEY') if variables.get(key)]
    READY.mkdir(mode=0o700,exist_ok=True)
    previous={}
    manifest=READY/'SHA256SUMS.txt'
    if manifest.exists():
        for line in manifest.read_text().splitlines():
            digest,name=line.split(maxsplit=1);previous[name]=digest
    for archive in sorted((ROOT/'dist').glob('MailDesk-*.zip')):
        dest=READY/(archive.stem+'-Personal.zip')
        if dest.exists():
            digest=hashlib.sha256(dest.read_bytes()).hexdigest()
            if previous.get(dest.name)!=digest:
                raise SystemExit('Existing private archive has changed; preserve it before replacement: '+dest.name)
            backup=READY/'previous';backup.mkdir(mode=0o700,exist_ok=True)
            backup_file=backup/(dest.stem+'-'+digest[:12]+'.zip')
            if not backup_file.exists():shutil.copyfile(dest,backup_file);backup_file.chmod(0o600)
        with zipfile.ZipFile(archive) as source:
            if source.testzip() is not None:raise SystemExit('Generic archive is damaged')
            for info in source.infolist():
                if any(secret in source.read(info) for secret in secrets):raise SystemExit('Generic archive contains credentials')
            root=source.namelist()[0].split('/')[0]
            with tempfile.NamedTemporaryFile(dir=READY,suffix='.zip',delete=False) as tmp:temporary=Path(tmp.name)
            try:
                with zipfile.ZipFile(temporary,'w',zipfile.ZIP_DEFLATED) as target:
                    for info in source.infolist():target.writestr(info,source.read(info))
                    for name,data in [('maildesk.env',''.join(f'{k}={v}\n' for k,v in variables.items()).encode()),('companies.csv',(LOCAL/'companies.csv').read_bytes())]:
                        info=zipfile.ZipInfo(root+'/'+name);info.create_system=3;info.external_attr=(stat.S_IFREG|0o600)<<16;info.compress_type=zipfile.ZIP_DEFLATED;target.writestr(info,data)
                with zipfile.ZipFile(temporary) as z:
                    if z.testzip() is not None:raise SystemExit('Personal archive verification failed')
                temporary.replace(dest);dest.chmod(0o600)
            finally:temporary.unlink(missing_ok=True)
        print('Updated private local archive:',dest.name)
    manifest.write_text(''.join(f'{hashlib.sha256(p.read_bytes()).hexdigest()}  {p.name}\n' for p in sorted(READY.glob('*.zip'))))


if __name__=='__main__':main()
