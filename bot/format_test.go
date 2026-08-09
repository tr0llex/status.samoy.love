package main

import (
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestHumanDur(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{30 * time.Second, "меньше минуты"},
		{time.Minute, "1 мин"},
		{59 * time.Minute, "59 мин"},
		{time.Hour, "1 ч"},
		{90 * time.Minute, "1 ч 30 мин"},
		{25 * time.Hour, "1 д 1 ч"},
		{48 * time.Hour, "2 д"},
		// Часы сервера иногда уходят назад (ntp), и разница становится
		// отрицательной. Показывать «-5 мин назад» нельзя.
		{-5 * time.Minute, "5 мин"},
	}
	for _, c := range cases {
		if got := humanDur(c.d); got != c.want {
			t.Errorf("humanDur(%s) = %q, ожидали %q", c.d, got, c.want)
		}
	}
}

func TestFmtTimeMoscow(t *testing.T) {
	// Агент пишет UTC, человеку нужно московское время.
	got := fmtTime(time.Date(2026, 8, 2, 9, 5, 0, 0, time.UTC))
	if got != "02.08 12:05 MSK" {
		t.Errorf("fmtTime = %q, ожидали 02.08 12:05 MSK", got)
	}
}

func TestFormatHelpListsCommands(t *testing.T) {
	help := formatHelp()
	for _, cmd := range []string{"/status", "/changelog", "/incidents", "/quiet", "/help"} {
		if !strings.Contains(help, cmd) {
			t.Errorf("в справке нет %s", cmd)
		}
	}
}

func TestFormatStatus(t *testing.T) {
	uptime := 99.98
	now := base
	s := &Summary{
		Updated: now.Add(-time.Minute).Format(time.RFC3339),
		Overall: "degraded",
		Projects: []Project{{
			Title: "Змейки", Status: "degraded", Up: 1, Total: 2,
			Checks: []Check{
				{
					Name: "Клиент", Status: "up", Critical: true, Ms: 120,
					Since:  now.Add(-2 * time.Hour).Format(time.RFC3339),
					Uptime: map[string]*float64{"d1": &uptime},
				},
				{
					Name: "Игровой сервер", Status: "down", Critical: true, Error: "HTTP 502",
					Impact: "Матчи не идут",
					Since:  now.Add(-10 * time.Minute).Format(time.RFC3339),
				},
			},
			Units: []Unit{{
				Title: "Игровой сервер", Active: false, State: "failed",
			}},
		}},
	}

	got := formatStatus(s, now, false, time.Time{})

	// Сломанное разворачивается целиком: что это, для кого плохо, почему и
	// сколько уже длится.
	for _, want := range []string{
		"Частичный сбой", "1/2",
		"Игровой сервер", "Матчи не идут", "HTTP 502", "10 мин",
		"failed", "Данные агента",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("в ответе нет %q:\n%s", want, got)
		}
	}

	// А исправное — сворачивается. Раньше бот печатал время ответа и проценты
	// у каждой зелёной проверки, и ответ на /status превращался в простыню,
	// в которой единственную красную строку приходилось искать глазами.
	for _, unwanted := range []string{"120 мс", "99.98%"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("подробности исправной проверки не нужны, но %q есть:\n%s", unwanted, got)
		}
	}
}

func TestFormatStatusShowsMuteState(t *testing.T) {
	// Раньше тишину нельзя было увидеть на самом экране статуса: узнать, что
	// бот молчит, можно было только вспомнив, что сам её включил.
	now := base
	s := &Summary{Updated: now.Format(time.RFC3339), Overall: "operational"}

	quiet := formatStatus(s, now, true, now.Add(2*time.Hour))
	if !strings.Contains(quiet, "молчу до") {
		t.Errorf("тишина включена, но на экране статуса это не видно:\n%s", quiet)
	}

	loud := formatStatus(s, now, false, time.Time{})
	if strings.Contains(loud, "молчу до") {
		t.Errorf("тишина выключена, но экран статуса говорит обратное:\n%s", loud)
	}
}

func TestFormatStatusWarnsAboutStaleData(t *testing.T) {
	now := base
	s := &Summary{
		// Агент молчит полчаса, но все проверки в файле зелёные: без
		// предупреждения ответ выглядел бы как «всё хорошо».
		Updated: now.Add(-30 * time.Minute).Format(time.RFC3339),
		Overall: "operational",
	}
	got := formatStatus(s, now, false, time.Time{})
	if !strings.Contains(got, "Данные устарели") {
		t.Errorf("нет предупреждения о несвежих данных:\n%s", got)
	}
}

func TestFormatEscapesHTML(t *testing.T) {
	now := base
	s := &Summary{
		Updated: now.Format(time.RFC3339),
		Overall: "down",
		Projects: []Project{{
			Title: "<b>взлом</b>", Status: "down", Up: 0, Total: 1,
			Checks: []Check{{Name: "A & B", Status: "down", Error: "<script>"}},
		}},
	}
	got := formatStatus(s, now, false, time.Time{})
	if strings.Contains(got, "<script>") || strings.Contains(got, "<b>взлом</b>") {
		t.Errorf("разметка из данных попала в сообщение как есть:\n%s", got)
	}
	if !strings.Contains(got, "A &amp; B") {
		t.Errorf("амперсанд не экранирован:\n%s", got)
	}
}

func TestFormatIncidents(t *testing.T) {
	now := base
	empty := formatIncidents(&Summary{}, now)
	if !strings.Contains(empty, "Инцидентов не было") {
		t.Errorf("пустая история описана неверно: %s", empty)
	}

	s := &Summary{Incidents: []Incident{
		{
			Name: "Snakes · Клиент", Start: now.Add(-20 * time.Minute).Format(time.RFC3339),
			Reason: "HTTP 502",
		},
		{
			Name: "samoy.love · Сайт", Start: now.Add(-25 * time.Hour).Format(time.RFC3339),
			End: now.Add(-24 * time.Hour).Format(time.RFC3339), DurationMs: 3600_000,
			Reason: "таймаут 12s",
		},
	}}
	got := formatIncidents(s, now)
	for _, want := range []string{"Snakes · Клиент", "идёт уже 20 мин", "HTTP 502", "длился 1 ч"} {
		if !strings.Contains(got, want) {
			t.Errorf("в ответе нет %q:\n%s", want, got)
		}
	}
}

