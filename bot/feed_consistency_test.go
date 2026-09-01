package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// Событий об одной цели приезжает два, и они дополняют друг друга. Строка цели
// живёт в состоянии, поэтому склеивать их надо здесь, а не у форматтера:
// раньше вторая перезаписывала первую до того, как её кто-либо увидел.
func TestСобытияОднойЦелиСкладываютсяВСостоянии(t *testing.T) {
	at := time.Unix(1787971000, 0).UTC()
	rec := &groupRecord{}

	rec.upsert(groupTarget{App: "metro", Kind: evStarted, At: at})
	// Сервер: знает previous, не знает ни коммита, ни списка изменений.
	rec.upsert(groupTarget{
		App: "metro", Kind: evSuccess,
		Version:  "release-20260901-045437-0bd2219",
		Previous: "release-20260829-055631-ae6e43d",
		At:       at.Add(time.Second),
	})
	// Пайплайн: знает коммит и прогон, previous не знает.
	rec.upsert(groupTarget{
		App: "metro", Kind: evSuccess,
		Version:   "release-20260901-045437-0bd2219",
		CommitURL: "https://github.com/tr0llex/metro-map/commit/0bd2219",
		RunURL:    "https://github.com/tr0llex/metro-map/actions/runs/1",
		At:        at.Add(2 * time.Second),
	})

	if len(rec.Targets) != 1 {
		t.Fatalf("цель обязана остаться одной строкой, получили %d", len(rec.Targets))
	}
	got := rec.Targets[0]
	if got.Previous != "release-20260829-055631-ae6e43d" {
		t.Errorf("previous с сервера стёрт событием пайплайна: %q", got.Previous)
	}
	if got.CommitURL == "" {
		t.Errorf("адрес коммита из пайплайна потерян")
	}
	if got.RunURL == "" {
		t.Errorf("адрес прогона потерян")
	}
	if got.Kind != evSuccess {
		t.Errorf("исход обязан остаться success, получили %q", got.Kind)
	}
}

// События могут доехать не по порядку, и «выкачен» не имеет права откатиться в
// «выкатывается…».
func TestЗапоздавшийStartedНеСтираетИсход(t *testing.T) {
	at := time.Unix(1787971000, 0).UTC()
	rec := &groupRecord{}
	rec.upsert(groupTarget{App: "snakes", Kind: evSuccess, Version: "release-1", At: at.Add(time.Second)})
	rec.upsert(groupTarget{App: "snakes", Kind: evStarted, At: at})

	if rec.Targets[0].Kind != evSuccess {
		t.Errorf("исход откатился в %q", rec.Targets[0].Kind)
	}
	if rec.Targets[0].Version != "release-1" {
		t.Errorf("версия потеряна: %q", rec.Targets[0].Version)
	}
}

// Временный отказ Telegram обязан повторяться, а не превращаться в дубль.
//
// 31.08 одна секунда недоступности дала в ленте два одинаковых сообщения о
// релизе 1.6.37: правка упала с Bad Gateway, и отправитель ушёл в запасной
// путь «шлю новым».
func TestВременныйОтказПравкиПовторяетсяАНеДублируетСообщение(t *testing.T) {
	saved := editRetries
	editRetries = []time.Duration{0, 0}
	defer func() { editRetries = saved }()

	clock := time.Unix(1785924102, 0).UTC()
	tg := &fakeTelegram{}
	o := testOutbox(t, tg, nil, &clock)

	// Первое событие группы — обычная отправка.
	o.Enqueue(evt("1785924102001-snakes-started.json", strings.Repeat("a", 64), "g1", 1, "started", "snakes", clock))
	o.Enqueue(evt("1785924102002-snakes-success.json", strings.Repeat("b", 64), "g1", 2, "success", "snakes", clock))
	if err := o.Flush(context.Background()); err != nil {
		t.Fatalf("первая доставка не прошла: %v", err)
	}
	sendsAfterFirst := len(tg.sends)

	// Вторая цель того же прогона: правка карточки падает дважды по-временному
	// и проходит с третьей попытки.
	tg.editFailN = 2
	tg.editFailErr = errors.New("editMessageText: telegram отказал: Bad Gateway")
	o.Enqueue(evt("1785924102003-metro-success.json", strings.Repeat("c", 64), "g1", 3, "success", "metro", clock))
	if err := o.Flush(context.Background()); err != nil {
		t.Fatalf("доставка после временного отказа не прошла: %v", err)
	}

	if len(tg.sends) != sendsAfterFirst {
		t.Errorf("временный отказ дал новое сообщение вместо повтора правки: было %d, стало %d",
			sendsAfterFirst, len(tg.sends))
	}
	if tg.editCalls < 3 {
		t.Errorf("повторов правки не было: попыток %d", tg.editCalls)
	}
}

// Постоянный отказ повторять нечего: карточку удалили, и её надо слать заново.
func TestПостояннаяОшибкаПравкиСразуШлётНовоеСообщение(t *testing.T) {
	saved := editRetries
	editRetries = []time.Duration{0, 0}
	defer func() { editRetries = saved }()

	clock := time.Unix(1785924102, 0).UTC()
	tg := &fakeTelegram{}
	o := testOutbox(t, tg, nil, &clock)

	o.Enqueue(evt("1785924102001-snakes-started.json", strings.Repeat("a", 64), "g1", 1, "started", "snakes", clock))
	o.Enqueue(evt("1785924102002-snakes-success.json", strings.Repeat("b", 64), "g1", 2, "success", "snakes", clock))
	if err := o.Flush(context.Background()); err != nil {
		t.Fatalf("первая доставка не прошла: %v", err)
	}
	sendsAfterFirst := len(tg.sends)
	editsAfterFirst := tg.editCalls

	tg.editErr = errors.New("editMessageText: telegram отказал: message to edit not found")
	o.Enqueue(evt("1785924102003-metro-success.json", strings.Repeat("c", 64), "g1", 3, "success", "metro", clock))
	if err := o.Flush(context.Background()); err != nil {
		t.Fatalf("доставка после постоянного отказа не прошла: %v", err)
	}

	if len(tg.sends) != sendsAfterFirst+1 {
		t.Errorf("карточку не переотправили: было %d, стало %d", sendsAfterFirst, len(tg.sends))
	}
	// Ровно одна попытка на эту доставку: постоянный отказ не повторяется.
	if got := tg.editCalls - editsAfterFirst; got != 1 {
		t.Errorf("постоянный отказ повторяли: попыток правки %d, ожидали 1", got)
	}
}

func TestРазборПостоянныхОшибокПравки(t *testing.T) {
	permanent := []string{
		"editMessageText: telegram отказал: message to edit not found",
		"editMessageText: telegram отказал: message can't be edited",
		"editMessageText: telegram отказал: Bad Request: MESSAGE_ID_INVALID",
		"editMessageText: telegram отказал: message is not modified",
	}
	for _, s := range permanent {
		if !editIsPermanent(errors.New(s)) {
			t.Errorf("считаем временной, а повтор ничего не изменит: %s", s)
		}
	}
	transient := []string{
		"editMessageText: telegram отказал: Bad Gateway",
		"editMessageText: context deadline exceeded (Client.Timeout exceeded while awaiting headers)",
		"editMessageText: telegram отказал: Too Many Requests: retry after 5",
		"editMessageText: неожиданный ответ (502)",
	}
	for _, s := range transient {
		if editIsPermanent(errors.New(s)) {
			t.Errorf("считаем постоянной — отсюда и брались дубли: %s", s)
		}
	}
	if editIsPermanent(nil) {
		t.Error("nil — не ошибка")
	}
}
