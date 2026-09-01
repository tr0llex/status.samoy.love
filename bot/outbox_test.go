package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeTelegram — Telegram, который отказывает по команде.
//
// Поддельный сервер из telegram_test.go проверяет протокол; здесь нужен
// управляемый отказ, поэтому подменяется не HTTP, а интерфейс отправителя.
// Проверка того, что настоящий *Telegram этот интерфейс удовлетворяет и
// возвращает message_id, живёт отдельно — TestSendGroupReturnsMessageID.
type fakeTelegram struct {
	mu      sync.Mutex
	sends   []string
	edits   []fakeEdit
	nextID  int64
	sendErr error
	editErr error

	// editCalls, editFailN и editFailErr нужны проверке повторов: она должна
	// уметь провалить ПЕРВЫЕ N правок и посчитать, сколько их было всего.
	// Нулевые значения ничего не меняют для остальных тестов.
	editCalls   int
	editFailN   int
	editFailErr error
}

type fakeEdit struct {
	messageID int64
	text      string
}

func (f *fakeTelegram) SendGroup(_ context.Context, _ int64, text string, _ *Keyboard) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sendErr != nil {
		return 0, f.sendErr
	}
	f.nextID++
	f.sends = append(f.sends, text)
	return f.nextID, nil
}

func (f *fakeTelegram) EditLong(_ context.Context, _, messageID int64, text string, _ *Keyboard) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.editCalls++
	if f.editFailN > 0 {
		f.editFailN--
		return f.editFailErr
	}
	if f.editErr != nil {
		return f.editErr
	}
	f.edits = append(f.edits, fakeEdit{messageID: messageID, text: text})
	return nil
}

func (f *fakeTelegram) sent() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.sends...)
}

func (f *fakeTelegram) edited() []fakeEdit {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeEdit(nil), f.edits...)
}

func (f *fakeTelegram) fail(send, edit error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sendErr, f.editErr = send, edit
}

// testOutbox — отправитель с управляемыми часами: пауза между попытками
// считается от них, и без подмены тест либо спал бы по-настоящему, либо зависел
// от того, насколько быстро отработала машина.
func testOutbox(t *testing.T, tg outboxTelegram, st *outboxState, clock *time.Time) *outbox {
	t.Helper()
	// Нулевой *outboxState, завёрнутый в интерфейс, — это НЕ nil-интерфейс, и
	// newOutbox его не подменит. Разворачиваем здесь, чтобы каждый тест не
	// повторял эту ловушку.
	if st == nil {
		st = newOutboxState()
	}
	o := newOutbox(tg, 173418650, st, nil, nil, testRenderGroup)
	o.now = func() time.Time { return *clock }
	o.log = t.Logf
	return o
}

// testRenderGroup — render для тестов, где форма сообщения не проверяется, но
// обязана быть настоящей: newOutbox запасного варианта больше не даёт.
// Используется тот же форматтер, что и в проде (formatDeployGroup), а не
// упрощённая заглушка — иначе тест устройства очереди перестал бы ловить
// поломку в связке отправитель→форматтер.
func testRenderGroup(v groupView) string {
	ds := make([]Deploy, 0, len(v.Targets))
	for _, t := range v.Targets {
		ds = append(ds, Deploy{
			Kind: t.Kind, App: t.App,
			Version: t.Version, Previous: t.Previous,
			Stage: t.Stage, Reason: t.Reason,
			CommitURL: t.CommitURL, RunURL: t.RunURL,
			Changelog: v.Changelog,
			At:        t.At,
		})
	}
	return formatDeployGroup(v.App, ds)
}

func evt(file, id, group string, seq int, kind, app string, at time.Time) outboxItem {
	return outboxItem{
		File: file, ID: id, Group: group, GroupSeq: seq,
		Kind: kind, App: app, Source: "ci", At: at, Version: "release-1",
	}
}

