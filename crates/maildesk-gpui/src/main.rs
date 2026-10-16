#![cfg_attr(target_os = "windows", windows_subsystem = "windows")]
extern crate gpui_kit as gpui;
use gpui_kit::{
    component::{
        Disableable,
        button::{Button, ButtonVariants},
        input::{Input, InputState, Textarea, TextareaState},
    },
    *,
};
use maildesk_core::*;
use std::{
    collections::{BTreeSet, HashMap},
    sync::{
        Arc, Mutex,
        atomic::{AtomicBool, Ordering},
        mpsc,
    },
    time::Duration,
};

type SharedStore = Arc<Mutex<Store>>;
struct Snapshot {
    leads: Vec<Lead>,
}
impl Snapshot {
    fn from(s: &Store) -> Self {
        Self {
            leads: s.workspace.leads.clone(),
        }
    }
}
enum Event {
    Progress(Snapshot, String),
    Done(Snapshot, Result<String, String>),
}
struct MailDesk {
    store: SharedStore,
    leads: Vec<Lead>,
    selected: BTreeSet<String>,
    current: Option<Lead>,
    inputs: HashMap<&'static str, Entity<InputState>>,
    areas: HashMap<&'static str, Entity<TextareaState>>,
    tab: u8,
    filter: String,
    status: String,
    error: bool,
    busy: bool,
    stop: Arc<AtomicBool>,
    tx: mpsc::Sender<Event>,
    rx: mpsc::Receiver<Event>,
    confirmation: Option<(String, u8)>,
    test_recipient: Option<String>,
    refresh_editor: bool,
    refresh_template: bool,
    icon: Arc<Image>,
}
fn label(text: impl Into<SharedString>) -> Div {
    div()
        .text_sm()
        .text_color(rgb(0x67756e))
        .mb_1()
        .child(text.into())
}
impl MailDesk {
    fn new(store: Store, window: &mut Window, cx: &mut Context<Self>) -> Self {
        let cfg = store.settings.clone();
        let t = store.workspace.template.clone();
        let snapshot = Snapshot::from(&store);
        let test_recipient = store.test_recipient.clone();
        let (tx, rx) = mpsc::channel();
        let mut inputs = HashMap::new();
        let mut areas = HashMap::new();
        for (id, placeholder, value, masked) in [
            (
                "search",
                "Поиск по компании, адресу, описанию",
                "".into(),
                false,
            ),
            (
                "to",
                "email@example.com, another@example.com",
                "".into(),
                false,
            ),
            ("subject", "Тема письма", "".into(), false),
            ("host", "smtp.mail.ru", cfg.host, false),
            ("port", "465", cfg.port.to_string(), false),
            ("security", "ssl или starttls", cfg.security, false),
            ("username", "Адрес почты", cfg.username, false),
            ("password", "Пароль приложения Mail.ru", cfg.password, true),
            ("from", "От кого", cfg.from, false),
            ("senderName", "Имя отправителя", cfg.sender_name, false),
            ("apiKey", "Ключ DeepSeek", cfg.api_key, true),
            ("templateSubject", "Тема по умолчанию", t.subject, false),
        ] {
            let state = cx.new(|cx| {
                InputState::new(window, cx)
                    .placeholder(placeholder)
                    .default_value(value)
                    .masked(masked)
            });
            cx.observe(&state, |_, _, cx| cx.notify()).detach();
            inputs.insert(id, state);
        }
        for (id, placeholder, value, rows) in [
            ("body", "Текст письма", "".into(), 14),
            ("description", "Описание компании-получателя", "".into(), 4),
            (
                "senderProfile",
                "Факты о нашей компании, услуги, предложение и подпись",
                t.sender_profile,
                6,
            ),
            (
                "instructions",
                "Пожелания к персонализации и стилю",
                t.instructions,
                4,
            ),
            (
                "templateBody",
                "Письмо для компаний без описания",
                t.body,
                7,
            ),
        ] {
            let state = cx.new(|cx| {
                TextareaState::new(window, cx)
                    .placeholder(placeholder)
                    .default_value(value)
                    .rows(rows)
            });
            cx.observe(&state, |_, _, cx| cx.notify()).detach();
            areas.insert(id, state);
        }
        cx.spawn(async move |this, cx| {
            loop {
                cx.background_executor()
                    .timer(Duration::from_millis(100))
                    .await;
                let Some(this) = this.upgrade() else { break };
                this.update(cx, |app, cx| {
                    while let Ok(event) = app.rx.try_recv() {
                        match event {
                            Event::Progress(s, message) => {
                                app.leads = s.leads;
                                app.status = message;
                            }
                            Event::Done(s, result) => {
                                app.leads = s.leads;
                                app.refresh_editor = true;
                                app.busy = false;
                                app.error = result.is_err();
                                app.status = result.unwrap_or_else(|e| e);
                                app.selected.retain(|id| {
                                    app.leads.iter().any(|l| &l.id == id && l.selectable())
                                });
                            }
                        }
                        if let Some(current) = &mut app.current
                            && let Some(l) = app.leads.iter().find(|l| l.id == current.id)
                        {
                            current.attachments = l.attachments.clone();
                            current.status = l.status.clone();
                            current.error = l.error.clone();
                            current.generation_error = l.generation_error.clone();
                            current.source = l.source.clone();
                        }
                        cx.notify();
                    }
                });
            }
        })
        .detach();
        Self {
            store: Arc::new(Mutex::new(store)),
            leads: snapshot.leads,
            selected: BTreeSet::new(),
            current: None,
            inputs,
            areas,
            tab: 0,
            filter: "all".into(),
            status: "Готово к работе. Импортируйте CSV или откройте проект JSON.".into(),
            error: false,
            busy: false,
            stop: Arc::new(AtomicBool::new(false)),
            tx,
            rx,
            confirmation: None,
            test_recipient,
            refresh_editor: false,
            refresh_template: false,
            icon: Arc::new(Image::from_bytes(
                ImageFormat::Png,
                include_bytes!("../../../assets/maildesk.png").to_vec(),
            )),
        }
    }
    fn value(&self, key: &str, cx: &App) -> String {
        self.inputs[key].read(cx).value().to_string()
    }
    fn text(&self, key: &str, cx: &App) -> String {
        self.areas[key].read(cx).value().to_string()
    }
    fn set_input(&self, key: &str, value: String, window: &mut Window, cx: &mut Context<Self>) {
        self.inputs[key].update(cx, |s, cx| s.set_value(value, window, cx));
    }
    fn set_text(&self, key: &str, value: String, window: &mut Window, cx: &mut Context<Self>) {
        self.areas[key].update(cx, |s, cx| s.set_value(value, window, cx));
    }
    fn result<T>(&mut self, result: anyhow::Result<T>) -> Option<T> {
        match result {
            Ok(v) => {
                self.error = false;
                Some(v)
            }
            Err(e) => {
                self.status = e.to_string();
                self.error = true;
                None
            }
        }
    }
    fn save_editor(&mut self, cx: &App) -> bool {
        let Some(mut l) = self.current.clone() else {
            return true;
        };
        if !l.editable() {
            return true;
        }
        l.subject = self.value("subject", cx);
        l.body = self.text("body", cx);
        let addresses = self.value("to", cx);
        if l.kind == "message" {
            if addresses.trim().is_empty()
                && l.subject.trim().is_empty()
                && l.body.trim().is_empty()
                && l.attachments.is_empty()
            {
                return true;
            }
            let Some(recipients) = self.result(normalize_recipients(&addresses)) else {
                return false;
            };
            l.recipients = recipients;
        } else if addresses.trim().is_empty() {
            l.email.clear();
            l.description = self.text("description", cx);
        } else {
            l.description = self.text("description", cx);
            let Some(recipients) = self.result(normalize_recipients(&addresses)) else {
                return false;
            };
            if recipients.len() != 1 {
                self.status="У компании должен быть один адрес; для нескольких адресов используйте «Написать»".into();
                self.error = true;
                return false;
            }
            l.email = recipients[0].clone();
        }
        if l.source.is_empty() {
            l.source = "manual".into();
        }
        let result = self.store.lock().unwrap().edit(l.clone());
        if self.result(result).is_none() {
            return false;
        }
        self.leads = self.store.lock().unwrap().workspace.leads.clone();
        self.current = Some(l);
        true
    }
    fn open_lead(&mut self, id: Option<String>, window: &mut Window, cx: &mut Context<Self>) {
        if self.busy || !self.save_editor(cx) {
            return;
        }
        let l = id
            .and_then(|id| self.leads.iter().find(|l| l.id == id).cloned())
            .unwrap_or_else(|| Lead {
                id: new_id(),
                kind: "message".into(),
                company: "Обычное письмо".into(),
                status: "pending".into(),
                ..Default::default()
            });
        self.set_input("to", l.recipients().join(", "), window, cx);
        self.set_input("subject", l.subject.clone(), window, cx);
        self.set_text("body", l.body.clone(), window, cx);
        self.set_text("description", l.description.clone(), window, cx);
        self.current = Some(l);
        self.tab = 0;
        cx.notify();
    }
    fn job(
        &mut self,
        message: &str,
        f: impl FnOnce(&mut Store) -> anyhow::Result<String> + Send + 'static,
        cx: &mut Context<Self>,
    ) {
        if self.busy {
            return;
        }
        self.busy = true;
        self.error = false;
        self.status = message.into();
        self.confirmation = None;
        let store = self.store.clone();
        let tx = self.tx.clone();
        std::thread::spawn(move || {
            let mut s = store.lock().unwrap();
            let result = f(&mut s).map_err(|e| e.to_string());
            let _ = tx.send(Event::Done(Snapshot::from(&s), result));
        });
        cx.notify();
    }
    fn import(&mut self, cx: &mut Context<Self>) {
        if !self.save_editor(cx) {
            return;
        }
        self.refresh_template = true;
        self.job(
            "Выберите CSV или проект JSON",
            |s| {
                let Some(path) = rfd::FileDialog::new()
                    .add_filter("CSV или проект JSON", &["csv", "json"])
                    .pick_file()
                else {
                    return Ok("Импорт отменён".into());
                };
                let (added, skipped) = s.import(&path)?;
                Ok(format!(
                    "Добавлено: {added}; обновлено или пропущено: {skipped}"
                ))
            },
            cx,
        );
    }
    fn export(&mut self, report: bool, cx: &mut Context<Self>) {
        if !self.save_editor(cx) {
            return;
        }
        self.job(
            "Сохранение файла",
            move |s| {
                let Some(path) = rfd::FileDialog::new()
                    .set_file_name(if report {
                        "MailDesk-report.csv"
                    } else {
                        "MailDesk-project.json"
                    })
                    .save_file()
                else {
                    return Ok("Экспорт отменён".into());
                };
                anyhow::ensure!(
                    path != s.dir.join("credentials.json"),
                    "Выберите другое имя файла"
                );
                if report {
                    s.report(&path)?;
                } else {
                    s.export(&path)?;
                }
                Ok("Файл сохранён".into())
            },
            cx,
        );
    }
    fn attach(&mut self, bulk: bool, cx: &mut Context<Self>) {
        if !self.save_editor(cx) {
            return;
        }
        let ids: Vec<_> = if bulk {
            self.selected.iter().cloned().collect()
        } else {
            self.current.iter().map(|l| l.id.clone()).collect()
        };
        if ids.is_empty() {
            self.status = "Выберите письма".into();
            return;
        }
        self.job(
            "Выберите вложения",
            move |s| {
                let Some(paths) = rfd::FileDialog::new().pick_files() else {
                    return Ok("Добавление отменено".into());
                };
                s.change(|w| w.add_files(&ids, &paths))?;
                Ok("Файлы прикреплены".into())
            },
            cx,
        );
    }
    fn generate(&mut self, cx: &mut Context<Self>) {
        if !self.save_editor(cx) {
            return;
        }
        let ids: Vec<_> = self
            .selected
            .iter()
            .filter(|id| {
                self.leads
                    .iter()
                    .any(|l| &l.id == *id && l.kind != "message")
            })
            .cloned()
            .collect();
        if ids.is_empty() {
            self.status = "Выберите компании для генерации".into();
            return;
        }
        self.busy = true;
        self.error = false;
        self.confirmation = None;
        self.stop.store(false, Ordering::SeqCst);
        // Unload the editor so a completed generation cannot be overwritten by its old fields.
        self.current = None;
        let store = self.store.clone();
        let tx = self.tx.clone();
        let stop = self.stop.clone();
        std::thread::spawn(move || {
            let mut generated = 0;
            let mut templates = 0;
            let mut errors = 0;
            let mut last_error = String::new();
            for (i, id) in ids.iter().enumerate() {
                if stop.load(Ordering::SeqCst) {
                    break;
                }
                let mut s = store.lock().unwrap();
                match s.generate_one(id) {
                    Ok(source) => {
                        if source == "template" {
                            templates += 1
                        } else {
                            generated += 1
                        }
                    }
                    Err(e) => {
                        errors += 1;
                        last_error = e.to_string();
                    }
                }
                let _ = tx.send(Event::Progress(
                    Snapshot::from(&s),
                    format!("Генерация: {} / {}", i + 1, ids.len()),
                ));
            }
            let s = store.lock().unwrap();
            let summary = format!(
                "DeepSeek: {generated}; шаблон без описания: {templates}; ошибок: {errors}{}",
                if last_error.is_empty() {
                    String::new()
                } else {
                    format!(". {last_error}")
                }
            );
            let _ = tx.send(Event::Done(
                Snapshot::from(&s),
                if errors > 0 {
                    Err(summary)
                } else {
                    Ok(summary)
                },
            ));
        });
        cx.notify();
    }
    fn prepare(&mut self, cx: &mut Context<Self>) {
        if !self.save_editor(cx) {
            return;
        }
        let ids = self.selected.iter().cloned().collect();
        let result = self.store.lock().unwrap().prepare(ids);
        if let Some(token) = self.result(result) {
            self.confirmation = Some((token, 1));
        }
        cx.notify();
    }
    fn confirm(&mut self, cx: &mut Context<Self>) {
        let Some((token, stage)) = self.confirmation.clone() else {
            return;
        };
        if stage == 1 {
            let result = self.store.lock().unwrap().confirm(&token);
            if self.result(result).is_some() {
                self.confirmation = Some((token, 2));
            } else {
                self.confirmation = None;
            }
            cx.notify();
            return;
        }
        let result = self.store.lock().unwrap().authorize(&token);
        self.confirmation = None;
        let Some(mut permit) = self.result(result) else {
            return;
        };
        self.busy = true;
        self.error = false;
        self.stop.store(false, Ordering::SeqCst);
        let store = self.store.clone();
        let tx = self.tx.clone();
        let stop = self.stop.clone();
        std::thread::spawn(move || {
            let mut sent = 0;
            let mut errors = 0;
            while !permit.is_empty() && !stop.load(Ordering::SeqCst) {
                {
                    let mut s = store.lock().unwrap();
                    if s.send_next(&mut permit).is_ok() {
                        sent += 1;
                    } else {
                        errors += 1;
                    }
                    let _ = tx.send(Event::Progress(
                        Snapshot::from(&s),
                        format!("Отправлено: {sent}; ошибок: {errors}"),
                    ));
                }
                if !permit.is_empty() {
                    std::thread::sleep(Duration::from_secs(1));
                }
            }
            let s = store.lock().unwrap();
            let summary = format!(
                "Отправлено: {sent}; ошибок: {errors}. {}",
                if stop.load(Ordering::SeqCst) {
                    "Очередь остановлена"
                } else {
                    "Очередь завершена"
                }
            );
            let _ = tx.send(Event::Done(
                Snapshot::from(&s),
                if errors > 0 {
                    Err(summary)
                } else {
                    Ok(summary)
                },
            ));
        });
        cx.notify();
    }
    fn save_template(&mut self, cx: &mut Context<Self>) {
        let t = Template {
            subject: self.value("templateSubject", cx),
            body: self.text("templateBody", cx),
            instructions: self.text("instructions", cx),
            sender_profile: self.text("senderProfile", cx),
        };
        let result = self.store.lock().unwrap().change(|w| {
            w.template = t;
            Ok(())
        });
        if self.result(result).is_some() {
            self.status = "Предложение и шаблон сохранены".into();
        }
        cx.notify();
    }
    fn settings(&self, cx: &App) -> anyhow::Result<Settings> {
        Ok(Settings {
            host: self.value("host", cx).trim().into(),
            port: self
                .value("port", cx)
                .trim()
                .parse()
                .map_err(|_| anyhow::anyhow!("Неверный порт"))?,
            security: self.value("security", cx).trim().to_lowercase(),
            username: self.value("username", cx).trim().into(),
            password: self.value("password", cx),
            from: self.value("from", cx).trim().into(),
            sender_name: self.value("senderName", cx),
            api_key: self.value("apiKey", cx),
        })
    }
    fn save_settings(&mut self, cx: &mut Context<Self>) {
        let Some(settings) = self.result(self.settings(cx)) else {
            return;
        };
        let result = self.store.lock().unwrap().save_settings(settings);
        if self.result(result).is_some() {
            self.status = "Подключения сохранены".into();
        }
        cx.notify();
    }
    fn visible(&self, cx: &App) -> Vec<Lead> {
        let q = self.value("search", cx).to_lowercase();
        self.leads
            .iter()
            .filter(|l| {
                (self.filter == "all"
                    || (self.filter == "pending" && l.status == "pending")
                    || self.filter == l.status)
                    && (q.is_empty()
                        || format!(
                            "{} {} {} {}",
                            l.company,
                            l.email,
                            l.recipients.join(" "),
                            l.description
                        )
                        .to_lowercase()
                        .contains(&q))
            })
            .cloned()
            .collect()
    }
    fn sidebar(&self, cx: &mut Context<Self>) -> impl IntoElement {
        let mut side = div()
            .w(px(206.))
            .h_full()
            .bg(rgb(0x163c30))
            .text_color(rgb(0xeaf4ed))
            .p_5()
            .flex()
            .flex_col()
            .gap_3()
            .child(img(self.icon.clone()).w(px(70.)).h(px(70.)))
            .child(
                div()
                    .text_2xl()
                    .font_weight(FontWeight::BOLD)
                    .child("MailDesk"),
            )
            .child(
                div()
                    .text_xs()
                    .text_color(rgb(0xa3c6b4))
                    .child("ПОЧТА ДЛЯ БИЗНЕСА"),
            )
            .child(div().h_6());
        for (tab, title) in [
            (0, "Письма и компании"),
            (1, "Наша компания"),
            (2, "Подключения"),
        ] {
            side = side.child(
                Button::new(("tab", tab as usize))
                    .label(title)
                    .disabled(self.busy)
                    .on_click(cx.listener(move |app, _, _, cx| {
                        if app.save_editor(cx) {
                            app.tab = tab;
                        }
                        cx.notify();
                    })),
            );
        }
        side.child(div().flex_1()).child(
            div()
                .text_sm()
                .text_color(rgb(0xa3c6b4))
                .child("Локальные проекты\nRust + GPUI · 2.0"),
        )
    }
    fn editor(&self, cx: &mut Context<Self>) -> AnyElement {
        let Some(l) = &self.current else {
            return div()
                .flex_1()
                .h_full()
                .flex()
                .flex_col()
                .items_center()
                .justify_center()
                .gap_3()
                .text_color(rgb(0x6c7c72))
                .child(div().text_2xl().child("Письмо начинается здесь"))
                .child("Выберите компанию, чтобы прочитать и изменить черновик.")
                .child(
                    Button::new("empty-compose")
                        .label("Написать письмо")
                        .disabled(self.busy)
                        .on_click(cx.listener(|app, _, w, cx| app.open_lead(None, w, cx))),
                )
                .into_any_element();
        };
        let disabled = self.busy || !l.editable();
        let mut content = div()
            .id("editor-scroll")
            .overflow_y_scroll()
            .flex_1()
            .h_full()
            .p_6()
            .flex()
            .flex_col()
            .gap_3()
            .child(
                div()
                    .text_xl()
                    .font_weight(FontWeight::SEMIBOLD)
                    .child(l.company.clone()),
            )
            .child(label(if l.kind == "message" {
                "Обычное письмо · все получатели видят список «Кому»"
            } else {
                "Черновик компании"
            }))
            .child(label("Кому"))
            .child(Input::new(&self.inputs["to"]).disabled(disabled))
            .child(label("Тема"))
            .child(Input::new(&self.inputs["subject"]).disabled(disabled))
            .child(label("Текст письма"))
            .child(
                Textarea::new(&self.areas["body"])
                    .h(px(300.))
                    .disabled(disabled),
            );
        if l.kind != "message" {
            content = content.child(label("Описание компании-получателя")).child(
                Textarea::new(&self.areas["description"])
                    .h(px(115.))
                    .disabled(disabled),
            );
        }
        if !l.generation_error.is_empty() {
            content = content.child(
                div()
                    .text_sm()
                    .text_color(rgb(0xb53e36))
                    .child(l.generation_error.clone()),
            );
        }
        if !l.error.is_empty() {
            content = content.child(
                div()
                    .text_sm()
                    .text_color(rgb(0xb53e36))
                    .child(l.error.clone()),
            );
        }
        if !l.source.is_empty() {
            content = content.child(label(match l.source.as_str() {
                "deepseek" => "✦ DeepSeek",
                "template" => "Шаблон · нет описания",
                _ => "Отредактировано вручную",
            }));
        }
        let mut files = div().flex().flex_col().gap_2();
        for (i, a) in l.attachments.iter().enumerate() {
            let id = l.id.clone();
            let file_id = a.id.clone();
            let name = a.name.clone();
            let row =
                div()
                    .flex()
                    .gap_2()
                    .items_center()
                    .child(div().flex_1().text_sm().child(format!(
                        "{} · {} КБ",
                        a.name,
                        a.size.div_ceil(1024)
                    )));
            files = files.child(
                row.child(
                    Button::new(("save-file", i))
                        .label("Сохранить")
                        .disabled(self.busy)
                        .on_click(cx.listener(move |app, _, _, cx| {
                            let file_id = file_id.clone();
                            let name = name.clone();
                            app.job(
                                "Сохранение вложения",
                                move |s| {
                                    let Some(path) =
                                        rfd::FileDialog::new().set_file_name(&name).save_file()
                                    else {
                                        return Ok("Отменено".into());
                                    };
                                    s.save_attachment(&file_id, &path)?;
                                    Ok("Вложение сохранено".into())
                                },
                                cx,
                            );
                        })),
                )
                .child(
                    Button::new(("remove-file", i))
                        .label("Убрать")
                        .disabled(disabled)
                        .on_click(cx.listener(move |app, _, _, cx| {
                            if !app.save_editor(cx) {
                                return;
                            }
                            let id = id.clone();
                            app.job(
                                "Удаление вложения",
                                move |s| {
                                    s.change(|w| {
                                        w.leads
                                            .iter_mut()
                                            .find(|l| l.id == id)
                                            .unwrap()
                                            .attachments
                                            .remove(i);
                                        w.prune_files();
                                        Ok(())
                                    })?;
                                    Ok("Вложение удалено".into())
                                },
                                cx,
                            );
                        })),
                ),
            );
        }
        content = content.child(files).child(
            div()
                .flex()
                .gap_2()
                .child(
                    Button::new("attach-editor")
                        .label("Прикрепить файлы")
                        .disabled(disabled)
                        .on_click(cx.listener(|a, _, _, cx| a.attach(false, cx))),
                )
                .child(
                    Button::new("save-draft")
                        .primary()
                        .label("Сохранить черновик")
                        .disabled(disabled)
                        .on_click(cx.listener(|a, _, _, cx| {
                            if a.save_editor(cx) {
                                a.status = "Черновик сохранён".into();
                            }
                            cx.notify();
                        })),
                ),
        );
        if l.status == "unknown" {
            let id1 = l.id.clone();
            let id2 = l.id.clone();
            content = content
                .child(label(
                    "Проверьте папку «Отправленные» у провайдера и выберите результат:",
                ))
                .child(
                    div()
                        .flex()
                        .gap_2()
                        .child(
                            Button::new("resolve-sent")
                                .label("Письмо доставлено")
                                .disabled(self.busy)
                                .on_click(cx.listener(move |app, _, _, cx| {
                                    let id = id1.clone();
                                    app.job(
                                        "Сохранение статуса",
                                        move |s| {
                                            s.resolve_unknown(&id, true)?;
                                            Ok("Отмечено отправленным".into())
                                        },
                                        cx,
                                    );
                                })),
                        )
                        .child(
                            Button::new("resolve-pending")
                                .label("Не отправлялось — разрешить повтор")
                                .disabled(self.busy)
                                .on_click(cx.listener(move |app, _, _, cx| {
                                    let id = id2.clone();
                                    app.job(
                                        "Сохранение статуса",
                                        move |s| {
                                            s.resolve_unknown(&id, false)?;
                                            Ok("Повторная отправка разрешена".into())
                                        },
                                        cx,
                                    );
                                })),
                        ),
                );
        }
        content.into_any_element()
    }
    fn mail_view(&self, cx: &mut Context<Self>) -> impl IntoElement {
        let visible = self.visible(cx);
        let mut list = div()
            .id("lead-list")
            .overflow_y_scroll()
            .flex_1()
            .min_h_0()
            .flex()
            .flex_col()
            .gap_2()
            .p_3();
        for l in &visible {
            let id = l.id.clone();
            let open = id.clone();
            let selectable = l.selectable();
            let chosen = self.selected.contains(&id);
            let active = self.current.as_ref().is_some_and(|old| old.id == id);
            let color = match l.status.as_str() {
                "failed" => 0xb53e36,
                "sent" => 0x929d97,
                "unknown" => 0xb48322,
                _ => 0x21674f,
            };
            let badge = match l.status.as_str() {
                "sent" => "Отправлено",
                "failed" => "Ошибка",
                "sending" => "Отправляется",
                "unknown" => "Проверьте доставку",
                _ => "Не отправлено",
            };
            list = list.child(
                div()
                    .flex()
                    .gap_2()
                    .p_3()
                    .rounded_md()
                    .border_1()
                    .border_color(rgb(if active { 0x9fbdad } else { 0xe3eae5 }))
                    .bg(rgb(if l.status == "sent" {
                        0xedf0ee
                    } else if active {
                        0xf0f6f1
                    } else {
                        0xffffff
                    }))
                    .child(
                        Button::new(SharedString::from(format!("check-{id}")))
                            .label(if chosen {
                                "✓"
                            } else if l.status == "sent" {
                                "—"
                            } else {
                                "□"
                            })
                            .text_color(rgb(color))
                            .disabled(self.busy || !selectable)
                            .on_click(cx.listener(move |a, _, _, cx| {
                                if !a.selected.remove(&id) {
                                    a.selected.insert(id.clone());
                                }
                                a.confirmation = None;
                                cx.notify();
                            })),
                    )
                    .child(
                        div()
                            .id(SharedString::from(format!("lead-{open}")))
                            .flex_1()
                            .min_w_0()
                            .overflow_hidden()
                            .cursor_pointer()
                            .on_click(cx.listener(move |a, _, w, cx| {
                                a.open_lead(Some(open.clone()), w, cx)
                            }))
                            .child(
                                div()
                                    .font_weight(FontWeight::MEDIUM)
                                    .truncate()
                                    .child(l.company.clone()),
                            )
                            .child(
                                div()
                                    .text_xs()
                                    .text_color(rgb(0x67756e))
                                    .child(l.recipients().join(", ")),
                            )
                            .child(div().text_xs().text_color(rgb(color)).mt_1().child(format!(
                                "{badge}{}",
                                if l.attachments.is_empty() {
                                    String::new()
                                } else {
                                    format!(" · файлов: {}", l.attachments.len())
                                }
                            ))),
                    ),
            );
        }
        if visible.is_empty() {
            list = list.child(
                div()
                    .p_6()
                    .text_sm()
                    .text_color(rgb(0x67756e))
                    .child("Здесь пока нет писем. Загрузите CSV или измените фильтр."),
            );
        }
        let mut filters = div().flex().flex_wrap().gap_1();
        for (key, title) in [
            ("all", "Все"),
            ("pending", "Не отправлены"),
            ("failed", "Ошибки"),
            ("sent", "Отправлены"),
            ("unknown", "Проверить"),
        ] {
            let count = self
                .leads
                .iter()
                .filter(|l| key == "all" || l.status == key)
                .count();
            filters = filters.child(
                Button::new(SharedString::from(format!("filter-{key}")))
                    .label(format!("{title} {count}"))
                    .on_click(cx.listener(move |a, _, _, cx| {
                        a.filter = key.into();
                        cx.notify();
                    })),
            );
        }
        div()
            .flex()
            .flex_col()
            .size_full()
            .gap_3()
            .child(
                div()
                    .flex()
                    .gap_2()
                    .flex_wrap()
                    .items_center()
                    .child(
                        Button::new("import")
                            .label("Загрузить CSV / JSON")
                            .disabled(self.busy)
                            .on_click(cx.listener(|a, _, _, cx| a.import(cx))),
                    )
                    .child(
                        Button::new("compose")
                            .label("Написать")
                            .disabled(self.busy)
                            .on_click(cx.listener(|a, _, w, cx| a.open_lead(None, w, cx))),
                    )
                    .child(
                        Button::new("export")
                            .label("Проект JSON")
                            .disabled(self.busy)
                            .on_click(cx.listener(|a, _, _, cx| a.export(false, cx))),
                    )
                    .child(
                        Button::new("report")
                            .label("Отчёт CSV")
                            .disabled(self.busy)
                            .on_click(cx.listener(|a, _, _, cx| a.export(true, cx))),
                    ),
            )
            .child(filters)
            .child(
                div()
                    .flex()
                    .gap_2()
                    .flex_wrap()
                    .items_center()
                    .child(
                        Button::new("select-all")
                            .label("Выделить все в фильтре")
                            .disabled(self.busy)
                            .on_click(cx.listener(|a, _, _, cx| {
                                for l in a.visible(cx) {
                                    if l.selectable() {
                                        a.selected.insert(l.id);
                                    }
                                }
                                a.confirmation = None;
                                cx.notify();
                            })),
                    )
                    .child(
                        Button::new("unselect")
                            .label("Снять выделение")
                            .disabled(self.busy)
                            .on_click(cx.listener(|a, _, _, cx| {
                                a.selected.clear();
                                a.confirmation = None;
                                cx.notify();
                            })),
                    )
                    .child(
                        Button::new("generate")
                            .label("✦ Создать письма")
                            .disabled(self.busy || self.selected.is_empty())
                            .on_click(cx.listener(|a, _, _, cx| a.generate(cx))),
                    )
                    .child(
                        Button::new("attach-bulk")
                            .label("Вложения выбранным")
                            .disabled(self.busy || self.selected.is_empty())
                            .on_click(cx.listener(|a, _, _, cx| a.attach(true, cx))),
                    ),
            )
            .child(
                div()
                    .flex()
                    .flex_1()
                    .min_h_0()
                    .rounded_lg()
                    .border_1()
                    .border_color(rgb(0xdde6df))
                    .bg(rgb(0xffffff))
                    .overflow_hidden()
                    .child(
                        div()
                            .w(px(355.))
                            .flex()
                            .flex_col()
                            .border_r_1()
                            .border_color(rgb(0xe0e8e2))
                            .child(div().p_3().child(Input::new(&self.inputs["search"])))
                            .child(list),
                    )
                    .child(self.editor(cx)),
            )
            .child(
                div()
                    .flex()
                    .items_center()
                    .gap_3()
                    .child(
                        div()
                            .flex_1()
                            .text_sm()
                            .text_color(rgb(0x67756e))
                            .child(format!("Выбрано писем: {}", self.selected.len())),
                    )
                    .child(
                        Button::new("stop")
                            .label("Остановить очередь")
                            .disabled(!self.busy)
                            .on_click(cx.listener(|a, _, _, cx| {
                                a.stop.store(true, Ordering::SeqCst);
                                a.status = "Останавливаем после текущего письма…".into();
                                cx.notify();
                            })),
                    )
                    .child(
                        Button::new("send")
                            .primary()
                            .label("Отправить выбранные")
                            .disabled(self.busy || self.selected.is_empty())
                            .on_click(cx.listener(|a, _, _, cx| a.prepare(cx))),
                    ),
            )
    }
    fn template_view(&self, cx: &mut Context<Self>) -> impl IntoElement {
        div().id("template-scroll").overflow_y_scroll().size_full().flex().flex_col().gap_3().max_w(px(850.))
            .child(div().text_2xl().font_weight(FontWeight::SEMIBOLD).child("Наша компания и предложение"))
            .child(label("Отсюда DeepSeek берёт факты о вас. Описание получателя используется отдельно."))
            .child(Textarea::new(&self.areas["senderProfile"]).h(px(160.)).disabled(self.busy))
            .child(label("Инструкции для персональных писем")).child(Textarea::new(&self.areas["instructions"]).h(px(110.)).disabled(self.busy))
            .child(div().text_lg().mt_3().child("Письмо по умолчанию"))
            .child(label("Используется только если описания компании нет. Переменные: {company}, {city}, {industry}, {website}, {description}."))
            .child(Input::new(&self.inputs["templateSubject"]).disabled(self.busy))
            .child(Textarea::new(&self.areas["templateBody"]).h(px(180.)).disabled(self.busy))
            .child(Button::new("save-template").primary().label("Сохранить").disabled(self.busy).on_click(cx.listener(|a,_,_,cx|a.save_template(cx))))
    }
    fn settings_view(&self, cx: &mut Context<Self>) -> impl IntoElement {
        let mut form = div()
            .id("settings-scroll")
            .overflow_y_scroll()
            .size_full()
            .max_w(px(700.))
            .flex()
            .flex_col()
            .gap_2()
            .child(
                div()
                    .text_2xl()
                    .font_weight(FontWeight::SEMIBOLD)
                    .mb_3()
                    .child("Подключения"),
            );
        for (key, title) in [
            ("host", "SMTP-хост"),
            ("port", "Порт"),
            ("security", "Защита: ssl или starttls"),
            ("username", "Логин SMTP"),
            ("password", "Пароль приложения"),
            ("from", "Адрес отправителя"),
            ("senderName", "Имя отправителя"),
            ("apiKey", "Ключ DeepSeek"),
        ] {
            form = form
                .child(label(title))
                .child(Input::new(&self.inputs[key]).disabled(self.busy));
        }
        form.child(
            div()
                .mt_3()
                .flex()
                .gap_2()
                .child(
                    Button::new("save-settings")
                        .primary()
                        .label("Сохранить подключения")
                        .disabled(self.busy)
                        .on_click(cx.listener(|a, _, _, cx| a.save_settings(cx))),
                )
                .child(
                    Button::new("test-smtp")
                        .label("Проверить SMTP")
                        .disabled(self.busy)
                        .on_click(cx.listener(|a, _, _, cx| {
                            let Some(settings) = a.result(a.settings(cx)) else {
                                return;
                            };
                            a.job(
                                "Проверяем вход SMTP без отправки письма",
                                move |_| {
                                    test_smtp(&settings)?;
                                    Ok("Подключение и вход SMTP работают. Письмо не отправлялось."
                                        .into())
                                },
                                cx,
                            );
                        })),
                ),
        )
        .child(label(
            "Подключения сохраняются на этом компьютере. В экспорт проекта входят письма и файлы.",
        ))
    }
}
impl Render for MailDesk {
    fn render(&mut self, window: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        if self.refresh_template && !self.busy {
            self.refresh_template = false;
            let t = self.store.lock().unwrap().workspace.template.clone();
            self.set_input("templateSubject", t.subject, window, cx);
            self.set_text("templateBody", t.body, window, cx);
            self.set_text("senderProfile", t.sender_profile, window, cx);
            self.set_text("instructions", t.instructions, window, cx);
        }
        if self.refresh_editor {
            self.refresh_editor = false;
            if let Some(id) = self.current.as_ref().map(|l| l.id.clone())
                && let Some(l) = self.leads.iter().find(|l| l.id == id).cloned()
            {
                self.set_input("to", l.recipients().join(", "), window, cx);
                self.set_input("subject", l.subject.clone(), window, cx);
                self.set_text("body", l.body.clone(), window, cx);
                self.set_text("description", l.description.clone(), window, cx);
                self.current = Some(l);
            }
        }
        let content = match self.tab {
            1 => self.template_view(cx).into_any_element(),
            2 => self.settings_view(cx).into_any_element(),
            _ => self.mail_view(cx).into_any_element(),
        };
        let mut root = div()
            .relative()
            .size_full()
            .flex()
            .bg(rgb(0xf7f9f6))
            .text_color(rgb(0x1a3024))
            .text_base()
            .child(self.sidebar(cx))
            .child(
                div()
                    .flex_1()
                    .min_w_0()
                    .h_full()
                    .p_5()
                    .flex()
                    .flex_col()
                    .gap_3()
                    .child(div().flex_1().min_h_0().child(content))
                    .child(
                        div()
                            .flex_shrink_0()
                            .text_sm()
                            .text_color(rgb(if self.error { 0xb53e36 } else { 0x607366 }))
                            .child(self.status.clone()),
                    ),
            );
        if let Some(test) = &self.test_recipient {
            root = root.child(
                div()
                    .absolute()
                    .top_0()
                    .right_4()
                    .text_xs()
                    .text_color(rgb(0x8a681e))
                    .child(format!("Тестовая отправка: {test}")),
            );
        }
        if let Some((_, stage)) = &self.confirmation {
            let recipients: Vec<_> = self
                .leads
                .iter()
                .filter(|l| self.selected.contains(&l.id))
                .flat_map(|l| l.recipients())
                .collect();
            root=root.child(div().absolute().inset_0().bg(rgba(0x10281caa)).flex().items_center().justify_center()
                .child(div().w(px(540.)).max_h(px(650.)).p_6().rounded_xl().bg(rgb(0xffffff)).flex().flex_col().gap_4()
                    .child(div().text_2xl().font_weight(FontWeight::SEMIBOLD).child(if *stage==1{"Подтвердите отправку"}else{"Точно подтверждаете?"}))
                    .child(format!("Писем: {}. Получателей: {}. Вложения будут отправлены вместе с письмами.",self.selected.len(),recipients.len()))
                    .child(div().id("confirmation-recipients").max_h(px(250.)).overflow_y_scroll().text_sm().child(recipients.join("\n")))
                    .child(div().flex().gap_3().justify_end()
                        .child(Button::new("cancel-confirmation").label("Отмена").on_click(cx.listener(|a,_,_,cx|{a.confirmation=None;cx.notify();})))
                        .child(Button::new("confirm-send").primary().label(if *stage==1{"Подтвердить"}else{"Да, точно подтверждаю"}).on_click(cx.listener(|a,_,_,cx|a.confirm(cx)))))));
        }
        root
    }
}
fn main() {
    let store = Options::from_args().and_then(Store::open);
    let store = match store {
        Ok(s) => s,
        Err(e) => {
            rfd::MessageDialog::new()
                .set_title("MailDesk")
                .set_description(e.to_string())
                .set_level(rfd::MessageLevel::Error)
                .show();
            std::process::exit(1);
        }
    };
    gpui_kit::application()
        .with_assets(gpui_kit::assets::Assets)
        .run(move |cx| {
            gpui_kit::init(cx);
            let options = WindowOptions {
                window_bounds: Some(WindowBounds::Windowed(Bounds::centered(
                    None,
                    size(px(1320.), px(900.)),
                    cx,
                ))),
                window_min_size: Some(size(px(1060.), px(720.))),
                titlebar: Some(TitlebarOptions {
                    title: Some("MailDesk".into()),
                    ..Default::default()
                }),
                app_id: Some("local.maildesk.app".into()),
                icon: Some(Arc::new(
                    image::load_from_memory(include_bytes!("../../../assets/maildesk.png"))
                        .expect("app icon")
                        .into_rgba8(),
                )),
                ..Default::default()
            };
            let (_, view) = gpui_kit::open_window(options, cx, move |window, cx| {
                let view = cx.new(|cx| MailDesk::new(store, window, cx));
                let weak = view.downgrade();
                window.on_window_should_close(cx, move |_, cx| {
                    let Some(view) = weak.upgrade() else {
                        return true;
                    };
                    view.update(cx, |app, cx| {
                        if app.busy {
                            app.stop.store(true, Ordering::SeqCst);
                            app.status =
                                "Дождитесь завершения текущей операции и закройте окно ещё раз"
                                    .into();
                            cx.notify();
                            false
                        } else if app.save_editor(cx) {
                            cx.quit();
                            true
                        } else {
                            cx.notify();
                            false
                        }
                    })
                });
                view
            })
            .expect("open MailDesk");
            let _ = view;
            cx.activate(true);
        });
}
