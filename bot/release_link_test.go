package main

import (
	"strings"
	"testing"
	"time"
)

// Две половины одного релиза приезжают РАЗНЫМИ событиями.
//
// Пайплайн знает адрес коммита и список изменений; release.sh на сервере —
// previous, то есть на что показывал симлинк до переключения. Пока сюда
// доезжало только последнее событие, карточка теряла половину.
func TestДваСобытияОднойЦелиСкладываютсяАНеЗатираются(t *testing.T) {
	at := time.Date(2026, 8, 22, 8, 0, 0, 0, time.UTC)
	fromCI := Deploy{
		Kind: deploySuccess, App: "metro", Title: "Метро", URL: "https://metro.samoy.love/",
		Version:   "release-20260822-094224-a590514",
		CommitURL: "https://github.com/tr0llex/metro-map/commit/a590514",
		Changelog: []string{"Не пересчитывать раскладку подписей на каждом кадре"},
		At:        at,
	}
	fromHost := Deploy{
		Kind: deploySuccess, App: "metro",
		Version:  "release-20260822-094224-a590514",
		Previous: "release-20260822-092847-cd6ef4b",
		At:       at.Add(time.Second),
	}

	got := deployOutcomes([]Deploy{fromCI, fromHost})
	if len(got) != 1 {
		t.Fatalf("цель должна остаться одной строкой, получили %d", len(got))
	}
	d := got[0]
	if d.Previous != "release-20260822-092847-cd6ef4b" {
		t.Errorf("previous с сервера потерян: %q", d.Previous)
	}
	if d.CommitURL != "https://github.com/tr0llex/metro-map/commit/a590514" {
		t.Errorf("адрес коммита из пайплайна потерян: %q", d.CommitURL)
	}
	if len(d.Changelog) != 1 {
		t.Errorf("список изменений из пайплайна потерян: %v", d.Changelog)
	}
	if d.Title == "" || d.URL == "" {
		t.Errorf("имя и адрес цели потеряны: %q %q", d.Title, d.URL)
	}
}

// Шапка прогона не имеет права называть версию одной цели версией всех.
func TestШапкаПрогонаНеПриклеиваетВерсиюОднойЦелиКОстальным(t *testing.T) {
	at := time.Date(2026, 8, 22, 8, 0, 0, 0, time.UTC)
	commit := "https://github.com/tr0llex/chillhub/commit/d96d6dc"
	site := Deploy{
		Kind: deploySuccess, App: "chillhub-site", Title: "Сайт", URL: "https://launcher.samoy.love/",
		Version: "release-20260822-105848-d96d6dc", CommitURL: commit, At: at,
	}
	api := Deploy{
		Kind: deploySuccess, App: "chillhub-api", Title: "API", URL: "https://launcher.samoy.love/",
		Version: "release-20260822-105949-d96d6dc", CommitURL: commit, At: at,
	}

	got := formatDeployGroup("ChillHub", []Deploy{site, api})

	head, _, _ := strings.Cut(got, "\n")
	if strings.Contains(head, "release-20260822-105848-d96d6dc") {
		t.Errorf("версия одной цели уехала в шапку и накрыла собой вторую:\n%s", got)
	}
	if !strings.Contains(head, ">d96d6dc<") {
		t.Errorf("в шапке нет общего для прогона коммита:\n%s", head)
	}
	for _, v := range []string{"release-20260822-105848-d96d6dc", "release-20260822-105949-d96d6dc"} {
		if !strings.Contains(got, v) {
			t.Errorf("версия цели пропала из карточки: %s\n%s", v, got)
		}
	}
}

// Одна версия на весь прогон — шапка прежняя, версий в строках нет.
func TestОдинаковыеВерсииОстаютсяВШапке(t *testing.T) {
	at := time.Date(2026, 8, 22, 8, 0, 0, 0, time.UTC)
	mk := func(app string) Deploy {
		return Deploy{
			Kind: deploySuccess, App: app, Title: app, URL: "https://samoy.love/",
			Version:   "release-20260822-094221-f328bc5",
			CommitURL: "https://github.com/tr0llex/status.samoy.love/commit/f328bc5",
			At:        at,
		}
	}
	got := formatDeployGroup("Статус", []Deploy{mk("status-site"), mk("status-agent")})
	head, _, _ := strings.Cut(got, "\n")
	if !strings.Contains(head, "release-20260822-094221-f328bc5") {
		t.Errorf("общая версия обязана остаться в шапке:\n%s", head)
	}
	if strings.Count(got, "release-20260822-094221-f328bc5") != 1 {
		t.Errorf("версию не надо повторять в строках, когда она одна:\n%s", got)
	}
}

func TestСсылкаНаСравнениеСтроитсяТолькоИзПроверенного(t *testing.T) {
	const repo = "https://github.com/tr0llex/metro-map"
	const commit = repo + "/commit/a590514"

	got := compareHTML(repo, "release-20260822-092847-cd6ef4b", commit)
	if !strings.Contains(got, repo+"/compare/cd6ef4b...a590514") {
		t.Errorf("диапазон собран неверно: %q", got)
	}

	// Версия чужой схемы (цель-файл) sha не содержит — ссылки быть не должно.
	if v := compareHTML(repo, "1.5.28", commit); v != "" {
		t.Errorf("из версии без коммита ссылка строиться не должна: %q", v)
	}
	// Первая выкатка цели: previous нет.
	if v := compareHTML(repo, "", commit); v != "" {
		t.Errorf("без previous ссылки быть не должно: %q", v)
	}
	// Диапазон в одну точку показывать нечего.
	if v := compareHTML(repo, "release-20260822-092847-a590514", commit); v != "" {
		t.Errorf("пустой диапазон не должен давать ссылку: %q", v)
	}
	// Адрес коммита не прошёл проверку — ничего не выдумываем.
	if v := compareHTML(repo, "release-20260822-092847-cd6ef4b", "http://зло/commit/a590514"); v != "" {
		t.Errorf("непроверенный адрес не должен попадать в ссылку: %q", v)
	}
}

// Пустой список изменений называется вслух и подкрепляется ссылкой.
func TestПустойСписокИзмененийНеМолчит(t *testing.T) {
	const repo = "https://github.com/tr0llex/metro-map"
	const commit = repo + "/commit/a590514"

	got := changelogTail(nil, repo, "release-20260822-092847-cd6ef4b", commit)
	if !strings.Contains(got, "изменений в этом релизе нет") {
		t.Errorf("пустой список обязан называться вслух: %q", got)
	}
	if !strings.Contains(got, "/compare/cd6ef4b...a590514") {
		t.Errorf("утверждение обязано быть проверяемым ссылкой: %q", got)
	}

	full := changelogTail([]string{"Починить обрыв скачивания"}, repo, "release-20260822-092847-cd6ef4b", commit)
	if !strings.Contains(full, "<b>Изменения</b>") {
		t.Errorf("непустой список обязан остаться блоком «Изменения»: %q", full)
	}
	if strings.Contains(full, "изменений в этом релизе нет") {
		t.Errorf("при непустом списке строки про «нет» быть не должно: %q", full)
	}
}