// Главное свойство файла: отказ Telegram не двигает курсор и не теряет
// событие. Без него «Telegram лежал — сообщение придёт позже» не работает.
func TestOutboxKeepsEventWhenTelegramIsDown(t *testing.T) {
	clock := time.Unix(1785924102, 0).UTC()
	tg := &fakeTelegram{sendErr: errors.New("sendMessage: telegram отказал: Bad Gateway")}
	o := testOutbox(t, tg, nil, &clock)

	o.Enqueue(evt("1785924102123-snakes-success.json", strings.Repeat("a", 64), "g1", 1, "success", "snakes", clock))
	if err := o.Flush(context.Background()); err == nil {
		t.Fatal("отказ Telegram обязан быть ошибкой: иначе событие сочтут доставленным")
	}
	if o.Pending() != 1 {
		t.Fatalf("событие пропало из очереди: осталось %d", o.Pending())
	}
	if o.Cursor() != "" {
		t.Fatalf("курсор сдвинулся без подтверждения Telegram: %q", o.Cursor())
	}

	// Telegram вернулся. Пауза между попытками отсчитана от часов, поэтому
	// двигаем их — иначе Flush честно промолчит.
	tg.fail(nil, nil)
	clock = clock.Add(time.Minute)
	if err := o.Flush(context.Background()); err != nil {
		t.Fatalf("после восстановления отправка не удалась: %v", err)
	}
	if got := tg.sent(); len(got) != 1 {
		t.Fatalf("отправлено %d сообщений, ожидали одно: %v", len(got), got)
	}
	if o.Cursor() != "1785924102123-snakes-success.json" {
		t.Fatalf("курсор не сдвинулся после успеха: %q", o.Cursor())
	}
	if o.Pending() != 0 {
		t.Fatalf("очередь не опустела: %d", o.Pending())
	}
}

// Пауза обязана расти: Telegram отвечает отказом и на секундной аварии, и на
// многочасовой, а долбиться в него с прежним темпом сутки — способ получить бан.
func TestOutboxBackoffGrowsAndHoldsQueue(t *testing.T) {
	clock := time.Unix(1785924102, 0).UTC()
	tg := &fakeTelegram{sendErr: errors.New("отказ")}
	o := testOutbox(t, tg, nil, &clock)
	o.Enqueue(evt("1785924102123-snakes-success.json", "id1", "g1", 1, "success", "snakes", clock))

	if err := o.Flush(context.Background()); err == nil {
		t.Fatal("ожидали ошибку")
	}
	// Внутри паузы Flush не ходит в Telegram вовсе — и не считается неудачей.
	if err := o.Flush(context.Background()); err != nil {
		t.Fatalf("внутри паузы Flush обязан молчать, а не возвращать ошибку: %v", err)
	}
	if o.fails != 1 {
		t.Fatalf("попытка внутри паузы засчитана как отказ: fails=%d", o.fails)
	}
	first := backoff(1)
	clock = clock.Add(first + time.Second)
	if err := o.Flush(context.Background()); err == nil {
		t.Fatal("ожидали вторую ошибку")
	}
	if backoff(2) <= first {
		t.Fatalf("пауза не растёт: %s → %s", first, backoff(2))
	}
	if backoff(100) != outboxRetryMax {
		t.Fatalf("пауза не упирается в потолок: %s", backoff(100))
	}
	if o.Pending() != 1 || o.Cursor() != "" {
		t.Fatalf("событие или курсор пострадали от отказов: pending=%d cursor=%q", o.Pending(), o.Cursor())
	}
}

// Порядок ленты — это порядок событий. Перескочить упавшее и отправить
// следующее значило бы показать выкатку раньше её же старта.
func TestOutboxPreservesOrder(t *testing.T) {
	clock := time.Unix(1785924102, 0).UTC()
	tg := &fakeTelegram{}
	o := testOutbox(t, tg, nil, &clock)
	o.render = func(v groupView) string { return v.Targets[len(v.Targets)-1].App }

	o.Enqueue(
		evt("1785924102001-a-success.json", "id-a", "ga", 1, "success", "a", clock),
		evt("1785924102002-b-success.json", "id-b", "gb", 1, "success", "b", clock),
		evt("1785924102003-c-success.json", "id-c", "gc", 1, "success", "c", clock),
	)
	// Роняем Telegram после первого сообщения: второе и третье обязаны
	// остаться в очереди в своём порядке.
	if err := o.Flush(context.Background()); err != nil {
		t.Fatalf("первый проход: %v", err)
	}
	tg.fail(errors.New("отказ"), nil)
	o.Enqueue(evt("1785924102004-d-success.json", "id-d", "gd", 1, "success", "d", clock))

	tg.fail(nil, nil)
	if err := o.Flush(context.Background()); err != nil {
		t.Fatalf("второй проход: %v", err)
	}
	want := []string{"a", "b", "c", "d"}
	got := tg.sent()
	if len(got) != len(want) {
		t.Fatalf("отправлено %v, ожидали %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("порядок нарушен: %v, ожидали %v", got, want)
		}
	}
	if o.Cursor() != "1785924102004-d-success.json" {
		t.Fatalf("курсор отстал: %q", o.Cursor())
	}
}

