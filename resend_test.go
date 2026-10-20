package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func restoreSent(t *testing.T, a *App, id string) {
	t.Helper()
	if code, out := call(t, a, "restore", map[string]string{"id": id}); code != 200 {
		t.Fatal(code, out)
	}
}

func TestRestoreRegenerateAndSendWithNewDraft(t *testing.T) {
	a := newTestApp(t)
	code, _ := multipartCall(t, a, "attachments", map[string]string{"ids": `["one"]`}, []testUpload{{"предложение.txt", []byte("attachment")}})
	if code != 200 {
		t.Fatal("could not attach fixture")
	}
	confirmSend(t, a)
	previous := a.w.Leads[0]
	restoreSent(t, a, previous.ID)
	l := a.w.Leads[0]
	if l.Status != "pending" || l.Resend == nil || l.MessageID != "" || l.SentAt != "" || l.Body != previous.Body || len(l.Attachments) != 1 || len(l.SendHistory) != 1 || a.w.Sent[testRecipient] != previous.SentAt {
		t.Fatal("restore must keep draft, attachments and previous delivery while clearing current delivery")
	}
	if code, _ := call(t, a, "restore", map[string]string{"id": previous.ID}); code != 409 {
		t.Fatal("restore was replayed")
	}
	code, _ = call(t, a, "lead", map[string]any{"id": previous.ID, "company": l.Company, "email": l.Email, "subject": "Новая тема", "body": "Новый текст", "description": "Производит воздуховоды"})
	if code != 200 {
		t.Fatal("restored company cannot be edited")
	}
	var generations atomic.Int32
	a.generate = func(Settings, Template, Lead) (string, string, error) {
		generations.Add(1)
		return "Повторное предложение", "Здравствуйте! Обсудим поставки воздуховодов.", nil
	}
	if code, _ = call(t, a, "generate", map[string]any{"ids": []string{previous.ID}}); code != 200 {
		t.Fatal("restored company cannot be regenerated")
	}
	waitIdle(t, a)
	if generations.Load() != 1 || a.w.Leads[0].Source != "deepseek" {
		t.Fatal("regeneration did not use company context")
	}
	a.w.Leads = append(a.w.Leads, Lead{ID: "new", Kind: "message", Recipients: []string{testRecipient}, Email: testRecipient, Company: "Новое письмо", Subject: "Новая тема", Body: "Новый текст", Status: "pending"})
	var sends atomic.Int32
	a.deliver = func(_ Settings, draft Lead) (bool, error) {
		sends.Add(1)
		if draft.ID == previous.ID && (draft.MessageID == previous.MessageID || len(draft.Attachments) != 1 || string(draft.files[draft.Attachments[0].ID]) != "attachment") {
			t.Error("resend lost attachments or reused the old Message-ID")
		}
		return false, nil
	}
	code, out := call(t, a, "prepare", map[string]any{"ids": []string{previous.ID, "new"}})
	if code != 200 || out["count"] != float64(2) || sends.Load() != 0 {
		t.Fatal("restored and new drafts must share one confirmation flow")
	}
	if code, _ = call(t, a, "send", map[string]any{"token": out["token"]}); code != 409 {
		t.Fatal("resend skipped the first confirmation")
	}
	code, out = call(t, a, "confirm", map[string]any{"token": out["token"]})
	if code != 200 || sends.Load() != 0 {
		t.Fatal("first confirmation sent messages")
	}
	token := out["token"]
	if code, _ = call(t, a, "send", map[string]any{"token": token}); code != 200 {
		t.Fatal("confirmed batch rejected")
	}
	waitIdle(t, a)
	l = a.w.Leads[0]
	if sends.Load() != 2 || l.Status != "sent" || l.Resend != nil || len(l.SendHistory) != 2 || l.SendHistory[0].MessageID != previous.MessageID || l.SendHistory[1].MessageID != l.MessageID {
		t.Fatal("resend results or delivery history incorrect")
	}
	if code, _ = call(t, a, "send", map[string]any{"token": token}); code != 409 || sends.Load() != 2 {
		t.Fatal("confirmation replay caused a duplicate resend")
	}
}

