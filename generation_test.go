package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestPersonalizedRequestExcludesFallbackAndCarriesDescription(t *testing.T) {
	template := Template{Subject: "FALLBACK_SUBJECT", Body: "FALLBACK_BODY", Instructions: "Предложи обслуживание оборудования. Подпись: Алексей.", SenderProfile: "Наша компания СервисВент обслуживает вентиляционное оборудование."}
	lead := Lead{Company: "ВентСистемы", Description: "Производит VAV-клапаны для промышленной вентиляции.", City: "Москва"}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if len(payload.Messages) != 2 {
			t.Fatal("unexpected messages")
		}
		var input map[string]string
		if err := json.Unmarshal([]byte(payload.Messages[1].Content), &input); err != nil {
			t.Fatal(err)
		}
		if input["description"] != lead.Description || input["company"] != lead.Company || input["instructions"] != template.Instructions || input["senderProfile"] != template.SenderProfile {
			t.Error("company context or custom instructions lost")
		}
		all := payload.Messages[0].Content + payload.Messages[1].Content
		if strings.Contains(all, template.Subject) || strings.Contains(all, template.Body) {
			t.Error("fallback letter leaked into personalized context")
		}
		if _, ok := input["templateBody"]; ok {
			t.Error("fallback body was sent")
		}
		respond(w, map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": `{"subject":"Обслуживание VAV-клапанов","body":"Здравствуйте! Вижу, что вы производите VAV-клапаны. Хотел бы обсудить обслуживание вашего оборудования. Алексей."}`}}}})
	}))
	defer server.Close()
	subject, body, err := deepSeekRequest(server.Client(), server.URL, Settings{APIKey: "test-key"}, template, lead)
	if err != nil || !strings.Contains(subject, "VAV") || !strings.Contains(body, "VAV-клапаны") {
		t.Fatal(subject, body, err)
	}
}

func TestExactFallbackResponseIsNotPresentedAsPersonalized(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		respond(w, map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": `{"subject":"Тема","body":"Здравствуйте, Компания!"}`}}}})
	}))
	defer server.Close()
	_, _, err := deepSeekRequest(server.Client(), server.URL, Settings{APIKey: "test-key"}, Template{Body: "Здравствуйте, {company}!"}, Lead{Company: "Компания", Description: "Производство вентиляционных клапанов"})
	if err == nil || !strings.Contains(err.Error(), "без персонализации") {
		t.Fatal("generic template was accepted as personalized", err)
	}
}

func TestGenerationUsesDescriptionOrFallbackAndReportsErrors(t *testing.T) {
	a := newTestApp(t)
	a.w.Leads = []Lead{
		{ID: "one", Company: "Клапаны", Email: testRecipient, Description: "Производит VAV-клапаны", Status: "pending"},
		{ID: "two", Company: "Нет описания", Email: testRecipient, Status: "pending"},
		{ID: "three", Company: "Ошибка API", Email: testRecipient, Description: "Выпускает фильтры", Subject: "Старый черновик", Body: "Не терять этот текст", Source: "manual", Status: "pending"},
	}
	var calls atomic.Int32
	a.generate = func(_ Settings, _ Template, l Lead) (string, string, error) {
		calls.Add(1)
		if l.ID == "three" {
			return "", "", errors.New("DeepSeek HTTP 401")
		}
		if l.Description == "" {
			t.Error("API called without a description")
		}
		return "О VAV-клапанах", "Заметили, что вы производите VAV-клапаны.", nil
	}
	code, out := call(t, a, "generate", map[string]any{"ids": []string{"one", "two", "three"}})
	if code != 200 {
		t.Fatal(out)
	}
	waitIdle(t, a)
	if calls.Load() != 2 || a.w.Leads[0].Source != "deepseek" || a.w.Leads[1].Source != "template" {
		t.Fatal("wrong generation mode")
	}
	if a.w.Leads[2].Body != "Не терять этот текст" || a.w.Leads[2].GenerationError == "" || a.w.Leads[2].Source != "manual" {
		t.Fatal("failed generation replaced the saved draft")
	}
	g := a.lastGeneration
	if g.Completed != 3 || g.Personalized != 1 || g.Templates != 1 || g.Failed != 1 || !strings.Contains(g.Error, "401") {
		t.Fatal(g)
	}
}

func TestCSVDescriptionAliasesAndReimportRefreshContext(t *testing.T) {
	for _, header := range []string{"Overview", "Company Overview", "Description", "Company description", "Описание", "Описание компании"} {
		csv := "Company,Email," + header + "\nКомпания," + testRecipient + ",Производство VAV-клапанов\n"
		leads, err := csvLeads(strings.NewReader(csv))
		if err != nil || leads[0].Description != "Производство VAV-клапанов" {
			t.Fatal(header, err)
		}
		a := newTestApp(t)
		before := a.w.Leads[0].Body
		a.merge(leads, nil)
		if a.w.Leads[0].Description != leads[0].Description || a.w.Leads[0].Body != before || len(a.w.Leads) != 1 {
			t.Fatal("reimport failed to update context while preserving draft")
		}
	}
}

func TestCompanyDescriptionEditableAndCustomInstructionsPreserved(t *testing.T) {
	a := newTestApp(t)
	code, out := call(t, a, "lead", map[string]any{"id": "one", "company": "Компания", "email": testRecipient, "subject": a.w.Leads[0].Subject, "body": a.w.Leads[0].Body, "description": "Выпускает промышленные вентиляторы"})
	if code != 200 || a.w.Leads[0].Description != "Выпускает промышленные вентиляторы" {
		t.Fatal(out)
	}
	for _, instructions := range []string{legacyGenerationInstructions, "Моё предложение и моя подпись"} {
		a.w.Template.Instructions = instructions
		if err := a.save(); err != nil {
			t.Fatal(err)
		}
		loaded, err := openApp(a.dir)
		if err != nil {
			t.Fatal(err)
		}
		expected := instructions
		if instructions == legacyGenerationInstructions {
			expected = defaultGenerationInstructions
		}
		if loaded.w.Template.Instructions != expected {
			t.Fatal("custom instructions overwritten or legacy default retained")
		}
	}
}