func TestFormatEvent(t *testing.T) {
	cases := []struct {
		name  string
		event Event
		want  []string
	}{
		{
			"падение",
			Event{Kind: KindDown, Title: "Snakes · Клиент", Reason: "HTTP 502", At: base},
			[]string{"Snakes · Клиент", "недоступен", "HTTP 502"},
		},
		{
			"напоминание",
			Event{Kind: KindStillDown, Title: "Snakes · Клиент", Duration: 45 * time.Minute},
			[]string{"лежит уже", "45 мин"},
		},
		{
			"восстановление",
			Event{Kind: KindUp, Title: "Snakes · Клиент", Duration: time.Hour, At: base},
			[]string{"снова работает", "простой: 1 ч"},
		},
		{
			"релиз",
			Event{
				Kind: KindRelease, Title: "Snakes · Сервер", Version: "v2",
				Previous: "v1", At: base,
			},
			[]string{"обновлён", "v2", "была", "v1", "собрано"},
		},
		{
			"релиз с ручным переключением",
			Event{
				Kind: KindRelease, Title: "ChillHub · Установщик", Version: "1.3.5",
				At: base, AdminURL: "https://launcher.samoy.love/admin/ui/admin.html#launcher",
			},
			[]string{"обновлён", "переключить", "https://launcher.samoy.love/admin/ui/admin.html#launcher"},
		},
		{
			"релиз без ручного переключения не показывает ссылку",
			Event{
				Kind: KindRelease, Title: "ChillHub · Сайт", Version: "v3", At: base,
			},
			[]string{"обновлён"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := formatEvent(c.event)
			for _, want := range c.want {
				if !strings.Contains(got, want) {
					t.Errorf("нет %q в сообщении:\n%s", want, got)
				}
			}
		})
	}
}

func TestПолоскаБерётХудшуюИзКритичныхПроверок(t *testing.T) {
	// Сайт открывался весь день, игровой сервер лежал. День плохой: брать
	// первую попавшуюся проверку значит показать благополучие, которого не было.
	full := func(up, total int64) []*Day {
		d := make([]*Day, stripDays)
		for i := range d {
			d[i] = &Day{D: "x", Up: up, Total: total}
		}
		return d
	}
	p := Project{Checks: []Check{
		{Name: "Клиент", Critical: true, Days: full(1440, 1440)},
		{Name: "Игровой сервер", Critical: true, Days: full(0, 1440)},
	}}
	if got := projectStrip(p); strings.Contains(got, "🟩") {
		t.Errorf("день с лежащим сервером не может быть зелёным: %s", got)
	}

	// Второстепенная проверка полоску портить не должна: вердикт она не роняет.
	p2 := Project{Checks: []Check{
		{Name: "Сайт", Critical: true, Days: full(1440, 1440)},
		{Name: "Админка", Critical: false, Days: full(0, 1440)},
	}}
	if got := projectStrip(p2); strings.Contains(got, "🟥") {
		t.Errorf("второстепенная проверка не должна красить полоску: %s", got)
	}
}

func TestСсылкиВедутНаСервисы(t *testing.T) {
	// Уведомление без ссылки заставляет искать адрес руками ровно тогда,
	// когда некогда: увидел «недоступен» — хочешь открыть и посмотреть.
	got := formatEvent(Event{
		Kind: KindDown, Title: "Snakes · Игровой сервер",
		URL: "https://snakes.samoy.love/healthz", Reason: "HTTP 502",
	})
	if !strings.Contains(got, `<a href="https://snakes.samoy.love/healthz">`) {
		t.Errorf("в уведомлении о падении нет ссылки: %s", got)
	}

	rel := formatEvent(Event{
		Kind: KindRelease, Title: "ChillHub · Публичный API",
		URL: "https://launcher.samoy.love/", Version: "1.2.3",
	})
	if !strings.Contains(rel, `<a href="https://launcher.samoy.love/">`) {
		t.Errorf("в сообщении о релизе нет ссылки на компонент: %s", rel)
	}

	// Мусор вместо адреса не должен уезжать в разметку.
	bad := formatEvent(Event{Kind: KindDown, Title: "X", URL: "javascript:alert(1)"})
	if strings.Contains(bad, "<a href") {
		t.Errorf("недопустимая схема попала в ссылку: %s", bad)
	}
}

func TestАптаймНеУмножаетсяДважды(t *testing.T) {
	// Агент отдаёт проценты (agent/main.go, pct). Лишнее умножение на сто
	// давало «9991.00% за 90 дней» — число, которое читается как поломка
	// бота, а не как доступность.
	v := 99.91
	full := 100.0
	cases := []struct {
		name   string
		uptime map[string]*float64
		want   string
	}{
		{"90 дней", map[string]*float64{"d90": &v}, "99.91% за 90 дней"},
		{"ровно сто", map[string]*float64{"d90": &full}, "100% за 90 дней"},
		{"только неделя", map[string]*float64{"d7": &v}, "99.91% за неделю"},
		{"нет данных", map[string]*float64{"d90": nil}, ""},
		{"пусто", nil, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := uptimeText(Check{Uptime: c.uptime}); got != c.want {
				t.Errorf("получили %q, ожидали %q", got, c.want)
			}
		})
	}
}

// --------------------------------------------------------- список изменений

func TestРелизБезИзмененийОстаётсяПрежним(t *testing.T) {
	// Главное требование: цель, чья выкатка ничего не публикует, обязана
	// давать ровно то же сообщение, что и до появления блока изменений.
	// Иначе украшение превращается в изменение всех уведомлений сразу.
	e := Event{
		Kind: KindRelease, Title: "Snakes · Сервер", URL: "https://snakes.samoy.love/",
		Version: "v2", Previous: "v1", At: base,
	}
	want := formatEvent(e)

	e.Changelog = []string{}
	if got := formatEvent(e); got != want {
		t.Errorf("пустой список изменил сообщение:\n%s", got)
	}
	e.Changelog = []string{"", "   ", "\n"}
	if got := formatEvent(e); got != want {
		t.Errorf("список из пустых строк изменил сообщение:\n%s", got)
	}
	if strings.Contains(want, "Изменения") {
		t.Errorf("блок изменений появился без данных:\n%s", want)
	}
}

func TestСписокИзмененийИдётПоследним(t *testing.T) {
	got := formatEvent(Event{
		Kind: KindRelease, Title: "Snakes · Сервер", URL: "https://snakes.samoy.love/",
		Version: "v2", Previous: "v1", At: base,
		Changelog: []string{
			"исправить падение на пустом конфиге",
			"обновить nginx до 1.24",
		},
	})

	// Всё, ради чего уведомление читают, осталось на месте.
	for _, want := range []string{"обновлён", "<code>v2</code>", "была <code>v1</code>", "собрано"} {
		if !strings.Contains(got, want) {
			t.Errorf("из сообщения о релизе пропало %q:\n%s", want, got)
		}
	}
	// Формат блока — тот же, что у deploy-kit/bin/changelog.
	if !strings.Contains(got, "<b>Изменения</b>\n• исправить падение на пустом конфиге\n• обновить nginx до 1.24") {
		t.Errorf("блок изменений собран не по общему формату:\n%s", got)
	}
	// Именно последним: он самый длинный и единственный необязательный.
	if !strings.HasSuffix(got, "• обновить nginx до 1.24") {
		t.Errorf("блок изменений не в конце сообщения:\n%s", got)
	}
	if strings.Index(got, "Изменения") < strings.Index(got, "собрано") {
		t.Errorf("блок изменений оказался выше даты сборки:\n%s", got)
	}
}

