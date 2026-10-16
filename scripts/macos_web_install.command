#!/bin/sh
# MailDesk macOS installer — also installed as the local update helper.
set -u

maildesk_stage=''
maildesk_unpack=''
maildesk_lock=''
maildesk_backup=''
maildesk_target=''
maildesk_replaced=0
maildesk_committed=0

maildesk_cleanup() {
    if [ "$maildesk_committed" -eq 0 ] && [ "$maildesk_replaced" -eq 1 ]; then
        if "$maildesk_target/Contents/MacOS/MailDesk" --prepare-update "$@"; then
            /bin/rm -rf "$maildesk_target"
        else
            printf '\nНе удалось остановить новую версию для отката. Резервная копия: %s\n' "$maildesk_backup" >&2
        fi
    fi
    if [ "$maildesk_committed" -eq 0 ] && [ -n "$maildesk_backup" ] &&
       [ -d "$maildesk_backup/MailDesk Web.app" ] && [ ! -e "$maildesk_target" ]; then
        /bin/mv "$maildesk_backup/MailDesk Web.app" "$maildesk_target" &&
            printf '\nПредыдущая версия восстановлена.\n'
    fi
    [ -z "$maildesk_stage" ] || /bin/rm -rf "$maildesk_stage"
    [ -z "$maildesk_unpack" ] || /bin/rm -rf "$maildesk_unpack"
    [ -z "$maildesk_lock" ] || /bin/rmdir "$maildesk_lock"
    return 0
}
trap 'maildesk_cleanup "$@"' EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

maildesk_fail() {
    printf '\nMailDesk: %s\n' "$1" >&2
    exit 1
}

maildesk_here=$(CDPATH= cd -- "$(dirname "$0")" && pwd -P) || exit 1
maildesk_root=''
maildesk_archive=''
maildesk_no_launch=0
while [ "$#" -gt 0 ]; do
    case "$1" in
        --install-dir)
            [ "$#" -ge 2 ] || maildesk_fail 'После --install-dir нужна папка.'
            maildesk_root=$2; shift 2 ;;
        --archive)
            [ "$#" -ge 2 ] || maildesk_fail 'После --archive нужен ZIP.'
            maildesk_archive=$2; shift 2 ;;
        --no-launch) maildesk_no_launch=1; shift ;;
        --) shift; break ;;
        *) break ;;
    esac
done
if [ -z "$maildesk_root" ]; then
    if [ -d "$maildesk_here/MailDesk Web.app" ]; then
        maildesk_root=$maildesk_here
    else
        maildesk_root="$HOME/Applications"
    fi
fi

printf 'MailDesk — установка и обновление веб-версии\n'
maildesk_source="$maildesk_here/MailDesk.app"
if [ -n "$maildesk_archive" ] || [ ! -d "$maildesk_source" ]; then
    if [ -z "$maildesk_archive" ]; then
        maildesk_archive=$(/usr/bin/osascript -e 'POSIX path of (choose file of type {"public.zip-archive"} with prompt "Выберите новый Mac ZIP веб-версии MailDesk")') || exit 1
    fi
    [ -f "$maildesk_archive" ] || maildesk_fail 'Выбранный ZIP не найден.'
    /usr/bin/unzip -tq "$maildesk_archive" >/dev/null || maildesk_fail 'ZIP повреждён. Установка отменена.'
    /usr/bin/unzip -Z -1 "$maildesk_archive" | /usr/bin/awk '
        /^\// || /(^|\/)\.\.($|\/)/ || /\\/ { bad = 1 }
        END { exit bad }
    ' || maildesk_fail 'В ZIP недопустимые пути.'
    /usr/bin/zipinfo -l "$maildesk_archive" | /usr/bin/awk '
        substr($0, 1, 1) == "l" { bad = 1 }
        END { exit bad }
    ' || maildesk_fail 'ZIP с символическими ссылками не поддерживается.'
    maildesk_unpack=$(/usr/bin/mktemp -d "${TMPDIR:-/tmp}/maildesk-unpack.XXXXXX") || exit 1
    /usr/bin/ditto -x -k "$maildesk_archive" "$maildesk_unpack" || maildesk_fail 'Не удалось распаковать ZIP.'
    maildesk_source=''
    for maildesk_candidate in "$maildesk_unpack"/MailDesk-macOS-*/MailDesk.app; do
        [ -d "$maildesk_candidate" ] || continue
        [ -z "$maildesk_source" ] || maildesk_fail 'В ZIP несколько приложений.'
        maildesk_source=$maildesk_candidate
    done
fi
[ -d "$maildesk_source" ] || maildesk_fail 'Нужен Mac ZIP веб-версии MailDesk.'

printf '\nПроверяем пакет…\n'
/usr/bin/codesign --verify --deep --strict --verbose=2 "$maildesk_source" ||
    maildesk_fail 'Проверка подписи не пройдена. Приложение не изменено и не запущено.'
maildesk_identifier=$(/usr/libexec/PlistBuddy -c 'Print :CFBundleIdentifier' "$maildesk_source/Contents/Info.plist") || exit 1
[ "$maildesk_identifier" = 'local.maildesk.web' ] || maildesk_fail 'Выбран пакет другого приложения.'
maildesk_installer_version=$(/usr/libexec/PlistBuddy -c 'Print :MailDeskInstallerVersion' "$maildesk_source/Contents/Info.plist" 2>/dev/null) ||
    maildesk_fail 'У этого старого архива нет установщика. Выберите версию 1.1.3 или новее.'
