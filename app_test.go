package main

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/mail"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const testRecipient = "ddkritsin@gmail.com"

func newTestApp(t *testing.T) *App {
	t.Helper()
	a, err := openApp(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a.cfg = Settings{Host: "smtp.mail.ru", Port: 465, Security: "ssl", Username: "sender@example.com", From: "sender@example.com", Password: "test-password", APIKey: "test-key"}
	a.delay = 0
	a.testRecipient = testRecipient
	a.w.Leads = []Lead{{ID: "one", Company: "Компания", Email: testRecipient, Subject: "Сотрудничество", Body: "Здравствуйте!\nПроверка UTF-8.", Status: "pending"}}
	a.deliver = func(Settings, Lead) (bool, error) { return false, nil }
	return a
}

func call(t *testing.T, a *App, path string, body any) (int, map[string]any) {
	t.Helper()
	b, _ := json.Marshal(body)
	r := httptest.NewRequest("POST", "http://localhost/api/"+path, bytes.NewReader(b))
	w := httptest.NewRecorder()
	a.routes().ServeHTTP(w, r)
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("%s: %s", path, w.Body.String())
	}
	return w.Code, out
}
func waitIdle(t *testing.T, a *App) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		a.mu.Lock()
		idle := !a.sending && !a.generating
		a.mu.Unlock()
		if idle {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("operation did not complete")
}
func confirmSend(t *testing.T, a *App) string {
	t.Helper()
	code, out := call(t, a, "prepare", map[string]any{"ids": []string{"one"}})
	if code != 200 {
		t.Fatal(out)
	}
	code, out = call(t, a, "confirm", map[string]any{"token": out["token"]})
	if code != 200 {
		t.Fatal(out)
	}
	token := out["token"].(string)
	code, out = call(t, a, "send", map[string]any{"token": token})
	if code != 200 {
		t.Fatal(out)
	}
	waitIdle(t, a)
	return token
}

func TestConfirmationsCannotBeSkippedReplayedOrChanged(t *testing.T) {
	a := newTestApp(t)
	var sends atomic.Int32
	a.deliver = func(Settings, Lead) (bool, error) { sends.Add(1); return false, nil }
	code, _ := call(t, a, "send", map[string]string{"token": "invented"})
	if code != 409 {
		t.Fatal(code)
	}
	_, p := call(t, a, "prepare", map[string]any{"ids": []string{"one"}})
	code, _ = call(t, a, "send", map[string]any{"token": p["token"]})
	if code != 409 {
		t.Fatal("first confirmation skipped")
	}
	_, c := call(t, a, "confirm", map[string]any{"token": p["token"]})
	a.w.Leads[0].Body = "Письмо изменилось"
	code, _ = call(t, a, "send", map[string]any{"token": c["token"]})
	if code != 409 || sends.Load() != 0 {
		t.Fatal("changed draft sent")
	}
	token := confirmSend(t, a)
	code, _ = call(t, a, "send", map[string]string{"token": token})
	if code != 409 || sends.Load() != 1 {
		t.Fatal("confirmation replay sent duplicate")
	}
	code, _ = call(t, a, "prepare", map[string]any{"ids": []string{"one"}})
	if code != 400 {
		t.Fatal("sent lead selectable")
	}
	if a.w.Leads[0].Status != "sent" || a.w.Leads[0].MessageID == "" {
		t.Fatal(a.w.Leads[0])
	}
}

func TestTestModeRejectsOtherRecipients(t *testing.T) {
	a := newTestApp(t)
	a.w.Leads[0].Email = "company@example.com"
	code, _ := call(t, a, "prepare", map[string]any{"ids": []string{"one"}})
	if code != 403 {
		t.Fatalf("test whitelist bypass: %d", code)
	}
}

func TestFailuresAndAmbiguousDelivery(t *testing.T) {
	for _, ambiguous := range []bool{false, true} {
		t.Run(fmt.Sprint(ambiguous), func(t *testing.T) {
			a := newTestApp(t)
			a.deliver = func(Settings, Lead) (bool, error) { return ambiguous, errors.New("SMTP unavailable") }
			confirmSend(t, a)
			expected := "error"
			if ambiguous {
				expected = "unknown"
			}
			if a.w.Leads[0].Status != expected {
				t.Fatal(a.w.Leads[0])
			}
			if ambiguous {
				code, _ := call(t, a, "prepare", map[string]any{"ids": []string{"one"}})
				if code != 400 {
					t.Fatal("uncertain delivery retried")
				}
				code, _ = call(t, a, "resolve", map[string]any{"id": "one", "checked": true})
				if code != 200 {
					t.Fatal(code)
				}
			}
			a.deliver = func(Settings, Lead) (bool, error) { return false, nil }
			confirmSend(t, a)
			if a.w.Leads[0].Status != "sent" {
				t.Fatal("retry failed")
			}
		})
	}
}

