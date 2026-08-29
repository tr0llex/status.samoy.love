package main

import (
	"strings"
	"testing"
	"time"
)

// Версия релиза как ссылка на коммит.
//
// Отдельный файл по той же причине, что и link_test.go: это граница доверия.
// Адрес приезжает из summary.json — файла на диске, который, как записано в
// summary.go, может быть собран и мимо агента, — а уходит в разметку с
// parse_mode=HTML. Проверки написаны от враждебного входа.
//
// Вторая половина — про ОТКАЗ ставить ссылку. Она важнее первой: сообщение о
// релизе обязано выйти и без адреса, ровно таким, каким выходило раньше.

const (
	relVersion = "release-20260803-101500-abc1234"
	relCommit  = "https://github.com/tr0llex/status.samoy.love/commit/abc1234"
)

func releaseEvent(commitURL string) Event {
	return Event{
		Kind: KindRelease, Title: "Статус · Страница",
		URL: "https://status.samoy.love/", Project: "status",
		Version: relVersion, Previous: "release-20260802-090000-def5678",
		CommitURL: commitURL,
		At:        time.Date(2026, 8, 3, 10, 15, 0, 0, time.UTC),
	}
}

func TestВерсияВСообщенииВедётНаКоммит(t *testing.T) {
	// Ради этого всё и делалось: версия release-<дата>-<sha> называет коммит,
	// но не ведёт к нему — sha приходилось искать руками.
	got := formatEvent(releaseEvent(relCommit))

	if want := `<a href="` + relCommit + `">` + relVersion + `</a>`; !strings.Contains(got, want) {
		t.Errorf("версия не стала ссылкой на коммит:\nожидали %s\nполучили %s", want, got)
	}
	// Всё прежнее содержимое остаётся на месте: ссылка добавлена, а не
	// заменила собой сообщение.
	for _, want := range []string{"обновлён", "была <code>release-20260802-090000-def5678</code>", "собрано"} {
		if !strings.Contains(got, want) {
			t.Errorf("из сообщения пропало %q:\n%s", want, got)
		}
	}
}

func TestБезАдресаКоммитаВерсияОстаётсяТекстом(t *testing.T) {
	// Обычный случай, а не сбой: у цели, чья версия читается симлинком
	// релиза, version.json нет вовсе, а значит нет ни коммита, ни адреса
	// репозитория. Сообщение обязано выйти прежним.
	got := formatEvent(releaseEvent(""))

	if want := "<code>" + relVersion + "</code>"; !strings.Contains(got, want) {
		t.Errorf("версия должна была остаться текстом:\nожидали %s\nполучили %s", want, got)
	}
	if strings.Contains(got, "/commit/") {
		t.Errorf("ссылка на коммит появилась из ниоткуда:\n%s", got)
	}
	// Ссылка на сам компонент — другая ссылка, и она обязана остаться.
	if !strings.Contains(got, `<a href="https://status.samoy.love/">`) {
		t.Errorf("пропала ссылка на компонент:\n%s", got)
	}
}

func TestЧужойАдресКоммитаНеСтановитсяСсылкой(t *testing.T) {
	// СПИСОК РАЗРЕШЁННОГО, А НЕ ЗАПРЕЩЁННОГО, и бот проверяет САМ: агент уже
	// проверил, но полагаться на это — значит полагаться на предположение.
	cases := map[string]string{
		"чужая схема":         "javascript:alert(1)",
		"data":                "data:text/html,<script>alert(1)</script>",
		"без https":           "http://github.com/tr0llex/repo/commit/abc1234",
		"чужой хост":          "https://evil.example/tr0llex/repo/commit/abc1234",
		"хост в поддомене":    "https://github.com.evil.example/o/r/commit/abc1234",
		"хост в пути":         "https://evil.example/https://github.com/o/r/commit/abc1234",
		"учётка перед хостом": "https://github.com@evil.example/o/r/commit/abc1234",
		"кавычка в адресе":    `https://github.com/o/r"/commit/abc1234`,
		"выход из атрибута":   `https://github.com/o/r/commit/abc1234"onmouseover="alert(1)`,
		"второй тег":          `https://github.com/o/r/commit/abc1234"><script>alert(1)</script>`,
		"пробел в адресе":     "https://github.com/o/r/commit/abc1234 x",
		"не коммит":           "https://github.com/tr0llex/repo/pull/26",
		"sha не sha":          "https://github.com/tr0llex/repo/commit/local",
		"sha короткий":        "https://github.com/tr0llex/repo/commit/abc12",
		"sha длинный":         "https://github.com/tr0llex/repo/commit/" + strings.Repeat("a", 41),
		"выход вверх":         "https://github.com/../../commit/abc1234",
		"хвост после sha":     "https://github.com/o/r/commit/abc1234/../../evil",
		"перевод строки":      "https://github.com/o/r/commit/abc1234\nhttps://evil.example",
	}

	for name, bad := range cases {
		t.Run(name, func(t *testing.T) {
			got := formatEvent(releaseEvent(bad))

			// Ровно одна ссылка в сообщении — на сам компонент. Вторая
			// означала бы, что чужой адрес доехал до разметки.
			if n := strings.Count(got, "<a href="); n != 1 {
				t.Errorf("ожидали одну ссылку (на компонент), нашли %d:\n%s", n, got)
			}
			if strings.Contains(got, "/commit/") {
				t.Errorf("непроверенный адрес попал в сообщение:\n%s", got)
			}
			if want := "<code>" + relVersion + "</code>"; !strings.Contains(got, want) {
				t.Errorf("версия обязана остаться текстом:\n%s", got)
			}
		})
	}
}

