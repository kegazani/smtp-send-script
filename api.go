package main

import (
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

func respond(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(value)
}
func fail(w http.ResponseWriter, code int, err error) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	d := json.NewDecoder(io.LimitReader(r.Body, 2*1024*1024))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		fail(w, 400, errors.New("некорректный запрос"))
		return false
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		fail(w, 400, errors.New("лишние данные в запросе"))
		return false
	}
	return true
}

func (a *App) state(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.cfg
	s.Password = ""
	s.APIKey = ""
	view := a.w
	view.Files = nil
	respond(w, map[string]any{"version": appVersion, "project": view, "settings": s, "smtpReady": a.cfg.Password != "" && validEmail(a.cfg.Username), "deepseekReady": a.cfg.APIKey != "", "sending": a.sending, "generating": a.generating, "jobError": a.jobError, "testRecipient": a.testRecipient, "generation": a.lastGeneration})
}

func (a *App) importFile(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxProjectJSON+(2<<20))
	if err := r.ParseMultipartForm(1024 * 1024); err != nil {
		if r.MultipartForm != nil {
			r.MultipartForm.RemoveAll()
		}
		fail(w, 400, errors.New("не удалось прочитать файл проекта (максимум 180 МБ)"))
		return
	}
	defer r.MultipartForm.RemoveAll()
	f, h, err := r.FormFile("file")
	if err != nil {
		fail(w, 400, errors.New("выберите CSV или проект JSON"))
		return
	}
	defer f.Close()
	var leads []Lead
	var imported Workspace
	if strings.EqualFold(filepath.Ext(h.Filename), ".json") {
		dec := json.NewDecoder(io.LimitReader(f, maxProjectJSON))
		dec.DisallowUnknownFields()
		if err = dec.Decode(&imported); err != nil || (imported.Version != 1 && imported.Version != 2) || len(imported.Leads) > 20000 {
			fail(w, 400, errors.New("неподдерживаемый файл проекта"))
			return
		}
		var trailing any
		if dec.Decode(&trailing) != io.EOF {
			fail(w, 400, errors.New("лишние данные в проекте"))
			return
		}
		if err = validateWorkspaceFiles(imported); err != nil {
			fail(w, 400, err)
			return
		}
		leads = imported.Leads
		for _, l := range leads {
			if l.Status != "pending" && l.Status != "sent" && l.Status != "error" && l.Status != "unknown" && l.Status != "sending" {
				fail(w, 400, errors.New("неизвестный статус в проекте"))
				return
			}
		}
	} else {
		leads, err = csvLeads(f)
		if err != nil {
			fail(w, 400, err)
			return
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.sending || a.generating {
		fail(w, 409, errors.New("дождитесь завершения текущей операции"))
		return
	}
	keys := map[string]bool{}
	for _, l := range a.w.Leads {
		keys[identity(l)] = true
	}
	for _, l := range leads {
		keys[identity(l)] = true
	}
	if len(keys) > 20000 {
		fail(w, 400, errors.New("в одном проекте допускается до 20 000 писем и компаний"))
		return
	}
	added, skipped := 0, 0
	files := combinedFiles(a.w.Files, imported.Files)
	if err = validateWorkspaceFiles(Workspace{Files: files}); err != nil {
		fail(w, 400, err)
		return
	}
	err = a.commit(func() {
		a.w.Files = files
		added, skipped = a.merge(leads, imported.Sent)
		a.collectFiles()
		if imported.Version == 1 || imported.Version == 2 {
			a.w.Template = imported.Template
		}
	})
	if err != nil {
		fail(w, 500, err)
		return
	}
	respond(w, map[string]int{"added": added, "skipped": skipped})
}

func (a *App) export(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if r.URL.Query().Get("format") == "csv" {
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="maildesk-report.csv"`)
		_, _ = io.WriteString(w, "\ufeff")
		c := csv.NewWriter(w)
		_ = c.Write([]string{"Company", "Email", "Website", "City", "Industry", "Overview", "Subject", "Body", "Status", "Error", "Sent at", "Message-ID", "Kind", "Attachments"})
		for _, l := range a.w.Leads {
			names := []string{}
			for _, f := range l.Attachments {
				names = append(names, f.Name)
			}
			row := []string{l.Company, strings.Join(recipients(l), ", "), l.Website, l.City, l.Industry, l.Description, l.Subject, l.Body, l.Status, l.Error, l.SentAt, l.MessageID, l.Kind, strings.Join(names, "; ")}
			for i, s := range row {
				if strings.HasPrefix(s, "=") || strings.HasPrefix(s, "+") || strings.HasPrefix(s, "-") || strings.HasPrefix(s, "@") {
					row[i] = "'" + s
				}
			}
			_ = c.Write(row)
		}
		c.Flush()
		return
	}
	w.Header().Set("Content-Disposition", `attachment; filename="maildesk-project.json"`)
	respond(w, a.w)
}

func (a *App) template(w http.ResponseWriter, r *http.Request) {
	var t Template
	if !decode(w, r, &t) {
		return
	}
	if strings.TrimSpace(t.Subject) == "" || strings.TrimSpace(t.Body) == "" || strings.ContainsAny(t.Subject, "\r\n") || len(t.Body) > 50000 || len(t.Instructions) > 10000 || len(t.SenderProfile) > 20000 {
		fail(w, 400, errors.New("нужны тема в одну строку и текст шаблона (до 50 000 символов)"))
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.sending || a.generating {
		fail(w, 409, errors.New("дождитесь завершения операции"))
		return
	}
	if err := a.commit(func() { a.w.Template = t }); err != nil {
		fail(w, 500, err)
		return
	}
	respond(w, map[string]bool{"ok": true})
}

func (a *App) settings(w http.ResponseWriter, r *http.Request) {
	var s Settings
	if !decode(w, r, &s) {
		return
	}
	s.Host = strings.TrimSpace(s.Host)
	s.Username = strings.TrimSpace(s.Username)
	s.From = strings.TrimSpace(s.From)
	if err := validateSettings(s); err != nil {
		fail(w, 400, err)
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.sending || a.generating {
		fail(w, 409, errors.New("дождитесь завершения операции"))
		return
	}
	if s.Password == "" {
		s.Password = a.cfg.Password
	}
	if s.APIKey == "" {
		s.APIKey = a.cfg.APIKey
	}
	if err := atomicJSON(filepath.Join(a.dir, "credentials.json"), s); err != nil {
		fail(w, 500, err)
		return
	}
	a.cfg = s
	respond(w, map[string]bool{"ok": true})
}

func (a *App) smtpTest(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	s := a.cfg
	a.mu.Unlock()
	c, err := smtpConnect(s, nil)
	if err != nil {
		fail(w, 400, errors.New(cleanError(err, s)))
		return
	}
	_ = c.Quit()
	respond(w, map[string]string{"message": "SMTP: соединение, TLS и авторизация работают. Письма не отправлялись."})
}

func (a *App) updateLead(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID          string   `json:"id"`
		Company     string   `json:"company"`
		Email       string   `json:"email"`
		Subject     string   `json:"subject"`
		Body        string   `json:"body"`
		Recipients  []string `json:"recipients"`
		Description *string  `json:"description"`
	}
	if !decode(w, r, &in) {
		return
	}
	if strings.ContainsAny(in.Subject, "\r\n") || len(in.Subject) > 1000 || len(in.Body) > 50000 {
		fail(w, 400, errors.New("проверьте название, тему и длину письма"))
		return
	}
	if in.Description != nil && len(*in.Description) > 50000 {
		fail(w, 400, errors.New("описание компании — не больше 50 000 символов"))
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.sending || a.generating {
		fail(w, 409, errors.New("дождитесь завершения операции"))
		return
	}
	for i, l := range a.w.Leads {
		if l.ID == in.ID {
			if !editable(l) {
				fail(w, 409, errors.New("отправленное письмо нельзя изменить"))
				return
			}
			var to []string
			if l.Kind == "message" {
				var err error
				to, err = normalizeRecipients(in.Recipients)
				if err != nil {
					fail(w, 400, err)
					return
				}
				in.Company = strings.TrimSpace(in.Subject)
				if in.Company == "" {
					in.Company = "Новое письмо"
				}
				in.Email = to[0]
			} else if strings.TrimSpace(in.Company) == "" {
				fail(w, 400, errors.New("укажите компанию"))
				return
			}
			for _, other := range a.w.Leads {
				if l.Kind != "message" && other.Kind != "message" && other.ID != l.ID && validEmail(in.Email) && emailKey(other.Email) == emailKey(in.Email) {
					fail(w, 409, errors.New("такой адрес уже есть в проекте"))
					return
				}
			}
			if _, ok := a.w.Sent[emailKey(in.Email)]; ok && l.Kind != "message" {
				fail(w, 409, errors.New("на этот адрес уже отправлено письмо"))
				return
			}
			err := a.commit(func() {
				p := &a.w.Leads[i]
				if p.Subject != strings.TrimSpace(in.Subject) || p.Body != strings.TrimSpace(in.Body) {
					p.Source = "manual"
					p.GenerationError = ""
				}
				p.Company = strings.TrimSpace(in.Company)
				p.Email = strings.TrimSpace(in.Email)
				if l.Kind == "message" {
					p.Recipients = to
				}
				p.Subject = strings.TrimSpace(in.Subject)
				p.Body = strings.TrimSpace(in.Body)
				if l.Kind != "message" && in.Description != nil {
					p.Description = strings.TrimSpace(*in.Description)
				}
			})
			if err != nil {
				fail(w, 500, err)
				return
			}
			respond(w, map[string]bool{"ok": true})
			return
		}
	}
	fail(w, 404, errors.New("компания не найдена"))
}

func (a *App) resolve(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID      string `json:"id"`
		Checked bool   `json:"checked"`
	}
	if !decode(w, r, &in) {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if !in.Checked || a.sending || a.generating {
		fail(w, 409, errors.New("сначала проверьте доставку у почтового провайдера"))
		return
	}
	for i, l := range a.w.Leads {
		if l.ID == in.ID && l.Status == "unknown" {
			if err := a.commit(func() {
				a.w.Leads[i].Status = "error"
				a.w.Leads[i].Error = "Пользователь проверил: письмо не было принято. Можно повторить отправку."
			}); err != nil {
				fail(w, 500, err)
				return
			}
			respond(w, map[string]bool{"ok": true})
			return
		}
	}
	fail(w, 400, errors.New("нет неопределённой отправки"))
}

func (a *App) findSelected(ids []string, drafts bool) ([]Lead, error) {
	if len(ids) == 0 || len(ids) > 10000 {
		return nil, errors.New("выберите хотя бы одну компанию")
	}
	set := map[string]bool{}
	for _, id := range ids {
		if set[id] {
			return nil, errors.New("повторяющиеся компании")
		}
		set[id] = true
	}
	var leads []Lead
	for _, l := range a.w.Leads {
		if !set[l.ID] {
			continue
		}
		if !selectable(l) {
			return nil, fmt.Errorf("%s: адрес отсутствует, письмо отправлено или требует проверки", l.Company)
		}
		if _, ok := a.w.Sent[emailKey(l.Email)]; ok && l.Kind != "message" {
			return nil, errors.New("на выбранный адрес уже отправлено письмо")
		}
		if drafts && (l.Subject == "" || l.Body == "" || strings.ContainsAny(l.Subject, "\r\n")) {
			return nil, fmt.Errorf("%s: сначала подготовьте и проверьте письмо", l.Company)
		}
		if drafts {
			if err := validateAttachments(l, a.w.Files); err != nil {
				return nil, err
			}
			l.files = a.w.Files
		}
		leads = append(leads, l)
	}
	if len(leads) != len(ids) {
		return nil, errors.New("список компаний изменился")
	}
	return leads, nil
}

func (a *App) generateDrafts(w http.ResponseWriter, r *http.Request) {
	var in struct {
		IDs []string `json:"ids"`
	}
	if !decode(w, r, &in) {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.sending || a.generating {
		fail(w, 409, errors.New("уже выполняется операция"))
		return
	}
	leads, err := a.findSelected(in.IDs, false)
	if err != nil {
		fail(w, 400, err)
		return
	}
	for _, l := range leads {
		if l.Kind == "message" {
			fail(w, 400, errors.New("обычные письма редактируются вручную; генерация доступна для компаний"))
			return
		}
	}
	a.generating = true
	a.lastGeneration = GenerationSummary{Total: len(leads)}
	a.stop = false
	a.jobError = ""
	s, t := a.cfg, a.w.Template
	go func() {
		defer func() { a.mu.Lock(); a.generating = false; a.mu.Unlock() }()
		for _, l := range leads {
			a.mu.Lock()
			stop := a.stop
			a.mu.Unlock()
			if stop {
				return
			}
			var subject, body, source string
			var e error
			if strings.TrimSpace(l.Description) == "" {
				subject, body = renderTemplate(t, l)
				source = "template"
			} else {
				subject, body, e = a.generate(s, t, l)
				source = "deepseek"
			}
			a.mu.Lock()
			err := a.commit(func() {
				for i, p := range a.w.Leads {
					if p.ID == l.ID {
						p.GenerationError = cleanError(e, s)
						if e == nil {
							p.Subject = subject
							p.Body = body
							p.Source = source
						}
						a.w.Leads[i] = p
						break
					}
				}
			})
			if err != nil {
				a.jobError = "Не удалось сохранить черновики: " + err.Error()
			} else {
				a.lastGeneration.Completed++
				if e != nil {
					a.lastGeneration.Failed++
					a.lastGeneration.Error = cleanError(e, s)
				} else if source == "deepseek" {
					a.lastGeneration.Personalized++
				} else {
					a.lastGeneration.Templates++
				}
			}
			a.mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	respond(w, map[string]bool{"ok": true})
}

func digest(leads []Lead, s Settings) string {
	sort.Slice(leads, func(i, j int) bool { return leads[i].ID < leads[j].ID })
	b, _ := json.Marshal(struct {
		Leads    []Lead
		Settings Settings
	}{leads, s})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func (a *App) prepare(w http.ResponseWriter, r *http.Request) {
	var in struct {
		IDs []string `json:"ids"`
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
	if err := validateSettings(a.cfg); err != nil {
		fail(w, 400, err)
		return
	}
	if a.cfg.Password == "" {
		fail(w, 400, errors.New("добавьте SMTP-пароль"))
		return
	}
	leads, err := a.findSelected(in.IDs, true)
	if err != nil {
		fail(w, 400, err)
		return
	}
	if a.testRecipient != "" {
		for _, l := range leads {
			if !a.testRecipientAllowed(l) {
				fail(w, 403, fmt.Errorf("тестовый режим: отправка разрешена только на %s", a.testRecipient))
				return
			}
		}
	}
	a.confirmation = &Confirmation{Token: randomID(), Stage: 1, IDs: in.IDs, Digest: digest(leads, a.cfg), Expires: time.Now().Add(5 * time.Minute)}
	respond(w, map[string]any{"token": a.confirmation.Token, "count": len(leads), "from": a.cfg.From})
}

func (a *App) confirmationStep(w http.ResponseWriter, r *http.Request, stage int) {
	var in struct {
		Token string `json:"token"`
	}
	if !decode(w, r, &in) {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	c := a.confirmation
	if a.sending || a.generating || c == nil || c.Token != in.Token || c.Stage != stage || time.Now().After(c.Expires) {
		fail(w, 409, errors.New("подтверждение истекло; начните заново"))
		return
	}
	leads, err := a.findSelected(c.IDs, true)
	if err != nil {
		fail(w, 409, err)
		return
	}
	if digest(leads, a.cfg) != c.Digest {
		a.confirmation = nil
		fail(w, 409, errors.New("письма или настройки изменились; проверьте и подтвердите заново"))
		return
	}
	if stage == 1 {
		c.Stage = 2
		c.Token = randomID()
		respond(w, map[string]string{"token": c.Token})
		return
	}
	s := a.cfg
	if err = a.commit(func() {
		for i := range a.w.Leads {
			for _, l := range leads {
				if a.w.Leads[i].ID == l.ID {
					a.w.Leads[i].Status = "sending"
					a.w.Leads[i].Error = ""
					a.w.Leads[i].MessageID = randomID() + "@" + strings.Split(s.From, "@")[1]
				}
			}
		}
	}); err != nil {
		fail(w, 500, err)
		return
	}
	for i, l := range leads {
		for _, p := range a.w.Leads {
			if p.ID == l.ID {
				p.files = l.files
				leads[i] = p
			}
		}
	}
	a.confirmation = nil
	a.sending = true
	a.stop = false
	a.jobError = ""
	go a.runSend(s, leads)
	respond(w, map[string]bool{"ok": true})
}

func (a *App) runSend(s Settings, leads []Lead) {
	defer func() { a.mu.Lock(); a.sending = false; a.mu.Unlock() }()
	for _, l := range leads {
		a.mu.Lock()
		stop := a.stop
		a.mu.Unlock()
		if stop {
			a.mu.Lock()
			err := a.commit(func() {
				for i := range a.w.Leads {
					if a.w.Leads[i].Status == "sending" {
						a.w.Leads[i].Status = "pending"
					}
				}
			})
			if err != nil {
				a.jobError = err.Error()
			}
			a.mu.Unlock()
			return
		}
		var ambiguous bool
		var err error
		if !a.testRecipientAllowed(l) {
			err = errors.New("тестовый режим запрещает этого получателя")
		} else {
			ambiguous, err = a.deliver(s, l)
		}
		a.mu.Lock()
		saveErr := a.commit(func() {
			for i, p := range a.w.Leads {
				if p.ID != l.ID {
					continue
				}
				p.Error = cleanError(err, s)
				if err == nil {
					p.Status = "sent"
					p.SentAt = time.Now().UTC().Format(time.RFC3339)
					if p.Kind != "message" {
						a.w.Sent[emailKey(p.Email)] = p.SentAt
					}
				} else if ambiguous {
					p.Status = "unknown"
				} else {
					p.Status = "error"
				}
				a.w.Leads[i] = p
			}
		})
		if saveErr != nil {
			a.jobError = "Не удалось записать результат отправки. Перезапустите приложение и проверьте доставку: " + saveErr.Error()
			for i := range a.w.Leads {
				if a.w.Leads[i].Status == "sending" {
					a.w.Leads[i].Status = "unknown"
					a.w.Leads[i].Error = a.jobError
				}
			}
		}
		a.mu.Unlock()
		if saveErr != nil {
			return
		}
		if a.delay > 0 {
			time.Sleep(a.delay)
		}
	}
}

func (a *App) routes() *http.ServeMux {
	m := http.NewServeMux()
	m.HandleFunc("GET /api/state", a.state)
	m.HandleFunc("POST /api/import", a.importFile)
	m.HandleFunc("GET /api/export", a.export)
	m.HandleFunc("POST /api/template", a.template)
	m.HandleFunc("POST /api/settings", a.settings)
	m.HandleFunc("POST /api/smtp-test", a.smtpTest)
	m.HandleFunc("POST /api/lead", a.updateLead)
	m.HandleFunc("POST /api/message", a.createMessage)
	m.HandleFunc("POST /api/attachments", a.addAttachments)
	m.HandleFunc("POST /api/attachments/remove", a.removeAttachment)
	m.HandleFunc("GET /api/attachment", a.downloadAttachment)
	m.HandleFunc("POST /api/resolve", a.resolve)
	m.HandleFunc("POST /api/generate", a.generateDrafts)
	m.HandleFunc("POST /api/prepare", a.prepare)
	m.HandleFunc("POST /api/confirm", func(w http.ResponseWriter, r *http.Request) { a.confirmationStep(w, r, 1) })
	m.HandleFunc("POST /api/send", func(w http.ResponseWriter, r *http.Request) { a.confirmationStep(w, r, 2) })
	m.HandleFunc("POST /api/stop", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		a.stop = true
		a.mu.Unlock()
		respond(w, map[string]bool{"ok": true})
	})
	m.HandleFunc("POST /api/quit", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.sending || a.generating {
			fail(w, 409, errors.New("дождитесь окончания текущей операции или остановите очередь"))
			return
		}
		respond(w, map[string]bool{"ok": true})
		if a.shutdown != nil {
			go func() { time.Sleep(200 * time.Millisecond); a.shutdown() }()
		}
	})
	return m
}
