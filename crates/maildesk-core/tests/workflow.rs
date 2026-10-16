use maildesk_core::*;
use std::path::Path;
fn lead() -> Lead {
    Lead {
        id: new_id(),
        company: "Тестовый завод".into(),
        email: "ddkritsin@gmail.com".into(),
        description: "Производит вентиляционные установки".into(),
        subject: "Сотрудничество".into(),
        body: "Здравствуйте!".into(),
        status: "pending".into(),
        ..Default::default()
    }
}
fn settings() -> Settings {
    Settings {
        username: "fixture@example.test".into(),
        password: "local-fixture-only".into(),
        from: "fixture@example.test".into(),
        ..Default::default()
    }
}
fn store(dir: &Path) -> Store {
    let mut s = Store::open(Options {
        data_dir: Some(dir.into()),
        test_recipient: Some("ddkritsin@gmail.com".into()),
        ..Default::default()
    })
    .unwrap();
    s.save_settings(settings()).unwrap();
    s.change(|w| {
        w.leads.push(lead());
        Ok(())
    })
    .unwrap();
    s
}
#[test]
fn two_confirmations_are_required_and_single_use() {
    let dir = tempfile::tempdir().unwrap();
    let mut s = store(dir.path());
    let id = s.workspace.leads[0].id.clone();
    let token = s.prepare(vec![id.clone()]).unwrap();
    assert!(s.authorize(&token).is_err());
    let token = s.prepare(vec![id.clone()]).unwrap();
    s.confirm(&token).unwrap();
    let permit = s.authorize(&token).unwrap();
    assert!(!permit.is_empty());
    assert!(s.authorize(&token).is_err());
    let token = s.prepare(vec![id]).unwrap();
    s.confirm(&token).unwrap();
    s.workspace.leads[0].body.push_str("изменено");
    assert!(s.authorize(&token).is_err());
}
#[test]
fn guard_checks_every_recipient_and_sent_history() {
    let dir = tempfile::tempdir().unwrap();
    let mut s = store(dir.path());
    let id = s.workspace.leads[0].id.clone();
    s.workspace.leads[0].kind = "message".into();
    s.workspace.leads[0].recipients =
        vec!["ddkritsin@gmail.com".into(), "other@example.test".into()];
    assert!(s.prepare(vec![id.clone()]).is_err());
    s.workspace.leads[0].kind = String::new();
    s.workspace.sent.insert("ddkritsin@gmail.com".into(), now());
    assert!(s.prepare(vec![id]).is_err());
}
#[test]
fn csv_aliases_bom_semicolon_dedup_and_history() {
    let dir = tempfile::tempdir().unwrap();
    let csv = dir.path().join("companies.csv");
    std::fs::write(&csv,"\u{feff}Company;Email;Overview\nЗавод;ddkritsin@gmail.com;Вентиляция\nБез почты;;Описание\n").unwrap();
    let mut w = csv_workspace(&csv).unwrap();
    assert_eq!(w.leads[0].description, "Вентиляция");
    assert_eq!(w.leads.len(), 2);
    let again = csv_workspace(&csv).unwrap();
    assert_eq!(w.merge(again).unwrap(), (0, 2));
    let mut imported = Workspace::default();
    imported.sent.insert("ddkritsin@gmail.com".into(), now());
    w.merge(imported).unwrap();
    assert_eq!(w.leads[0].status, "sent");
}
#[test]
fn attachments_roundtrip_and_transaction_rollback() {
    let dir = tempfile::tempdir().unwrap();
    let mut s = store(dir.path());
    s.change(|w| {
        w.template.sender_profile = "Наши контроллеры".into();
        Ok(())
    })
    .unwrap();
    let id = s.workspace.leads[0].id.clone();
    let path = dir.path().join("предложение.txt");
    std::fs::write(&path, b"test attachment").unwrap();
    s.change(|w| w.add_files(std::slice::from_ref(&id), std::slice::from_ref(&path)))
        .unwrap();
    s.change(|w| w.add_files(std::slice::from_ref(&id), &[path]))
        .unwrap();
    assert_eq!(s.workspace.files.len(), 1);
    assert_eq!(s.workspace.leads[0].attachments.len(), 1);
    let export = dir.path().join("transfer.json");
    s.export(&export).unwrap();
    let text = std::fs::read_to_string(&export).unwrap();
    assert!(!text.contains("local-fixture-only"));
    assert!(!text.contains("apiKey"));
    let mut portable: Workspace = serde_json::from_str(&text).unwrap();
    portable.validate().unwrap();
    portable.files.values_mut().next().unwrap().push('x');
    assert!(portable.validate().is_err());
    let old = s.workspace.leads[0].subject.clone();
    assert!(
        s.change::<()>(|w| {
            w.leads[0].subject = "changed".into();
            anyhow::bail!("rollback")
        })
        .is_err()
    );
    assert_eq!(s.workspace.leads[0].subject, old);
    let other = tempfile::tempdir().unwrap();
    let mut target = Store::open(Options {
        data_dir: Some(other.path().into()),
        ..Default::default()
    })
    .unwrap();
    target.import(&export).unwrap();
    assert_eq!(target.workspace.template.sender_profile, "Наши контроллеры");
    assert_eq!(
        target.workspace.leads[0].attachments[0].name,
        "предложение.txt"
    );
}
#[test]
fn interrupted_send_becomes_unknown_and_profile_is_locked() {
    let dir = tempfile::tempdir().unwrap();
    let mut s = store(dir.path());
    assert!(
        Store::open(Options {
            data_dir: Some(dir.path().into()),
            ..Default::default()
        })
        .is_err()
    );
    s.change(|w| {
        w.leads[0].status = "sending".into();
        Ok(())
    })
    .unwrap();
    drop(s);
    let mut s = Store::open(Options {
        data_dir: Some(dir.path().into()),
        ..Default::default()
    })
    .unwrap();
    let id = s.workspace.leads[0].id.clone();
    assert_eq!(s.workspace.leads[0].status, "unknown");
    assert!(!s.workspace.leads[0].selectable());
    s.resolve_unknown(&id, false).unwrap();
    assert_eq!(s.workspace.leads[0].status, "pending");
}
#[test]
fn personalization_uses_sender_facts_and_never_fallback_in_prompt() {
    let l = lead();
    let t = Template {
        sender_profile: "Мы производим контроллеры автоматики".into(),
        body: "FALLBACK-ONLY-SENTINEL".into(),
        ..Default::default()
    };
    let payload = generation_payload(&settings(), &t, &l).to_string();
    assert!(payload.contains("контроллеры"));
    assert!(payload.contains("вентиляционные"));
    assert!(!payload.contains("FALLBACK-ONLY-SENTINEL"));
    let mut no_description = l.clone();
    no_description.description.clear();
    let (subject, body, source) = generate(&Settings::default(), &t, &no_description).unwrap();
    assert_eq!(source, "template");
    assert!(!subject.is_empty());
    assert_eq!(body, "FALLBACK-ONLY-SENTINEL");
    let (subject, body) = t.render(&l);
    let same = serde_json::json!({"subject":subject,"body":body}).to_string();
    assert!(parse_generation(&same, &t, &l).is_err());
    assert!(parse_generation("not JSON", &t, &l).is_err());
    assert!(parse_generation(r#"{"subject":"Автоматика для вентиляции","body":"Вижу, вы выпускаете вентиляционные установки. Обсудим управление?"}"#,&t,&l).is_ok());
}
#[test]
fn message_has_multiple_recipients_and_attachment() {
    let mut w = Workspace::default();
    let mut l = lead();
    l.kind = "message".into();
    l.recipients =
        normalize_recipients("ddkritsin@gmail.com, second@example.test; DDKRITSIN@gmail.com")
            .unwrap();
    assert_eq!(l.recipients.len(), 2);
    l.message_id = "<fixture@maildesk.local>".into();
    w.leads.push(l);
    let dir = tempfile::tempdir().unwrap();
    let file = dir.path().join("offer.txt");
    std::fs::write(&file, "hello attachment").unwrap();
    let id = w.leads[0].id.clone();
    w.add_files(&[id], &[file]).unwrap();
    let message = build_message(&settings(), &w.leads[0], &w).unwrap();
    assert_eq!(message.envelope().to().len(), 2);
    let wire = String::from_utf8(message.formatted()).unwrap();
    assert!(wire.contains("multipart/mixed"));
    assert!(wire.contains("offer.txt"));
    let parsed = mailparse::parse_mail(wire.as_bytes()).unwrap();
    assert!(
        parsed
            .subparts
            .iter()
            .any(|p| p.get_body_raw().unwrap() == b"hello attachment")
    );
}
#[test]
fn version_one_backup_is_preserved() {
    let dir = tempfile::tempdir().unwrap();
    let mut w = Workspace {
        version: 1,
        ..Default::default()
    };
    w.leads.push(lead());
    let original = serde_json::to_vec(&w).unwrap();
    std::fs::write(dir.path().join("project.json"), &original).unwrap();
    let s = Store::open(Options {
        data_dir: Some(dir.path().into()),
        ..Default::default()
    })
    .unwrap();
    assert_eq!(s.workspace.version, 2);
    assert_eq!(
        std::fs::read(dir.path().join("project-v1.backup.json")).unwrap(),
        original
    );
}

#[test]
fn unrelated_json_is_not_a_project() {
    assert!(serde_json::from_str::<Workspace>(r#"{"host":"smtp.mail.ru"}"#).is_err());
}
