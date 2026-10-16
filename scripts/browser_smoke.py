"""Optional developer E2E check: Chromium, local TLS SMTP; no Internet email.

Run from repository root after building dist/maildesk:
    python3 scripts/browser_smoke.py
Requires developer-only Playwright and OpenSSL, not needed by end users.
"""
import json
from email import policy
from email.parser import BytesParser
import os
from pathlib import Path
import socketserver
import ssl
import subprocess
import tempfile
import threading
import time

from playwright.sync_api import sync_playwright, expect

RECIPIENT = 'ddkritsin@gmail.com'
ROOT = Path(__file__).resolve().parents[1]


class SMTPHandler(socketserver.StreamRequestHandler):
    def handle(self):
        self.wfile.write(b'220 local-fixture ESMTP\r\n')
        while line := self.rfile.readline():
            command = line.decode().strip()
            if command.startswith('EHLO'):
                reply = b'250-local-fixture\r\n250 AUTH PLAIN\r\n'
            elif command.startswith('AUTH'):
                reply = b'235 authenticated\r\n'
            elif command.startswith('MAIL'):
                reply = b'250 accepted\r\n'
            elif command.startswith('RCPT'):
                assert command == f'RCPT TO:<{RECIPIENT}>', 'Forbidden test recipient'
                if self.server.fail_next:
                    self.server.fail_next = False
                    reply = b'550 test rejection\r\n'
                else:
                    reply = b'250 accepted\r\n'
            elif command == 'DATA':
                self.wfile.write(b'354 continue\r\n')
                message = bytearray()
                while (part := self.rfile.readline()) and part != b'.\r\n':
                    message.extend(part)
                self.server.messages.append(bytes(message))
                reply = b'250 queued\r\n'
            elif command == 'QUIT':
                self.wfile.write(b'221 bye\r\n')
                return
            else:
                reply = b'500 unexpected command\r\n'
            self.wfile.write(reply)


class LocalSMTP(socketserver.ThreadingTCPServer):
    daemon_threads = True
    def get_request(self):
        conn, addr = super().get_request()
        return self.context.wrap_socket(conn, server_side=True), addr


def wait_instance(folder, process):
    for _ in range(150):
        path = folder / 'instance.json'
        if path.exists():
            return json.loads(path.read_text())
        if process.poll() is not None:
            raise AssertionError('App stopped during startup')
        time.sleep(.1)
    raise AssertionError('App did not start')