func TestPersistenceAndSentMerge(t *testing.T) {
	a := newTestApp(t)
	confirmSend(t, a)
	a.merge([]Lead{{ID: "copy", Company: "Other name", Email: strings.ToUpper(testRecipient), Status: "pending"}}, nil)
	if len(a.w.Leads) != 1 || a.w.Leads[0].Status != "sent" {
		t.Fatal("import reset sent status")
	}
	a.w.Leads = append(a.w.Leads, Lead{ID: "crash", Company: "Interrupted", Status: "sending"})
	if err := a.save(); err != nil {
		t.Fatal(err)
	}
	b, err := openApp(a.dir)
	if err != nil {
		t.Fatal(err)
	}
	if b.w.Leads[1].Status != "unknown" || b.w.Leads[0].Status != "sent" {
		t.Fatal("restart lost delivery status")
	}
	w := httptest.NewRecorder()
	a.export(w, httptest.NewRequest("GET", "/api/export", nil))
	for _, secret := range []string{a.cfg.Password, a.cfg.APIKey, "password", "apiKey"} {
		if strings.Contains(w.Body.String(), secret) {
			t.Fatal("export contains credential")
		}
	}
	var portable Workspace
	if err = json.Unmarshal(w.Body.Bytes(), &portable); err != nil {
		t.Fatal(err)
	}
	c := newTestApp(t)
	c.w.Leads = nil
	c.merge(portable.Leads, portable.Sent)
	if c.w.Leads[0].Status != "sent" {
		t.Fatal("transfer lost sent status")
	}
}

func TestCSVFormatsAndValidation(t *testing.T) {
	for _, delimiter := range []string{",", ";"} {
		csv := "\ufeffCompany" + delimiter + "Email" + delimiter + "Overview\r\n\"Компания\"" + delimiter + testRecipient + delimiter + "\"Первая строка\nвторая строка\"\r\n"
		leads, err := csvLeads(strings.NewReader(csv))
		if err != nil || len(leads) != 1 || !strings.Contains(leads[0].Description, "вторая") {
			t.Fatalf("%v %v", leads, err)
		}
	}
	for _, bad := range []string{"Company,Other\nTest,xxx", "Company,Email\n", "Company,Email\n\"broken", "Company,Email\n\xff,email"} {
		if _, err := csvLeads(strings.NewReader(bad)); err == nil {
			t.Fatal("bad CSV accepted")
		}
	}
}

func TestTemplateFallbackAndDeepSeekErrorPreserveDraft(t *testing.T) {
	a := newTestApp(t)
	var calls atomic.Int32
	a.generate = func(Settings, Template, Lead) (string, string, error) {
		calls.Add(1)
		return "", "", errors.New("API refused test-key")
	}
	a.w.Template.Subject = "Для {company}"
	a.w.Template.Body = "Здравствуйте, {company}"
	code, _ := call(t, a, "generate", map[string]any{"ids": []string{"one"}})
	if code != 200 {
		t.Fatal(code)
	}
	waitIdle(t, a)
	if calls.Load() != 0 || a.w.Leads[0].Subject != "Для Компания" || a.w.Leads[0].Source != "template" {
		t.Fatal("fallback called API")
	}
	a.w.Leads[0].Description = "Описание"
	code, _ = call(t, a, "generate", map[string]any{"ids": []string{"one"}})
	if code != 200 {
		t.Fatal(code)
	}
	waitIdle(t, a)
	if calls.Load() != 1 || a.w.Leads[0].Subject != "Для Компания" || strings.Contains(a.w.Leads[0].GenerationError, "test-key") {
		t.Fatal("generation failure destroyed draft or leaked secret")
	}
}