func TestИзмененияЭкранируются(t *testing.T) {
	// Тема коммита — чужой текст, а сообщение уходит с parse_mode=HTML: один
	// «<» делает разметку невалидной, Telegram отвечает ошибкой, и владелец
	// не получает уведомления о релизе вообще.
	got := formatChangelog([]string{
		"поднять go до 1.22 <-- важно",
		"<b>жирный</b> & <i>курсив</i>",
		`закрыть <a href="javascript:alert(1)">дыру</a>`,
	}, "")
	if strings.Contains(got, "<-- важно") || strings.Contains(got, "<b>жирный") ||
		strings.Contains(got, "<a href") || strings.Contains(got, "<i>") {
		t.Errorf("чужая разметка уехала в сообщение как есть:\n%s", got)
	}
	for _, want := range []string{"&lt;-- важно", "&lt;b&gt;жирный&lt;/b&gt; &amp; ", "&lt;a href="} {
		if !strings.Contains(got, want) {
			t.Errorf("нет экранированного %q:\n%s", want, got)
		}
	}
	// Заголовок блока рисуем мы сами, и он остаётся разметкой.
	if !strings.HasPrefix(got, "<b>Изменения</b>\n") {
		t.Errorf("заголовок блока потерялся:\n%s", got)
	}
}

func TestВраньёВСпискеУпираетсяВПотолок(t *testing.T) {
	// Лимит Telegram снят не сокращением списка, а разбиением на сообщения, но
	// потолок против чужого файла остался: тысяча строк по 540 символов — это
	// уже не релиз, а враньё в version.json. Потолок обязан быть виден.
	var huge []string
	for i := 0; i < 1000; i++ {
		huge = append(huge, strings.Repeat("очень длинная тема коммита ", 20))
	}
	msg := formatEvent(Event{
		Kind: KindRelease, Title: "Snakes · Сервер", URL: "https://snakes.samoy.love/",
		Version: "v2", Previous: "v1", At: base, Changelog: huge,
	})
	if !utf8.ValidString(msg) {
		t.Error("обрезка разрезала символ UTF-8: такое сообщение Telegram отвергнет целиком")
	}
	if n := utf8.RuneCountInString(msg); n > changelogBudget+changelogReserve+400 {
		t.Errorf("сообщение на %d символов — потолок %d не сработал", n, changelogBudget)
	}
	// Молча оборванный список читается как «больше ничего и не было».
	if !strings.Contains(msg, "список не поместился") {
		t.Errorf("потолок сработал молча:\n%s", msg[:200])
	}
	// И всё это обязано разложиться по сообщениям, каждое из которых Telegram
	// примет: превышение он не режет, а отвергает целиком.
	for i, p := range splitMessage(msg, telegramTextLimit) {
		if n := utf16Len(p); n > telegramTextLimit {
			t.Errorf("часть %d: %d единиц UTF-16 — Telegram её не примет", i+1, n)
		}
	}
}

func TestХвостГенератораНеСтановитсяПунктом(t *testing.T) {
	// deploy-kit/bin/changelog заканчивает блок строкой «…и ещё 12 коммитов».
	// Она приезжает сюда такой же строкой, как и темы коммитов, но пунктом
	// списка не является.
	got := formatChangelog([]string{"обновить nginx до 1.24", "…и ещё 12 коммитов"}, "")
	if strings.Contains(got, "• …и ещё") {
		t.Errorf("хвост получил маркер пункта:\n%s", got)
	}
	if !strings.HasSuffix(got, "\n…и ещё 12 коммитов") {
		t.Errorf("хвост не сохранён:\n%s", got)
	}
	// Одного хвоста без пунктов мало: блок «Изменения» ни о чём не сообщает.
	if got := formatChangelog([]string{"…и ещё 12 коммитов"}, ""); got != "" {
		t.Errorf("блок собрался из одного хвоста: %q", got)
	}
}

func TestБотРазбираетВыводГенератораСам(t *testing.T) {
	// Путь «summary.json собран мимо агента» описан в summary.go как рабочий.
	// На нём сюда приезжает не разобранный агентом список, а ровно то, что
	// напечатал deploy-kit/bin/changelog: с заголовком «Изменения», с маркерами
	// и с уже экранированными & < >.
	//
	// Пока разбор был односторонним (заголовок и маркеры снимал только агент),
	// на этом пути заголовок доезжал до владельца ПУНКТОМ СПИСКА — под
	// настоящим заголовком, который бот рисует сам, и экранированным во второй
	// раз.
	got := formatChangelog([]string{
		"<b>Изменения</b>",
		"• ускорить расписание",
		"• поднять go до 1.22 &lt;-- важно",
		"- перевести карту на новый тайлсет",
		"…и ещё 3 коммита",
	}, "")

	if strings.Count(got, "Изменения") != 1 {
		t.Errorf("заголовок задвоился:\n%s", got)
	}
	if strings.Contains(got, "&lt;b&gt;Изменения") {
		t.Errorf("заголовок генератора стал пунктом списка:\n%s", got)
	}
	// Маркер ставит бот, и чужой маркер дал бы «• • ускорить расписание».
	for _, bad := range []string{"• •", "• -", "• *"} {
		if strings.Contains(got, bad) {
			t.Errorf("маркер задвоился (%q):\n%s", bad, got)
		}
	}
	// Экранирование одно, а не два: иначе «go 1.22 <-- важно» доезжает до
	// владельца как «go 1.22 &amp;lt;-- важно».
	if !strings.Contains(got, "go до 1.22 &lt;-- важно") || strings.Contains(got, "&amp;lt;") {
		t.Errorf("экранирование наложилось дважды:\n%s", got)
	}
	// Всё остальное осталось на своих местах.
	if !strings.HasPrefix(got, "<b>Изменения</b>\n• ускорить расписание") {
		t.Errorf("список собран не по общему формату:\n%s", got)
	}
	if !strings.HasSuffix(got, "\n…и ещё 3 коммита") {
		t.Errorf("хвост генератора потерялся:\n%s", got)
	}
	if strings.Count(got, "\n• ") != 3 {
		t.Errorf("пунктов должно быть три:\n%s", got)
	}

	// Один заголовок без пунктов — не блок: сообщение о релизе обязано
	// остаться прежним, а не получить пустую шапку.
	if g := formatChangelog([]string{"<b>Изменения</b>", "Изменения", "•", "-"}, ""); g != "" {
		t.Errorf("блок собрался из одной разметки: %q", g)
	}
}

