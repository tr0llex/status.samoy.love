package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// ------------------------------------------------------------------ снаряжение

// farmSummary — хозяйство из двух проектов, у одного из которых две цели.
// Ровно та форма, ради которой команда и заведена: «/changelog metro» обязан
// показать обе цели проекта, а не первую попавшуюся.
func farmSummary() *Summary {
	return &Summary{
		Updated: base.Format(time.RFC3339),
		Overall: "operational",
		Projects: []Project{
			{
				ID: "metro", Title: "Метро", URL: "https://metro.samoy.love/",
				Builds: []Build{
					{Title: "Сайт", Version: "v3", At: base.Add(-2 * time.Hour).Format(time.RFC3339)},
					{Title: "API", Version: "a7", At: base.Add(-30 * time.Hour).Format(time.RFC3339)},
				},
			},
			{
				ID: "snakes", Title: "Snakes", URL: "https://snakes.samoy.love/",
				Builds: []Build{
					{Title: "Сервер и клиент", Version: "v1", At: base.Add(-time.Hour).Format(time.RFC3339)},
				},
			},
		},
	}
}

func writeReleases(t *testing.T, summaryPath string, rel map[string][]Release) {
	t.Helper()
	b, err := json.Marshal(rel)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(releasesPath(summaryPath), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// release — запись журнала со списком изменений.
func release(version string, at time.Time, changelog ...string) Release {
	return Release{
		Version:   version,
		At:        at.Format(time.RFC3339),
		Seen:      at.Add(time.Minute).Format(time.RFC3339),
		Changelog: changelog,
	}
}

// farmData кладёт рядом сводку и журнал выкаток и возвращает путь к сводке.
func farmData(t *testing.T) string {
	t.Helper()
	path := writeSummaryOf(t, farmSummary())
	writeReleases(t, path, map[string][]Release{
		"metro::Сайт": {
			release("v3", base.Add(-2*time.Hour), "ускорить расписание", "починить поиск станции"),
			release("v2", base.Add(-50*time.Hour), "перевести карту на новый тайлсет"),
			release("v1", base.Add(-200*time.Hour)),
		},
		"metro::API": {
			release("a7", base.Add(-30*time.Hour), "отдавать ETA в секундах"),
		},
		"snakes::Сервер и клиент": {
			release("v1", base.Add(-time.Hour), "поднять go до 1.22"),
		},
	})
	return path
}

// ------------------------------------------------------------------ хозяйство

func TestИзмененияПоВсемуХозяйству(t *testing.T) {
	// Команда без аргумента отвечает на вопрос «что вообще происходило»:
	// последняя выкатка КАЖДОЙ цели, чтобы не пришлось обходить их по одной.
	text, kb := renderView(ViewChangelog, farmData(t), base)

	for _, want := range []string{
		"Метро · Сайт", "Метро · API", "Snakes · Сервер и клиент",
		"<code>v3</code>", "<code>a7</code>", "<code>v1</code>",
		"• ускорить расписание", "• отдавать ETA в секундах", "• поднять go до 1.22",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("в обзоре нет %q:\n%s", want, text)
		}
	}
	// Обзор — про последнюю выкатку. Позапрошлые версии здесь только шумят,
	// для них есть экран цели.
	if strings.Contains(text, "<code>v2</code>") {
		t.Errorf("в обзор попала не последняя выкатка:\n%s", text)
	}
	// Свежесть данных — как на всех экранах: молчащий агент иначе выглядит
	// как «выкаток давно не было».
	if !strings.Contains(text, "Данные агента") {
		t.Errorf("не сказано, насколько свежи данные:\n%s", text)
	}
	if kb == nil || len(kb.InlineKeyboard) == 0 {
		t.Fatal("под экраном изменений нет клавиатуры")
	}
}

func TestКнопкиЭкранаИзмененийВедутВЦель(t *testing.T) {
	_, kb := renderView(ViewChangelog, farmData(t), base)
	var found bool
	for _, row := range kb.InlineKeyboard {
		for _, b := range row {
			if b.CallbackData == ViewChangelogOf+"metro" {
				found = true
			}
			if len(b.CallbackData) > callbackDataMax {
				t.Errorf("callback_data длиннее %d байт — Telegram отвергнет сообщение целиком: %q",
					callbackDataMax, b.CallbackData)
			}
		}
	}
	if !found {
		t.Error("с экрана изменений нельзя провалиться в проект, не набирая имя")
	}
}

// ------------------------------------------------------------------ одна цель

func TestИзмененияОдногоСервиса(t *testing.T) {
	// С именем показываем историю: несколько последних выкаток подряд, сверху
	// свежая. Именно этого не хватало — уведомление о релизе приходит один раз
	// и уезжает вверх ленты.
	text, _ := renderView(ViewChangelogOf+"metro", farmData(t), base)

	for _, want := range []string{
		"Метро · Сайт", "<code>v3</code>", "<code>v2</code>", "<code>v1</code>",
		"• ускорить расписание", "• перевести карту на новый тайлсет",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("на экране цели нет %q:\n%s", want, text)
		}
	}
	// У проекта две цели, и «/changelog metro» — это про проект целиком.
	if !strings.Contains(text, "Метро · API") {
		t.Errorf("вторая цель проекта потерялась:\n%s", text)
	}
	// Чужой проект сюда не приезжает.
	if strings.Contains(text, "Snakes") {
		t.Errorf("на экране цели чужой проект:\n%s", text)
	}
	// Выкатка без списка изменений — обычное дело, и о ней надо сказать, иначе
	// она выглядит как выкатка без единого изменения.
	if !strings.Contains(text, "изменения не записаны") {
		t.Errorf("про выкатку без списка ничего не сказано:\n%s", text)
	}
}