func TestВерсияСРазметкойЭкранируетсяДажеСоСсылкой(t *testing.T) {
	// Версия — это имя каталога релиза на чужой машине, и читать её как
	// доверенную нечего. Со ссылкой она становится ПОДПИСЬЮ якоря, то есть
	// местом, где непроэкранированный «<» открыл бы тег.
	got := formatEvent(Event{
		Kind: KindRelease, Title: "Статус · Страница",
		URL:       "https://status.samoy.love/",
		Version:   `release-<script>alert(1)</script>`,
		CommitURL: relCommit,
		At:        time.Date(2026, 8, 3, 10, 15, 0, 0, time.UTC),
	})

	if strings.Contains(got, "<script>") {
		t.Errorf("разметка из версии доехала до сообщения:\n%s", got)
	}
	if !strings.Contains(got, "&lt;script&gt;") {
		t.Errorf("версия должна была уехать экранированной:\n%s", got)
	}
}

func TestАдресКоммитаДоезжаетИзЖурналаВСобытие(t *testing.T) {
	// Проверяем стык, а не только рисование: поле обязано пройти путь
	// файл события → DeployEvent, иначе ссылка есть в журнале и нет в чате.
	//
	// Раньше этот стык проверялся на пути summary.json → Build → Event: релиз
	// выводился из разницы снимков версий. Тот путь удалён вместе с самой
	// механикой — она и теряла выкатки, случившиеся между двумя наблюдениями.
	// Проверка осталась, потому что осталось требование: связь «коммит →
	// выкатка» в сообщении выражена ровно этой ссылкой, и её потеря молчалива.
	dir := t.TempDir()
	const ms = 1785924102123

	e := inboxEvent(ms, "status-site", evSuccess)
	e["commitURL"] = relCommit
	name := inboxWrite(t, dir, ms, "status-site", evSuccess, e)

	st := inboxState()
	events := newInbox(dir).Poll(st, inboxNow(t))

	if len(events) != 1 {
		t.Fatalf("событие из журнала не прочитано: %d", len(events))
	}
	if events[0].File != name {
		t.Errorf("прочитано не то событие: %q", events[0].File)
	}
	if events[0].CommitURL != relCommit {
		t.Errorf("адрес коммита не доехал до события: %q", events[0].CommitURL)
	}
}

func TestСоставСообщенияОРелизеЧитаемЦеликом(t *testing.T) {
	// Одна сборка сообщения целиком — чтобы правка формата была видна как
	// правка, а не как перестановка проверок на подстроки.
	e := releaseEvent(relCommit)
	e.Changelog = []string{"Починить обрыв скачивания больших файлов"}

	want := "🚀 <b><a href=\"https://status.samoy.love/\">Статус · Страница</a></b> обновлён\n" +
		"<a href=\"" + relCommit + "\">" + relVersion + "</a>\n" +
		"была <code>release-20260802-090000-def5678</code>\n" +
		"<i>собрано " + fmtTime(e.At) + "</i>" +
		// Хвост целиком: список изменений и ссылка на диапазон, по которому его
		// можно проверить. Обе половины ссылки у события есть и обе проверены.
		changelogTail(e.Changelog, repoFromCommitURL(e.CommitURL), e.Previous, e.CommitURL)

	if got := formatEvent(e); got != want {
		t.Errorf("сообщение о релизе изменилось:\nожидали %q\nполучили %q", want, got)
	}
}