func TestМногострочнаяТемаНеРвётСписок(t *testing.T) {
	// Тема приходит многострочной, если в сообщении коммита нет пустой
	// строки после первой. В пункт списка это не годится.
	got := formatChangelog([]string{"исправить падение\nна пустом конфиге\tи не только"}, "")
	if strings.Count(got, "\n") != 1 {
		t.Errorf("пункт разорвал список:\n%s", got)
	}
	if !strings.HasSuffix(got, "• исправить падение на пустом конфиге и не только") {
		t.Errorf("строки не склеены:\n%s", got)
	}
}

func TestОбрезкаНеРежетСимволы(t *testing.T) {
	// Битую строку UTF-8 Telegram отвергает целиком, то есть сообщение
	// молча не приходит. Проверяем обе обрезки: и ту, что считает символы для
	// читателя, и ту, что считает байты для callback_data.
	const s = "обновить nginx до версии 1.24 и перечитать конфиг"
	for n := 1; n < 80; n++ {
		got := cutRunes(s, n)
		if !utf8.ValidString(got) {
			t.Fatalf("cutRunes(%d) разрезал символ: %q", n, got)
		}
		if c := utf8.RuneCountInString(got); c > n {
			t.Fatalf("cutRunes(%d) вернул %d символов: %q", n, c, got)
		}

		got = cutBytes(s, n)
		if !utf8.ValidString(got) {
			t.Fatalf("cutBytes(%d) разрезал символ: %q", n, got)
		}
		if len(got) > n {
			t.Fatalf("cutBytes(%d) вернул %d байт: %q", n, len(got), got)
		}
	}
	// Короткая строка не трогается вовсе.
	if got := cutRunes("коротко", 100); got != "коротко" {
		t.Errorf("cutRunes обрезал то, что помещалось: %q", got)
	}
	if got := cutBytes("коротко", 100); got != "коротко" {
		t.Errorf("cutBytes обрезал то, что помещалось: %q", got)
	}
	// Ровно по пределу — тоже «помещается»: иначе тема максимальной
	// разрешённой длины получала бы многоточие ни за что.
	edge := strings.Repeat("я", changelogWidth)
	if got := cutRunes(edge, changelogWidth); got != edge {
		t.Errorf("строка ровно по пределу обрезана: %q", got)
	}
}

// changelogSubject120 — тема ровно по владельческому потолку: 120 СИМВОЛОВ,
// 240 байт. Кириллица здесь не для экзотики, а потому, что именно на ней всё и
// ломалось: прежний предел в 100 БАЙТ обрывал такую тему на сорок девятом
// символе.
func changelogSubject120(t *testing.T) string {
	t.Helper()
	s := strings.Repeat("тема ", 23) + "конец"
	if n := utf8.RuneCountInString(s); n != changelogWidth {
		t.Fatalf("подготовка теста: тема на %d символов, нужна ровно %d", n, changelogWidth)
	}
	return s
}

func TestТемаВ120СимволовДоезжаетЦеликом(t *testing.T) {
	// Потолок темы задан владельцем в СИМВОЛАХ (CLAUDE.md), и ровно столько же
	// режет генератор. Бот — последний в цепочке: стоит ему оказаться строже
	// генератора, и одну тему режут дважды на разной длине, причём второй рез
	// приходится уже на многоточие первого.
	subject := changelogSubject120(t)

	// Вход — ровно то, что печатает deploy-kit/bin/changelog: заголовок,
	// маркер, экранированный текст. Этот путь описан в summary.go как рабочий.
	got := formatChangelog([]string{"<b>Изменения</b>", "• " + subject}, "")
	if !strings.Contains(got, "• "+subject) {
		t.Errorf("тема в 120 символов не доехала целиком:\n%s", got)
	}
	if strings.Contains(got, "…") {
		t.Errorf("тема ровно по потолку получила многоточие:\n%s", got)
	}

	// Тот же список на экране «/changelog имя» — слово в слово. Экран и
	// сообщение рассказывают про одну и ту же выкатку, и расходиться им нельзя.
	screen := itemLines(release("v1", base, subject), "")
	if !strings.Contains(screen, "• "+subject) {
		t.Errorf("экран разошёлся с сообщением на теме предельной длины:\n%s", screen)
	}
}

// changelogOf40 — релиз из сорока тем предельной длины.
//
// Число не выдумано: сорок один коммит — самый крупный релиз хозяйства за год,
// и именно на нём становится видно, что 4096 единиц Telegram недостаточно.
// Темы разные, но длина у каждой ровно предельная: одинаковые пункты позволили
// бы проверке пройти, ничего не проверив.
func changelogOf40(t *testing.T) []string {
	t.Helper()
	subject := []rune(changelogSubject120(t))
	lines := []string{"<b>Изменения</b>"}
	for i := 0; i < 40; i++ {
		item := fmt.Sprintf("%02d ", i) + string(subject[3:])
		if n := utf8.RuneCountInString(item); n != changelogWidth {
			t.Fatalf("подготовка теста: пункт на %d символов, нужно %d", n, changelogWidth)
		}
		lines = append(lines, "• "+item)
	}
	return lines
}

func TestРелизИзСорокаКоммитовНеТеряетНиОдного(t *testing.T) {
	// ГЛАВНАЯ ПРОВЕРКА ЗАДАЧИ. Владелец сказал: не обрезай список выкаченных
	// коммитов. Сорок тем по 120 символов — это около 4800 единиц UTF-16, то
	// есть больше одного сообщения Telegram. Ответ на это — не показать
	// меньше, а разложить по нескольким сообщениям подряд.
	lines := changelogOf40(t)
	msg := formatEvent(Event{
		Kind: KindRelease, Title: "Snakes · Сервер", URL: "https://snakes.samoy.love/",
		Version: "v2", Previous: "v1", At: base, Changelog: lines,
	})
	if n := strings.Count(msg, "\n• "); n != 40 {
		t.Fatalf("в сообщение попало %d пунктов из 40:\n%s", n, msg)
	}
	if strings.Contains(msg, "…и ещё") {
		t.Errorf("список свёрнут в «…и ещё N» — ровно то, от чего уходили:\n%s", msg)
	}

	parts := splitMessage(msg, telegramTextLimit)
	if len(parts) < 2 {
		t.Fatalf("сорок тем предельной длины (%d единиц) обязаны занять больше одного сообщения",
			utf16Len(msg))
	}
	for i, p := range parts {
		if n := utf16Len(p); n > telegramTextLimit {
			t.Errorf("часть %d: %d единиц UTF-16 — Telegram её не примет", i+1, n)
		}
		if !utf8.ValidString(p) {
			t.Errorf("часть %d разрезала символ UTF-8", i+1)
		}
		// Разметка не должна оказаться разрезанной пополам: негодный HTML —
		// это отказ Telegram, то есть молчание вместо уведомления.
		if strings.Count(p, "<b>") != strings.Count(p, "</b>") {
			t.Errorf("часть %d разрезана посреди разметки:\n%s", i+1, p)
		}
		// Продолжение обязано узнаваться: иначе вторая половина списка
		// читается как отдельное сообщение о другой выкатке.
		if i > 0 && !strings.HasPrefix(p, "<i>продолжение (") {
			t.Errorf("часть %d не помечена продолжением:\n%s", i+1, p)
		}
	}

	// НИЧЕГО НЕ ПОТЕРЯНО И ПОРЯДОК СОХРАНЁН — ради этого всё и затевалось.
	joined := strings.Join(parts, "\n")
	prev := -1
	for i := 0; i < 40; i++ {
		want := fmt.Sprintf("\n• %02d ", i)
		at := strings.Index(joined, want)
		if at < 0 {
			t.Fatalf("пункт %02d не доехал ни до одной части", i)
		}
		if at < prev {
			t.Fatalf("пункт %02d уехал вперёд предыдущего: порядок нарушен", i)
		}
		prev = at
	}
}

