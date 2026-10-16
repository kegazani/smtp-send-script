package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path"
	"strings"
	"unicode"
)

const (
	maxFileSize     = 10 << 20
	maxMessageFiles = 15 << 20
	maxProjectFiles = 100 << 20
	maxProjectJSON  = 180 << 20
	maxAttachments  = 20
)

type Attachment struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Size        int64  `json:"size"`
	ContentType string `json:"contentType"`
}

func fileID(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }

func recipients(l Lead) []string {
	if l.Kind == "message" {
		return l.Recipients
	}
	return []string{l.Email}
}

func validRecipients(l Lead) bool {
	r := recipients(l)
	if len(r) == 0 || len(r) > 100 {
		return false
	}
	for _, email := range r {
		if !validEmail(email) {
			return false
		}
	}
	return true
}

func normalizeRecipients(input []string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for _, entry := range input {
		for _, email := range strings.FieldsFunc(entry, func(r rune) bool { return r == ',' || r == ';' || unicode.IsSpace(r) }) {
			if !validEmail(email) {
				return nil, fmt.Errorf("некорректный адрес: %s", email)
			}
			key := emailKey(email)
			if !seen[key] {
				seen[key] = true
				out = append(out, email)
			}
		}
	}
	if len(out) == 0 || len(out) > 100 {
		return nil, errors.New("укажите от 1 до 100 получателей")
	}
	return out, nil
}

func (a *App) testRecipientAllowed(l Lead) bool {
	if a.testRecipient == "" {
		return true
	}
	for _, email := range recipients(l) {
		if emailKey(email) != emailKey(a.testRecipient) {
			return false
		}
	}
	return true
}

func validateAttachments(l Lead, files map[string][]byte) error {
	if len(l.Attachments) > maxAttachments {
		return errors.New("не больше 20 вложений в одном письме")
	}
	var total int64
	for _, f := range l.Attachments {
		data, ok := files[f.ID]
		if !ok || int64(len(data)) != f.Size || f.Size > maxFileSize || fileID(data) != f.ID {
			return fmt.Errorf("вложение %q отсутствует или повреждено", f.Name)
		}
		if cleanFilename(f.Name) != f.Name || f.Name == "" {
			return errors.New("некорректное имя вложения")
		}
		if _, _, err := mime.ParseMediaType(f.ContentType); err != nil || strings.ContainsAny(f.ContentType, "\r\n") {
			return errors.New("некорректный тип вложения")
		}
		total += f.Size
	}
	if total > maxMessageFiles {
		return errors.New("суммарный размер вложений в письме — не больше 15 МБ")
	}
	return nil
}

func validateWorkspaceFiles(w Workspace) error {
	var total int64
	for id, data := range w.Files {
		if len(data) > maxFileSize || id != fileID(data) {
			return errors.New("проект содержит повреждённый файл или файл больше 10 МБ")
		}
		total += int64(len(data))
		if total > maxProjectFiles {
			return errors.New("вложения проекта превышают 100 МБ")
		}
	}
	ids := map[string]bool{}
	for _, l := range w.Leads {
		if l.ID == "" || len(l.ID) > 128 || strings.ContainsAny(l.ID, "\r\n") || ids[l.ID] {
			return errors.New("неверные или повторяющиеся идентификаторы писем")
		}
		ids[l.ID] = true
		if l.Kind != "" && l.Kind != "message" {
			return errors.New("неизвестный тип письма")
		}
		if l.Kind == "message" {
			normalized, err := normalizeRecipients(l.Recipients)
			if err != nil || len(normalized) != len(l.Recipients) || l.ID == "" {
				return errors.New("некорректные получатели обычного письма")
			}
		}
		if err := validateAttachments(l, w.Files); err != nil {
			return err
		}
	}
	return nil
}

func cleanFilename(name string) string {
	name = path.Base(strings.ReplaceAll(name, "\\", "/"))
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, name)
	name = strings.TrimSpace(name)
	if name == "." || name == ".." {
		return ""
	}
	if len(name) > 240 {
		return ""
	}
	return name
}

func readUploads(w http.ResponseWriter, r *http.Request) ([]Attachment, map[string][]byte, error) {
	r.Body = http.MaxBytesReader(w, r.Body, maxMessageFiles+(2<<20))
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		if r.MultipartForm != nil {
			r.MultipartForm.RemoveAll()
		}
		return nil, nil, errors.New("вложения не прочитаны: максимум 15 МБ за загрузку")
	}
	refs := []Attachment{}
	files := map[string][]byte{}
	total := 0
	for _, header := range r.MultipartForm.File["files"] {
		if len(refs) >= maxAttachments {
			return nil, nil, errors.New("не больше 20 файлов за загрузку")
		}
		name := cleanFilename(header.Filename)
		if name == "" {
			return nil, nil, errors.New("некорректное имя файла")
		}
		f, err := header.Open()
		if err != nil {
			return nil, nil, err
		}
		data, err := io.ReadAll(io.LimitReader(f, maxFileSize+1))
		f.Close()
		if err != nil {
			return nil, nil, err
		}
		if len(data) > maxFileSize {
			return nil, nil, fmt.Errorf("%s: файл больше 10 МБ", name)
		}
		total += len(data)
		if total > maxMessageFiles {
			return nil, nil, errors.New("вложения превышают 15 МБ")
		}
		contentType := mime.TypeByExtension(path.Ext(name))
		if contentType == "" {
			contentType = http.DetectContentType(data)
		}
		id := fileID(data)
		files[id] = data
		refs = append(refs, Attachment{ID: id, Name: name, Size: int64(len(data)), ContentType: contentType})
	}
	return refs, files, nil
}

func combinedFiles(a, b map[string][]byte) map[string][]byte {
	out := make(map[string][]byte, len(a)+len(b))
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}