func TestИмяЦелиУзнаётсяПоРазномуНаписанию(t *testing.T) {
	// Команду набирают с телефона, и заглядывать в конфиг агента ради точного
	// написания владелец не должен.
	path := farmData(t)
	for _, q := range []string{"metro", "METRO", "Сайт", "metro/Сайт", "Метро · Сайт", "метр"} {
		text, _ := renderView(ViewChangelogOf+q, path, base)
		if strings.Contains(text, "Не знаю цель") {
			t.Errorf("написание %q не опознано:\n%s", q, text)
		}
	}
	// Точное совпадение важнее подстроки: «API» — это цель, а не всё, где
	// встречается это слово.
	text, _ := renderView(ViewChangelogOf+"api", path, base)
	if !strings.Contains(text, "Метро · API") || strings.Contains(text, "Метро · Сайт") {
		t.Errorf("точное имя цели уступило подстроке:\n%s", text)
	}
}

func TestЭкранИСообщениеПоказываютОдинСписок(t *testing.T) {
	// Разбор у экрана и у уведомления о релизе общий. Если они разойдутся,
	// экран будет читаться как рассказ о другой выкатке.
	//
	// Заодно проверяется вход, собранный мимо агента: в журнал попало ровно то,
	// что напечатал deploy-kit/bin/changelog — с заголовком и маркерами.
	lines := []string{"<b>Изменения</b>", "• ускорить расписание", "• починить поиск &amp; фильтр"}
	path := writeSummaryOf(t, farmSummary())
	writeReleases(t, path, map[string][]Release{
		"metro::Сайт": {release("v3", base, lines...)},
	})

	text, _ := renderView(ViewChangelogOf+"metro", path, base)
	for _, want := range []string{"• ускорить расписание", "• починить поиск &amp; фильтр"} {
		if !strings.Contains(text, want) {
			t.Errorf("на экране нет %q:\n%s", want, text)
		}
	}
	// Заголовок блока экрану не нужен вовсе: название цели уже над списком.
	if strings.Contains(text, "Изменения") {
		t.Errorf("заголовок генератора уехал на экран:\n%s", text)
	}
	// Тот же список в уведомлении о релизе — слово в слово.
	msg := formatChangelog(lines, "")
	for _, want := range []string{"• ускорить расписание", "• починить поиск &amp; фильтр"} {
		if !strings.Contains(msg, want) {
			t.Errorf("уведомление и экран разошлись на %q:\n%s", want, msg)
		}
	}
}

// ------------------------------------------------------------- пустые ответы