func TestПолныйСписокГенератораНеОбрезаетсяБюджетом(t *testing.T) {
	// Восемь тем по 120 символов кириллицы — это около 2100 БАЙТ, и прежний
	// бюджет в 1400 байт обрывал такой блок на третьем пункте. Генератор
	// прислал восемь — читатель обязан увидеть восемь.
	subject := []rune(changelogSubject120(t))
	lines := []string{"<b>Изменения</b>"}
	for i := 0; i < 8; i++ {
		// Темы разные, но длина у каждой ровно предельная: одинаковые бот
		// пропустил бы, а проверка вышла бы ни о чём.
		item := fmt.Sprintf("%d ", i) + string(subject[2:])
		if n := utf8.RuneCountInString(item); n != changelogWidth {
			t.Fatalf("подготовка теста: пункт на %d символов, нужно %d", n, changelogWidth)
		}
		lines = append(lines, "• "+item)
	}

	got := formatChangelog(lines, "")
	if n := strings.Count(got, "\n• "); n != 8 {
		t.Fatalf("пунктов %d, ожидали 8:\n%s", n, got)
	}
	if strings.Contains(got, "…") {
		t.Errorf("что-то обрезано или объявлено непоместившимся:\n%s", got)
	}

	// Восьмикоммитный релиз — обычная выкатка, и она обязана по-прежнему
	// приходить ОДНИМ сообщением: разбиение заводится ради длинных, а не ради
	// каждого.
	msg := formatEvent(Event{
		Kind: KindRelease, Title: "Snakes · Сервер", URL: "https://snakes.samoy.love/",
		Version: "v2", Previous: "v1", At: base, Changelog: lines,
	})
	if n := utf16Len(msg); n > 4096 {
		t.Errorf("сообщение на %d единиц UTF-16 — Telegram его не примет", n)
	}
	if n := len(splitMessage(msg, telegramTextLimit)); n != 1 {
		t.Errorf("обычная выкатка разъехалась на %d сообщений", n)
	}
}

func TestВраньёВПолеЗажимаетсяПоСимволам(t *testing.T) {
	// Генератор мог не запускаться вовсе: version.json собирает выкатка, а её
	// пишут руками. Пределы бота — защита от чужого файла, и на 500 символах
	// в одном пункте они обязаны сработать.
	hostile := strings.Repeat("я", 500)

	got := formatChangelog([]string{hostile}, "")
	line, ok := strings.CutPrefix(got, "<b>Изменения</b>\n• ")
	if !ok {
		t.Fatalf("блок собран не по формату:\n%s", got)
	}
	if n := utf8.RuneCountInString(line); n > changelogWidth {
		t.Errorf("пункт на %d символов, предел %d", n, changelogWidth)
	}
	if !strings.HasSuffix(line, "…") {
		t.Errorf("обрезка не отмечена многоточием: %q", line)
	}
	if !utf8.ValidString(got) {
		t.Error("обрезка разрезала символ UTF-8: такое сообщение Telegram отвергнет целиком")
	}

	// Та же защита на экране цели: он читает тот же чужой файл.
	screen := itemLines(release("v1", base, hostile), "")
	for _, l := range strings.Split(strings.TrimSuffix(screen, "\n"), "\n") {
		if n := utf8.RuneCountInString(strings.TrimPrefix(l, "• ")); n > changelogWidth {
			t.Errorf("на экране пункт на %d символов, предел %d", n, changelogWidth)
		}
	}
}

func TestСклонениеКоммитов(t *testing.T) {
	// «…и ещё 2 коммитов» читается как поломка формата, а не как список.
	cases := map[int]string{
		1: "коммит", 2: "коммита", 4: "коммита", 5: "коммитов",
		11: "коммитов", 12: "коммитов", 14: "коммитов", 21: "коммит",
		22: "коммита", 25: "коммитов", 111: "коммитов", 101: "коммит",
	}
	for n, want := range cases {
		if got := pluralCommits(n); got != want {
			t.Errorf("pluralCommits(%d) = %q, ожидали %q", n, got, want)
		}
	}
}

// ------------------------------------------------------------ события выкатки
//
// Новые виды сообщений и весь список ИБ-правок. Проверки написаны от
// враждебного входа: событие приезжает файлом с диска, и подделать его может
// всякий, кто в этот каталог пишет.

func deployOf(kind string) Deploy {
	return Deploy{
		Kind: kind, App: "snakes", Title: "Snakes · Сервер",
		URL: "https://snakes.samoy.love/", Project: "snakes",
		Version: "release-20260805-101502-1a2b3c4",
		RunURL:  "https://github.com/tr0llex/snakes/actions/runs/16542330981",
		At:      base,
	}
}

func TestПровалВыкаткиНазываетСтадиюИПрогон(t *testing.T) {
	// Сегодня провал не виден никак: release.sh не запускался, версия прежняя,
	// сравнивать снимкам нечего. Сообщение обязано ответить на три вопроса —
	// что не выкачено, где остановилось и куда идти смотреть.
	d := deployOf(deployFailure)
	d.Stage = "units"
	got := formatDeploy(d)

	for _, want := range []string{
		"Snakes · Сервер", "не выкачен", "службы systemd",
		`<a href="https://github.com/tr0llex/snakes/actions/runs/16542330981">прогон</a>`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("в сообщении о провале нет %q:\n%s", want, got)
		}
	}
	// Провал — не релиз: «обновлён» в нём быть не может.
	if strings.Contains(got, "обновлён") {
		t.Errorf("провал выдан за релиз:\n%s", got)
	}
}