// Транспорт повторяет доставку: два файла с РАЗНЫМИ именами могут нести один
// id, и курсор здесь не помогает никак. Второе сообщение в чате врало бы о
// числе выкаток.
func TestOutboxDropsDuplicateByID(t *testing.T) {
	clock := time.Unix(1785924102, 0).UTC()
	tg := &fakeTelegram{}
	o := testOutbox(t, tg, nil, &clock)

	const id = "d209f71003ade058bf845a91031f2cf424fe40567d446ecb689bfb15df32fa91"
	o.Enqueue(evt("1785924102123-snakes-success.json", id, "g1", 1, "success", "snakes", clock))
	o.Enqueue(evt("1785924105999-snakes-success.json", id, "g1", 1, "success", "snakes", clock))
	if err := o.Flush(context.Background()); err != nil {
		t.Fatalf("отправка: %v", err)
	}
	if got := tg.sent(); len(got) != 1 {
		t.Fatalf("дубль по id уехал в чат: %d сообщений", len(got))
	}
	if len(tg.edited()) != 0 {
		t.Fatalf("дубль превратился в правку: %v", tg.edited())
	}
	// Курсор обязан пройти и по дублю: иначе журнал встанет на нём навсегда.
	if o.Cursor() != "1785924105999-snakes-success.json" {
		t.Fatalf("курсор застрял на дубле: %q", o.Cursor())
	}
}

// Падение между отправкой и сдвигом курсора: перезапущенный бот начинает с
// outboxCursor и перечитывает журнал. Дубля в чате быть не должно — за это
// отвечает recentIds, и он обязан пережить сериализацию состояния.
func TestOutboxNoDuplicateAfterRestart(t *testing.T) {
	clock := time.Unix(1785924102, 0).UTC()
	tg := &fakeTelegram{}
	st := newOutboxState()
	o := testOutbox(t, tg, st, &clock)

	const id = "d209f71003ade058bf845a91031f2cf424fe40567d446ecb689bfb15df32fa91"
	item := evt("1785924102123-snakes-success.json", id, "760e", 1, "success", "snakes", clock)
	o.Enqueue(item)
	if err := o.Flush(context.Background()); err != nil {
		t.Fatalf("отправка: %v", err)
	}

	// Состояние легло на диск и прочитано заново — ровно то, что делает
	// перезапуск бота.
	raw, err := json.Marshal(st)
	if err != nil {
		t.Fatalf("состояние не сериализуется: %v", err)
	}
	for _, key := range []string{`"outboxCursor"`, `"recentIds"`, `"groups"`, `"messageID"`} {
		if !strings.Contains(string(raw), key) {
			t.Fatalf("в состоянии нет ключа %s по контракту: %s", key, raw)
		}
	}
	restored := newOutboxState()
	if err := json.Unmarshal(raw, restored); err != nil {
		t.Fatalf("состояние не читается обратно: %v", err)
	}

	o2 := testOutbox(t, tg, restored, &clock)
	// Приёмник после перезапуска начинает с outboxCursor и выдаёт тот же файл
	// ещё раз; транспорт мог положить и второй файл с тем же id.
	o2.Enqueue(item)
	o2.Enqueue(evt("1785924199999-snakes-success.json", id, "760e", 1, "success", "snakes", clock))
	if err := o2.Flush(context.Background()); err != nil {
		t.Fatalf("отправка после перезапуска: %v", err)
	}
	if got := tg.sent(); len(got) != 1 {
		t.Fatalf("после перезапуска событие ушло повторно: %d сообщений", len(got))
	}
}

