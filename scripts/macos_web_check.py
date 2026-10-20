"""Exercise the exact macOS installer/update flow with disposable profiles."""
import http.server
import json
import os
from pathlib import Path
import subprocess
import tempfile
import threading
import time
import traceback
import urllib.request


def smoke(archive, report_path=None):
    report = {'checks': [], 'sourceGatekeeper': {}}
    client = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    environment = os.environ.copy()
    for key in ('SMTP_USERNAME', 'SMTP_PASSWORD', 'SMTP_FROM_EMAIL', 'LLM_API_KEY'):
        environment.pop(key, None)

    def passed(message):
        report['checks'].append(message)
        print('::notice::' + message, flush=True)

    def failure(message):
        escaped = message.replace('%', '%25').replace('\r', '%0D').replace('\n', '%0A')
        print('::error::' + escaped, flush=True)

    def state(profile):
        instance = json.loads((profile / 'instance.json').read_text())
        origin = f'http://127.0.0.1:{instance["port"]}'
        headers = {'Cookie':'maildesk=' + instance['session'], 'Origin':origin}
        with client.open(urllib.request.Request(origin + '/api/state', headers=headers), timeout=3) as response:
            result = json.load(response)
        assert result['version'] == '1.1.5'
        assert result['testRecipient'] == 'ddkritsin@gmail.com'
        return result, origin, headers

    def post(profile, path, data, content_type='application/json'):
        _, origin, headers = state(profile)
        request = urllib.request.Request(origin + path, data=data,
            headers={**headers, 'Content-Type':content_type}, method='POST')
        with client.open(request, timeout=5) as response:
            return json.load(response)

    def command(args, success=True, timeout=60):
        result = subprocess.run(args, input='\n', text=True, env=environment,
            encoding='utf-8', errors='backslashreplace', capture_output=True, timeout=timeout)
        if (result.returncode == 0) != success:
            failure(result.stdout + '\n' + result.stderr)
            raise AssertionError(f'Unexpected installer status: {result.returncode}')
        return result

    with tempfile.TemporaryDirectory(prefix='maildesk-install-check-') as temporary:
        folder = Path(temporary) / 'Downloaded MailDesk'
        folder.mkdir()
        apps = folder / 'User Applications'
        profile = folder / 'profile'
        app_args = ['--no-browser', '--port', '0', '--data-dir', str(profile),
            '--test-recipient', 'ddkritsin@gmail.com']
        installed = apps / 'MailDesk Web.app'
        binary = installed / 'Contents/MacOS/MailDesk'
        busy_server = None
        try:
            subprocess.run(['ditto', '-x', '-k', str(archive), str(folder)], check=True)
            source = folder / archive.stem / 'MailDesk.app'
            installer = source.parent / 'Установить MailDesk.command'
            quarantine = f'0083;{int(time.time()):x};MailDesk CI;00000000-0000-4000-8000-000000000001'
            # Quarantine is applied BEFORE the first execution of any binary.
            # Assessment is reported separately from functional installation.
            for path in (source, source / 'Contents/MacOS/MailDesk', installer, archive):
                subprocess.run(['xattr', '-w', 'com.apple.quarantine', quarantine, str(path)], check=True)
            subprocess.run(['codesign', '--verify', '--deep', '--strict', str(source)], check=True)
            assessment = subprocess.run(['spctl', '--assess', '--type', 'execute', '--verbose=4', str(source)],
                capture_output=True, text=True, timeout=30)
            policy = subprocess.run(['spctl', '--status'], capture_output=True, text=True, timeout=10)
            report['sourceGatekeeper'] = {'exitCode':assessment.returncode,
                'output':(assessment.stdout + assessment.stderr).strip(), 'policy':policy.stdout.strip()}
            print('::notice::Fresh downloaded source Gatekeeper assessment: ' +
                json.dumps(report['sourceGatekeeper']), flush=True)
            install_args = ['--install-dir', str(apps)] + app_args
            result = command(['/bin/sh', str(installer)] + install_args)
            first, _, _ = state(profile)
            assert first['project']['leads'] == []
            assert 'MailDesk 1.1.5 установлен' in result.stdout
            assert subprocess.run(['xattr', '-p', 'com.apple.quarantine', str(installed)], capture_output=True).returncode != 0
            assert subprocess.run(['xattr', '-p', 'com.apple.quarantine', str(source)], capture_output=True).returncode == 0
            subprocess.run(['codesign', '--verify', '--deep', '--strict', str(installed)], check=True)
            helper = apps / 'Обновить MailDesk.command'
            assert helper.read_bytes() == (installed / 'Contents/Resources/Install.command').read_bytes()
            assert subprocess.run(['xattr', '-p', 'com.apple.quarantine', str(helper)], capture_output=True).returncode != 0
            passed('Fresh quarantined ZIP: installer verified the bundle, installed a separate copy and launched the actual web server through Launch Services.')

            # Populate a real project through the importer; never use real SMTP.
            fixture = 'Name,Email,Company Overview\nТест установки,ddkritsin@gmail.com,Производит вентиляционные клапаны.\n'
            body = ('--fixture\r\nContent-Disposition: form-data; name="file"; filename="company.csv"\r\n'
                'Content-Type: text/csv\r\n\r\n' + fixture + '\r\n--fixture--\r\n').encode()
            assert post(profile, '/api/import', body, 'multipart/form-data; boundary=fixture')['added'] == 1
            current, _, _ = state(profile)
            lead = current['project']['leads'][0]
            post(profile, '/api/lead', json.dumps({'id':lead['id'], 'company':lead['company'],
                'email':lead['email'], 'subject':'Сохранённый черновик', 'body':'Не отправлять: локальный тест.'}).encode())
            # Import a synthetic sent record, then use the real restore API.
            # No SMTP connection is made; restored state must survive the update.
            current, _, _ = state(profile)
            sent_project = current['project']
            sent_project['leads'][0].update(status='sent', sentAt='2026-10-19T12:00:00Z', messageId='installer-fixture@example.test')
            sent_project['sent'] = {lead['email']:'2026-10-19T12:00:00Z'}
            body = (b'--fixture\r\nContent-Disposition: form-data; name="file"; filename="sent.json"\r\n'
                b'Content-Type: application/json\r\n\r\n' + json.dumps(sent_project).encode() + b'\r\n--fixture--\r\n')
            post(profile, '/api/import', body, 'multipart/form-data; boundary=fixture')
            post(profile, '/api/restore', json.dumps({'id':lead['id']}).encode())
            current, _, _ = state(profile)
            restored = current['project']['leads'][0]
            assert restored['status'] == 'pending' and restored['resend']['email'] == lead['email']
            assert restored['sendHistory'][0]['messageId'] == 'installer-fixture@example.test'
            assert restored['body'] == 'Не отправлять: локальный тест.'
            passed('Sent-message restore: synthetic accepted delivery returned to pending with its draft and delivery history intact; no SMTP connection.')
            saved_project = json.loads((profile / 'project.json').read_text())
            saved_credentials = (profile / 'credentials.json').read_bytes()

            # Close and reopen exactly as a user opens the installed .app.
            command([str(binary), '--prepare-update'] + app_args)
            command(['open', '-n', str(installed), '--args'] + app_args)
            command([str(binary), '--wait-ready'] + app_args)
            assert json.loads((profile / 'project.json').read_text()) == saved_project
            passed('Installed Finder launch: stopped the server, reopened the installed .app through Launch Services, received authenticated state with the saved project.')

            old_session = json.loads((profile / 'instance.json').read_text())['session']
            old_inode = installed.stat().st_ino
            command(['/bin/sh', str(helper), '--archive', str(archive)] + app_args)
            assert installed.stat().st_ino != old_inode
            assert json.loads((profile / 'instance.json').read_text())['session'] != old_session
            assert json.loads((profile / 'project.json').read_text()) == saved_project
            assert (profile / 'credentials.json').read_bytes() == saved_credentials
            backups = list(apps.glob('.maildesk-backup.*/MailDesk Web.app'))
            assert len(backups) == 1
            passed('Update from local ZIP: existing idle instance closed, app replaced, backup retained, new instance launched; project and credentials preserved exactly.')

            class BusyHandler(http.server.BaseHTTPRequestHandler):
                def log_message(self, *args):
                    pass

                def do_GET(self):
                    self.send_response(200)
                    self.end_headers()
                    self.wfile.write(b'{"sending":true,"generating":false}')

                def do_POST(self):
                    raise AssertionError('Installer attempted to interrupt an active operation')

            busy_server = http.server.ThreadingHTTPServer(('127.0.0.1', 0), BusyHandler)
            threading.Thread(target=busy_server.serve_forever, daemon=True).start()
            busy_profile = folder / 'busy-profile'
            busy_profile.mkdir()
            (busy_profile / 'instance.json').write_text(json.dumps({'port':busy_server.server_port,'session':'busy-fixture'}))
            inode = installed.stat().st_ino
            refused = command(['/bin/sh', str(helper), '--archive', str(archive), '--data-dir',
                str(busy_profile), '--test-recipient', 'ddkritsin@gmail.com'], success=False)
            assert 'идёт отправка или генерация' in refused.stderr
            assert installed.stat().st_ino == inode and len(list(apps.glob('.maildesk-backup.*/MailDesk Web.app'))) == 1
            state(profile)
            passed('Active operation refused: no quit request, no installed app replacement, existing server left running.')

            # A startup failure must restore the previous app, without resetting data.
            broken_profile = folder / 'broken-profile'
            broken_profile.mkdir()
            original = b'{invalid-project'
            (broken_profile / 'project.json').write_bytes(original)
            rejected = command(['/bin/sh', str(helper), '--archive', str(archive), '--no-browser',
                '--data-dir', str(broken_profile), '--test-recipient', 'ddkritsin@gmail.com'], success=False)
            assert 'Предыдущая версия восстановлена' in rejected.stdout
            assert installed.stat().st_ino == inode
            assert (broken_profile / 'project.json').read_bytes() == original
            assert json.loads((profile / 'project.json').read_text()) == saved_project
            state(profile)
            passed('Startup failure rolled back: prior installed app restored; original project bytes unchanged; existing separate test profile still healthy.')

            # Corruption must be rejected before permission changes or shutdown.
            icon = source / 'Contents/Resources/MailDesk.icns'
            icon.write_bytes(icon.read_bytes() + b'corrupted-fixture')
            refused = command(['/bin/sh', str(installer)] + install_args, success=False)
            assert 'Проверка подписи не пройдена' in refused.stderr
            assert installed.stat().st_ino == inode
            assert subprocess.run(['xattr', '-p', 'com.apple.quarantine', str(source)], capture_output=True).returncode == 0
            state(profile)
            passed('Corrupted signed resource rejected before permission changes, existing server shutdown or installed file replacement.')
            report['result'] = 'passed'
        except Exception as error:
            report['result'] = 'failed'
            report['error'] = str(error)
            failure(traceback.format_exc())
            raise
        finally:
            if busy_server:
                busy_server.shutdown()
                busy_server.server_close()
            try:
                post(profile, '/api/quit', b'{}')
            except (OSError, ValueError, AssertionError):
                pass
            if report_path:
                report_path.parent.mkdir(parents=True, exist_ok=True)
                report_path.write_text(json.dumps(report, ensure_ascii=False, indent=2))
