"""Verify the distributed .app, then launch it through Launch Services.

Uses only Python and macOS/Xcode tools already present on the native runner.
Never reads the user's MailDesk profile or contacts an email provider.
"""
import argparse
import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import tempfile
import time
import zipfile

PROBE = r'''
import CoreGraphics
import Foundation
import Metal
let info = CGWindowListCopyWindowInfo([.optionOnScreenOnly, .excludeDesktopElements], kCGNullWindowID) as? [[String: Any]] ?? []
let windows = info.compactMap { item -> [String: Any]? in
    guard let owner = item[kCGWindowOwnerName as String] as? String,
          owner == "MailDesk", let pid = item[kCGWindowOwnerPID as String] as? Int,
          let layer = item[kCGWindowLayer as String] as? Int, layer == 0 else { return nil }
    return ["pid": pid, "bounds": item[kCGWindowBounds as String] ?? [:]]
}
let report: [String: Any] = ["metal": MTLCreateSystemDefaultDevice() != nil,
    "display": CGMainDisplayID() != 0, "windows": windows]
let data = try JSONSerialization.data(withJSONObject: report, options: [.sortedKeys])
print(String(data: data, encoding: .utf8)!)
'''


def run(archive, report):
    if sys.platform != 'darwin':
        raise RuntimeError('This check must run on macOS')
    with tempfile.TemporaryDirectory(prefix='maildesk-macos-smoke-') as tmp:
        folder = Path(tmp)
        with zipfile.ZipFile(archive) as z:
            binaries = [n for n in z.namelist() if n.endswith('/Contents/MacOS/MailDesk')]
            assert len(binaries) == 1, 'Expected one app executable'
            assert z.getinfo(binaries[0]).external_attr >> 16 & 0o111, 'Executable bit missing in ZIP'
        subprocess.run(['ditto', '-x', '-k', str(archive), str(folder)], check=True)
        app = next(folder.glob('*/MailDesk.app'))
        executable = app / 'Contents/MacOS/MailDesk'
        assert os.access(executable, os.X_OK), 'Extracted executable is not executable'
        subprocess.run(['plutil', '-lint', str(app / 'Contents/Info.plist')], check=True)
        subprocess.run(['codesign', '--verify', '--deep', '--strict', '--verbose=2', str(app)], check=True)
        assert (app / 'Contents/_CodeSignature/CodeResources').is_file()
        source = folder / 'probe.swift'
        source.write_text(PROBE)
        probe = folder / 'probe'
        subprocess.run(['xcrun', 'swiftc', str(source), '-o', str(probe)], check=True, timeout=120)

        def window_state():
            return json.loads(subprocess.check_output([str(probe)], text=True, timeout=15))

        capability = window_state()
        result = {'archive': archive.name, 'signature': 'valid ad-hoc bundle',
                  'permissions': 'valid', 'metal': capability['metal'], 'display': capability['display']}
        if not capability['metal'] or not capability['display']:
            result['launch'] = 'unavailable: runner has no Metal device or WindowServer display'
            report.write_text(json.dumps(result, indent=2) + '\n')
            print('::warning::macOS bundle signature verified; GUI launch unavailable on this runner (no Metal or display).')
            return

        # No account bindings or user profiles are inherited by the app fixture.
        env = os.environ.copy()
        for key in ['SMTP_USERNAME', 'SMTP_FROM_EMAIL', 'SMTP_PASSWORD', 'LLM_API_KEY']:
            env.pop(key, None)
        profile = folder / 'fixture-profile'
        log_path = report.with_suffix('.log')
        pids = set()
        with log_path.open('w') as log:
            process = subprocess.Popen(['open', '-n', '-W', str(app), '--args',
                '--data-dir', str(profile), '--test-recipient', 'ddkritsin@gmail.com'],
                env=env, stdout=log, stderr=log)
            try:
                deadline = time.monotonic() + 30
                while time.monotonic() < deadline:
                    if process.poll() is not None:
                        raise RuntimeError(f'Launch Services exited early ({process.returncode}): {log_path.read_text()}')
                    for line in subprocess.check_output(['ps', '-axo', 'pid=,command='], text=True).splitlines():
                        if str(executable) in line:
                            pids.add(int(line.strip().split(None, 1)[0]))
                    windows = window_state()['windows']
                    if any(w['pid'] in pids for w in windows):
                        break
                    time.sleep(0.5)
                else:
                    raise RuntimeError(f'MailDesk did not open a visible window: {log_path.read_text()}')
                time.sleep(3)
                assert process.poll() is None, f'App closed after startup: {log_path.read_text()}'
                assert any(w['pid'] in pids for w in window_state()['windows']), 'App window disappeared'
                project = json.loads((profile / 'project.json').read_text())
                assert project['version'] == 2 and project['leads'] == []
                result['launch'] = 'passed: Launch Services, visible window, persistent empty fixture'
                report.write_text(json.dumps(result, indent=2) + '\n')
                print('::notice::macOS ZIP extracted, bundle signature verified, app launched through Launch Services with a visible window.')
            finally:
                # Only processes from this temporary application are stopped.
                for pid in pids:
                    try:
                        os.kill(pid, signal.SIGTERM)
                    except ProcessLookupError:
                        pass
                try:
                    process.wait(timeout=5)
                except subprocess.TimeoutExpired:
                    process.terminate()
                    process.wait(timeout=5)


if __name__ == '__main__':
    parser = argparse.ArgumentParser()
    parser.add_argument('archive', type=Path)
    parser.add_argument('--report', type=Path, required=True)
    args = parser.parse_args()
    run(args.archive.resolve(), args.report.resolve())