def main():
    with tempfile.TemporaryDirectory(prefix='maildesk-browser-') as tmp:
        folder = Path(tmp)
        cert, key = folder/'cert.pem', folder/'key.pem'
        subprocess.run(['openssl','req','-x509','-newkey','rsa:2048','-nodes','-keyout',str(key),'-out',str(cert),'-days','1','-subj','/CN=127.0.0.1','-addext','subjectAltName=IP:127.0.0.1','-addext','basicConstraints=critical,CA:TRUE'],check=True,stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
        server = LocalSMTP(('127.0.0.1',0),SMTPHandler)
        server.context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        server.context.load_cert_chain(cert,key)
        server.fail_next = True
        server.messages = []
        threading.Thread(target=server.serve_forever,daemon=True).start()
        data_dir = folder/'data'
        environment = dict(os.environ, SSL_CERT_FILE=str(cert))
        # Fixture runs must not inherit personal provider credentials.
        for name in ('LLM_API_KEY','SMTP_USERNAME','SMTP_FROM_EMAIL','SMTP_PASSWORD'):
            environment.pop(name,None)
        process = subprocess.Popen([str(ROOT/'dist/maildesk'),'--no-browser','--port','0','--data-dir',str(data_dir),'--test-recipient',RECIPIENT],env=environment,stdout=subprocess.DEVNULL)
        try:
            info = wait_instance(data_dir,process)
            with sync_playwright() as playwright:
                browser = playwright.chromium.launch(executable_path=os.environ.get('CHROMIUM','/usr/bin/chromium'),headless=True,args=['--no-sandbox'])
                page = browser.new_page(viewport={'width':1440,'height':1000})
                errors = []
                page.on('pageerror',lambda e: errors.append(str(e)))
                page.goto(f"http://127.0.0.1:{info['port']}/?session={info['session']}")
                page.locator('[data-page="settings"]').click()
                form = page.locator('#settings-form')
                for name,value in {'host':'127.0.0.1','port':str(server.server_address[1]),'username':'sender@example.com','from':'sender@example.com','password':'test-only-password','senderName':'Тестовая команда'}.items():
                    form.locator(f'[name="{name}"]').fill(value)
                form.get_by_role('button',name='Сохранить подключения').click()
                expect(page.locator('#smtp-status')).to_have_text('Данные сохранены')
                page.locator('#smtp-test').click()
                expect(page.locator('#connection-result')).to_contain_text('авторизация работают',timeout=10000)
                assert not server.messages
                page.locator('[data-page="template"]').click()
                page.locator('#template-form [name="senderProfile"]').fill('Тестовая команда обслуживает вентиляционное оборудование.')
                page.locator('#template-form [name="subject"]').fill('Вопрос для {company}')
                page.locator('#template-form [name="body"]').fill('Здравствуйте!\nТестовое письмо для {company}.')
                page.get_by_role('button',name='Сохранить шаблон').click()
                expect(page.locator('#toast')).to_have_text('Шаблон сохранён')
                page.locator('[data-page="campaign"]').click()
                csv_data = f'Company,Email,Overview\nТестовая компания,{RECIPIENT},\nБез адреса,,\n'
                page.locator('#file-input').set_input_files({'name':'companies.csv','mimeType':'text/csv','buffer':csv_data.encode()})
                expect(page.locator('#stat-total')).to_have_text('2')
                page.locator('#select-all').check()
                expect(page.locator('#selected-count')).to_have_text('1')
                brochure=b'%PDF-1.4\nattachment-test\x00\xff'
                page.locator('#bulk-attachment-input').set_input_files({'name':'Предложение.pdf','mimeType':'application/pdf','buffer':brochure})
                expect(page.locator('#toast')).to_contain_text('Вложения сохранены')
                page.locator('#generate-button').click()
                expect(page.locator('.status-chip.ready')).to_have_count(1,timeout=10000)
                page.locator('.lead-row').first.click()
                expect(page.locator('#editor-form [name="body"]')).to_have_value('Здравствуйте!\nТестовое письмо для Тестовая компания.')
                expect(page.locator('.attachment-item')).to_have_count(1)
                with page.expect_download() as attachment_download:
                    page.locator('.attachment-name').click()
                downloaded=folder/'brochure.pdf';attachment_download.value.save_as(downloaded)
                assert downloaded.read_bytes()==brochure
                page.locator('#editor-form [name="body"]').fill('Здравствуйте!\nПроверенный вручную текст.')
                expect(page.locator('#send-button')).to_be_disabled()
                page.get_by_role('button',name='Сохранить письмо').click()
                expect(page.locator('#send-button')).to_be_enabled()
                # An API failure must be visible and retain the previous draft.
                page.locator('details.description > summary').click()
                page.locator('#editor-form [name="description"]').fill('Производство VAV-клапанов')
                page.get_by_role('button',name='Сохранить письмо').click()
                expect(page.locator('#toast')).to_have_text('Письмо сохранено')
                page.once('dialog',lambda dialog:dialog.accept())
                page.locator('#generate-button').click()
                expect(page.locator('#job-banner')).to_contain_text('ошибки: 1',timeout=10000)
                expect(page.locator('.lead-row .status-chip')).to_contain_text(['Ошибка генерации','Нет адреса'])
                expect(page.locator('#editor-form [name="body"]')).to_have_value('Здравствуйте!\nПроверенный вручную текст.')
                expect(page.locator('.inline-error')).to_contain_text('добавьте ключ DeepSeek')
                page.locator('#editor-form [name="body"]').fill('Здравствуйте!\nПроверенный вручную текст. Продолжим общение.')
                page.get_by_role('button',name='Сохранить письмо').click()
                expect(page.locator('#toast')).to_have_text('Письмо сохранено')

                def send_with_confirmations():
                    before=len(server.messages)
                    page.locator('#send-button').click()
                    expect(page.locator('#confirm-title')).to_have_text('Подтвердите отправку')
                    assert len(server.messages)==before
                    page.locator('#confirm-next').click()
                    expect(page.locator('#confirm-title')).to_have_text('Точно подтверждаете?')
                    assert len(server.messages)==before
                    page.locator('#confirm-next').click()

                send_with_confirmations()
                expect(page.locator('#stat-error')).to_have_text('1',timeout=10000)
                expect(page.locator('.lead-row.error input')).to_be_enabled()
                page.locator('[data-filter="error"]').click()
                expect(page.locator('.lead-row')).to_have_count(1)
                expect(page.locator('.inline-error')).to_contain_text('550')
                # Failed recipients remain selected, ready for an explicit retry.
                send_with_confirmations()
                expect(page.locator('#stat-sent')).to_have_text('1',timeout=10000)
                page.locator('[data-filter="sent"]').click()
                expect(page.locator('.lead-row')).to_have_count(1)
                checkbox=page.locator('.lead-row.sent input')
                expect(checkbox).to_be_disabled();expect(checkbox).to_be_checked()
                expect(page.locator('#selected-count')).to_have_text('0')
                assert len(server.messages)==1
                assert f'To: {RECIPIENT}'.encode() in server.messages[0]
                mail=BytesParser(policy=policy.default).parsebytes(server.messages[0])
                attached=list(mail.iter_attachments())
                assert len(attached)==1 and attached[0].get_filename()=='Предложение.pdf'
                assert attached[0].get_payload(decode=True)==brochure

                # Compose with several recipients; the server must enforce the
                # test address on EVERY recipient before making an SMTP call.
                page.locator('[data-page="compose"]').click()
                page.locator('#compose-form [name="recipients"]').fill(RECIPIENT+'; second@example.test')
                page.locator('#compose-form [name="subject"]').fill('Ручное письмо с файлами')
                page.locator('#compose-form [name="body"]').fill('Здравствуйте!\nСоздано вручную.')
                notes='Пример текстового вложения'.encode()
                page.locator('#compose-attachment-input').set_input_files([
                    {'name':'Предложение.pdf','mimeType':'application/pdf','buffer':brochure},
                    {'name':'описание.txt','mimeType':'text/plain','buffer':notes}])
                expect(page.locator('#compose-attachments .attachment-item')).to_have_count(2)
                page.get_by_role('button',name='Сохранить черновик',exact=True).click()
                expect(page.locator('#stat-total')).to_have_text('3')
                expect(page.locator('#editor-form [name="recipients"]')).to_have_value(RECIPIENT+', second@example.test')
                expect(page.locator('.attachment-item')).to_have_count(2)
                page.locator('#send-button').click()
                expect(page.locator('#toast')).to_contain_text('тестовый режим')
                assert len(server.messages)==1
                page.locator('#editor-form [name="recipients"]').fill(RECIPIENT)
                page.get_by_role('button',name='Сохранить письмо',exact=True).click()
                expect(page.locator('#toast')).to_have_text('Письмо сохранено')
                page.get_by_role('button',name='Удалить описание.txt',exact=True).click()
                expect(page.locator('.attachment-item')).to_have_count(1)
                page.locator('#editor-attachment-input').set_input_files({'name':'описание.txt','mimeType':'text/plain','buffer':notes})
                expect(page.locator('.attachment-item')).to_have_count(2)
                screenshot_dir=os.environ.get('MAILDESK_SCREENSHOTS')
                if screenshot_dir:
                    screenshot_path=Path(screenshot_dir);screenshot_path.mkdir(parents=True,exist_ok=True)
                    page.evaluate('window.scrollTo(0,0)')
                    page.screenshot(path=str(screenshot_path/'manual-message.png'),full_page=True)
                send_with_confirmations()
                expect(page.locator('#stat-sent')).to_have_text('2',timeout=10000)
                assert len(server.messages)==2
                mail=BytesParser(policy=policy.default).parsebytes(server.messages[1])
                attached=list(mail.iter_attachments())
                assert len(attached)==2 and attached[1].get_filename()=='описание.txt'
                assert attached[1].get_payload(decode=True)==notes
                with page.expect_download() as download:
                    page.locator('#export-menu-button').click()
                    page.get_by_text('Проект с вложениями · JSON').click()
                export_file=folder/'project.json';download.value.save_as(export_file)
                exported=json.loads(export_file.read_text())
                assert exported['leads'][0]['status']=='sent'
                assert exported['version']==2 and len(exported['files'])==2
                assert exported['template']['senderProfile']=='Тестовая команда обслуживает вентиляционное оборудование.'
                assert len(exported['leads'][2]['attachments'])==2
                assert 'test-only-password' not in export_file.read_text()
                page.reload()
                expect(page.locator('#stat-sent')).to_have_text('2')
                page.locator('#file-input').set_input_files(export_file)
                expect(page.locator('#toast')).to_contain_text('Добавлено: 0')
                expect(page.locator('#stat-sent')).to_have_text('2')
                page.set_viewport_size({'width':390,'height':844})
                assert not page.evaluate('document.documentElement.scrollWidth > innerWidth')
                page.locator('[data-page="compose"]').click()
                page.locator('#compose-form [name="recipients"]').fill(RECIPIENT)
                page.locator('#compose-form [name="subject"]').fill('Следующее письмо')
                page.locator('#compose-form [name="body"]').fill('Только черновик — без отправки.')
                page.locator('#compose-attachment-input').set_input_files({'name':'описание.txt','mimeType':'text/plain','buffer':notes})
                assert not page.evaluate('document.documentElement.scrollWidth > innerWidth')
                if screenshot_dir:
                    page.screenshot(path=str(screenshot_path/'compose-mobile.png'),full_page=True)
                assert not errors, errors
                browser.close()
            print('PASS: settings, TLS SMTP auth, CSV, template, bulk/company attachments, MIME bytes, manual compose, multiple recipients, test guard, attachment add/remove/download, both confirmations, rejection/retry/sent colors, filters, JSON file transfer, persistence, desktop/mobile. Two messages accepted by local fixture; no external SMTP traffic.')
        finally:
            process.terminate();process.wait(timeout=10);server.shutdown();server.server_close()


if __name__ == '__main__':
    main()
