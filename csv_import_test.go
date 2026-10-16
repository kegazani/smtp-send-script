package main

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"mime/multipart"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func companyExportFixture(t *testing.T, delimiter rune) string {
	t.Helper()
	var out strings.Builder
	out.WriteString("\ufeff")
	w := csv.NewWriter(&out)
	w.Comma = delimiter
	w.UseCRLF = true
	err := w.WriteAll([][]string{
		{"Id", "Name", "Domain Name / Link URL", "Domain Name / Link Label", "Domain Name / Secondary Links", "Phone", "Email", "Industry", "HQ Location", "Company Overview", "Creation date"},
		{"external-one", "Завод «Пример»", "example.com", "Official website", "[]", "+7 (000) 000-00-00", testRecipient, "Вентиляция", "Москва, Россия", "Производит клапаны, фильтры и воздуховоды.\nМонтаж для предприятий.", "2026-10-15T09:27:58Z"},
		{"external-two", "Компания без email", "empty.example.com", "Official website", "[]", "", "", "Оборудование", "Казань, Россия", "Выпускает промышленные вентиляторы.", "2026-10-15T09:27:58Z"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return out.String()
}

func TestCSVCompanyExportColumns(t *testing.T) {
	for _, delimiter := range []rune{',', ';'} {
		t.Run(string(delimiter), func(t *testing.T) {
			leads, err := csvLeads(strings.NewReader(companyExportFixture(t, delimiter)))
			if err != nil || len(leads) != 2 {
				t.Fatalf("unexpected import: %d leads, %v", len(leads), err)
			}
			l := leads[0]
			if l.Company != "Завод «Пример»" || l.Email != testRecipient || l.Website != "example.com" || l.City != "Москва, Россия" || l.Industry != "Вентиляция" || l.Description != "Производит клапаны, фильтры и воздуховоды.\nМонтаж для предприятий." {
				t.Fatalf("company context lost: %+v", l)
			}
			if !selectable(l) || leads[1].Company != "Компания без email" || leads[1].Description == "" || selectable(leads[1]) {
				t.Fatal("missing email should keep the company but prevent selection")
			}
		})
	}
}

func TestCSVMixedAliasesUseFirstNonemptyValue(t *testing.T) {
	data := " Company , name ,Email,Website,Domain Name / Link URL,City,HQ Location,Overview,Company Overview\n" +
		"Прежнее имя,Новое имя," + testRecipient + ",old.example.com,new.example.com,Москва,Казань,Старое описание,Новое описание\n" +
		",Новое имя,, ,new.example.com,,Казань, ,Новое описание\n"
	leads, err := csvLeads(strings.NewReader(data))
	if err != nil || len(leads) != 2 {
		t.Fatal(leads, err)
	}
	if leads[0].Company != "Прежнее имя" || leads[0].Website != "old.example.com" || leads[0].City != "Москва" || leads[0].Description != "Старое описание" {
		t.Fatal("legacy columns should take precedence when nonempty")
	}
	if leads[1].Company != "Новое имя" || leads[1].Website != "new.example.com" || leads[1].City != "Казань" || leads[1].Description != "Новое описание" {
		t.Fatal("empty legacy cells should fall back to export columns")
	}
}

func TestCompanyExportImportUsesDeepSeekAndPreservesDraftOnReimport(t *testing.T) {
	a := newTestApp(t)
	a.w.Leads = nil
	importCSV := func() map[string]int {
		t.Helper()
		var body bytes.Buffer
		form := multipart.NewWriter(&body)
		part, err := form.CreateFormFile("file", "company-export.csv")
		if err != nil {
			t.Fatal(err)
		}
		if _, err = part.Write([]byte(companyExportFixture(t, ','))); err != nil {
			t.Fatal(err)
		}
		if err = form.Close(); err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest("POST", "http://localhost/api/import", &body)
		r.Header.Set("Content-Type", form.FormDataContentType())
		w := httptest.NewRecorder()
		a.routes().ServeHTTP(w, r)
		var result map[string]int
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil {
			t.Fatal(w.Code, w.Body.String())
		}
		return result
	}
	if result := importCSV(); result["added"] != 2 {
		t.Fatal(result)
	}
	var calls atomic.Int32
	a.generate = func(_ Settings, _ Template, l Lead) (string, string, error) {
		calls.Add(1)
		if !strings.Contains(l.Description, "Монтаж для предприятий") || l.Website != "example.com" {
			t.Error("new CSV company context did not reach generation")
		}
		return "Монтаж вентиляции", "Индивидуальное предложение для завода.", nil
	}
	id := a.w.Leads[0].ID
	if code, out := call(t, a, "generate", map[string]any{"ids": []string{id}}); code != 200 {
		t.Fatal(out)
	}
	waitIdle(t, a)
	if calls.Load() != 1 || a.lastGeneration.Personalized != 1 || a.lastGeneration.Templates != 0 || a.w.Leads[0].Source != "deepseek" {
		t.Fatal("Company Overview must select DeepSeek rather than the fallback template")
	}
	before := a.w.Leads[0]
	if result := importCSV(); result["added"] != 0 || result["skipped"] != 2 {
		t.Fatal("reimport created duplicates", result)
	}
	if after := a.w.Leads[0]; after.ID != id || after.Body != before.Body || after.Source != before.Source {
		t.Fatal("reimport changed the prepared draft")
	}
}