// Один пуш катит несколько целей, коммит и список изменений у них ОДИН. Шесть
// сообщений с одинаковым блоком «Изменения» — способ перестать читать чат.
func TestOutboxGroupEditsFirstMessage(t *testing.T) {
	clock := time.Unix(1785924102, 0).UTC()
	tg := &fakeTelegram{}
	o := testOutbox(t, tg, nil, &clock)

	first := evt("1785924102001-site-success.json", "id-1", "grp", 1, "success", "site", clock)
	first.Changelog = []string{"Не выдавать недоставленное уведомление за успех"}
	o.Enqueue(first)
	o.Enqueue(evt("1785924102002-launcher-success.json", "id-2", "grp", 2, "success", "launcher", clock))
	o.Enqueue(evt("1785924102003-api-failure.json", "id-3", "grp", 3, "failure", "api", clock))
	if err := o.Flush(context.Background()); err != nil {
		t.Fatalf("отправка: %v", err)
	}

	if got := tg.sent(); len(got) != 1 {
		t.Fatalf("прогон дал %d сообщений вместо одного: %v", len(got), got)
	}
	edits := tg.edited()
	if len(edits) != 2 {
		t.Fatalf("второе и третье события обязаны править первое сообщение, правок: %d", len(edits))
	}
	for _, e := range edits {
		if e.messageID != 1 {
			t.Fatalf("правка ушла не в то сообщение: %d", e.messageID)
		}
	}
	last := edits[len(edits)-1].text
	for _, want := range []string{"site", "launcher", "api"} {
		if !strings.Contains(last, want) {
			t.Fatalf("в карточке прогона нет цели %q:\n%s", want, last)
		}
	}
	// Список изменений печатается один раз и не размножается по правкам.
	if n := strings.Count(last, "Не выдавать недоставленное уведомление за успех"); n != 1 {
		t.Fatalf("список изменений повторён %d раз(а):\n%s", n, last)
	}
}

// Правка не удалась (карточку удалили руками, Telegram отказал) — шлём новым
// сообщением. Молчание тут недопустимо: цель выкачена, а в чате об этом нет ни
// слова.
func TestOutboxFallsBackToNewMessageWhenEditFails(t *testing.T) {
	clock := time.Unix(1785924102, 0).UTC()
	tg := &fakeTelegram{}
	o := testOutbox(t, tg, nil, &clock)

	o.Enqueue(evt("1785924102001-site-success.json", "id-1", "grp", 1, "success", "site", clock))
	if err := o.Flush(context.Background()); err != nil {
		t.Fatalf("первое событие: %v", err)
	}
	tg.fail(nil, errors.New("editMessageText: telegram отказал: message to edit not found"))
	o.Enqueue(evt("1785924102002-launcher-success.json", "id-2", "grp", 2, "success", "launcher", clock))
	if err := o.Flush(context.Background()); err != nil {
		t.Fatalf("после неудачной правки отправка обязана пройти: %v", err)
	}
	if got := tg.sent(); len(got) != 2 {
		t.Fatalf("неудачная правка не превратилась в сообщение: %d", len(got))
	}
	// message_id перепривязан, иначе следующая цель снова упрётся в удалённое
	// сообщение.
	tg.fail(nil, nil)
	o.Enqueue(evt("1785924102003-api-success.json", "id-3", "grp", 3, "success", "api", clock))
	if err := o.Flush(context.Background()); err != nil {
		t.Fatalf("третье событие: %v", err)
	}
	edits := tg.edited()
	if len(edits) != 1 || edits[0].messageID != 2 {
		t.Fatalf("message_id группы не перепривязан: %+v", edits)
	}
}

// Событие группы, пришедшее через сутки, — новое сообщение, а не правка:
// править то, что давно пролистали, бессмысленно, и Telegram этого не даст.
func TestOutboxExpiredGroupStartsNewMessage(t *testing.T) {
	clock := time.Unix(1785924102, 0).UTC()
	tg := &fakeTelegram{}
	o := testOutbox(t, tg, nil, &clock)

	o.Enqueue(evt("1785924102001-site-success.json", "id-1", "grp", 1, "success", "site", clock))
	if err := o.Flush(context.Background()); err != nil {
		t.Fatalf("первое событие: %v", err)
	}
	clock = clock.Add(outboxGroupTTL + time.Minute)
	o.Enqueue(evt("1785999999999-site-success.json", "id-2", "grp", 2, "success", "site", clock))
	if err := o.Flush(context.Background()); err != nil {
		t.Fatalf("опоздавшее событие: %v", err)
	}
	if got := tg.sent(); len(got) != 2 {
		t.Fatalf("опоздавшее событие ушло правкой: сообщений %d, правок %d", len(got), len(tg.edited()))
	}
	if len(tg.edited()) != 0 {
		t.Fatalf("сообщение шестичасовой давности правили: %+v", tg.edited())
	}
}