func TestНеизвестнаяЦельПодсказываетИзвестные(t *testing.T) {
	// Опечатка в имени неотличима от «бот сломался», пока не видно, что бот
	// вообще знает.
	text, kb := renderView(ViewChangelogOf+"metroo", farmData(t), base)
	if !strings.Contains(text, "Не знаю цель «metroo»") {
		t.Errorf("не сказано, что имя неизвестно:\n%s", text)
	}
	for _, want := range []string{"<code>metro</code>", "<code>snakes</code>"} {
		if !strings.Contains(text, want) {
			t.Errorf("нет подсказки %q:\n%s", want, text)
		}
	}
	if kb == nil {
		t.Fatal("после ошибки в имени клавиатура пропала — вернуться некуда")
	}
	// Имя набирает владелец, а сообщение уходит с parse_mode=HTML: без
	// экранирования один «<» превращает ответ в ошибку Telegram, то есть в
	// молчание в ответ на команду.
	text, _ = renderView(ViewChangelogOf+"<b>ой</b>", farmData(t), base)
	if strings.Contains(text, "<b>ой") {
		t.Errorf("чужая разметка уехала в ответ как есть:\n%s", text)
	}
	if !strings.Contains(text, "&lt;b&gt;ой") {
		t.Errorf("имя не экранировано:\n%s", text)
	}
}

func TestЦельБезИсторииНеПугает(t *testing.T) {
	// Журнал агента начинается с первой замеченной СМЕНЫ версии: у цели,
	// которую с тех пор не выкатывали, записей нет вовсе. Это не поломка, и
	// выглядеть как поломка не должно.
	path := writeSummaryOf(t, farmSummary())

	text, _ := renderView(ViewChangelogOf+"snakes", path, base)
	if !strings.Contains(text, "Выкаток пока не записано") {
		t.Errorf("про пустую историю сказано непонятно:\n%s", text)
	}
	if strings.Contains(text, "🔴") {
		t.Errorf("пустая история отрисована как авария:\n%s", text)
	}

	// То же на обзоре: цели остаются на месте, просто без истории.
	text, _ = renderView(ViewChangelog, path, base)
	for _, want := range []string{"Метро · Сайт", "Snakes · Сервер и клиент", "выкаток пока не записано"} {
		if !strings.Contains(text, want) {
			t.Errorf("в обзоре без журнала нет %q:\n%s", want, text)
		}
	}
}

func TestБитыйЖурналНеЛишаетОтвета(t *testing.T) {
	// releases.json пишет агент, и он же может не дописать его при перезапуске.
	// Названия целей и версии известны из сводки — ответ обязан быть.
	path := writeSummaryOf(t, farmSummary())
	if err := os.WriteFile(releasesPath(path), []byte("{не json"), 0o600); err != nil {
		t.Fatal(err)
	}
	text, kb := renderView(ViewChangelog, path, base)
	if !strings.Contains(text, "Метро · Сайт") {
		t.Errorf("битый журнал оставил владельца без ответа:\n%s", text)
	}
	if kb == nil {
		t.Fatal("клавиатура пропала вместе с журналом")
	}
}

// ------------------------------------------------------------------- лимиты

