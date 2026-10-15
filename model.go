package main

import (
	"crypto/rand"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/mail"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

type Lead struct {
	ID              string `json:"id"`
	Company         string `json:"company"`
	Email           string `json:"email"`
	Website         string `json:"website"`
	City            string `json:"city"`
	Industry        string `json:"industry"`
	Description     string `json:"description"`
	Subject         string `json:"subject"`
	Body            string `json:"body"`
	Status          string `json:"status"`
	Error           string `json:"error,omitempty"`
	GenerationError string `json:"generationError,omitempty"`
	SentAt          string `json:"sentAt,omitempty"`
	MessageID       string `json:"messageId,omitempty"`
	Source          string `json:"source,omitempty"`
}

type Template struct {
	Subject      string `json:"subject"`
	Body         string `json:"body"`
	Instructions string `json:"instructions"`
}

type Workspace struct {
	Version  int               `json:"version"`
	Template Template          `json:"template"`
	Leads    []Lead            `json:"leads"`
	Sent     map[string]string `json:"sent"`
}

type Settings struct {
	Host       string `json:"host"`
	Port       int    `json:"port"`
	Security   string `json:"security"`
	Username   string `json:"username"`
	Password   string `json:"password,omitempty"`
	From       string `json:"from"`
	SenderName string `json:"senderName"`
	APIKey     string `json:"apiKey,omitempty"`
}

type Confirmation struct {
	Token   string
	Stage   int
	IDs     []string
	Digest  string
	Expires time.Time
}

type App struct {
	mu            sync.Mutex
	w             Workspace
	cfg           Settings
	dir           string
	session       string
	origin        string
	confirmation  *Confirmation
	sending       bool
	generating    bool
	stop          bool
	jobError      string
	delay         time.Duration
	testRecipient string
	shutdown      func()
	// Test seams exercise real protocol adapters without contacting recipients.
	deliver  func(Settings, Lead) (bool, error)
	generate func(Settings, Template, Lead) (string, string, error)
}

func randomID() string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func atomicJSON(path string, value any) error {
	b, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".maildesk-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(b)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(f.Name(), path)
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func openApp(dir string) (*App, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	a := &App{dir: dir, session: randomID(), deliver: sendSMTP, generate: generateDeepSeek, delay: time.Second}
	a.w = Workspace{Version: 1, Leads: []Lead{}, Sent: map[string]string{}, Template: Template{
		Subject:      "Возможное сотрудничество с {company}",
		Body:         "Здравствуйте!\n\nХотели бы обсудить возможное сотрудничество с {company}. Подскажите, с кем можно связаться по этому вопросу?\n\nС уважением,\nКоманда",
		Instructions: "Напиши короткое деловое письмо на русском языке. Используй предложение и подпись из шаблона. Не выдумывай факты о нас или компании получателя."}}
	if err := readJSON(filepath.Join(dir, "project.json"), &a.w); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("не удалось прочитать проект: %w", err)
	}
	if a.w.Version != 1 {
		return nil, errors.New("неподдерживаемая версия проекта")
	}
	if a.w.Sent == nil {
		a.w.Sent = map[string]string{}
	}
	if a.w.Leads == nil {
		a.w.Leads = []Lead{}
	}
	a.cfg = Settings{Host: "smtp.mail.ru", Port: 465, Security: "ssl"}
	if err := readJSON(filepath.Join(dir, "credentials.json"), &a.cfg); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("не удалось прочитать настройки: %w", err)
	}
	// Explicit local environment bindings are used only for missing settings.
	if a.cfg.Username == "" {
		a.cfg.Username = os.Getenv("SMTP_USERNAME")
	}
	if a.cfg.From == "" {
		a.cfg.From = os.Getenv("SMTP_FROM_EMAIL")
		if a.cfg.From == "" {
			a.cfg.From = a.cfg.Username
		}
	}
	if a.cfg.Password == "" {
		a.cfg.Password = os.Getenv("SMTP_PASSWORD")
	}
	if a.cfg.APIKey == "" {
		a.cfg.APIKey = os.Getenv("LLM_API_KEY")
	}
	for i := range a.w.Leads {
		l := &a.w.Leads[i]
		if l.Status == "sending" {
			l.Status = "unknown"
			l.Error = "Приложение закрылось во время отправки. Проверьте доставку у провайдера перед повтором."
		}
		if l.Status == "sent" {
			a.w.Sent[emailKey(l.Email)] = l.SentAt
		}
	}
	if err := a.save(); err != nil {
		return nil, err
	}
	if err := atomicJSON(filepath.Join(dir, "credentials.json"), a.cfg); err != nil {
		return nil, err
	}
	return a, nil
}

func (a *App) save() error { return atomicJSON(filepath.Join(a.dir, "project.json"), a.w) }