func TestАвтооткатНазываетПричину(t *testing.T) {
	// Автооткат (release.sh:203-265) сегодня невидим совсем: версия вернулась
	// на место, разницы между снимками нет, сообщения нет.
	d := deployOf(deployRolledBack)
	d.Stage, d.Reason = "health", "health_failed"
	got := formatDeploy(d)

	for _, want := range []string{"откачен автоматически", "healthcheck не дождался ответа"} {
		if !strings.Contains(got, want) {
			t.Errorf("в сообщении об автооткате нет %q:\n%s", want, got)
		}
	}
	// Причина точнее стадии и говорит о том же месте — печатать обе значит
	// сказать одно дважды.
	if strings.Contains(got, "остановились на стадии") {
		t.Errorf("стадия напечатана рядом с причиной:\n%s", got)
	}

	// Причина незнакома (писатель уехал вперёд бота) — остаётся стадия, а
	// событие не теряется.
	d.Reason = "reactor_meltdown"
	if got := formatDeploy(d); !strings.Contains(got, "healthcheck") || !strings.Contains(got, "откачен") {
		t.Errorf("незнакомая причина съела сообщение:\n%s", got)
	}
}

func TestРучнойОткатНазываетРелиз(t *testing.T) {
	// dk rollback <цель> сегодня молчит: сервер откатился, в чате пусто.
	d := deployOf(deployRollback)
	d.Version, d.Reason, d.RunURL = "release-20260804-090000-def5678", "manual", ""
	got := formatDeploy(d)

	for _, want := range []string{"откачен руками", "вернули", "release-20260804-090000-def5678"} {
		if !strings.Contains(got, want) {
			t.Errorf("в сообщении о ручном откате нет %q:\n%s", want, got)
		}
	}
}

func TestСлужебныеВидыВЧатНеИдут(t *testing.T) {
	// started нужен детектору аномалий и метрике времени выкатки, но не чату
	// (контракт, §4). Незнакомый вид показывать тоже нечем.
	for _, kind := range []string{deployStarted, "перезагрузить", ""} {
		if got := formatDeploy(deployOf(kind)); got != "" {
			t.Errorf("вид %q дал сообщение: %q", kind, got)
		}
	}
}

func TestСыройЛогНеПроезжаетВместоСтадии(t *testing.T) {
	// ГЛАВНАЯ ПРОВЕРКА ПО ИБ. В логе выкатки бывают пути на сервере, имена
	// хостов и куски конфигурации, а сообщение уезжает в чат и потенциально на
	// публичную страницу. Стадия и причина — перечисления (контракт, §7): всё,
	// что не совпало с ключом, не показывается вовсе.
	d := deployOf(deployFailure)
	d.Stage = "/etc/nginx/sites/status.samoy.love.conf:41 test failed"
	d.Reason = "connect to 10.0.0.5:5432 refused, PGPASSWORD=hunter2"
	got := formatDeploy(d)

	for _, leak := range []string{"nginx", "10.0.0.5", "hunter2", "/etc/"} {
		if strings.Contains(got, leak) {
			t.Errorf("внутренности сервера уехали в чат (%q):\n%s", leak, got)
		}
	}
	// И при этом событие не потеряно: о провале сказано.
	if !strings.Contains(got, "не выкачен") {
		t.Errorf("событие потеряно из-за незнакомой стадии:\n%s", got)
	}
}

func TestОдиночнаяЦельДаётПрежнююФормуРелиза(t *testing.T) {
	// Условие задачи: форма сообщения о релизе не меняется. Прогон из одной
	// цели — большинство хозяйства, и следов группировки в нём быть не должно,
	// даже когда цель успела объявить о себе дважды (started, потом success).
	d := deployOf(deploySuccess)
	d.Previous = "release-20260804-090000-def5678"
	d.Changelog = []string{"Не выдавать недоставленное уведомление за успех"}

	want := formatEvent(Event{
		Kind: KindRelease, Title: d.Title, URL: d.URL, Project: d.Project,
		Version: d.Version, Previous: d.Previous, Changelog: d.Changelog, At: d.At,
	})
	got := formatDeployGroup("Snakes", []Deploy{deployOf(deployStarted), d})
	if got != want {
		t.Errorf("одиночная цель дала не прежнюю форму:\nожидали %q\nполучили %q", want, got)
	}
	for _, trace := range []string{"выкачен", "выкатывается"} {
		if strings.Contains(got, trace) {
			t.Errorf("в одиночном сообщении остался след группировки (%q):\n%s", trace, got)
		}
	}
}

func TestУстановщикЗоветВАдминкуПереключитьВерсию(t *testing.T) {
	// publish-file.sh кладёт сборку на сервер, но канал самообновления не
	// переключает — это решение человека (см. deployAdminLinks). Без прямой
	// ссылки релиз в чате выглядит завершённым, а переключатель ждёт в другой
	// вкладке, о которой напоминает только память.
	d := deployOf(deploySuccess)
	d.App = "chillhub-installer"
	got := formatDeploy(d)
	if !strings.Contains(got, "переключить") || !strings.Contains(got, deployAdminLinks["chillhub-installer"]) {
		t.Errorf("нет ссылки на переключение канала:\n%s", got)
	}
}

func TestОбычныйРелизНеЗовётВАдминку(t *testing.T) {
	// У целей без записи в deployAdminLinks (подавляющее большинство) ничего
	// переключать не нужно, и сообщение обязано остаться прежним — без лишней
	// строки, которая для них ничего не значит.
	got := formatDeploy(deployOf(deploySuccess))
	if strings.Contains(got, "переключить") {
		t.Errorf("лишняя ссылка на переключение у цели без ручного шага:\n%s", got)
	}
}

func TestУстановщикВГрупповомСообщенииТожеЗовётВАдминку(t *testing.T) {
	// Та же ссылка обязана появиться и когда установщик приехал в одном
	// прогоне с другими целями (обычный случай chillhub — installer едет
	// вместе с site/api/admin/admin-ui), а не только в одиночном сообщении.
	installer := deployOf(deploySuccess)
	installer.App = "chillhub-installer"
	installer.Title = "Установщик"
	site := deployOf(deploySuccess)
	site.App = "chillhub-site"
	site.Title = "Сайт"
	got := formatDeployGroup("ChillHub", []Deploy{site, installer})
	if !strings.Contains(got, "переключить") || !strings.Contains(got, deployAdminLinks["chillhub-installer"]) {
		t.Errorf("нет ссылки на переключение канала в групповом сообщении:\n%s", got)
	}
}