[ "$maildesk_installer_version" = '1' ] || maildesk_fail 'Неизвестный формат установщика.'
maildesk_version=$(/usr/libexec/PlistBuddy -c 'Print :CFBundleShortVersionString' "$maildesk_source/Contents/Info.plist") || exit 1
maildesk_package=$(dirname "$maildesk_source")
if [ -f "$maildesk_package/maildesk.env" ]; then
    set -- --env-file "$maildesk_package/maildesk.env" "$@"
fi
if [ -f "$maildesk_package/companies.csv" ]; then
    set -- --import "$maildesk_package/companies.csv" "$@"
fi

printf '\nУстановить MailDesk %s в:\n%s/MailDesk Web.app\n' "$maildesk_version" "$maildesk_root"
printf 'Компании и подключения сохранятся. Предыдущая установленная версия будет сохранена для отката.\n'
printf 'После проверки пакета будет разрешён запуск установленной копии. Нажмите Enter для установки или Ctrl+C для отмены. '
IFS= read -r maildesk_reply || exit 1

/bin/mkdir -p "$maildesk_root" || maildesk_fail 'Нет права записи в папку установки.'
maildesk_root=$(CDPATH= cd -- "$maildesk_root" && pwd -P) || exit 1
[ "$maildesk_root" != '/' ] || maildesk_fail 'Нельзя устанавливать в корень диска.'
maildesk_target="$maildesk_root/MailDesk Web.app"
[ ! -L "$maildesk_target" ] || maildesk_fail 'Вместо приложения обнаружена символическая ссылка.'
if [ -e "$maildesk_target" ]; then
    [ -d "$maildesk_target" ] || maildesk_fail 'Путь установки занят другим файлом.'
    maildesk_existing_id=$(/usr/libexec/PlistBuddy -c 'Print :CFBundleIdentifier' "$maildesk_target/Contents/Info.plist" 2>/dev/null) || exit 1
    [ "$maildesk_existing_id" = 'local.maildesk.web' ] || maildesk_fail 'Установщик не заменит другое приложение.'
fi
maildesk_helper="$maildesk_root/Обновить MailDesk.command"
if [ -e "$maildesk_helper" ]; then
    [ ! -L "$maildesk_helper" ] && /usr/bin/grep -q '^# MailDesk macOS installer' "$maildesk_helper" ||
        maildesk_fail 'Файл обновления занят другим файлом.'
fi
/bin/mkdir "$maildesk_root/.maildesk-install.lock" || maildesk_fail 'Другой установщик уже работает.'
maildesk_lock="$maildesk_root/.maildesk-install.lock"
maildesk_stage=$(/usr/bin/mktemp -d "$maildesk_root/.maildesk-stage.XXXXXX") || exit 1
maildesk_candidate="$maildesk_stage/MailDesk Web.app"
/usr/bin/ditto "$maildesk_source" "$maildesk_candidate" || maildesk_fail 'Не удалось скопировать приложение.'
/usr/bin/codesign --verify --deep --strict "$maildesk_candidate" || maildesk_fail 'Скопированный пакет не прошёл проверку.'
/usr/bin/xattr -dr com.apple.quarantine "$maildesk_candidate" || maildesk_fail 'Не удалось разрешить запуск установленной копии.'
/bin/chmod u+x "$maildesk_candidate/Contents/MacOS/MailDesk" || exit 1
/bin/cat "$maildesk_candidate/Contents/Resources/Install.command" > "$maildesk_stage/update.command" || exit 1
/bin/chmod 755 "$maildesk_stage/update.command" || exit 1

printf '\nПроверяем, можно ли завершить предыдущую версию…\n'
"$maildesk_candidate/Contents/MacOS/MailDesk" --prepare-update "$@" ||
    maildesk_fail 'Обновление отменено. Установленная версия сохранена.'
if [ -d "$maildesk_target" ]; then
    maildesk_backup=$(/usr/bin/mktemp -d "$maildesk_root/.maildesk-backup.XXXXXX") || exit 1
    /bin/mv "$maildesk_target" "$maildesk_backup/MailDesk Web.app" || exit 1
fi
/bin/mv "$maildesk_candidate" "$maildesk_target" || maildesk_fail 'Не удалось установить приложение.'
maildesk_replaced=1

if [ "$maildesk_no_launch" -eq 0 ]; then
    /usr/bin/touch "$maildesk_stage/startup.log"
    /bin/chmod 600 "$maildesk_stage/startup.log"
    /usr/bin/open -n --stdout "$maildesk_stage/startup.log" --stderr "$maildesk_stage/startup.log" "$maildesk_target" --args --no-browser "$@" ||
        maildesk_fail 'macOS отказалась запустить установленную копию.'
    if ! "$maildesk_target/Contents/MacOS/MailDesk" --wait-ready "$@"; then
        /bin/cat "$maildesk_stage/startup.log" >&2
        maildesk_fail 'Сервер не запустился. Выполняется откат приложения; рабочие данные не сбрасываются.'
    fi
fi
/bin/mv "$maildesk_stage/update.command" "$maildesk_helper" || maildesk_fail 'Не удалось сохранить средство обновления.'
maildesk_committed=1
printf '\nMailDesk %s установлен.\nЗапуск: %s\n' "$maildesk_version" "$maildesk_target"
printf 'Для следующего обновления откройте «Обновить MailDesk.command» рядом с приложением и выберите новый ZIP.\n'
if [ "$maildesk_no_launch" -eq 0 ]; then
    "$maildesk_target/Contents/MacOS/MailDesk" --open-running "$@" ||
        printf 'Откройте установленный MailDesk Web.app из папки «Программы» пользователя.\n' >&2
fi
