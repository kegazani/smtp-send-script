#!/bin/sh
# Can also be run as: /bin/sh "/path/to/Запустить MailDesk.command"
set -u

maildesk_pause() {
    if [ -t 0 ]; then
        printf '\nНажмите Enter, чтобы закрыть окно. '
        IFS= read -r maildesk_reply || :
    fi
}

maildesk_fail() {
    printf '\nMailDesk: %s\n' "$1" >&2
    maildesk_pause
    exit 1
}

maildesk_folder=$(CDPATH= cd -- "$(dirname "$0")" && pwd -P) || exit 1
maildesk_app="$maildesk_folder/MailDesk.app"
maildesk_binary="$maildesk_app/Contents/MacOS/MailDesk"

printf 'MailDesk — локальная веб-версия\n'
printf 'macOS: '
/usr/bin/sw_vers -productVersion
printf 'Процессор: '
/usr/bin/uname -m

[ -f "$maildesk_binary" ] || maildesk_fail 'Не найдено MailDesk.app. Распакуйте весь ZIP в обычную папку и оставьте файл запуска рядом с приложением.'

# Never replace a failed signature with a new one or run a changed bundle.
printf '\nПроверка целостности MailDesk.app…\n'
/usr/bin/codesign --verify --deep --strict --verbose=2 "$maildesk_app" ||
    maildesk_fail 'Подпись пакета не прошла проверку. Скачайте и распакуйте архив заново. Приложение не запущено.'
/bin/chmod u+x "$maildesk_binary" || maildesk_fail 'Не удалось восстановить право запуска. Переместите всю папку MailDesk в папку с правом записи.'

if /usr/bin/xattr -p com.apple.quarantine "$maildesk_app" >/dev/null 2>&1 ||
   /usr/bin/xattr -p com.apple.quarantine "$maildesk_binary" >/dev/null 2>&1; then
    printf '\nmacOS пометила эту копию MailDesk как скачанную из интернета.\n'
    printf 'Целостность проверена. Нажмите Enter, чтобы разрешить запуск только этого MailDesk.app, или Ctrl+C для отмены. '
    IFS= read -r maildesk_reply || exit 1
    /usr/bin/xattr -dr com.apple.quarantine "$maildesk_app" ||
        maildesk_fail 'Не удалось разрешить запуск. Переместите всю папку MailDesk в папку с правом записи и повторите.'
fi

printf '\nЗапускаем сервер. Интерфейс откроется в браузере.\n'
"$maildesk_binary" "$@"
maildesk_result=$?
if [ "$maildesk_result" -ne 0 ]; then
    printf '\nMailDesk завершился с кодом %s. Скопируйте показанную выше ошибку.\n' "$maildesk_result" >&2
    maildesk_pause
fi
exit "$maildesk_result"
