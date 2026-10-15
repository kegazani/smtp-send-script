"""Install the verified Go toolchain used by this cloud workspace (Linux x64)."""
import hashlib
from pathlib import Path
import subprocess
import tarfile
import tempfile
import urllib.request

VERSION='go1.27.2'
FILENAME=VERSION+'.linux-amd64.tar.gz'
# Official go.dev/dl JSON SHA-256, retrieved during initial setup.
SHA256='ecbadb99091a3f46e31f5f934b068b1864eafa7995211b39eaddf76996045fe5'
ROOT=Path('/workspace/toolchains')
binary=ROOT/'go/bin/go'
if binary.exists():
    actual=subprocess.check_output([str(binary),'version'],text=True)
    if VERSION not in actual:raise SystemExit('Different existing toolchain; preserve it and select a separate installation path.')
    print(actual.strip())
else:
    ROOT.mkdir(parents=True,exist_ok=True)
    with tempfile.TemporaryDirectory() as tmp:
        archive=Path(tmp)/FILENAME
        urllib.request.urlretrieve('https://go.dev/dl/'+FILENAME,archive)
        if hashlib.sha256(archive.read_bytes()).hexdigest()!=SHA256:raise SystemExit('Official Go checksum mismatch')
        with tarfile.open(archive) as t:t.extractall(ROOT,filter='data')
    print('Go installed; official SHA-256 verified.')
