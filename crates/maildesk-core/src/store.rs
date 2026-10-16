use crate::*;
use anyhow::{Context, Result, ensure};
use fs2::FileExt;
use serde::{Serialize, de::DeserializeOwned};
use std::{
    fs::{self, File, OpenOptions},
    path::{Path, PathBuf},
    time::{Duration, Instant},
};

pub fn atomic_json<T: Serialize>(path: &Path, value: &T) -> Result<()> {
    let mut f =
        tempfile::NamedTempFile::new_in(path.parent().context("Папка проекта отсутствует")?)?;
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        f.as_file()
            .set_permissions(fs::Permissions::from_mode(0o600))?;
    }
    serde_json::to_writer_pretty(&mut f, value)?;
    f.as_file().sync_all()?;
    f.persist(path).map_err(|e| e.error)?;
    Ok(())
}
fn read_json<T: DeserializeOwned>(path: &Path) -> Result<T> {
    ensure!(
        path.metadata()?.len() <= MAX_PROJECT,
        "Проект слишком большой"
    );
    Ok(serde_json::from_reader(File::open(path)?)?)
}
pub fn default_data_dir() -> PathBuf {
    dirs::config_dir()
        .unwrap_or_else(|| PathBuf::from("."))
        .join("MailDesk")
}
#[derive(Default)]
pub struct Options {
    pub data_dir: Option<PathBuf>,
    pub env_file: Option<PathBuf>,
    pub import: Option<PathBuf>,
    pub test_recipient: Option<String>,
}
impl Options {
    pub fn from_args() -> Result<Self> {
        let mut out = Self::default();
        let mut args = std::env::args().skip(1);
        while let Some(arg) = args.next() {
            if arg == "--help" {
                println!(
                    "MailDesk 2 — Rust / GPUI\n--data-dir PATH  --env-file PATH  --import CSV  --test-recipient EMAIL"
                );
                std::process::exit(0);
            }
            let value = args
                .next()
                .context("После параметра нужен путь или адрес")?;
            match arg.as_str() {
                "--data-dir" => out.data_dir = Some(value.into()),
                "--env-file" => out.env_file = Some(value.into()),
                "--import" => out.import = Some(value.into()),
                "--test-recipient" => {
                    ensure!(valid_email(&value), "Неверный тестовый адрес");
                    out.test_recipient = Some(value)
                }
                _ => anyhow::bail!("Неизвестный параметр {arg}"),
            }
        }
        Ok(out)
    }
}
pub struct SendPermit {
    ids: std::collections::VecDeque<String>,
    hashes: std::collections::HashMap<String, String>,
}
impl SendPermit {
    pub fn is_empty(&self) -> bool {
        self.ids.is_empty()
    }
}
struct Confirmation {
    token: String,
    stage: u8,
    ids: Vec<String>,
    digest: String,
    expires: Instant,
}
pub struct Store {
    pub workspace: Workspace,
    pub settings: Settings,
    pub dir: PathBuf,
    pub test_recipient: Option<String>,
    _lock: File,
    confirmation: Option<Confirmation>,
}
impl Store {
    pub fn open(mut options: Options) -> Result<Self> {
        let dir = options.data_dir.take().unwrap_or_else(default_data_dir);
        fs::create_dir_all(&dir)?;
        // The previous web application has its own instance protocol. Refuse a
        // shared profile while it is alive, without exposing its session token.
        if let Ok(old) = read_json::<serde_json::Value>(&dir.join("instance.json"))
            && let (Some(port), Some(session)) = (old["port"].as_u64(), old["session"].as_str())
            && port > 0
            && port <= 65535
        {
            let response = reqwest::blocking::Client::builder()
                .no_proxy()
                .timeout(Duration::from_millis(800))
                .build()?
                .get(format!("http://127.0.0.1:{port}/api/state"))
                .header("Cookie", format!("maildesk={session}"))
                .send();
            ensure!(
                !response.is_ok_and(|r| r.status().is_success()),
                "Закройте веб-версию MailDesk перед открытием этого проекта в нативной версии"
            );
        }
        let lock = OpenOptions::new()
            .create(true)
            .truncate(false)
            .read(true)
            .write(true)
            .open(dir.join("native.lock"))?;
        lock.try_lock_exclusive()
            .context("Этот профиль уже открыт в MailDesk")?;
        let mut adjacent = std::env::current_exe()?
            .parent()
            .context("Нет папки приложения")?
            .to_path_buf();
        if adjacent.ends_with("Contents/MacOS") {
            adjacent = adjacent.join("../../..");
        }
        let env_file = options.env_file.or_else(|| {
            let p = adjacent.join("maildesk.env");
            p.exists().then_some(p)
        });
        // Read credentials without changing process environment (safe in Rust 2024).
        let mut env = std::collections::HashMap::new();
        if let Some(path) = env_file {
            for item in dotenvy::from_path_iter(path)? {
                let (k, v) = item?;
                env.insert(k, v);
            }
        }
        let get = |key: &str| {
            std::env::var(key)
                .ok()
                .or_else(|| env.get(key).cloned())
                .unwrap_or_default()
        };
        let project = dir.join("project.json");
        let mut workspace = if project.exists() {
            read_json::<Workspace>(&project)?
        } else {
            Workspace::default()
        };
        workspace.validate()?;
        if workspace.version == 1 {
            let backup = dir.join("project-v1.backup.json");
            if !backup.exists() {
                let mut out = OpenOptions::new()
                    .create_new(true)
                    .write(true)
                    .open(backup)?;
                std::io::copy(&mut File::open(&project)?, &mut out)?;
                out.sync_all()?;
            }
            workspace.version = 2;
        }
        for l in &mut workspace.leads {
            if l.status == "sending" {
                l.status = "unknown".into();
                l.error =
                    "Приложение закрылось во время отправки. Проверьте доставку перед повтором."
                        .into();
            }
        }
        let settings_path = dir.join("credentials.json");
        let mut settings = if settings_path.exists() {
            read_json::<Settings>(&settings_path)?
        } else {
            Settings::default()
        };
        if settings.username.is_empty() {
            settings.username = get("SMTP_USERNAME");
        }
        if settings.from.is_empty() {
            settings.from = get("SMTP_FROM_EMAIL");
            if settings.from.is_empty() {
                settings.from = settings.username.clone();
            }
        }
        if settings.password.is_empty() {
            settings.password = get("SMTP_PASSWORD");
        }
        if settings.api_key.is_empty() {
            settings.api_key = get("LLM_API_KEY");
        }
        let seed = options.import.or_else(|| {
            let p = adjacent.join("companies.csv");
            p.exists().then_some(p)
        });
        if workspace.leads.is_empty()
            && let Some(p) = seed
        {
            workspace.merge(csv_workspace(&p)?)?;
        }
        let out = Self {
            workspace,
            settings,
            dir,
            _lock: lock,
            confirmation: None,
            test_recipient: options.test_recipient,
        };
        out.save()?;
        atomic_json(&settings_path, &out.settings)?;
        Ok(out)
    }
    pub fn save(&self) -> Result<()> {
        atomic_json(&self.dir.join("project.json"), &self.workspace)
    }
    pub fn change<T>(&mut self, f: impl FnOnce(&mut Workspace) -> Result<T>) -> Result<T> {
        let before = self.workspace.clone();
        let outcome = f(&mut self.workspace).and_then(|v| {
            self.workspace.validate()?;
            self.save()?;
            Ok(v)
        });
        if outcome.is_err() {
            self.workspace = before;
        } else {
            self.confirmation = None;
        }
        outcome
    }
    pub fn save_settings(&mut self, settings: Settings) -> Result<()> {
        ensure!(
            matches!(settings.security.as_str(), "ssl" | "starttls"),
            "Выберите SSL или STARTTLS"
        );
        atomic_json(&self.dir.join("credentials.json"), &settings)?;
        self.settings = settings;
        self.confirmation = None;
        Ok(())
    }
    pub fn import(&mut self, path: &Path) -> Result<(usize, usize)> {
        let is_csv = path
            .extension()
            .is_some_and(|s| s.eq_ignore_ascii_case("csv"));
        let incoming = if is_csv {
            csv_workspace(path)?
        } else {
            read_json(path)?
        };
        let template = incoming.template.clone();
        self.change(|w| {
            let result = w.merge(incoming)?;
            if !is_csv {
                w.template = template;
            }
            Ok(result)
        })
    }
    pub fn save_attachment(&self, id: &str, path: &Path) -> Result<()> {
        use base64::{Engine, engine::general_purpose::STANDARD};
        use std::io::Write;
        ensure!(
            path != self.dir.join("credentials.json") && path != self.dir.join("project.json"),
            "Выберите другое имя файла"
        );
        let data = STANDARD.decode(
            self.workspace
                .files
                .get(id)
                .context("Вложение отсутствует")?,
        )?;
        let mut f = tempfile::NamedTempFile::new_in(path.parent().context("Нет папки")?)?;
        f.write_all(&data)?;
        f.as_file().sync_all()?;
        f.persist(path).map_err(|e| e.error)?;
        Ok(())
    }
    pub fn export(&self, path: &Path) -> Result<()> {
        atomic_json(path, &self.workspace)
    }
    pub fn report(&self, path: &Path) -> Result<()> {
        let mut bytes = vec![0xef, 0xbb, 0xbf];
        {
            let mut writer = csv::Writer::from_writer(&mut bytes);
            writer.write_record([
                "Company",
                "Email",
                "Status",
                "SentAt",
                "Subject",
                "Body",
                "Error",
                "Attachments",
            ])?;
            for l in &self.workspace.leads {
                let cells = [
                    l.company.clone(),
                    l.recipients().join(", "),
                    l.status.clone(),
                    l.sent_at.clone(),
                    l.subject.clone(),
                    l.body.clone(),
                    l.error.clone(),
                    l.attachments
                        .iter()
                        .map(|a| a.name.as_str())
                        .collect::<Vec<_>>()
                        .join("; "),
                ];
                writer.write_record(cells.iter().map(|s| {
                    if s.starts_with(['=', '+', '-', '@']) {
                        format!("'{s}")
                    } else {
                        s.clone()
                    }
                }))?;
            }
            writer.flush()?;
        }
        let mut f = tempfile::NamedTempFile::new_in(path.parent().context("Нет папки")?)?;
        use std::io::Write;
        f.write_all(&bytes)?;
        f.as_file().sync_all()?;
        f.persist(path).map_err(|e| e.error)?;
        Ok(())
    }
    pub fn edit(&mut self, lead: Lead) -> Result<()> {
        self.change(|w| {
            if let Some(old) = w.leads.iter_mut().find(|l| l.id == lead.id) {
                ensure!(old.editable(), "Письмо уже отправлено или требует проверки");
                *old = lead;
            } else {
                ensure!(lead.kind == "message", "Нельзя добавить компанию вручную");
                w.leads.push(lead);
            }
            w.prune_files();
            Ok(())
        })
    }
    pub fn resolve_unknown(&mut self, id: &str, sent: bool) -> Result<()> {
        self.change(|w| {
            let l = w
                .leads
                .iter_mut()
                .find(|l| l.id == id)
                .context("Письмо не найдено")?;
            ensure!(l.status == "unknown", "Статус уже изменился");
            l.status = if sent { "sent" } else { "pending" }.into();
            l.error.clear();
            if sent {
                l.sent_at = now();
                if l.kind != "message" {
                    w.sent.insert(email_key(&l.email), l.sent_at.clone());
                }
            }
            Ok(())
        })
    }
    fn validate_send(&self, ids: &[String]) -> Result<()> {
        ensure!(!ids.is_empty(), "Выберите письма");
        self.settings.validate_smtp()?;
        let mut unique = std::collections::HashSet::new();
        let mut addresses = std::collections::HashSet::new();
        for id in ids {
            ensure!(unique.insert(id), "Письмо выбрано дважды");
            let l = self
                .workspace
                .leads
                .iter()
                .find(|l| &l.id == id)
                .context("Письмо не найдено")?;
            ensure!(l.selectable(), "Письмо нельзя отправить: {}", l.company);
            ensure!(
                !l.subject.trim().is_empty()
                    && !l.subject.contains(['\r', '\n'])
                    && !l.body.trim().is_empty(),
                "Заполните тему и текст: {}",
                l.company
            );
            self.workspace.validate_attachments(l)?;
            for r in l.recipients() {
                if let Some(test) = &self.test_recipient {
                    ensure!(
                        email_key(&r) == email_key(test),
                        "Тестовая отправка разрешена только на {test}"
                    );
                }
                if l.kind != "message" {
                    ensure!(
                        !self.workspace.sent.contains_key(&email_key(&r)),
                        "На этот адрес уже отправлено"
                    );
                    ensure!(
                        addresses.insert(email_key(&r)),
                        "В очереди повторяется адрес"
                    );
                }
            }
        }
        Ok(())
    }
    fn confirmation_digest(&self, ids: &[String]) -> Result<String> {
        let leads: Vec<_> = ids
            .iter()
            .filter_map(|id| self.workspace.leads.iter().find(|l| &l.id == id))
            .collect();
        Ok(digest(&serde_json::to_vec(&(leads, &self.settings))?))
    }
    pub fn prepare(&mut self, ids: Vec<String>) -> Result<String> {
        self.validate_send(&ids)?;
        let token = new_id();
        let digest = self.confirmation_digest(&ids)?;
        self.confirmation = Some(Confirmation {
            token: token.clone(),
            stage: 1,
            ids,
            digest,
            expires: Instant::now() + Duration::from_secs(300),
        });
        Ok(token)
    }
    pub fn confirm(&mut self, token: &str) -> Result<()> {
        let c = self
            .confirmation
            .as_ref()
            .context("Запросите подтверждение заново")?;
        ensure!(
            c.token == token && c.stage == 1 && Instant::now() < c.expires,
            "Подтверждение недействительно"
        );
        ensure!(
            c.digest == self.confirmation_digest(&c.ids)?,
            "Письма изменились"
        );
        self.confirmation.as_mut().unwrap().stage = 2;
        Ok(())
    }
    pub fn authorize(&mut self, token: &str) -> Result<SendPermit> {
        // Consume even on validation failure. A token cannot send twice.
        let c = self.confirmation.take().context("Нет подтверждения")?;
        ensure!(
            c.token == token && c.stage == 2 && Instant::now() < c.expires,
            "Нужны два подтверждения"
        );
        ensure!(
            c.digest == self.confirmation_digest(&c.ids)?,
            "Письма изменились"
        );
        self.validate_send(&c.ids)?;
        let hashes = c
            .ids
            .iter()
            .map(|id| {
                Ok((
                    id.clone(),
                    self.confirmation_digest(std::slice::from_ref(id))?,
                ))
            })
            .collect::<Result<_>>()?;
        Ok(SendPermit {
            ids: c.ids.into(),
            hashes,
        })
    }
    pub fn generate_one(&mut self, id: &str) -> Result<String> {
        let l = self
            .workspace
            .leads
            .iter()
            .find(|l| l.id == id)
            .context("Письмо не найдено")?
            .clone();
        ensure!(
            l.editable() && l.kind != "message",
            "Выберите неотправленную компанию"
        );
        let result = generate(&self.settings, &self.workspace.template, &l);
        match result {
            Ok((subject, body, source)) => {
                let source_copy = source.clone();
                self.change(|w| {
                    let l = w.leads.iter_mut().find(|l| l.id == id).unwrap();
                    l.subject = subject;
                    l.body = body;
                    l.source = source;
                    l.generation_error.clear();
                    Ok(())
                })?;
                Ok(source_copy)
            }
            Err(e) => {
                let message = e.to_string();
                self.change(|w| {
                    w.leads
                        .iter_mut()
                        .find(|l| l.id == id)
                        .unwrap()
                        .generation_error = message.clone();
                    Ok(())
                })?;
                Err(e)
            }
        }
    }
    pub fn send_next(&mut self, permit: &mut SendPermit) -> Result<()> {
        let owned_id = permit.ids.pop_front().context("Очередь пуста")?;
        let id = owned_id.as_str();
        ensure!(
            permit.hashes.remove(id).as_deref()
                == Some(
                    self.confirmation_digest(std::slice::from_ref(&owned_id))?
                        .as_str()
                ),
            "Письмо или настройки изменились; подтвердите заново"
        );
        let mut l = self
            .workspace
            .leads
            .iter()
            .find(|l| l.id == id)
            .context("Письмо не найдено")?
            .clone();
        self.validate_send(&[id.to_owned()])?;
        l.message_id = format!("<{}@maildesk.local>", new_id());
        l.status = "sending".into();
        l.error.clear();
        self.change(|w| {
            *w.leads.iter_mut().find(|old| old.id == id).unwrap() = l.clone();
            Ok(())
        })?;
        let result = deliver(&self.settings, &l, &self.workspace);
        let (status, error) = match result {
            Ok(()) => ("sent", String::new()),
            Err(e) => (if e.ambiguous { "unknown" } else { "failed" }, e.message),
        };
        self.change(|w| {
            let l = w.leads.iter_mut().find(|l| l.id == id).unwrap();
            l.status = status.into();
            l.error = error.clone();
            if status == "sent" {
                l.sent_at = now();
                if l.kind != "message" {
                    w.sent.insert(email_key(&l.email), l.sent_at.clone());
                }
            }
            Ok(())
        })?;
        if status != "sent" {
            anyhow::bail!("{error}");
        }
        Ok(())
    }
}