// started — служебный (§4, §12 контракта): сам по себе в чат не идёт, но в уже
// висящей карточке прогона строка «выкатывается…» полезна.
func TestOutboxStartedIsSilentAloneAndFoldsIntoGroup(t *testing.T) {
	clock := time.Unix(1785924102, 0).UTC()
	tg := &fakeTelegram{}
	o := testOutbox(t, tg, nil, &clock)

	o.Enqueue(evt("1785924102001-site-started.json", "id-0", "grp", 1, "started", "site", clock))
	if err := o.Flush(context.Background()); err != nil {
		t.Fatalf("started: %v", err)
	}
	if len(tg.sent()) != 0 {
		t.Fatalf("started ушёл в чат сам по себе: %v", tg.sent())
	}
	if o.Cursor() != "1785924102001-site-started.json" {
		t.Fatalf("курсор не прошёл служебное событие — журнал встанет: %q", o.Cursor())
	}

	o.Enqueue(evt("1785924102002-site-success.json", "id-1", "grp", 2, "success", "site", clock))
	o.Enqueue(evt("1785924102003-api-started.json", "id-2", "grp", 3, "started", "api", clock))
	if err := o.Flush(context.Background()); err != nil {
		t.Fatalf("группа: %v", err)
	}
	if len(tg.sent()) != 1 {
		t.Fatalf("сообщений %d вместо одного", len(tg.sent()))
	}
	edits := tg.edited()
	if len(edits) != 1 || !strings.Contains(edits[0].text, "выкатывается") {
		t.Fatalf("started не дорисовал строку в карточке прогона: %+v", edits)
	}
}

// Одноцелевой прогон обязан давать ровно одно сообщение и ни одной правки:
// таких прогонов в хозяйстве большинство, и прежняя карточка релиза должна
// остаться прежней.
func TestOutboxSingleTargetRunSendsOneMessage(t *testing.T) {
	clock := time.Unix(1785924102, 0).UTC()
	tg := &fakeTelegram{}
	o := testOutbox(t, tg, nil, &clock)
	o.Enqueue(evt("1785924102123-snakes-success.json", "id-1", "grp", 1, "success", "snakes", clock))
	if err := o.Flush(context.Background()); err != nil {
		t.Fatalf("отправка: %v", err)
	}
	if len(tg.sent()) != 1 || len(tg.edited()) != 0 {
		t.Fatalf("сообщений %d, правок %d", len(tg.sent()), len(tg.edited()))
	}
	if !strings.Contains(tg.sent()[0], "snakes") {
		t.Fatalf("в сообщении нет цели:\n%s", tg.sent()[0])
	}
}

// Записи о прогонах чистятся: сотня прогонов за шесть часов — уже аномалия, и
// расти состоянию без предела незачем.
func TestOutboxCompactsGroupsAndRecentIDs(t *testing.T) {
	clock := time.Unix(1785924102, 0).UTC()
	st := newOutboxState()
	for i := 0; i < outboxGroupsMax+20; i++ {
		// Все прежние прогоны старше нового, но внутри срока жизни группы:
		// проверяем именно потолок на число записей, а не чистку по времени.
		st.Groups[fmt.Sprintf("g%03d", i)] = &groupRecord{
			MessageID: int64(i),
			At:        clock.Add(-time.Duration(i+1) * time.Second),
		}
	}
	st.RecentIDs["протухший"] = clock.Add(-outboxRecentTTL - time.Hour)
	st.RecentIDs["свежий"] = clock

	tg := &fakeTelegram{}
	o := testOutbox(t, tg, st, &clock)
	o.Enqueue(evt("1785924102123-snakes-success.json", "id-1", "новая", 1, "success", "snakes", clock))
	if err := o.Flush(context.Background()); err != nil {
		t.Fatalf("отправка: %v", err)
	}
	if len(st.Groups) > outboxGroupsMax {
		t.Fatalf("групп в состоянии %d при потолке %d", len(st.Groups), outboxGroupsMax)
	}
	if _, ok := st.Groups["новая"]; !ok {
		t.Fatal("выброшена свежая запись вместо самой старой")
	}
	if _, ok := st.RecentIDs["протухший"]; ok {
		t.Fatal("id старше срока жизни файла события остался в состоянии")
	}
	if _, ok := st.RecentIDs["свежий"]; !ok {
		t.Fatal("свежий id забыт — после перезапуска бот повторит сообщение")
	}
}

// Настоящий *Telegram обязан удовлетворять интерфейсу отправителя и возвращать
// message_id: без него правка карточки прогона невозможна в принципе.
func TestSendGroupReturnsMessageID(t *testing.T) {
	var texts []string
	tg := testBot(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		texts = append(texts, body["text"].(string))
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":4821,"chat":{"id":173418650}}}`))
	})

	var o outboxTelegram = tg
	id, err := o.SendGroup(context.Background(), 173418650, "🚀 <b>snakes</b>", nil)
	if err != nil {
		t.Fatalf("отправка: %v", err)
	}
	if id != 4821 {
		t.Fatalf("message_id потерян: %d", id)
	}
	if len(texts) != 1 || texts[0] != "🚀 <b>snakes</b>" {
		t.Fatalf("текст искажён: %v", texts)
	}
}