// chillhubRun — прогон из шести целей одного пуша: главный полигон хозяйства.
func chillhubRun() []Deploy {
	changelog := []string{
		"Не выдавать недоставленное уведомление за успех",
		"Считать пропавший файл ошибкой обновления",
	}
	at := base
	mk := func(app, title, kind string) Deploy {
		at = at.Add(time.Minute)
		return Deploy{
			Kind: kind, App: app, Title: title, URL: "https://launcher.samoy.love/",
			Version: "release-20260805-130115-1a2b3c4", Changelog: changelog, At: at,
		}
	}
	site := mk("chillhub-site", "Сайт", deployStarted)
	api := mk("chillhub-api", "API", deployFailure)
	api.Stage = "units"
	admin := mk("chillhub-admin", "Админка", deployRolledBack)
	admin.Reason = "health_failed"
	return []Deploy{
		site,
		mk("chillhub-launcher", "Лаунчер", deploySuccess),
		api, admin,
		mk("chillhub-bot", "Бот", deployStarted),
		mk("chillhub-site", "Сайт", deploySuccess), // та же цель объявила исход
	}
}

func TestСообщениеПрогонаПечатаетИзмененияОдинРаз(t *testing.T) {
	// Ради этого группировка и заводится: шесть сообщений с одинаковым блоком
	// «Изменения» — это способ перестать читать чат.
	got := formatDeployGroup("ChillHub", chillhubRun())

	if n := strings.Count(got, "<b>Изменения</b>"); n != 1 {
		t.Errorf("блок изменений напечатан %d раз(а):\n%s", n, got)
	}
	if n := strings.Count(got, "Считать пропавший файл ошибкой обновления"); n != 1 {
		t.Errorf("пункт списка задвоился (%d):\n%s", n, got)
	}
	// Шапка называет проект и версию, а не первую попавшуюся цель.
	if !strings.HasPrefix(got, "🚀 <b>ChillHub</b> · ") {
		t.Errorf("шапка прогона названа не проектом:\n%s", got)
	}
	// Цели — строками с исходом. Каждая ровно одна: цель, объявившая о себе
	// дважды, занимает одну строку, а не две.
	for _, want := range []string{
		"Сайт</a> — выкачен",
		"Лаунчер</a> — выкачен",
		"API</a> — не выкачен — остановились на стадии: службы systemd",
		"Админка</a> — откачен автоматически — причина: healthcheck не дождался ответа",
		"Бот</a> — выкатывается…",
	} {
		if n := strings.Count(got, want); n != 1 {
			t.Errorf("строка цели %q встречается %d раз(а):\n%s", want, n, got)
		}
	}
	// Порядок строк — порядок, в котором цели заявили о себе: он совпадает с
	// порядком выкатки и не прыгает при каждой правке сообщения.
	if strings.Index(got, ">Сайт<") > strings.Index(got, ">Лаунчер<") {
		t.Errorf("порядок целей перепутан:\n%s", got)
	}
	// Изменения — последним блоком, как и в сообщении об одной цели.
	if !strings.HasSuffix(got, "• Считать пропавший файл ошибкой обновления") {
		t.Errorf("список изменений не в конце сообщения:\n%s", got)
	}
}

func TestСообщениеПрогонаРастётПоМереГотовности(t *testing.T) {
	// Отправитель правит одно и то же сообщение по мере готовности целей, а
	// значит форматтер вызывается заново на каждое событие и обязан рисовать
	// сообщение целиком. Уже объявленные строки не имеют права исчезнуть:
	// затирание пяти строк одной — ровно то, чего боится контракт (§10).
	run := chillhubRun()
	for n := 3; n <= len(run); n++ {
		got := formatDeployGroup("ChillHub", run[:n])
		if !strings.Contains(got, "Лаунчер</a> — выкачен") {
			t.Errorf("на %d событиях исчезла уже объявленная цель:\n%s", n, got)
		}
		if c := strings.Count(got, "<b>Изменения</b>"); c != 1 {
			t.Errorf("блок изменений напечатан %d раз(а)", c)
		}
	}

	// Запоздавший started не имеет права стереть уже объявленный итог:
	// события могут доехать не по порядку (контракт, §6).
	late := append(chillhubRun(), Deploy{
		Kind: deployStarted, App: "chillhub-launcher", Title: "Лаунчер", At: base,
	})
	if got := formatDeployGroup("ChillHub", late); !strings.Contains(got, "Лаунчер — выкачен") &&
		!strings.Contains(got, "Лаунчер</a> — выкачен") {
		t.Errorf("запоздавший started откатил исход цели назад:\n%s", got)
	}
}

func TestЧислоЦелейВСообщенииОграничено(t *testing.T) {
	// Двадцать целей в одном прогоне — это уже не выкатка, а враньё в журнале:
	// больше шести не катит ни один репозиторий хозяйства.
	var ds []Deploy
	for i := 0; i < 40; i++ {
		ds = append(ds, Deploy{
			Kind: deploySuccess, App: fmt.Sprintf("app-%02d", i),
			Title: fmt.Sprintf("Цель %02d", i), At: base,
		})
	}
	got := formatDeployGroup("Хозяйство", ds)
	if n := strings.Count(got, "— выкачен"); n != deployTargets {
		t.Errorf("строк целей %d, предел %d:\n%s", n, deployTargets, got)
	}
	// Молчать про отброшенное нельзя: обрезанный список читается как полный.
	if !strings.Contains(got, "…и ещё 20 целей") {
		t.Errorf("потолок сработал молча:\n%s", got)
	}
}

func TestПродВсегдаПервымНезависимоОтПорядкаВыкатки(t *testing.T) {
	// Nightly едет первым по построению выкатки (docs/DEPLOY.md), и его успех
	// доезжает до бота раньше прода. Раньше карточка печатала цели в порядке
	// прихода — прод оказывался в ней вторым всегда. Прод узнаётся по тому же
	// правилу, что и во всём хозяйстве: APP совпадает с id проекта.
	nightly := Deploy{
		Kind: deploySuccess, App: "die-dev", Project: "die", Title: "Double or Die · Nightly",
		URL: "https://dev.die.samoy.love/", Version: "release-1", At: base,
	}
	prod := Deploy{
		Kind: deploySuccess, App: "die", Project: "die", Title: "Double or Die",
		URL: "https://die.samoy.love/", Version: "release-1", At: base.Add(time.Minute),
	}
	got := formatDeployGroup("Double or Die", []Deploy{nightly, prod})
	if i, j := strings.Index(got, "Double or Die<"), strings.Index(got, "Nightly<"); i == -1 || j == -1 || i > j {
		t.Errorf("прод не первым в карточке прогона:\n%s", got)
	}
}

// ---------------------------------------------------------------------- ИБ