// TestЭкранИзмененийУкладываетсяВЛимитTelegram — главная защита экрана.
//
// Лимит Telegram — 4096 единиц UTF-16 на ОДНО сообщение, и превышение не
// обрезается, а отвергается целиком: в ответ на команду владелец получает
// молчание. Упереться легко — у хозяйства десяток целей, у каждой выкатки
// своя простыня изменений.
//
// Проверяется теперь не длина ЭКРАНА, а длина КАЖДОЙ ЧАСТИ, на которые он
// раскладывается при отправке. Разница принципиальная: экран специально
// перестал укладываться в одно сообщение — иначе пришлось бы резать список
// выкаченных коммитов, а именно этого владелец и просил не делать.
func TestЭкранИзмененийУкладываетсяВЛимитTelegram(t *testing.T) {
	var projects []Project
	rel := map[string][]Release{}
	for i := 0; i < 20; i++ {
		id := fmt.Sprintf("проект-%02d", i)
		title := fmt.Sprintf("Проект номер %02d с длинным названием", i)
		projects = append(projects, Project{
			ID: id, Title: title, URL: "https://example.samoy.love/",
			Builds: []Build{{Title: "Сайт", Version: fmt.Sprintf("release-2026080%d", i%10)}},
		})
		var history []Release
		for r := 0; r < 30; r++ {
			var items []string
			for k := 0; k < 12; k++ {
				items = append(items, strings.Repeat("очень длинная тема коммита ", 6))
			}
			history = append(history, release(fmt.Sprintf("release-%02d-%02d", i, r), base, items...))
		}
		rel[id+"::Сайт"] = history
	}
	path := writeSummaryOf(t, &Summary{
		Updated: base.Format(time.RFC3339), Overall: "operational", Projects: projects,
	})
	writeReleases(t, path, rel)

	for _, view := range []string{ViewChangelog, ViewChangelogOf + "проект-03"} {
		text, _ := renderView(view, path, base)
		parts := splitMessage(text, telegramTextLimit)
		if len(parts) < 2 {
			t.Errorf("экран %s на %d единиц уложился в %d частей — раскладка не сработала",
				view, utf16Len(text), len(parts))
		}
		for i, p := range parts {
			if n := utf16Len(p); n > telegramTextLimit {
				t.Errorf("экран %s, часть %d: %d единиц UTF-16 — Telegram её не примет", view, i+1, n)
			}
			// Разметка обязана оставаться разметкой, а не куском тега:
			// негодный HTML Telegram отвергает целиком.
			if strings.Count(p, "<b>") != strings.Count(p, "</b>") {
				t.Errorf("экран %s, часть %d разрезана посреди разметки:\n%s", view, i+1, p)
			}
			if i > 0 && !strings.HasPrefix(p, "<i>продолжение (") {
				t.Errorf("экран %s, часть %d не помечена продолжением", view, i+1)
			}
		}
		// Потолок против вранья в журнале (тридцать выкаток по двенадцать
		// простыней на цель — это не история, это чужой файл) обязан быть
		// виден: молча оборванный экран читается как «больше ничего и не было».
		if !strings.Contains(text, "…") {
			t.Errorf("экран %s обрезан молча:\n%s", view, text)
		}
	}
}

func TestЭкранЦелиПоказываетВсеКоммитыВыкатки(t *testing.T) {
	// «в боте не обрезай все релизнутые коммиты» — это и про экран тоже.
	// Экран цели и уведомление о релизе рассказывают про одну выкатку, и если
	// экран потеряет пункты, два рассказа разойдутся.
	var items []string
	for i := 0; i < 40; i++ {
		items = append(items, fmt.Sprintf("изменение номер %02d", i))
	}
	path := writeSummaryOf(t, farmSummary())
	writeReleases(t, path, map[string][]Release{
		"metro::Сайт": {release("v3", base, items...)},
	})

	text, _ := renderView(ViewChangelogOf+"metro", path, base)
	for i := 0; i < 40; i++ {
		if want := fmt.Sprintf("• изменение номер %02d\n", i); !strings.Contains(text, want) {
			t.Errorf("на экране нет пункта %02d", i)
		}
	}
	if strings.Contains(text, "…и ещё") {
		t.Errorf("список свёрнут в «…и ещё N» — ровно то, от чего уходили:\n%s", text)
	}
	// И то же самое в обзоре хозяйства: три пункта на цель тоже были обрезкой.
	farm, _ := renderView(ViewChangelog, path, base)
	for i := 0; i < 40; i++ {
		if want := fmt.Sprintf("• изменение номер %02d\n", i); !strings.Contains(farm, want) {
			t.Errorf("в обзоре хозяйства нет пункта %02d", i)
		}
	}
}

