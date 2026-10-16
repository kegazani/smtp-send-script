use anyhow::{Result, bail, ensure};
use base64::{Engine, engine::general_purpose::STANDARD};
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use std::{
    collections::{BTreeMap, HashSet},
    path::Path,
};

pub const MAX_FILE: usize = 10 << 20;
pub const MAX_PROJECT: u64 = 180 << 20;
pub fn new_id() -> String {
    uuid::Uuid::new_v4().simple().to_string()
}
pub fn now() -> String {
    chrono::Utc::now().to_rfc3339_opts(chrono::SecondsFormat::Secs, true)
}
pub fn digest(data: &[u8]) -> String {
    format!("{:x}", Sha256::digest(data))
}
pub fn email_key(s: &str) -> String {
    s.trim().to_lowercase()
}
pub fn valid_email(s: &str) -> bool {
    !s.contains(['\r', '\n', ' ']) && s.contains('.') && s.parse::<lettre::Address>().is_ok()
}
pub fn normalize_recipients(s: &str) -> Result<Vec<String>> {
    let mut seen = HashSet::new();
    let mut out = vec![];
    for s in s
        .split(|c: char| c == ',' || c == ';' || c.is_whitespace())
        .filter(|s| !s.is_empty())
    {
        ensure!(valid_email(s), "Некорректный адрес: {s}");
        if seen.insert(email_key(s)) {
            out.push(s.to_owned());
        }
    }
    ensure!(
        !out.is_empty() && out.len() <= 100,
        "Укажите от 1 до 100 получателей"
    );
    Ok(out)
}
#[derive(Clone, Default, Debug, Serialize, Deserialize)]
#[serde(rename_all = "camelCase", default)]
pub struct Attachment {
    pub id: String,
    pub name: String,
    pub size: usize,
    pub content_type: String,
}
#[derive(Clone, Default, Debug, Serialize, Deserialize)]
#[serde(rename_all = "camelCase", default)]
pub struct Lead {
    pub id: String,
    pub kind: String,
    pub recipients: Vec<String>,
    pub attachments: Vec<Attachment>,
    pub company: String,
    pub email: String,
    pub website: String,
    pub city: String,
    pub industry: String,
    pub description: String,
    pub subject: String,
    pub body: String,
    pub status: String,
    pub error: String,
    pub generation_error: String,
    pub sent_at: String,
    pub message_id: String,
    pub source: String,
}
impl Lead {
    pub fn recipients(&self) -> Vec<String> {
        if self.kind == "message" {
            self.recipients.clone()
        } else {
            vec![self.email.clone()]
        }
    }
    pub fn editable(&self) -> bool {
        !matches!(self.status.as_str(), "sent" | "sending" | "unknown")
    }
    pub fn selectable(&self) -> bool {
        self.editable()
            && !self.recipients().is_empty()
            && self.recipients().iter().all(|s| valid_email(s))
    }
    pub fn identity(&self) -> String {
        if self.kind == "message" {
            format!("message:{}", self.id)
        } else if valid_email(&self.email) {
            email_key(&self.email)
        } else {
            format!("company:{}", self.company.trim().to_lowercase())
        }
    }
}
#[derive(Clone, Debug, Serialize, Deserialize)]
#[serde(rename_all = "camelCase", default)]
pub struct Template {
    pub subject: String,
    pub body: String,
    pub instructions: String,
    pub sender_profile: String,
}
impl Default for Template {
    fn default() -> Self {
        Self {subject:"Возможное сотрудничество с {company}".into(),body:"Здравствуйте!\n\nХотели бы обсудить возможное сотрудничество с {company}. Подскажите, с кем можно связаться?\n\nС уважением,\nКоманда".into(),instructions:"Напиши индивидуальное деловое письмо на русском языке на основе описания компании. Упомяни её конкретную деятельность. Пиши кратко. Не выдумывай сведения о нас или получателе.".into(),sender_profile:String::new()}
    }
}
impl Template {
    pub fn render(&self, lead: &Lead) -> (String, String) {
        let render = |s: &str| {
            let mut s = s.to_owned();
            for (k, v) in [
                ("company", &lead.company),
                ("city", &lead.city),
                ("industry", &lead.industry),
                ("description", &lead.description),
                ("website", &lead.website),
            ] {
                s = s.replace(&format!("{{{k}}}"), v);
            }
            s
        };
        (render(&self.subject), render(&self.body))
    }
}
// Intentionally no Debug: settings contain credentials.
#[derive(Clone, Serialize, Deserialize)]
#[serde(rename_all = "camelCase", default)]
pub struct Settings {
    pub host: String,
    pub port: u16,
    pub security: String,
    pub username: String,
    pub password: String,
    pub from: String,
    pub sender_name: String,
    #[serde(rename = "apiKey")]
    pub api_key: String,
}
impl Default for Settings {
    fn default() -> Self {
        Self {
            host: "smtp.mail.ru".into(),
            port: 465,
            security: "ssl".into(),
            username: String::new(),
            password: String::new(),
            from: String::new(),
            sender_name: String::new(),
            api_key: String::new(),
        }
    }
}
impl Settings {
    pub fn validate_smtp(&self) -> Result<()> {
        ensure!(
            !self.host.is_empty()
                && !self.host.contains(['/', ':', ' ', '\r', '\n'])
                && self.port > 0,
            "Укажите SMTP-хост и порт"
        );
        ensure!(
            matches!(self.security.as_str(), "ssl" | "starttls"),
            "Допустимы SSL или STARTTLS"
        );
        ensure!(
            valid_email(&self.username) && valid_email(&self.from),
            "Укажите логин SMTP и адрес отправителя"
        );
        ensure!(!self.password.is_empty(), "Не указан SMTP-пароль");
        ensure!(
            !self.sender_name.contains(['\r', '\n']),
            "Недопустимое имя отправителя"
        );
        Ok(())
    }
}
#[derive(Clone, Debug, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct Workspace {
    pub version: u32,
    #[serde(default)]
    pub template: Template,
    #[serde(default)]
    pub leads: Vec<Lead>,
    #[serde(default)]
    pub sent: BTreeMap<String, String>,
    // Go encodes []byte as base64. Keep exactly the same transfer format.
    #[serde(default)]
    pub files: BTreeMap<String, String>,
}
impl Default for Workspace {
    fn default() -> Self {
        Self {
            version: 2,
            template: Template::default(),
            leads: vec![],
            sent: BTreeMap::new(),
            files: BTreeMap::new(),
        }
    }
}
impl Workspace {
    pub fn validate(&self) -> Result<()> {
        ensure!(
            self.template.subject.len() <= 1000
                && self.template.body.len() <= 50000
                && self.template.instructions.len() <= 10000
                && self.template.sender_profile.len() <= 20000,
            "Превышен размер шаблона или сведений о компании"
        );
        ensure!(
            matches!(self.version, 1 | 2),
            "Неподдерживаемая версия проекта"
        );
        ensure!(self.leads.len() <= 10000, "В проекте больше 10 000 писем");
        let mut total = 0;
        for (id, s) in &self.files {
            let data = STANDARD.decode(s)?;
            total += data.len();
            ensure!(
                data.len() <= MAX_FILE && digest(&data) == *id,
                "Повреждено вложение проекта"
            );
        }
        ensure!(total <= 100 << 20, "Вложения проекта превышают 100 МБ");
        let mut ids = HashSet::new();
        for l in &self.leads {
            ensure!(
                l.subject.len() <= 1000
                    && !l.subject.contains(['\r', '\n'])
                    && l.body.len() <= 50000
                    && l.description.len() <= 50000,
                "Слишком длинная тема, текст или описание"
            );
            ensure!(
                !l.id.is_empty()
                    && l.id.len() <= 128
                    && !l.id.contains(['\r', '\n'])
                    && ids.insert(&l.id),
                "Повторяющийся или неверный ID письма"
            );
            ensure!(
                l.kind.is_empty() || l.kind == "message",
                "Неизвестный тип письма"
            );
            ensure!(
                matches!(
                    l.status.as_str(),
                    "" | "pending" | "sent" | "failed" | "sending" | "unknown"
                ),
                "Неизвестный статус письма"
            );
            if l.kind == "message" {
                ensure!(
                    normalize_recipients(&l.recipients.join(","))? == l.recipients,
                    "Некорректные получатели"
                );
            }
            self.validate_attachments(l)?;
        }
        Ok(())
    }
    pub fn validate_attachments(&self, l: &Lead) -> Result<()> {
        ensure!(l.attachments.len() <= 20, "Не больше 20 вложений");
        let mut total = 0;
        for a in &l.attachments {
            let s = self
                .files
                .get(&a.id)
                .ok_or_else(|| anyhow::anyhow!("Вложение {} отсутствует", a.name))?;
            let bytes = STANDARD.decode(s)?;
            ensure!(
                bytes.len() == a.size && a.size <= MAX_FILE && digest(&bytes) == a.id,
                "Вложение повреждено: {}",
                a.name
            );
            ensure!(
                !a.name.is_empty()
                    && a.name.len() <= 240
                    && !a
                        .name
                        .chars()
                        .any(|c| c.is_control() || c == '/' || c == '\\'),
                "Неверное имя вложения"
            );
            let _: lettre::message::header::ContentType = a.content_type.parse()?;
            total += a.size;
        }
        ensure!(total <= 15 << 20, "Вложения одного письма превышают 15 МБ");
        Ok(())
    }
    pub fn merge(&mut self, other: Workspace) -> Result<(usize, usize)> {
        other.validate()?;
        let mut added = 0;
        let mut skipped = 0;
        for (k, v) in other.sent {
            if valid_email(&k) {
                self.sent.insert(email_key(&k), v);
            }
        }
        self.files.extend(other.files);
        for mut l in other.leads {
            if l.status == "sending" {
                l.status = "unknown".into();
                l.error = "Перенесено во время отправки; проверьте доставку".into();
            }
            if l.status == "sent" && l.kind != "message" {
                self.sent.insert(email_key(&l.email), l.sent_at.clone());
            }
            if let Some(old) = self
                .leads
                .iter_mut()
                .find(|old| old.identity() == l.identity())
            {
                skipped += 1;
                if old.status != "sent" && (old.status != "unknown" || l.status == "sent") {
                    if l.status == "sent"
                        || !l.subject.is_empty()
                        || !l.body.is_empty()
                        || !l.attachments.is_empty()
                    {
                        l.id = old.id.clone();
                        *old = l;
                    } else {
                        for (a, b) in [
                            (&mut old.description, l.description),
                            (&mut old.website, l.website),
                            (&mut old.city, l.city),
                            (&mut old.industry, l.industry),
                        ] {
                            if !b.is_empty() {
                                *a = b;
                            }
                        }
                    }
                }
            } else {
                if self.leads.iter().any(|old| old.id == l.id) {
                    l.id = new_id();
                }
                if l.status.is_empty() {
                    l.status = "pending".into();
                }
                self.leads.push(l);
                added += 1;
            }
        }
        for l in &mut self.leads {
            if l.kind != "message"
                && let Some(at) = self.sent.get(&email_key(&l.email))
            {
                l.status = "sent".into();
                l.sent_at = at.clone();
                l.error.clear();
            }
        }
        self.prune_files();
        self.validate()?;
        Ok((added, skipped))
    }
    pub fn prune_files(&mut self) {
        let used: HashSet<_> = self
            .leads
            .iter()
            .flat_map(|l| l.attachments.iter().map(|a| a.id.clone()))
            .collect();
        self.files.retain(|id, _| used.contains(id));
    }
    pub fn add_files(&mut self, ids: &[String], paths: &[std::path::PathBuf]) -> Result<()> {
        ensure!(!ids.is_empty(), "Выберите письма");
        let mut refs = vec![];
        for path in paths {
            ensure!(
                path.metadata()?.len() <= MAX_FILE as u64,
                "Файл больше 10 МБ"
            );
            let data = std::fs::read(path)?;
            ensure!(data.len() <= MAX_FILE, "Файл больше 10 МБ");
            let name = path
                .file_name()
                .and_then(|s| s.to_str())
                .ok_or_else(|| anyhow::anyhow!("Неверное имя файла"))?
                .to_owned();
            let id = digest(&data);
            self.files.insert(id.clone(), STANDARD.encode(&data));
            refs.push(Attachment {
                id,
                name,
                size: data.len(),
                content_type: mime_guess::from_path(path)
                    .first_or_octet_stream()
                    .to_string(),
            });
        }
        for id in ids {
            let l = self
                .leads
                .iter_mut()
                .find(|l| &l.id == id)
                .ok_or_else(|| anyhow::anyhow!("Письмо не найдено"))?;
            ensure!(
                l.editable(),
                "Письмо уже отправлено или статус требует проверки"
            );
            for a in &refs {
                if !l
                    .attachments
                    .iter()
                    .any(|old| old.id == a.id && old.name == a.name)
                {
                    l.attachments.push(a.clone());
                }
            }
        }
        self.validate()
    }
}
pub fn csv_workspace(path: &Path) -> Result<Workspace> {
    ensure!(path.metadata()?.len() <= 20 << 20, "CSV больше 20 МБ");
    let data =
        std::fs::read_to_string(path).map_err(|_| anyhow::anyhow!("Сохраните CSV в UTF-8"))?;
    let s = data.trim_start_matches('\u{feff}');
    let first = s.lines().next().unwrap_or_default();
    let delimiter = if first.matches(';').count() > first.matches(',').count() {
        b';'
    } else {
        b','
    };
    let mut reader = csv::ReaderBuilder::new()
        .delimiter(delimiter)
        .flexible(true)
        .from_reader(s.as_bytes());
    let columns: Vec<_> = reader
        .headers()?
        .iter()
        .map(|s| s.trim().to_lowercase())
        .collect();
    ensure!(
        columns.iter().any(|s| s == "company") && columns.iter().any(|s| s == "email"),
        "Нужны колонки Company и Email"
    );
    let mut w = Workspace::default();
    for row in reader.records() {
        let row = row?;
        let get = |name: &str| {
            columns
                .iter()
                .position(|s| s == name)
                .and_then(|i| row.get(i))
                .unwrap_or_default()
                .trim()
                .to_owned()
        };
        let mut l = Lead {
            id: new_id(),
            company: get("company"),
            email: get("email"),
            website: get("website"),
            city: get("city"),
            industry: get("industry"),
            status: "pending".into(),
            ..Default::default()
        };
        for name in [
            "overview",
            "description",
            "company description",
            "описание",
            "описание компании",
        ] {
            let v = get(name);
            if !v.is_empty() {
                l.description = v;
                break;
            }
        }
        if l.company.is_empty() && l.email.is_empty() {
            continue;
        }
        if l.company.is_empty() {
            l.company = l.email.clone();
        }
        w.leads.push(l);
        ensure!(w.leads.len() <= 10000, "Не больше 10 000 компаний");
    }
    if w.leads.is_empty() {
        bail!("В CSV нет компаний");
    }
    Ok(w)
}