func TestСсылкойСтановитсяТолькоРазрешённыйАдрес(t *testing.T) {
	// Сообщения бота — это то, чему владелец доверяет по определению, и ссылку
	// в них открывают не глядя. Список разрешённого, а не запрещённого.
	bad := map[string]string{
		"javascript":          "javascript:alert(1)",
		"data":                "data:text/html,<script>alert(1)</script>",
		"без https":           "http://snakes.samoy.love/",
		"чужой домен":         "https://evil.example/",
		"домен в поддомене":   "https://samoy.love.evil.example/",
		"учётка перед хостом": "https://samoy.love@evil.example/",
		"хост в пути":         "https://evil.example/https://samoy.love/",
		"похожий хвост":       "https://notsamoy.love/",
		"выход из атрибута":   `https://samoy.love/" onmouseover="alert(1)`,
		"перевод строки":      "https://samoy.love/\nhttps://evil.example",
		"пробел":              "https://samoy.love/ x",
		"пусто":               "",
	}
	for name, u := range bad {
		t.Run(name, func(t *testing.T) {
			got := link("Snakes · Сервер", u)
			if strings.Contains(got, "<a href") {
				t.Errorf("недопустимый адрес стал ссылкой: %s", got)
			}
			// Подпись при этом обязана остаться: без адреса — как у цели, для
			// которой он не настроен вовсе.
			if !strings.Contains(got, "Snakes · Сервер") {
				t.Errorf("вместе с адресом потерялась подпись: %s", got)
			}
		})
	}

	for _, u := range []string{
		"https://samoy.love/", "https://snakes.samoy.love/healthz",
		"https://github.com/tr0llex/snakes",
	} {
		if got := link("Snakes", u); !strings.Contains(got, `<a href="`+u+`">`) {
			t.Errorf("свой адрес не стал ссылкой: %s", got)
		}
	}
}

func TestАдресПрогонаТолькоНастоящий(t *testing.T) {
	// «Прогон», ведущий не в прогон, хуже отсутствующей ссылки: по ней идут не
	// глядя, потому что она подписана.
	for _, bad := range []string{
		"https://evil.example/tr0llex/snakes/actions/runs/1",
		"http://github.com/tr0llex/snakes/actions/runs/1",
		"https://github.com/tr0llex/snakes/actions/runs/1/../../evil",
		"https://github.com@evil.example/o/r/actions/runs/1",
		"https://github.com/tr0llex/snakes/pull/26",
		"javascript:alert(1)",
	} {
		d := deployOf(deployFailure)
		d.RunURL = bad
		got := formatDeploy(d)
		if strings.Contains(got, "прогон") {
			t.Errorf("непроверенный адрес стал ссылкой на прогон (%s):\n%s", bad, got)
		}
		if !strings.Contains(got, "не выкачен") {
			t.Errorf("событие потеряно из-за негодного адреса:\n%s", got)
		}
	}
}

func TestПереворотТекстаНеПроезжает(t *testing.T) {
	// U+202E ничего не печатает, но переворачивает всё, что идёт после него:
	// в чате видно не то, что записано в поле, и заметить подмену нечем.
	d := deployOf(deployFailure)
	d.Title = "Snakes\u202E · Сервер"
	d.Version = "release-\u202E20260805"
	d.Stage = "units"
	d.Changelog = []string{"обновить\u202E nginx"}
	got := formatDeploy(d)

	for _, r := range []string{"\u202E", "\u200F", "\u2066", "\u200B", "\uFEFF"} {
		if strings.Contains(got, r) {
			t.Errorf("символ %U доехал до сообщения:\n%q", []rune(r)[0], got)
		}
	}
	// Текст при этом на месте: вырезаются невидимки, а не строка.
	if !strings.Contains(got, "Snakes · Сервер") {
		t.Errorf("вместе с невидимкой потерялся текст:\n%s", got)
	}
}

func TestПереводСтрокиВПолеНеПодделываетСообщение(t *testing.T) {
	// «Прод недоступен» с новой строки читается как отдельное уведомление от
	// бота — то есть поле события подделывает сообщение бота целиком.
	d := deployOf(deployFailure)
	d.Title = "Сайт\n🔴 <b>Прод</b> недоступен"
	d.Stage = "units"
	got := formatDeploy(d)

	if strings.Contains(got, "\n🔴") {
		t.Errorf("поле подделало строку сообщения:\n%s", got)
	}
	if strings.Contains(got, "<b>Прод</b>") {
		t.Errorf("разметка из поля доехала как разметка:\n%s", got)
	}
	if !strings.Contains(got, "&lt;b&gt;Прод") {
		t.Errorf("разметка из поля должна была уехать текстом:\n%s", got)
	}
}

func TestДлинноеПолеОбрезается(t *testing.T) {
	// Событие приезжает файлом с диска, и его размер бот не выбирал.
	d := deployOf(deployFailure)
	d.Title = strings.Repeat("я", 500)
	d.Version = strings.Repeat("v", 500)
	d.App = strings.Repeat("a", 500)
	d.Stage = "units"
	got := formatDeploy(d)

	if n := strings.Count(got, "я"); n > deployTitleMax {
		t.Errorf("заголовок доехал на %d символов, предел %d", n, deployTitleMax)
	}
	if n := strings.Count(got, "v"); n > deployVerMax {
		t.Errorf("версия доехала на %d символов, предел %d", n, deployVerMax)
	}
	if !utf8.ValidString(got) {
		t.Error("обрезка разрезала символ UTF-8: такое сообщение Telegram отвергнет целиком")
	}

	// Пунктов списка изменений тоже не бесконечность.
	d2 := deployOf(deploySuccess)
	for i := 0; i < deployChangelogMax+50; i++ {
		d2.Changelog = append(d2.Changelog, fmt.Sprintf("тема %03d", i))
	}
	got2 := formatDeploy(d2)
	if n := strings.Count(got2, "\n• "); n > deployChangelogMax {
		t.Errorf("пунктов доехало %d, предел %d", n, deployChangelogMax)
	}
	if !strings.Contains(got2, "список не поместился") {
		t.Errorf("потолок списка сработал молча:\n%s", got2[len(got2)-200:])
	}
}

func TestЖурналНеПодделываетсяПолемСобытия(t *testing.T) {
	// CRLF в поле превращает одну запись journald в две, и вторая выглядит
	// сообщением самого бота. В журнале управляющий символ надо ПОКАЗАТЬ, а не
	// стереть: разбирающий инцидент должен увидеть, что в поле лежала подделка.
	got := logSafe("snakes\r\nAug 05 13:01:02 bot samoylove-bot[1]: выкатка разрешена")
	if strings.ContainsAny(got, "\r\n") {
		t.Errorf("перевод строки доехал до журнала: %q", got)
	}
	if !strings.Contains(got, `\r\n`) {
		t.Errorf("подделка не видна в журнале: %q", got)
	}
	if g := logSafe("тема\u202E\u0000"); strings.ContainsRune(g, 0) || strings.Contains(g, "\u202E") {
		t.Errorf("управляющий символ доехал до журнала: %q", g)
	}
	if n := utf8.RuneCountInString(logSafe(strings.Repeat("я", 5000))); n > logFieldMax {
		t.Errorf("в журнал уехало %d символов, предел %d", n, logFieldMax)
	}
}
