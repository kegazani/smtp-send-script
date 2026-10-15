# MailDesk development

During development and testing, send actual email only to `ddkritsin@gmail.com`, as explicitly required by the user. Never send test mail to companies in the imported leads or any other address. Run the development server with `--test-recipient ddkritsin@gmail.com`. Protocol tests use local fake services and must not contact real SMTP recipients.

Never commit credentials, local project data, uploaded lead files, or built artifacts. Existing credentials are stored outside this checkout in `/workspace/smtp-send-script-local/.env`; do not print their values. This is a coding task; application files may be edited.
