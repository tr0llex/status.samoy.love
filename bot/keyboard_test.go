package main

import (
	"strings"
	"testing"
	"time"
)

func TestКлавиатураПомечаетТекущийЭкран(t *testing.T) {
	kb := navKeyboard(ViewIncidents)
	var marked []string
	for _, row := range kb.InlineKeyboard {
		for _, b := range row {
			if strings.HasPrefix(b.Text, "· ") {
				marked = append(marked, b.Text)
			}
		}
	}
	if len(marked) != 1 {
		t.Fatalf("помечен должен быть ровно один экран, получили %v", marked)
	}
	if !strings.Contains(marked[0], "Инциденты") {
		t.Errorf("помечен не тот экран: %q", marked[0])
	}
}

func TestКнопкаОбновитьВедётНаТотЖеЭкран(t *testing.T) {
	// Иначе «Обновить» на экране инцидентов молча возвращало бы на статус.
	for _, view := range []string{ViewStatus, ViewIncidents, ViewChangelog} {
		kb := navKeyboard(view)
		var found bool
		for _, row := range kb.InlineKeyboard {
			for _, b := range row {
				if strings.Contains(b.Text, "Обновить") {
					found = true
					if b.CallbackData != view {
						t.Errorf("на экране %s «Обновить» ведёт на %s", view, b.CallbackData)
					}
				}
			}
		}
		if !found {
			t.Errorf("на экране %s нет кнопки «Обновить»", view)
		}
	}
}

func TestКнопкаМиниПриложенияТребуетHTTPS(t *testing.T) {
	// Telegram открывает мини-приложение только по https. Если адрес другой,
	// кнопка обязана остаться, но уже обычной ссылкой — иначе она молча
	// перестала бы работать.
	// Адрес приходит из конфига — единственного места настроек.
	t.Cleanup(func() { applyConfig(Config{MiniApp: "https://status.samoy.love/tg/"}) })

	applyConfig(Config{MiniApp: "https://status.samoy.love/tg/"})
	if b := openButton(); b.WebApp == nil || b.URL != "" {
		t.Errorf("для https ожидали web_app, получили %+v", b)
	}
	applyConfig(Config{MiniApp: "http://localhost:4331/tg/"})
	if b := openButton(); b.WebApp != nil || b.URL == "" {
		t.Errorf("для http ожидали обычную ссылку, получили %+v", b)
	}
}

func TestКнопкаМиниПриложенияНеЛомаетГруппу(t *testing.T) {
	// Telegram отклоняет web_app-кнопку вне личной переписки: sendMessage с
	// ней в reply_markup отказывает целиком, а не молча теряет кнопку. Owner
	// в конфиге уже отличает группу (id отрицательный) от личного чата — эта
	// проверка обязана дойти и до клавиатуры, не только до прав владельца.
	t.Cleanup(func() { applyConfig(Config{MiniApp: "https://status.samoy.love/tg/"}) })

	applyConfig(Config{MiniApp: "https://status.samoy.love/tg/", Owner: 173418650})
	if b := openButton(); b.WebApp == nil || b.URL != "" {
		t.Errorf("в личном чате ожидали web_app, получили %+v", b)
	}
	applyConfig(Config{MiniApp: "https://status.samoy.love/tg/", Owner: -1001234567890})
	if b := openButton(); b.WebApp != nil || b.URL == "" {
		t.Errorf("в группе ожидали обычную ссылку вместо web_app, получили %+v", b)
	}
}

func TestНеизвестнаяКнопкаНеЛомаетЭкран(t *testing.T) {
	// Сообщение могло быть отправлено прошлой версией бота с другими кнопками.
	if got := viewOf("несуществующая"); got != ViewStatus {
		t.Errorf("неизвестная команда должна вести на статус, получили %q", got)
	}
}