// Счётчики: «принято, но не отправлено» отличает лежачий Telegram от тишины, а
// возраст самого старого события — то, по чему заводится правило «принято и не
// отправлено больше пяти минут».
func TestOutboxMetrics(t *testing.T) {
	clock := time.Unix(1785924102, 0).UTC()
	m := newBotMetrics(t.TempDir()+"/bot.prom", clock)
	tg := &fakeTelegram{sendErr: errors.New("отказ")}
	o := newOutbox(tg, 1, nil, nil, m, testRenderGroup)
	o.now = func() time.Time { return clock }
	o.log = t.Logf

	const id = "id-1"
	o.Enqueue(evt("1785924102001-snakes-success.json", id, "grp", 1, "success", "snakes", clock))
	if err := o.Flush(context.Background()); err == nil {
		t.Fatal("ожидали отказ")
	}
	out := m.render(clock.Add(10 * time.Minute))
	for _, want := range []string{
		"statusbot_deploy_events_accepted_total 1",
		"statusbot_deploy_events_sent_total 0",
		"statusbot_deploy_events_pending_age_seconds 600",
		"statusbot_send_failures_total 1",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("нет строки %q в:\n%s", want, out)
		}
	}

	tg.fail(nil, nil)
	clock = clock.Add(time.Hour)
	o.Enqueue(evt("1785924102002-snakes-success.json", id, "grp", 2, "success", "snakes", clock))
	if err := o.Flush(context.Background()); err != nil {
		t.Fatalf("отправка: %v", err)
	}
	out = m.render(clock)
	for _, want := range []string{
		"statusbot_deploy_events_accepted_total 2",
		"statusbot_deploy_events_sent_total 1",
		`statusbot_deploy_events_dropped_total{reason="дубль"} 1`,
		"statusbot_deploy_events_pending_age_seconds 0",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("нет строки %q в:\n%s", want, out)
		}
	}
}

// Выгрузка метрик выключается пустым путём, и отправитель обязан работать в
// таком запуске без единой проверки на nil в своём коде.
func TestOutboxMetricsNilIsSafe(t *testing.T) {
	var m *botMetrics
	m.eventAccepted()
	m.eventSent()
	m.eventDropped("дубль")
	m.pendingSince(time.Now())
	m.pendingSince(time.Time{})
	if s := m.render(time.Now()); s != "" {
		t.Fatalf("выключенная выгрузка вернула текст: %q", s)
	}
}

// Неудачная запись состояния не повод повторить сообщение: отправленное уже в
// чате, память — источник правды, диск только догоняет.
func TestOutboxSurvivesFailedSave(t *testing.T) {
	clock := time.Unix(1785924102, 0).UTC()
	tg := &fakeTelegram{}
	o := newOutbox(tg, 1, nil, func() error { return errors.New("диск только для чтения") }, nil, testRenderGroup)
	o.now = func() time.Time { return clock }
	o.log = t.Logf

	o.Enqueue(evt("1785924102001-snakes-success.json", "id-1", "grp", 1, "success", "snakes", clock))
	if err := o.Flush(context.Background()); err != nil {
		t.Fatalf("неудачная запись состояния не должна валить отправку: %v", err)
	}
	if len(tg.sent()) != 1 || o.Pending() != 0 {
		t.Fatalf("сообщений %d, в очереди %d", len(tg.sent()), o.Pending())
	}
	o.Enqueue(evt("1785924102002-snakes-success.json", "id-1", "grp", 2, "success", "snakes", clock))
	if err := o.Flush(context.Background()); err != nil {
		t.Fatalf("повтор: %v", err)
	}
	if len(tg.sent()) != 1 {
		t.Fatalf("после неудачной записи состояния событие ушло повторно: %d", len(tg.sent()))
	}
}

// Отправитель больше не носит запасной форматтер: render обязателен у
// newOutbox (см. комментарий там же), и экранирование стадии/причины
// проверяется на настоящем форматтере — TestFormatEscapesHTML и соседние
// тесты в format_test.go.
func TestNewOutboxPanicsWithoutRender(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("newOutbox с render=nil обязан паниковать: форма сообщения не может быть запасной")
		}
	}()
	newOutbox(&fakeTelegram{}, 1, nil, nil, nil, nil)
}