func appendAttachments(existing, added []Attachment) []Attachment {
	out := append([]Attachment(nil), existing...)
	for _, f := range added {
		found := false
		for _, p := range out {
			if f.ID == p.ID && f.Name == p.Name {
				found = true
				break
			}
		}
		if !found {
			out = append(out, f)
		}
	}
	return out
}

func (a *App) collectFiles() {
	needed := map[string]bool{}
	for _, l := range a.w.Leads {
		for _, f := range l.Attachments {
			needed[f.ID] = true
		}
	}
	for id := range a.w.Files {
		if !needed[id] {
			delete(a.w.Files, id)
		}
	}
}

func (a *App) addAttachments(w http.ResponseWriter, r *http.Request) {
	refs, files, err := readUploads(w, r)
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}
	if err != nil {
		fail(w, 400, err)
		return
	}
	var ids []string
	if err = json.Unmarshal([]byte(r.FormValue("ids")), &ids); err != nil || len(ids) == 0 || len(refs) == 0 {
		fail(w, 400, errors.New("выберите письма и файлы"))
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.sending || a.generating {
		fail(w, 409, errors.New("дождитесь завершения операции"))
		return
	}
	allFiles := combinedFiles(a.w.Files, files)
	set := map[string]bool{}
	for _, id := range ids {
		set[id] = true
	}
	count := 0
	candidate := a.w
	candidate.Files = allFiles
	candidate.Leads = append([]Lead(nil), a.w.Leads...)
	for i, l := range candidate.Leads {
		if !set[l.ID] {
			continue
		}
		if !editable(l) {
			fail(w, 409, errors.New("нельзя изменить вложения отправленного письма"))
			return
		}
		l.Attachments = appendAttachments(l.Attachments, refs)
		candidate.Leads[i] = l
		count++
	}
	if count != len(set) {
		fail(w, 404, errors.New("письмо не найдено"))
		return
	}
	if err = validateWorkspaceFiles(candidate); err != nil {
		fail(w, 400, err)
		return
	}
	if err = a.commit(func() { a.w = candidate; a.collectFiles() }); err != nil {
		fail(w, 500, err)
		return
	}
	respond(w, map[string]int{"updated": count})
}

func (a *App) removeAttachment(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID           string `json:"id"`
		AttachmentID string `json:"attachmentId"`
		Name         string `json:"name"`
	}
	if !decode(w, r, &in) {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.sending || a.generating {
		fail(w, 409, errors.New("дождитесь завершения операции"))
		return
	}
	for i, l := range a.w.Leads {
		if l.ID != in.ID {
			continue
		}
		if !editable(l) {
			fail(w, 409, errors.New("письмо уже отправлено"))
			return
		}
		found := false
		refs := []Attachment{}
		for _, f := range l.Attachments {
			if f.ID == in.AttachmentID && f.Name == in.Name {
				found = true
			} else {
				refs = append(refs, f)
			}
		}
		if !found {
			fail(w, 404, errors.New("вложение не найдено"))
			return
		}
		if err := a.commit(func() { a.w.Leads[i].Attachments = refs; a.collectFiles() }); err != nil {
			fail(w, 500, err)
			return
		}
		respond(w, map[string]bool{"ok": true})
		return
	}
	fail(w, 404, errors.New("письмо не найдено"))
}

func (a *App) downloadAttachment(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, l := range a.w.Leads {
		if l.ID != r.URL.Query().Get("leadId") {
			continue
		}
		for _, f := range l.Attachments {
			if f.ID == r.URL.Query().Get("id") && f.Name == r.URL.Query().Get("name") {
				data, ok := a.w.Files[f.ID]
				if !ok {
					break
				}
				w.Header().Set("Content-Type", "application/octet-stream")
				w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": f.Name}))
				_, _ = w.Write(data)
				return
			}
		}
	}
	fail(w, 404, errors.New("вложение не найдено"))
}

func (a *App) createMessage(w http.ResponseWriter, r *http.Request) {
	refs, files, err := readUploads(w, r)
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}
	if err != nil {
		fail(w, 400, err)
		return
	}
	var in struct {
		Recipients []string `json:"recipients"`
		Subject    string   `json:"subject"`
		Body       string   `json:"body"`
	}
	if err = json.Unmarshal([]byte(r.FormValue("message")), &in); err != nil {
		fail(w, 400, errors.New("некорректное письмо"))
		return
	}
	to, err := normalizeRecipients(in.Recipients)
	if err != nil {
		fail(w, 400, err)
		return
	}
	if strings.ContainsAny(in.Subject, "\r\n") || len(in.Subject) > 1000 || len(in.Body) > 50000 {
		fail(w, 400, errors.New("проверьте тему и длину письма"))
		return
	}
	l := Lead{ID: randomID(), Kind: "message", Recipients: to, Email: to[0], Company: strings.TrimSpace(in.Subject), Subject: strings.TrimSpace(in.Subject), Body: strings.TrimSpace(in.Body), Status: "pending", Attachments: refs}
	if l.Company == "" {
		l.Company = "Новое письмо"
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.sending || a.generating {
		fail(w, 409, errors.New("дождитесь завершения операции"))
		return
	}
	if len(a.w.Leads) >= 20000 {
		fail(w, 400, errors.New("слишком много писем в проекте"))
		return
	}
	candidate := a.w
	candidate.Files = combinedFiles(a.w.Files, files)
	candidate.Leads = append(append([]Lead(nil), a.w.Leads...), l)
	if err = validateWorkspaceFiles(candidate); err != nil {
		fail(w, 400, err)
		return
	}
	if err = a.commit(func() { a.w = candidate }); err != nil {
		fail(w, 500, err)
		return
	}
	respond(w, map[string]string{"id": l.ID})
}