func TestЭкранСправкиНеЧитаетДанные(t *testing.T) {
	// Справка обязана открываться, даже когда агент не работает: это
	// единственный экран, которому данные не нужны.
	got, _ := renderView(ViewHelp, "/нет/такого/файла.json", base)
	if !strings.Contains(got, "Статус samoy.love") {
		t.Errorf("справка не отрисовалась: %q", got)
	}
}

func TestЭкранСообщаетОНедоступныхДанных(t *testing.T) {
	got, _ := renderView(ViewStatus, "/нет/такого/файла.json", base)
	if !strings.Contains(got, "данные агента") && !strings.Contains(got, "не работает") {
		t.Errorf("о нечитаемых данных надо сказать прямо: %q", got)
	}
}

func TestКнопкиПроектовНесутСостояние(t *testing.T) {
	// Состояние проекта переехало на кнопку: общий экран от этого короткий,
	// а «что с чем» видно, не читая текст.
	s := summaryAt(base, "down", base, false, "v1")
	kb := statusKeyboard(s)
	var found bool
	for _, row := range kb.InlineKeyboard {
		for _, b := range row {
			if b.CallbackData == ViewProject+"snakes" {
				found = true
				if !strings.Contains(b.Text, "Snakes") {
					t.Errorf("на кнопке нет названия проекта: %q", b.Text)
				}
				if !strings.Contains(b.Text, down) {
					t.Errorf("на кнопке нет состояния: %q", b.Text)
				}
			}
		}
	}
	if !found {
		t.Fatal("на экране статуса нет кнопки проекта")
	}
}

func TestЭкранПроектаПоказываетПодробности(t *testing.T) {
	s := summaryAt(base, "down", base.Add(-time.Hour), false, "v1")
	path := writeSummaryOf(t, s)

	text, kb := renderView(ViewProject+"snakes", path, base)
	for _, want := range []string{"Snakes", "Клиент", "HTTP 502", "Игровой сервер", "v1"} {
		if !strings.Contains(text, want) {
			t.Errorf("на экране проекта нет %q:\n%s", want, text)
		}
	}
	if kb == nil {
		t.Fatal("под экраном проекта нет клавиатуры")
	}
}

func TestИсчезнувшийПроектВозвращаетНаСтатус(t *testing.T) {
	// Кнопка могла прийти из старого сообщения, а проект — уехать из конфига.
	path := writeSummaryOf(t, summaryAt(base, "up", base, true, "v1"))
	text, _ := renderView(ViewProject+"нет-такого", path, base)
	if !strings.Contains(text, "ключевых проверок") {
		t.Errorf("ожидали общий экран, получили:\n%s", text)
	}
}

func TestКнопкаПодУведомлениемВедётВУпавшийПроект(t *testing.T) {
	kb := alertKeyboard("metro")
	if got := kb.InlineKeyboard[0][0].CallbackData; got != ActWhatNowPrefix+ViewProject+"metro" {
		t.Errorf("кнопка ведёт на %q, а не в упавший проект", got)
	}
	// Событие без проекта (устаревшие данные агента) — общий экран.
	if got := alertKeyboard("").InlineKeyboard[0][0].CallbackData; got != ActWhatNowPrefix+ViewStatus {
		t.Errorf("без проекта ожидали общий экран, получили %q", got)
	}
}

// Кнопка здорового проекта не должна нести значок: в норме зелёные все, и ряд
// кнопок превращается в стену одинаковых ярких пятен, в которой не видно
// единственную проблемную.
func TestButtonIconIsQuietWhenAllIsWell(t *testing.T) {
	for _, ok := range []string{"up", "operational"} {
		if got := buttonIcon(ok); got != "" {
			t.Errorf("здоровый проект (%s) должен идти без значка, получили %q", ok, got)
		}
	}
	// А вот проблему значок обязан показать — иначе кнопки станут неразличимы
	// в другую сторону.
	for _, bad := range []string{"down", "slow", "degraded", "major"} {
		if buttonIcon(bad) == "" {
			t.Errorf("состояние %q обязано быть видно на кнопке", bad)
		}
	}
}
