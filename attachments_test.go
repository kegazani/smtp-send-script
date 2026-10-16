package main

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	"net/http"
	"net/http/httptest"
	"net/mail"
	"net/textproto"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

type testUpload struct {
	name string
	data []byte
}

func multipartCall(t *testing.T, a *App, route string, fields map[string]string, uploads []testUpload) (int, map[string]any) {
	t.Helper()
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	for key, value := range fields {
		if err := form.WriteField(key, value); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range uploads {
		part, err := form.CreateFormFile("files", f.name)
		if route == "import" {
			t.Fatal("use importProject fixture")
		}
		if err != nil {
			t.Fatal(err)
		}
		if _, err = part.Write(f.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := form.Close(); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "http://localhost/api/"+route, &body)
	r.Header.Set("Content-Type", form.FormDataContentType())
	w := httptest.NewRecorder()
	a.routes().ServeHTTP(w, r)
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(w.Body.String())
	}
	return w.Code, out
}

func importProject(t *testing.T, a *App, b []byte) (int, map[string]any) {
	t.Helper()
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	part, err := form.CreateFormFile("file", "project.json")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = part.Write(b)
	_ = form.Close()
	r := httptest.NewRequest("POST", "http://localhost/api/import", &body)
	r.Header.Set("Content-Type", form.FormDataContentType())
	w := httptest.NewRecorder()
	a.routes().ServeHTTP(w, r)
	var out map[string]any
	if err = json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return w.Code, out
}

func TestAttachmentsBulkPortableDownloadAndRemoval(t *testing.T) {
	a := newTestApp(t)
	a.w.Leads = append(a.w.Leads, Lead{ID: "two", Company: "Без адреса", Status: "pending"})
	data := []byte("%PDF-1.4\nПример вложения\x00\xff")
	code, out := multipartCall(t, a, "attachments", map[string]string{"ids": `["one","two"]`}, []testUpload{{"Предложение.pdf", data}})
	if code != 200 || out["updated"] != float64(2) {
		t.Fatal(code, out)
	}
	if len(a.w.Files) != 1 || len(a.w.Leads[0].Attachments) != 1 || len(a.w.Leads[1].Attachments) != 1 {
		t.Fatal("shared blob was not deduplicated")
	}
	code, out = multipartCall(t, a, "attachments", map[string]string{"ids": `["one"]`}, []testUpload{{"Предложение.pdf", data}})
	if code != 200 || len(a.w.Leads[0].Attachments) != 1 {
		t.Fatal(code, out)
	}
	id := fileID(data)
	r := httptest.NewRequest("GET", "/api/attachment?leadId=one&id="+id+"&name=%D0%9F%D1%80%D0%B5%D0%B4%D0%BB%D0%BE%D0%B6%D0%B5%D0%BD%D0%B8%D0%B5.pdf", nil)
	w := httptest.NewRecorder()
	a.routes().ServeHTTP(w, r)
	if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), data) || !strings.Contains(w.Header().Get("Content-Disposition"), "filename*") {
		t.Fatal("download lost file or Cyrillic filename")
	}
	w = httptest.NewRecorder()
	a.state(w, httptest.NewRequest("GET", "/api/state", nil))
	var view struct {
		Project Workspace `json:"project"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &view)
	if len(view.Project.Files) != 0 || len(view.Project.Leads[0].Attachments) != 1 {
		t.Fatal("state leaked file payload or lost metadata")
	}
	w = httptest.NewRecorder()
	a.export(w, httptest.NewRequest("GET", "/api/export", nil))
	b := newTestApp(t)
	code, out = importProject(t, b, w.Body.Bytes())
	if code != 200 {
		t.Fatal(code, out)
	}
	if !bytes.Equal(b.w.Files[id], data) || len(b.w.Leads[0].Attachments) != 1 {
		t.Fatal("project import lost attachments on an existing company")
	}
	code, out = importProject(t, b, w.Body.Bytes())
	if code != 200 || out["added"] != float64(0) {
		t.Fatal("repeated import duplicated data", code, out)
	}
	code, _ = call(t, b, "attachments/remove", map[string]string{"id": b.w.Leads[0].ID, "attachmentId": id, "name": "Предложение.pdf"})
	if code != 200 || len(b.w.Files) != 1 {
		t.Fatal("removed a file still referenced by another company")
	}
	code, _ = call(t, b, "attachments/remove", map[string]string{"id": b.w.Leads[1].ID, "attachmentId": id, "name": "Предложение.pdf"})
	if code != 200 || len(b.w.Files) != 0 {
		t.Fatal("unreferenced file retained")
	}
	reopened, err := openApp(b.dir)
	if err != nil || len(reopened.w.Leads[0].Attachments) != 0 {
		t.Fatal(err)
	}
}

func TestAttachmentChangeInvalidatesConfirmationAndSentFilesAreLocked(t *testing.T) {
	a := newTestApp(t)
	_, out := call(t, a, "prepare", map[string]any{"ids": []string{"one"}})
	code, _ := multipartCall(t, a, "attachments", map[string]string{"ids": `["one"]`}, []testUpload{{"test.txt", []byte("text")}})
	if code != 200 {
		t.Fatal(code)
	}
	code, _ = call(t, a, "confirm", map[string]any{"token": out["token"]})
	if code != 409 {
		t.Fatal("attachment change did not invalidate confirmation")
	}
	a.deliver = func(_ Settings, l Lead) (bool, error) {
		if string(l.files[l.Attachments[0].ID]) != "text" {
			t.Error("send snapshot lacks file bytes")
		}
		return false, nil
	}
	confirmSend(t, a)
	code, _ = multipartCall(t, a, "attachments", map[string]string{"ids": `["one"]`}, []testUpload{{"later.txt", []byte("new")}})
	if code != 409 {
		t.Fatal("sent message attachment changed")
	}
}

func TestManualMessageRecipientsPersistenceAndTestGuard(t *testing.T) {
	a := newTestApp(t)
	message := `{"recipients":["ddkritsin@gmail.com; DDKRITSIN@gmail.com","second@example.test"],"subject":"Обычное письмо","body":"Здравствуйте!"}`
	code, out := multipartCall(t, a, "message", map[string]string{"message": message}, []testUpload{{"файл.txt", []byte("текст")}})
	if code != 200 {
		t.Fatal(code, out)
	}
	id := out["id"].(string)
	l := a.w.Leads[1]
	if l.Kind != "message" || len(l.Recipients) != 2 || len(l.Attachments) != 1 {
		t.Fatal(l)
	}
	code, _ = call(t, a, "prepare", map[string]any{"ids": []string{id}})
	if code != 403 {
		t.Fatal("test guard only checked the first recipient")
	}
	code, _ = call(t, a, "lead", map[string]any{"id": id, "recipients": []string{testRecipient}, "subject": "Изменено", "body": "Готово"})
	if code != 200 {
		t.Fatal(code)
	}
	a.w.Sent[emailKey(testRecipient)] = "previous-company-message"
	// A new ordinary message may follow up an earlier company message.
	_, err := a.findSelected([]string{id}, true)
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	a.export(w, httptest.NewRequest("GET", "/api/export", nil))
	b := newTestApp(t)
	code, out = importProject(t, b, w.Body.Bytes())
	if code != 200 {
		t.Fatal(code, out)
	}
	count := len(b.w.Leads)
	code, out = importProject(t, b, w.Body.Bytes())
	if code != 200 || len(b.w.Leads) != count {
		t.Fatal("manual draft duplicated on import")
	}
	reopened, err := openApp(b.dir)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, lead := range reopened.w.Leads {
		if lead.ID == id && lead.Kind == "message" {
			found = true
			if lead.Subject != "Изменено" || len(lead.Attachments) != 1 {
				t.Fatal(lead)
			}
		}
	}
	if !found {
		t.Fatal("manual draft not persisted")
	}
}

func TestMIMEWithUnicodeAndBinaryAttachmentsAndMultipleRecipients(t *testing.T) {
	text := []byte("Тестовое вложение\nС кириллицей")
	binary := []byte{0, 255, 1, 2, 3, 13, 10, 42}
	l := Lead{Kind: "message", Recipients: []string{testRecipient, "second@example.test"}, Subject: "Тема с кириллицей", Body: "Здравствуйте!\nПисьмо с файлами.", MessageID: "fixture@example.test", files: map[string][]byte{fileID(text): text, fileID(binary): binary}, Attachments: []Attachment{{ID: fileID(text), Name: "описание.txt", Size: int64(len(text)), ContentType: "text/plain; charset=utf-8"}, {ID: fileID(binary), Name: "данные.bin", Size: int64(len(binary)), ContentType: "application/octet-stream"}}}
	b, err := messageBytes(Settings{From: "sender@example.test", SenderName: "Отправитель"}, l)
	if err != nil {
		t.Fatal(err)
	}
	msg, err := mail.ReadMessage(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	to, err := mail.ParseAddressList(msg.Header.Get("To"))
	if err != nil || len(to) != 2 {
		t.Fatal(to, err)
	}
	subject, err := new(mime.WordDecoder).DecodeHeader(msg.Header.Get("Subject"))
	if err != nil || subject != l.Subject {
		t.Fatal(subject, err)
	}
	typ, params, err := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	if err != nil || typ != "multipart/mixed" {
		t.Fatal(typ, err)
	}
	mixed := multipart.NewReader(msg.Body, params["boundary"])
	part, err := mixed.NextRawPart()
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(quotedprintable.NewReader(part))
	if err != nil || string(body) != strings.ReplaceAll(l.Body, "\n", "\r\n") {
		t.Fatal(string(body), err)
	}
	for _, f := range l.Attachments {
		part, err = mixed.NextRawPart()
		if err != nil {
			t.Fatal(err)
		}
		if part.FileName() != f.Name || part.Header.Get("Content-Transfer-Encoding") != "base64" {
			t.Fatal(part.Header)
		}
		contentType, _, e := mime.ParseMediaType(part.Header.Get("Content-Type"))
		if e != nil || contentType == "" {
			t.Fatal(part.Header)
		}
		data, e := io.ReadAll(base64.NewDecoder(base64.StdEncoding, part))
		if e != nil || !bytes.Equal(data, l.files[f.ID]) {
			t.Fatal("attachment bytes changed", e)
		}
	}
	if _, err = mixed.NextRawPart(); err != io.EOF {
		t.Fatal("multipart was not terminated", err)
	}
}

func TestAttachmentLimitsIntegrityAndAtomicity(t *testing.T) {
	a := newTestApp(t)
	code, _ := multipartCall(t, a, "attachments", map[string]string{"ids": `["one"]`}, []testUpload{{"too-big.bin", make([]byte, maxFileSize+1)}})
	if code != 400 || len(a.w.Files) != 0 {
		t.Fatal("oversized file accepted")
	}
	data := []byte("valid")
	ref := Attachment{ID: fileID(data), Name: "valid.txt", Size: int64(len(data)), ContentType: "text/plain"}
	for _, bad := range []Attachment{{ID: ref.ID, Name: "../bad.txt", Size: ref.Size, ContentType: ref.ContentType}, {ID: ref.ID, Name: "valid.txt", Size: ref.Size + 1, ContentType: ref.ContentType}, {ID: ref.ID, Name: "valid.txt", Size: ref.Size, ContentType: "text/plain\r\nX-Test: bad"}} {
		if err := validateAttachments(Lead{Attachments: []Attachment{bad}}, map[string][]byte{ref.ID: data}); err == nil {
			t.Fatal("invalid attachment accepted")
		}
	}
	if err := validateWorkspaceFiles(Workspace{Files: map[string][]byte{ref.ID: []byte("tampered")}}); err == nil {
		t.Fatal("tampered blob accepted")
	}
	w := Workspace{Version: 2, Files: map[string][]byte{ref.ID: data}, Leads: []Lead{{ID: "valid", Status: "pending", Attachments: []Attachment{ref}}}}
	b, _ := json.Marshal(w)
	code, _ = importProject(t, a, b)
	if code != 200 {
		t.Fatal(code)
	}
	before, _ := json.Marshal(a.w)
	_ = os.Remove(filepath.Join(a.dir, "project.json"))
	_ = os.Mkdir(filepath.Join(a.dir, "project.json"), 0700)
	code, _ = multipartCall(t, a, "attachments", map[string]string{"ids": `["one"]`}, []testUpload{{"new.txt", []byte("new blob")}})
	if code != 500 {
		t.Fatal(code)
	}
	after, _ := json.Marshal(a.w)
	if !bytes.Equal(before, after) {
		t.Fatal("failed save changed attachment pool")
	}
}

func TestLegacyProjectMigrationKeepsBackup(t *testing.T) {
	dir := t.TempDir()
	legacy := Workspace{Version: 1, Leads: []Lead{{ID: "old", Company: "Old", Email: testRecipient, Status: "pending"}}, Sent: map[string]string{}}
	b, _ := json.Marshal(legacy)
	if err := os.WriteFile(filepath.Join(dir, "project.json"), b, 0600); err != nil {
		t.Fatal(err)
	}
	a, err := openApp(dir)
	if err != nil || a.w.Version != 2 || a.w.Leads[0].Company != "Old" {
		t.Fatal(err)
	}
	backup, err := os.ReadFile(filepath.Join(dir, "project-v1.backup.json"))
	if err != nil || !bytes.Equal(backup, b) {
		t.Fatal("legacy backup changed", err)
	}
}

func TestImportCannotResetUncertainDelivery(t *testing.T) {
	a := newTestApp(t)
	a.w.Leads[0].Status = "unknown"
	a.w.Leads[0].Error = "Check delivery first"
	imported := a.w.Leads[0]
	imported.Status = "pending"
	imported.Body = "Older copy"
	imported.Error = ""
	a.merge([]Lead{imported}, nil)
	if a.w.Leads[0].Status != "unknown" || a.w.Leads[0].Error == "" {
		t.Fatal("import made an uncertain message sendable")
	}
	a.w.Leads[0].Status = "pending"
	imported.Status = "sending"
	a.merge([]Lead{imported}, nil)
	if a.w.Leads[0].Status != "unknown" {
		t.Fatal("imported in-flight message was not marked uncertain")
	}
}

func TestMultiRecipientSMTPTransactionUsesAllOrNoData(t *testing.T) {
	// Both recipients are consumed exclusively by a loopback SMTP fixture.
	certServer := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer certServer.Close()
	pool := x509.NewCertPool()
	pool.AddCert(certServer.Certificate())
	for _, rejectSecond := range []bool{false, true} {
		t.Run(fmt.Sprint(rejectSecond), func(t *testing.T) {
			listener, err := tls.Listen("tcp", "127.0.0.1:0", certServer.TLS.Clone())
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			type result struct {
				to   []string
				data bool
			}
			done := make(chan result, 1)
			go func() {
				var result result
				defer func() { done <- result }()
				conn, e := listener.Accept()
				if e != nil {
					return
				}
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(4 * time.Second))
				r := bufio.NewReader(conn)
				fmt.Fprint(conn, "220 local ESMTP\r\n")
				for {
					line, e := r.ReadString('\n')
					if e != nil {
						return
					}
					switch {
					case strings.HasPrefix(line, "EHLO"):
						fmt.Fprint(conn, "250-local\r\n250 AUTH PLAIN\r\n")
					case strings.HasPrefix(line, "AUTH"):
						fmt.Fprint(conn, "235 ok\r\n")
					case strings.HasPrefix(line, "MAIL"):
						fmt.Fprint(conn, "250 ok\r\n")
					case strings.HasPrefix(line, "RCPT"):
						result.to = append(result.to, strings.TrimSpace(line))
						if rejectSecond && len(result.to) == 2 {
							fmt.Fprint(conn, "550 rejected\r\n")
						} else {
							fmt.Fprint(conn, "250 ok\r\n")
						}
					case strings.HasPrefix(line, "DATA"):
						result.data = true
						fmt.Fprint(conn, "354 continue\r\n")
						_, _ = io.Copy(io.Discard, textproto.NewReader(r).DotReader())
						fmt.Fprint(conn, "250 accepted\r\n")
					case strings.HasPrefix(line, "QUIT"):
						fmt.Fprint(conn, "221 bye\r\n")
						return
					}
				}
			}()
			a := newTestApp(t)
			a.cfg.Host = "127.0.0.1"
			a.cfg.Port = listener.Addr().(*net.TCPAddr).Port
			c, err := smtpConnect(a.cfg, &tls.Config{ServerName: "127.0.0.1", RootCAs: pool, MinVersion: tls.VersionTLS12})
			if err != nil {
				t.Fatal(err)
			}
			l := a.w.Leads[0]
			l.Kind = "message"
			l.Recipients = []string{testRecipient, "second@example.test"}
			l.MessageID = "multi@example.test"
			ambiguous, err := deliverSMTP(c, a.cfg, l)
			if ambiguous || (err != nil) != rejectSecond {
				t.Fatal(ambiguous, err)
			}
			got := <-done
			if len(got.to) != 2 || got.data == rejectSecond || !reflect.DeepEqual(got.to, []string{"RCPT TO:<" + testRecipient + ">", "RCPT TO:<second@example.test>"}) {
				t.Fatal(got)
			}
		})
	}
}