func TestДлинныйОтветУходитНесколькимиСообщениями(t *testing.T) {
	// Проверка идёт до самой отправки: раскладка бесполезна, если ответ всё
	// равно уезжает одним куском и Telegram его отвергает.
	var items []string
	for i := 0; i < 60; i++ {
		items = append(items, fmt.Sprintf("изменение номер %02d %s", i, strings.Repeat("тема ", 18)))
	}
	path := writeSummaryOf(t, farmSummary())
	writeReleases(t, path, map[string][]Release{
		"metro::Сайт": {release("v3", base, items...)},
	})

	var sent []string
	tg := recorder(t, &sent)
	handleUpdate(context.Background(), tg, message(owner, "/changelog metro"), owner, owner, "samoy_love_bot", path)
	if len(sent) < 2 {
		t.Fatalf("длинный ответ ушёл %d сообщением — Telegram его не примет", len(sent))
	}
	// Ни один пункт не потерян. Смотрим по всем сообщениям сразу: где именно
	// прошла граница — дело раскладки, а вот пропасть не должно ничего.
	all := strings.Join(sent, "\n")
	for i := 0; i < 60; i++ {
		if !strings.Contains(all, fmt.Sprintf("изменение номер %02d", i)) {
			t.Errorf("пункт %02d потерялся при разбиении", i)
		}
	}
	// Клавиатура относится ко всему ответу и висит под его концом.
	if strings.Contains(sent[0], "inline_keyboard") {
		t.Error("кнопки уехали под первую часть, а не под конец ответа")
	}
	if !strings.Contains(sent[len(sent)-1], "inline_keyboard") {
		t.Error("под концом длинного ответа не оказалось кнопок")
	}
}

func TestДлинноеИмяЦелиНеЛомаетКнопку(t *testing.T) {
	// Ключ экрана уезжает в callback_data кнопки «Обновить», а там 64 байта на
	// всё. Сообщение с негодной кнопкой Telegram не отправляет целиком, то есть
	// длинное слово в аргументе стоило бы владельцу всего ответа.
	long := strings.Repeat("длинное-имя-цели-", 20)
	view := viewFor(CmdChangelog, long)
	if len(view) > callbackDataMax {
		t.Fatalf("ключ экрана %d байт, предел %d: %q", len(view), callbackDataMax, view)
	}
	text, kb := renderView(view, farmData(t), base)
	if !strings.Contains(text, "Не знаю цель") {
		t.Errorf("на выдуманное имя ожидали подсказку:\n%s", text)
	}
	for _, row := range kb.InlineKeyboard {
		for _, b := range row {
			if len(b.CallbackData) > callbackDataMax {
				t.Errorf("кнопка с callback_data %d байт: %q", len(b.CallbackData), b.CallbackData)
			}
		}
	}
}

// --------------------------------------------------------- команда и аргумент

func TestАргументКоманды(t *testing.T) {
	cases := map[string]string{
		"/changelog metro":      "metro",
		"/changelog  metro  ":   "metro",
		"/changelog metro сайт": "metro сайт",
		"/changelog@bot metro":  "metro",
		"/changelog":            "",
		"  /changelog  ":        "",
		"/status":               "",
	}
	for text, want := range cases {
		if got := commandArg(text); got != want {
			t.Errorf("commandArg(%q) = %q, ожидали %q", text, got, want)
		}
	}
	// Аргумент есть только у /changelog: «/status всё ли живо» — это /status,
	// как и было.
	if got := viewFor(CmdStatus, "всё ли живо"); got != ViewStatus {
		t.Errorf("аргумент изменил экран статуса: %q", got)
	}
	if got := viewFor(CmdChangelog, ""); got != ViewChangelog {
		t.Errorf("без аргумента ожидали обзор хозяйства, получили %q", got)
	}
	if got := viewFor(CmdChangelog, "Metro"); got != ViewChangelogOf+"metro" {
		t.Errorf("имя цели не доехало до экрана: %q", got)
	}
}

func TestКомандаИзмененийОтвечаетВладельцу(t *testing.T) {
	path := farmData(t)
	cases := []struct{ text, want string }{
		{"/changelog", "Метро · Сайт"},
		{"/c", "Snakes · Сервер и клиент"},
		{"/changelog metro", "перевести карту на новый тайлсет"},
		{"/changelog нет-такой", "Не знаю цель"},
	}
	for _, c := range cases {
		t.Run(c.text, func(t *testing.T) {
			var sent []string
			tg := recorder(t, &sent)
			handleUpdate(context.Background(), tg, message(owner, c.text), owner, owner, "samoy_love_bot", path)
			if len(sent) != 1 {
				t.Fatalf("ожидали один ответ, получили %d: %v", len(sent), sent)
			}
			if !strings.Contains(sent[0], c.want) {
				t.Errorf("в ответе нет %q:\n%s", c.want, sent[0])
			}
		})
	}
}