func TestRestoredDraftSurvivesRestartCSVAndJSONTransfer(t *testing.T) {
	a := newTestApp(t)
	confirmSend(t, a)
	sentJSON, _ := json.Marshal(a.w)
	restoreSent(t, a, "one")
	before := a.w.Leads[0]
	a.merge([]Lead{{Company: "CSV copy", Email: testRecipient, Status: "pending", Description: "Updated context"}}, nil)
	if a.w.Leads[0].Status != "pending" || a.alreadySent(a.w.Leads[0]) {
		t.Fatal("CSV reimport revoked explicit resend")
	}
	if err := a.save(); err != nil {
		t.Fatal(err)
	}
	reopened, err := openApp(a.dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = reopened.findSelected([]string{"one"}, true); err != nil || len(reopened.w.Leads[0].SendHistory) != 1 {
		t.Fatal("restart lost resend permission or history", err)
	}
	restoredJSON, _ := json.Marshal(reopened.w)
	b := newTestApp(t)
	if code, _ := importProject(t, b, sentJSON); code != 200 || b.w.Leads[0].Status != "sent" {
		t.Fatal("sent fixture not imported")
	}
	if code, _ := importProject(t, b, restoredJSON); code != 200 || b.w.Leads[0].Status != "pending" || b.alreadySent(b.w.Leads[0]) {
		t.Fatal("JSON transfer did not preserve explicit restore")
	}
	if code, _ := importProject(t, b, sentJSON); code != 200 || b.w.Leads[0].Status != "pending" {
		t.Fatal("old sent project undid the user's restore")
	}
	// A newer accepted delivery supersedes a stale restore transferred from elsewhere.
	newer, _ := time.Parse(time.RFC3339Nano, before.SendHistory[0].SentAt)
	latest := newer.Add(time.Second).Format(time.RFC3339Nano)
	b.w.Sent[testRecipient] = latest
	if code, _ := importProject(t, b, restoredJSON); code != 200 || b.w.Leads[0].Status != "sent" || b.w.Leads[0].Resend != nil {
		t.Fatal("stale restore bypassed a newer delivery")
	}
	if code, _ := importProject(t, b, sentJSON); code != 200 || b.w.Sent[testRecipient] != latest {
		t.Fatal("old project rolled back sent history")
	}
	c := newTestApp(t)
	if code, _ := importProject(t, c, sentJSON); code != 200 {
		t.Fatal("could not import original sent record")
	}
	var newest Workspace
	if err := json.Unmarshal(sentJSON, &newest); err != nil {
		t.Fatal(err)
	}
	newest.Leads[0].SentAt, newest.Leads[0].MessageID, newest.Leads[0].Body = latest, "newer@example.test", "Новое принятое письмо"
	newest.Sent[testRecipient] = latest
	newestJSON, _ := json.Marshal(newest)
	if code, _ := importProject(t, c, newestJSON); code != 200 || c.w.Leads[0].MessageID != "newer@example.test" || len(c.w.Leads[0].SendHistory) != 2 {
		t.Fatal("newer sent project did not merge delivery history and latest draft")
	}
	if code, _ := importProject(t, c, sentJSON); code != 200 || c.w.Leads[0].Body != "Новое принятое письмо" || len(c.w.Leads[0].SendHistory) != 2 {
		t.Fatal("older sent project overwrote newer delivery or duplicated history")
	}
}

func TestRestoreRejectsOtherStatesBusyAndFailedPersistence(t *testing.T) {
	for _, status := range []string{"pending", "error", "unknown", "sending"} {
		t.Run(status, func(t *testing.T) {
			a := newTestApp(t)
			a.w.Leads[0].Status = status
			if code, _ := call(t, a, "restore", map[string]string{"id": "one"}); code != 409 {
				t.Fatal("restore accepted an unsent or uncertain message")
			}
		})
	}
	a := newTestApp(t)
	confirmSend(t, a)
	for _, operation := range []string{"sending", "generating"} {
		a.sending, a.generating = operation == "sending", operation == "generating"
		if code, _ := call(t, a, "restore", map[string]string{"id": "one"}); code != 409 {
			t.Fatal("restore allowed during an active operation")
		}
	}
	a.sending, a.generating = false, false
	if code, _ := call(t, a, "restore", map[string]string{"id": "missing"}); code != 404 {
		t.Fatal("unknown ID accepted")
	}
	before, _ := json.Marshal(a.w)
	// Replace the project file with a directory to force the atomic rename to fail.
	if err := os.Remove(filepath.Join(a.dir, "project.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(a.dir, "project.json"), 0700); err != nil {
		t.Fatal(err)
	}
	if code, _ := call(t, a, "restore", map[string]string{"id": "one"}); code != 500 {
		t.Fatal("failed save reported successful restore")
	}
	after, _ := json.Marshal(a.w)
	if !bytes.Equal(before, after) {
		t.Fatal("failed restore changed in-memory state")
	}
}

func TestResendFailureRetainsHistoryAndRequiresUnknownResolution(t *testing.T) {
	a := newTestApp(t)
	confirmSend(t, a)
	restoreSent(t, a, "one")
	a.deliver = func(Settings, Lead) (bool, error) { return true, errors.New("connection lost") }
	confirmSend(t, a)
	if a.w.Leads[0].Status != "unknown" || len(a.w.Leads[0].SendHistory) != 1 {
		t.Fatal("uncertain resend must retain previous delivery and stay blocked")
	}
	if code, _ := call(t, a, "restore", map[string]string{"id": "one"}); code != 409 {
		t.Fatal("restore bypassed uncertain-delivery handling")
	}
	if code, _ := call(t, a, "resolve", map[string]any{"id": "one", "checked": true}); code != 200 {
		t.Fatal("could not explicitly resolve uncertain resend")
	}
	a.deliver = func(Settings, Lead) (bool, error) { return false, nil }
	confirmSend(t, a)
	if len(a.w.Leads[0].SendHistory) != 2 || a.w.Leads[0].Resend != nil {
		t.Fatal("successful retry lost history or left resend permission active")
	}
}

func TestResendPermissionStaysBoundToRecipientAndRestoreInvalidatesConfirmation(t *testing.T) {
	a := newTestApp(t)
	confirmSend(t, a)
	a.w.Leads = append(a.w.Leads, Lead{ID: "other", Kind: "message", Recipients: []string{testRecipient}, Subject: "Тема", Body: "Текст", Status: "pending"})
	_, out := call(t, a, "prepare", map[string]any{"ids": []string{"other"}})
	restoreSent(t, a, "one")
	if code, _ := call(t, a, "confirm", map[string]any{"token": out["token"]}); code != 409 {
		t.Fatal("restore did not invalidate an existing confirmation")
	}
	a.w.Sent["other@example.test"] = a.w.Sent[testRecipient]
	if code, _ := call(t, a, "lead", map[string]string{"id": "one", "company": "Компания", "email": "other@example.test", "subject": "Тема", "body": "Текст"}); code != 409 {
		t.Fatal("resend permission authorized another previously sent recipient")
	}
}

func TestRestoreLegacySentManualMessage(t *testing.T) {
	a := newTestApp(t)
	a.w.Leads[0] = Lead{ID: "one", Kind: "message", Recipients: []string{testRecipient}, Email: testRecipient, Subject: "Обычное письмо", Body: "Текст", Status: "sent", SentAt: "2026-10-18T12:00:00Z", MessageID: "legacy@example.test"}
	restoreSent(t, a, "one")
	confirmSend(t, a)
	if len(a.w.Leads[0].SendHistory) != 2 || a.w.Leads[0].SendHistory[0].MessageID != "legacy@example.test" {
		t.Fatal("legacy manual message did not keep its previous delivery")
	}
}