func TestDeepSeekProtocol(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Error("missing API auth")
		}
		var p map[string]any
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			t.Error(err)
		}
		if p["model"] != "deepseek-chat" {
			t.Error(p)
		}
		respond(w, map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": `{"subject":"Вопрос","body":"Здравствуйте!"}`}}}})
	}))
	defer server.Close()
	sub, body, err := deepSeekRequest(server.Client(), server.URL, Settings{APIKey: "test-key"}, Template{}, Lead{Company: "Компания", Description: "Описание"})
	if err != nil || sub != "Вопрос" || body != "Здравствуйте!" {
		t.Fatal(sub, body, err)
	}
}

func TestLocalOnlyAPIAndSecretRedaction(t *testing.T) {
	a := newTestApp(t)
	a.origin = "http://127.0.0.1:8765"
	h := a.handler()
	for _, test := range []struct {
		host, origin, cookie string
		code                 int
	}{{"127.0.0.1:8765", "", "", 401}, {"127.0.0.1:8765", "https://other.example", a.session, 403}, {"evil.example", "", a.session, 403}, {"127.0.0.1:8765", "", a.session, 200}} {
		r := httptest.NewRequest("GET", "http://"+test.host+"/api/state", nil)
		r.Header.Set("Origin", test.origin)
		if test.cookie != "" {
			r.AddCookie(&http.Cookie{Name: "maildesk", Value: test.cookie})
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != test.code {
			t.Fatalf("%+v: %d", test, w.Code)
		}
		if strings.Contains(w.Body.String(), a.cfg.Password) || strings.Contains(w.Body.String(), a.cfg.APIKey) {
			t.Fatal("state leaked secrets")
		}
	}
}

func TestSettingsKeepSavedSecretsAcrossBlankSaveAndRestart(t *testing.T) {
	t.Setenv("SMTP_PASSWORD", "")
	t.Setenv("LLM_API_KEY", "")
	a := newTestApp(t)
	if code, _ := call(t, a, "settings", a.cfg); code != 200 {
		t.Fatal("initial settings save failed")
	}
	draft := a.cfg
	draft.Password, draft.APIKey = "", ""
	draft.SenderName = "Новое имя отправителя"
	if code, _ := call(t, a, "settings", draft); code != 200 {
		t.Fatal("blank secret fields must preserve saved credentials")
	}
	reopened, err := openApp(a.dir)
	if err != nil {
		t.Fatal(err)
	}
	if reopened.cfg.Password != "test-password" || reopened.cfg.APIKey != "test-key" || reopened.cfg.SenderName != draft.SenderName {
		t.Fatal("saved credentials or settings lost after restart")
	}
	w := httptest.NewRecorder()
	reopened.state(w, httptest.NewRequest("GET", "/api/state", nil))
	var state map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	if state["smtpPasswordSaved"] != true || state["smtpReady"] != true {
		t.Fatal("state must distinguish a saved password from an empty input")
	}
	if strings.Contains(w.Body.String(), "test-password") || strings.Contains(w.Body.String(), "test-key") {
		t.Fatal("saved secret leaked in state")
	}
	reopened.cfg.From = "invalid"
	w = httptest.NewRecorder()
	reopened.state(w, httptest.NewRequest("GET", "/api/state", nil))
	if err := json.Unmarshal(w.Body.Bytes(), &state); err != nil || state["smtpReady"] != false || state["smtpPasswordSaved"] != true {
		t.Fatal("SMTP readiness must not confuse invalid addresses with a missing password")
	}
}

func TestMissingSMTPPasswordIsExplicitAndDraftCheckDoesNotSave(t *testing.T) {
	a := newTestApp(t)
	a.cfg.Password = ""
	if code, _ := call(t, a, "settings", a.cfg); code != 200 {
		t.Fatal("settings without a password may be saved for later completion")
	}
	before, err := os.ReadFile(filepath.Join(a.dir, "credentials.json"))
	if err != nil {
		t.Fatal(err)
	}
	code, result := call(t, a, "prepare", map[string]any{"ids": []string{"one"}})
	if code != 400 || !strings.Contains(result["error"].(string), "Сохранить подключения") {
		t.Fatal("missing password must explain how to save it")
	}
	for _, draft := range []any{map[string]any{}, map[string]any{"settings": a.cfg}} {
		code, result = call(t, a, "smtp-test", draft)
		if code != 400 || !strings.Contains(result["error"].(string), "SMTP-пароль") {
			t.Fatal("empty password must fail before any connection")
		}
	}
	draft := a.cfg
	draft.Password = "draft-only-password"
	draft.Host = "invalid/host" // Fails validation without contacting any SMTP server.
	code, _ = call(t, a, "smtp-test", map[string]any{"settings": draft})
	if code != 400 {
		t.Fatal("invalid draft settings accepted")
	}
	after, err := os.ReadFile(filepath.Join(a.dir, "credentials.json"))
	if err != nil || !bytes.Equal(before, after) || a.cfg.Password != "" {
		t.Fatal("testing a draft must not persist credentials")
	}
}

// A local TLS SMTP fixture validates AUTH, the recipient and the MIME body.
func TestSMTPSubmissionAndRejection(t *testing.T) {
	certServer := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer certServer.Close()
	pool := x509.NewCertPool()
	pool.AddCert(certServer.Certificate())
	for _, reject := range []bool{false, true} {
		t.Run(fmt.Sprint(reject), func(t *testing.T) {
			listener, err := tls.Listen("tcp", "127.0.0.1:0", certServer.TLS.Clone())
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			result := make(chan string, 1)
			go func() {
				conn, e := listener.Accept()
				if e != nil {
					result <- "accept failed"
					return
				}
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
				r := bufio.NewReader(conn)
				fmt.Fprint(conn, "220 local-test ESMTP\r\n")
				for {
					line, e := r.ReadString('\n')
					if e != nil {
						result <- "closed"
						return
					}
					switch {
					case strings.HasPrefix(line, "EHLO"):
						fmt.Fprint(conn, "250-local-test\r\n250 AUTH PLAIN\r\n")
					case strings.HasPrefix(line, "AUTH"):
						fmt.Fprint(conn, "235 authenticated\r\n")
					case strings.HasPrefix(line, "MAIL"):
						fmt.Fprint(conn, "250 sender accepted\r\n")
					case strings.HasPrefix(line, "RCPT"):
						if !strings.Contains(line, testRecipient) {
							result <- "FORBIDDEN RECIPIENT"
							return
						}
						if reject {
							fmt.Fprint(conn, "550 rejected\r\n")
							result <- "rejected"
							return
						}
						fmt.Fprint(conn, "250 recipient accepted\r\n")
					case strings.HasPrefix(line, "DATA"):
						fmt.Fprint(conn, "354 send data\r\n")
						tp := textproto.NewReader(r)
						b, e := io.ReadAll(tp.DotReader())
						if e != nil {
							result <- e.Error()
							return
						}
						msg, e := mail.ReadMessage(bytes.NewReader(b))
						if e != nil || msg.Header.Get("To") != testRecipient || msg.Header.Get("Message-Id") == "" {
							result <- "bad MIME"
							return
						}
						result <- "accepted"
						fmt.Fprint(conn, "250 queued\r\n")
					case strings.HasPrefix(line, "QUIT"):
						fmt.Fprint(conn, "221 bye\r\n")
						return
					}
				}
			}()
			a := newTestApp(t)
			a.cfg.Host = "127.0.0.1"
			a.cfg.Port = listener.Addr().(*net.TCPAddr).Port
			a.w.Leads[0].MessageID = "test-message@example.com"
			c, e := smtpConnect(a.cfg, &tls.Config{ServerName: "127.0.0.1", RootCAs: pool, MinVersion: tls.VersionTLS12})
			if e != nil {
				t.Fatal(e)
			}
			ambiguous, e := deliverSMTP(c, a.cfg, a.w.Leads[0])
			if ambiguous || (e != nil) != reject {
				t.Fatalf("ambiguous=%v error=%v", ambiguous, e)
			}
			want := "accepted"
			if reject {
				want = "rejected"
			}
			if got := <-result; got != want {
				t.Fatal(got)
			}
		})
	}
}

func TestCommitRollsBackOnStorageFailure(t *testing.T) {
	a := newTestApp(t)
	before := a.w.Template.Subject
	if err := os.Remove(filepath.Join(a.dir, "project.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(a.dir, "project.json"), 0700); err != nil {
		t.Fatal(err)
	}
	err := a.commit(func() { a.w.Template.Subject = "changed" })
	if err == nil || a.w.Template.Subject != before {
		t.Fatal("failed write changed in-memory project")
	}
}