// commit rolls back mutations when the persistent write fails. Caller holds mu.
func (a *App) commit(change func()) error {
	before, _ := json.Marshal(a.w)
	change()
	if err := a.save(); err != nil {
		_ = json.Unmarshal(before, &a.w)
		return err
	}
	return nil
}

func emailKey(s string) string { return strings.ToLower(strings.TrimSpace(s)) }
func validEmail(s string) bool {
	if strings.ContainsAny(s, "\r\n") {
		return false
	}
	a, err := mail.ParseAddress(strings.TrimSpace(s))
	return err == nil && a.Address == strings.TrimSpace(s) && strings.Contains(a.Address, ".")
}
func identity(l Lead) string {
	if validEmail(l.Email) {
		return emailKey(l.Email)
	}
	return "company:" + strings.ToLower(strings.TrimSpace(l.Company))
}
func editable(l Lead) bool   { return l.Status != "sent" && l.Status != "sending" }
func selectable(l Lead) bool { return editable(l) && l.Status != "unknown" && validEmail(l.Email) }

func csvLeads(r io.Reader) ([]Lead, error) {
	b, err := io.ReadAll(io.LimitReader(r, 20*1024*1024+1))
	if err != nil {
		return nil, err
	}
	if len(b) > 20*1024*1024 {
		return nil, errors.New("файл больше 20 МБ")
	}
	if !utf8.Valid(b) {
		return nil, errors.New("сохраните CSV в кодировке UTF-8")
	}
	s := strings.TrimPrefix(string(b), "\ufeff")
	first := strings.SplitN(s, "\n", 2)[0]
	c := csv.NewReader(strings.NewReader(s))
	c.FieldsPerRecord = -1
	if strings.Count(first, ";") > strings.Count(first, ",") {
		c.Comma = ';'
	}
	head, err := c.Read()
	if err != nil {
		return nil, errors.New("пустой или повреждённый CSV")
	}
	cols := map[string]int{}
	for i, h := range head {
		cols[strings.ToLower(strings.TrimSpace(h))] = i
	}
	if _, ok := cols["company"]; !ok {
		return nil, errors.New("нужна колонка Company")
	}
	if _, ok := cols["email"]; !ok {
		return nil, errors.New("нужна колонка Email")
	}
	var leads []Lead
	for row := 2; ; row++ {
		rec, e := c.Read()
		if e == io.EOF {
			break
		}
		if e != nil {
			return nil, fmt.Errorf("строка %d: %w", row, e)
		}
		get := func(name string) string {
			i, ok := cols[name]
			if !ok || i >= len(rec) {
				return ""
			}
			return strings.TrimSpace(rec[i])
		}
		l := Lead{ID: randomID(), Company: get("company"), Email: get("email"), Website: get("website"), City: get("city"), Industry: get("industry"), Description: get("overview"), Status: "pending"}
		if l.Company == "" && l.Email == "" {
			continue
		}
		if l.Company == "" {
			l.Company = l.Email
		}
		leads = append(leads, l)
		if len(leads) > 10000 {
			return nil, errors.New("не больше 10 000 компаний за один импорт")
		}
	}
	if len(leads) == 0 {
		return nil, errors.New("в CSV нет компаний")
	}
	return leads, nil
}

func (a *App) merge(leads []Lead, sent map[string]string) (added, skipped int) {
	for email, at := range sent {
		if validEmail(email) {
			a.w.Sent[emailKey(email)] = at
		}
	}
	known := map[string]int{}
	for i, l := range a.w.Leads {
		known[identity(l)] = i
	}
	for _, l := range leads {
		if l.Status == "sent" {
			a.w.Sent[emailKey(l.Email)] = l.SentAt
		}
		if i, ok := known[identity(l)]; ok {
			skipped++
			if l.Status == "sent" && a.w.Leads[i].Status != "sent" {
				a.w.Leads[i] = l
			}
			continue
		}
		l.ID = randomID()
		if l.Status == "" {
			l.Status = "pending"
		}
		if l.Status == "sending" {
			l.Status = "unknown"
			l.Error = "Перенесено во время отправки. Проверьте доставку перед повтором."
		}
		known[identity(l)] = len(a.w.Leads)
		a.w.Leads = append(a.w.Leads, l)
		added++
	}
	for i := range a.w.Leads {
		l := &a.w.Leads[i]
		if at, ok := a.w.Sent[emailKey(l.Email)]; ok {
			l.Status = "sent"
			l.SentAt = at
			l.Error = ""
		}
	}
	return
}

func renderTemplate(t Template, l Lead) (string, string) {
	r := strings.NewReplacer("{company}", l.Company, "{city}", l.City, "{industry}", l.Industry, "{description}", l.Description, "{website}", l.Website)
	return r.Replace(t.Subject), r.Replace(t.Body)
}
