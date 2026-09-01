package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Сквозной тест событий выкатки: каталог на диске → приёмник → отправитель →
// форматтер → Telegram.
//
// ЗАЧЕМ ОН ОТДЕЛЬНО ОТ inbox_test.go, outbox_test.go И format_test.go. Каждая
// из трёх сторон зелена по отдельности уже сейчас, и ровно это опаснее всего:
// стык между ними не проверяет никто, а сценарии приёмки (план §9) написаны не
// про функции, а про то, что владелец увидит в чате. Три выкатки за минуту —
// это «сколько сообщений пришло», а не «сколько событий разобралось». Здесь
// проверяется только это: сколько сообщений, какие и не потерялось ли хоть одно.
//
// Сети нет: Telegram подменён fakeTelegram (outbox_test.go), журнал лежит во
// временном каталоге, часы у отправителя свои.
//
// СВЯЗЫВАНИЕ ЖИВЁТ ЗДЕСЬ, В ТЕСТЕ, И ЭТО ВРЕМЕННО. Приёмник, отправитель и
// форматтер писались параллельно и намеренно не знают друг о друге: у каждого
// свой тип события. Переходник (e2eRig.tick), хранилище поверх State
// (e2eStore) и адаптер форматтера (e2eRig.render) — это ровно то, что обязан
// написать main.go при сборке. Пока его нет, тест собирает конвейер сам: иначе
// сценарии приёмки было бы нечем проверить до самого конца работы, то есть
// тогда, когда чинить дорого.

// e2eStart — момент, от которого идут все сценарии. Тот же день, что в
// образцах контракта (docs/events/*.json): совпадение дат делает вывод теста
// сравнимым с образцами глазами.
const e2eStart = 1785924102123 // 2026-08-05T10:01:42.123Z

// Группы прогонов. Настоящие они sha256 (§6 контракта), и приёмник проверяет
// только форму: 64 hex. Читаемые константы здесь честнее случайных хешей —
// видно, какое событие к какому прогону относится.
var (
	e2eRunA = strings.Repeat("a", 64)
	e2eRunB = strings.Repeat("b", 64)
	e2eRunC = strings.Repeat("c", 64)
)

// e2eStore — состояние отправителя поверх State бота.
//
// Курсор и недавние id ложатся в те же поля State, что завёл приёмник: ключи
// outboxCursor и recentIds обязаны сериализоваться из ОДНОГО места, иначе
// какое из двух победит, зависело бы от порядка полей в структуре.
//
// Записи о прогонах (§10 контракта, ключ groups) в State сегодня НЕТ, поэтому
// они держатся здесь, в карте, которую тест сохраняет между «перезапусками»
// сам. Настоящий перезапуск бота их сегодня потеряет — см. комментарий у
// TestE2EBotStartedAfterEventsPileUp.
type e2eStore struct {
	st     *State
	in     *Inbox
	groups map[string]*groupRecord
}

func (s *e2eStore) Cursor() string { return s.st.OutboxCursor }

func (s *e2eStore) Advance(file string) {
	if file > s.st.OutboxCursor {
		s.st.OutboxCursor = file
		s.st.dirty = true
	}
}

func (s *e2eStore) Seen(id string) bool {
	_, ok := s.st.RecentIDs[id]
	return ok
}

// Remember отдаётся приёмнику: Confirmed кладёт id в долгую память и снимает
// его с pending. Два действия обязаны случиться вместе — иначе дедупликация
// разъедется с курсором на первом же повторе доставки.
func (s *e2eStore) Remember(id string, at time.Time) { s.in.Confirmed(s.st, id, at) }

func (s *e2eStore) Group(key string) *groupRecord { return s.groups[key] }

func (s *e2eStore) SetGroup(key string, rec *groupRecord) { s.groups[key] = rec }

func (s *e2eStore) Compact(now time.Time) {
	for g, rec := range s.groups {
		if rec == nil || now.Sub(rec.At) > outboxGroupTTL {
			delete(s.groups, g)
		}
	}
}

// e2eRig — бот целиком: журнал, состояние на диске, приёмник, отправитель,
// поддельный Telegram и общие часы.
type e2eRig struct {
	t         *testing.T
	dir       string
	statePath string
	project   string

	st     *State
	in     *Inbox
	out    *outbox
	tg     *fakeTelegram
	groups map[string]*groupRecord

	clock time.Time
}

func newE2E(t *testing.T) *e2eRig {
	t.Helper()
	r := &e2eRig{
		t:         t,
		dir:       t.TempDir(),
		statePath: filepath.Join(t.TempDir(), "state.json"),
		project:   "chillhub",
		tg:        &fakeTelegram{},
		groups:    map[string]*groupRecord{},
		clock:     time.UnixMilli(e2eStart).UTC(),
	}
	r.boot()
	return r
}

// boot поднимает бота с диска — то же, что делает main.go при старте службы.
func (r *e2eRig) boot() {
	r.t.Helper()
	r.st = loadState(r.statePath)
	if r.st.RecentIDs == nil {
		r.st.RecentIDs = map[string]string{}
	}
	// Пустой курсор означает «новая установка»: приёмник в этом случае считает
	// весь журнал прочитанным, чтобы не вывалить в чат две недели истории
	// разом. Сценарии приёмки — про бота, который уже работал, поэтому курсор
	// выставлен заведомо меньше любого имени события.
	if r.st.InboxCursor == "" && r.st.OutboxCursor == "" {
		r.st.InboxCursor, r.st.OutboxCursor = "0", "0"
	}
	r.in = newInbox(r.dir)
	store := &e2eStore{st: r.st, in: r.in, groups: r.groups}
	r.out = newOutbox(r.tg, 173418650, store, func() error { return saveState(r.statePath, r.st) }, nil, r.render)
	r.out.now = func() time.Time { return r.clock }
	r.out.log = r.t.Logf
}

// restart — бот убит и поднят заново. Всё, что не легло в state.json, теряется:
// именно это свойство и проверяют сценарии про лежачего бота.
func (r *e2eRig) restart() {
	r.t.Helper()
	if r.st.dirty {
		if err := saveState(r.statePath, r.st); err != nil {
			r.t.Fatal(err)
		}
	}
	r.boot()
}

// render — адаптер форматтера. Тип groupView принадлежит отправителю, Deploy —
// форматтеру, и связать их обязан тот, кто собирает бота; десять строк ниже —
// вся стыковка целиком.
func (r *e2eRig) render(v groupView) string {
	ds := make([]Deploy, 0, len(v.Targets))
	for _, t := range v.Targets {
		ds = append(ds, Deploy{
			Kind: t.Kind, App: t.App,
			Version: t.Version, Previous: t.Previous,
			Stage: t.Stage, Reason: t.Reason,
			CommitURL: t.CommitURL, RunURL: t.RunURL,
			// Список изменений у прогона один и тот же у всех целей: он
			// считается по истории репозитория, а не по цели (§6 контракта).
			Changelog: v.Changelog,
			At:        t.At,
		})
	}
	return formatDeployGroup(r.project, ds)
}

// tick — один такт бота: прочитать журнал, сложить в очередь, отправить.
func (r *e2eRig) tick() error {
	r.t.Helper()
	evs := r.in.Poll(r.st, r.clock)
	items := make([]outboxItem, 0, len(evs))
	for _, e := range evs {
		items = append(items, outboxItem{
			File: e.File, ID: e.ID, Group: e.Group, GroupSeq: e.GroupSeq,
			Kind: e.Kind, App: e.App, Source: e.Source, At: e.At,
			Version: e.Version, Previous: e.Previous, Changelog: e.Changelog,
			CommitURL: e.CommitURL, RunURL: e.RunURL,
			Stage: e.Stage, Reason: e.Reason,
		})
	}
	r.out.Enqueue(items...)
	err := r.out.Flush(context.Background())
	if r.st.dirty {
		if serr := saveState(r.statePath, r.st); serr != nil {
			r.t.Fatal(serr)
		}
		r.st.dirty = false
	}
	return err
}

// put кладёт в журнал событие ровно так, как это делает писатель: имя по
// шаблону §2, поля по §4.
func (r *e2eRig) put(ms int64, app, kind, group string, seq int, tweak func(map[string]any)) string {
	r.t.Helper()
	e := map[string]any{
		"v":        1,
		"id":       e2eID(ms, app, kind, group),
		"kind":     kind,
		"app":      app,
		"at":       time.UnixMilli(ms).UTC().Format(time.RFC3339),
		"source":   "ci",
		"group":    group,
		"groupSeq": seq,
	}
	switch kind {
	case evSuccess, evPublished:
		e["version"] = "release-20260805-130115-1a2b3c4"
	case evRollback:
		e["version"] = "release-20260804-221407-9f8e7d6"
	case evFailure:
		e["stage"] = "gates"
	case evRolledBack:
		e["version"] = "release-20260805-130115-1a2b3c4"
		e["stage"] = "health"
		e["reason"] = "health_failed"
	}
	if tweak != nil {
		tweak(e)
	}
	return inboxWrite(r.t, r.dir, ms, app, kind, e)
}

// e2eID — id по правилу §5: 64 hex от прообраза. Внутрь никто не заглядывает,
// важно только то, что у разных событий он разный, а у повторной доставки —
// один и тот же.
func e2eID(ms int64, app, kind, group string) string {
	h := sha256.Sum256([]byte(fmt.Sprintf("%d|%s|%s|%s", ms, group, app, kind)))
	return hex.EncodeToString(h[:])
}

func (r *e2eRig) sent() []string          { return r.tg.sent() }
func (r *e2eRig) edits() []fakeEdit       { return r.tg.edited() }
func (r *e2eRig) lastSent() string        { s := r.tg.sent(); return s[len(s)-1] }
func (r *e2eRig) lastEdit() string        { e := r.tg.edited(); return e[len(e)-1].text }
func (r *e2eRig) advance(d time.Duration) { r.clock = r.clock.Add(d) }

func (r *e2eRig) wantCounts(what string, sends, edits int) {
	r.t.Helper()
	if got := len(r.sent()); got != sends {
		r.t.Errorf("%s: сообщений %d, ожидали %d\n%s", what, got, sends, strings.Join(r.sent(), "\n---\n"))
	}
	if got := len(r.edits()); got != edits {
		r.t.Errorf("%s: правок %d, ожидали %d", what, got, edits)
	}
}

func wantContains(t *testing.T, what, text string, subs ...string) {
	t.Helper()
	for _, s := range subs {
		if !strings.Contains(text, s) {
			t.Errorf("%s: в сообщении нет «%s»\n%s", what, s, text)
		}
	}
}

// ----------------------------------------------------------------- сценарии

// Три выкатки за минуту → три сообщения (план §9).
//
// Наблюдение за версиями давало на них ОДНО сообщение: разница снимков видит
// только последнюю версию, две предыдущие не существовали ни для чата, ни для
// истории. Три выкатки — это три прогона, то есть три разные группы; события
// одного прогона сворачиваются в одно сообщение и проверяются отдельно.
func TestE2EThreeDeploysInOneSecond(t *testing.T) {
	r := newE2E(t)
	for i, g := range []string{e2eRunA, e2eRunB, e2eRunC} {
		ver := fmt.Sprintf("release-20260805-13011%d-1a2b3c4", i)
		r.put(e2eStart+int64(i), "snakes", evSuccess, g, 1, func(e map[string]any) {
			e["version"] = ver
		})
	}
	if err := r.tick(); err != nil {
		t.Fatalf("такт не прошёл: %v", err)
	}

	r.wantCounts("три выкатки в одну секунду", 3, 0)
	for i, msg := range r.sent() {
		wantContains(t, "сообщение о выкатке", msg, fmt.Sprintf("release-20260805-13011%d-1a2b3c4", i))
	}
}

// Выкатка и откат → два сообщения (план §9).
//
// Сегодня их ноль: версия ушла и вернулась, разницы между соседними снимками
// нет, и в чате не сказано ни про выкатку, ни про откат.
func TestE2EDeployThenRollback(t *testing.T) {
	r := newE2E(t)
	r.put(e2eStart, "metro", evSuccess, e2eRunA, 1, nil)
	r.put(e2eStart+1000, "metro", evRollback, e2eRunB, 1, nil)
	if err := r.tick(); err != nil {
		t.Fatalf("такт не прошёл: %v", err)
	}

	r.wantCounts("выкатка и откат", 2, 0)
	wantContains(t, "выкатка", r.sent()[0], "release-20260805-130115-1a2b3c4")
	wantContains(t, "откат", r.sent()[1], "откачен руками", "release-20260804-221407-9f8e7d6")
}

// Провал на гейтах → сообщение со стадией (план §9).
//
// Сегодня это молчание: release.sh не запускался, версия на проде прежняя,
// сравнивать нечего. Стадия обязана приехать РАСШИФРОВАННОЙ: в чат уезжает
// значение из карты форматтера, а не поле события (§7 контракта).
func TestE2EFailureNamesStage(t *testing.T) {
	r := newE2E(t)
	r.put(e2eStart, "chillhub-site", evFailure, e2eRunA, 1, func(e map[string]any) {
		e["runURL"] = "https://github.com/tr0llex/chillhub/actions/runs/16542331744/attempts/2"
	})
	if err := r.tick(); err != nil {
		t.Fatalf("такт не прошёл: %v", err)
	}

	r.wantCounts("провал на гейтах", 1, 0)
	wantContains(t, "провал", r.lastSent(),
		"не выкачен", "остановились на стадии: "+deployStages["gates"], "прогон")
	if strings.Contains(r.lastSent(), ">gates<") {
		t.Error("в чат уехало сырое значение stage вместо расшифровки")
	}
}

// Автооткат → сообщение с причиной (план §9).
//
// Самый невидимый сегодня случай: release.sh откатывается сам, прод остаётся на
// прежней версии, и о том, что выкатка была и не удержалась, не узнаёт никто.
func TestE2EAutoRollbackNamesReason(t *testing.T) {
	r := newE2E(t)
	r.put(e2eStart, "metro", evRolledBack, e2eRunA, 1, nil)
	if err := r.tick(); err != nil {
		t.Fatalf("такт не прошёл: %v", err)
	}

	r.wantCounts("автооткат", 1, 0)
	wantContains(t, "автооткат", r.lastSent(),
		"откачен автоматически", "причина: "+deployReasons["health_failed"])
}

// Лежачий бот → сообщения приходят после старта (план §9).
//
// Ради этого журнал и лежит на диске: события копятся, пока бота нет, и
// доезжают целиком, когда он поднялся.
//
// Прогоны здесь РАЗНЫЕ намеренно. Записи о прогонах (§10 контракта, ключ
// groups) в State сегодня нет, и перезапуск посреди одного прогона потеряет
// message_id: продолжение уедет новым сообщением вместо правки. Это дефект
// связывания, а не отправителя, и место для него в watch.go — см. отчёт.
func TestE2EBotStartedAfterEventsPileUp(t *testing.T) {
	r := newE2E(t)
	// Бот успел поработать: один такт по пустому журналу ставит курсор и
	// сохраняет состояние.
	if err := r.tick(); err != nil {
		t.Fatalf("такт не прошёл: %v", err)
	}
	r.restart()

	// Пока бота нет, три прогона объявили о себе.
	r.put(e2eStart+1000, "snakes", evSuccess, e2eRunA, 1, nil)
	r.put(e2eStart+2000, "metro", evFailure, e2eRunB, 1, nil)
	r.put(e2eStart+3000, "chillhub-site", evRolledBack, e2eRunC, 1, nil)

	if err := r.tick(); err != nil {
		t.Fatalf("такт после старта не прошёл: %v", err)
	}
	r.wantCounts("бот поднят на непустом журнале", 3, 0)
}

// Отказ Telegram → событие не потеряно, приходит после восстановления
// (план §9).
//
// Главное свойство отправителя: курсор двигается только после подтверждения.
// Проверяется вместе с приёмником, потому что потерять событие можно и на их
// стыке — приёмник уже сдвинул свой курсор, а отправитель ещё нет.
func TestE2ETelegramDownThenRecovers(t *testing.T) {
	r := newE2E(t)
	down := errors.New("bot api недоступен")
	r.tg.fail(down, down)

	r.put(e2eStart, "snakes", evSuccess, e2eRunA, 1, nil)
	if err := r.tick(); err == nil {
		t.Fatal("отказ Telegram обязан вернуться ошибкой, иначе событие считается доставленным")
	}
	r.wantCounts("Telegram лежит", 0, 0)
	if r.st.OutboxCursor != "0" {
		t.Errorf("курсор отправки сдвинулся на отказе: %q — событие потеряно", r.st.OutboxCursor)
	}
	if len(r.st.RecentIDs) != 0 {
		t.Error("id недоставленного события попал в recentIds — после восстановления сообщение не придёт")
	}

	// Пауза между попытками растёт; ждём её и лечим Telegram.
	r.tg.fail(nil, nil)
	r.advance(time.Minute)
	if err := r.tick(); err != nil {
		t.Fatalf("после восстановления такт не прошёл: %v", err)
	}
	r.wantCounts("Telegram ожил", 1, 0)
	if r.st.OutboxCursor == "0" {
		t.Error("курсор не сдвинулся после подтверждения — событие уедет в чат второй раз")
	}
}

// Повтор того же id → второго сообщения нет.
//
// Доставка повторяется по построению: три попытки транспорта, повтор прогона
// одной кнопкой, перечитывание журнала после перезапуска. Два файла с РАЗНЫМИ
// именами несут при этом один id, и курсор тут не помогает никак.
func TestE2EDuplicateIDSendsOnce(t *testing.T) {
	r := newE2E(t)
	first := r.put(e2eStart, "snakes", evSuccess, e2eRunA, 1, nil)
	// Повтор транспорта: то же событие, другое имя файла.
	r.put(e2eStart+1, "snakes", evSuccess, e2eRunA, 1, func(e map[string]any) {
		e["id"] = e2eID(e2eStart, "snakes", evSuccess, e2eRunA)
	})
	if err := r.tick(); err != nil {
		t.Fatalf("такт не прошёл: %v", err)
	}
	r.wantCounts("повтор доставки", 1, 0)

	// Тот же id после перезапуска — тоже одно сообщение: recentIds живёт на
	// диске ровно столько же, сколько файл события.
	r.restart()
	r.put(e2eStart+2, "snakes", evSuccess, e2eRunA, 1, func(e map[string]any) {
		e["id"] = e2eID(e2eStart, "snakes", evSuccess, e2eRunA)
	})
	if err := r.tick(); err != nil {
		t.Fatalf("такт после перезапуска не прошёл: %v", err)
	}
	r.wantCounts("повтор после перезапуска", 1, 0)
	if first == "" {
		t.Fatal("имя первого файла пустое")
	}
}

// Шесть целей одного прогона → ОДНО сообщение и пять правок (§6 контракта).
//
// Так катится chillhub: один пуш, шесть целей, один и тот же список изменений.
// Шесть сообщений с одинаковым блоком «Изменения» — это способ перестать
// читать чат, и именно так лента релизов умирает.
func TestE2ESixTargetsOfOneRunGiveOneMessage(t *testing.T) {
	r := newE2E(t)
	changelog := []any{
		"Не выдавать недоставленное уведомление за успех",
		"Считать пропавший файл ошибкой обновления",
	}
	targets := []struct {
		app  string
		kind string
	}{
		{"chillhub-site", evSuccess},
		{"chillhub-launcher", evSuccess},
		{"chillhub-admin", evRolledBack},
		{"chillhub-api", evFailure},
		{"chillhub-bot", evSuccess},
		{"chillhub-downloads", evPublished},
	}
	for i, tg := range targets {
		r.put(e2eStart+int64(i), tg.app, tg.kind, e2eRunA, i+1, func(e map[string]any) {
			e["changelog"] = changelog
		})
	}
	if err := r.tick(); err != nil {
		t.Fatalf("такт не прошёл: %v", err)
	}

	// Одно сообщение на прогон и пять правок — а не шесть сообщений.
	r.wantCounts("прогон chillhub", 1, 5)

	final := r.lastEdit()
	for _, tg := range targets {
		wantContains(t, "карточка прогона", final, tg.app)
	}
	wantContains(t, "карточка прогона", final,
		"выкачен", "не выкачен — остановились на стадии: "+deployStages["gates"],
		"откачен автоматически — причина: "+deployReasons["health_failed"], "опубликован")

	// Список изменений печатается ОДИН раз: он общий для прогона.
	if n := strings.Count(final, "<b>Изменения</b>"); n != 1 {
		t.Errorf("блок «Изменения» напечатан %d раз, ожидали 1\n%s", n, final)
	}
	for _, item := range changelog {
		if n := strings.Count(final, item.(string)); n != 1 {
			t.Errorf("пункт изменений напечатан %d раз, ожидали 1: %s", n, item)
		}
	}
	// Заголовок прогона — имя проекта, а не первой попавшейся цели.
	wantContains(t, "шапка прогона", final, "<b>"+r.project+"</b>")
}

// Одиночная цель → РОВНО сегодняшняя форма сообщения о релизе (§12 контракта).
//
// Большинство хозяйства катится по одной цели за прогон. Ленту релизов читают
// годами, и переписать её форму заодно с транспортом значило бы сломать
// единственное, что в этой работе и так работало. Ожидание строится через
// formatEvent — то есть через тот самый код, который печатает релизы сегодня.
func TestE2EОдинаковаяКарточкаНезависимоОтЧислаЦелей(t *testing.T) {
	// Тест назывался «одиночная цель сохраняет сегодняшнюю форму релиза» и
	// сторожил ровно то, от чего пришлось отказаться: две разные вёрстки на
	// один и тот же факт, выбор между которыми делало число целей в прогоне.
	// В ленте это и читалось как отсутствие единого дизайна.
	//
	// Теперь проверяется обратное: прогон из одной цели и прогон из двух дают
	// сообщения одного строения — та же шапка, те же строки целей, тот же
	// хвост.
	r := newE2E(t)
	r.put(e2eStart, "snakes", evSuccess, e2eRunA, 1, func(e map[string]any) {
		e["previous"] = "release-20260804-221407-9f8e7d6"
		e["changelog"] = []any{"Починить обрыв скачивания больших файлов #21"}
	})
	if err := r.tick(); err != nil {
		t.Fatalf("такт не прошёл: %v", err)
	}
	r.wantCounts("одиночная цель", 1, 0)

	one := r.lastSent()
	wantContains(t, "одиночная цель", one,
		"выкачен",
		"release-20260805-130115-1a2b3c4",
		"была <code>release-20260804-221407-9f8e7d6</code>",
		"<b>Изменения</b>")
	if strings.Contains(one, "обновлён") {
		t.Errorf("осталась прежняя одиночная форма:\n%s", one)
	}

	// Вторая цель того же прогона: строение сообщения не меняется, к нему лишь
	// добавляется строка.
	r.put(e2eStart+1, "metro", evSuccess, e2eRunA, 2, nil)
	if err := r.tick(); err != nil {
		t.Fatalf("такт не прошёл: %v", err)
	}
	two := r.lastEdit()
	wantContains(t, "две цели", two, "выкачен", "<b>Изменения</b>")
	if strings.Count(two, "выкачен") < 2 {
		t.Errorf("вторая цель не попала в ту же карточку:\n%s", two)
	}
}
func TestE2EFailedEditFallsBackToNewMessage(t *testing.T) {
	r := newE2E(t)
	r.put(e2eStart, "chillhub-site", evSuccess, e2eRunA, 1, nil)
	if err := r.tick(); err != nil {
		t.Fatalf("первый такт не прошёл: %v", err)
	}
	r.wantCounts("первая цель прогона", 1, 0)

	r.tg.fail(nil, errors.New("message to edit not found"))
	r.put(e2eStart+1000, "chillhub-api", evSuccess, e2eRunA, 2, nil)
	if err := r.tick(); err != nil {
		t.Fatalf("второй такт не прошёл: %v", err)
	}

	if got := len(r.sent()); got != 2 {
		t.Fatalf("после неудачной правки сообщений %d, ожидали 2 — уведомление потеряно", got)
	}
	// Новое сообщение несёт состояние ВСЕГО прогона, а не только последней цели.
	wantContains(t, "перепосланная карточка", r.lastSent(), "chillhub-site", "chillhub-api")
}

// Событие прогона, пришедшее много позже → новое сообщение (§6).
//
// Править сообщение недельной давности бессмысленно: его давно пролистали, и
// правка не поднимет его в чате. Telegram старые сообщения править и не даст.
func TestE2ELateGroupEventStartsNewMessage(t *testing.T) {
	r := newE2E(t)
	r.put(e2eStart, "chillhub-site", evSuccess, e2eRunA, 1, nil)
	if err := r.tick(); err != nil {
		t.Fatalf("первый такт не прошёл: %v", err)
	}

	// Семь часов при сроке жизни группы в шесть.
	r.advance(outboxGroupTTL + time.Hour)
	r.put(e2eStart+int64((outboxGroupTTL+time.Hour)/time.Millisecond),
		"chillhub-api", evSuccess, e2eRunA, 2, nil)
	if err := r.tick(); err != nil {
		t.Fatalf("второй такт не прошёл: %v", err)
	}

	r.wantCounts("просроченная группа", 2, 0)
	// Запись о группе заводится заново — без исходов, о которых говорилось
	// сутки назад.
	if strings.Contains(r.lastSent(), "chillhub-site") {
		t.Error("новая карточка тянет цели просроченного прогона")
	}
}

// Журнал, произведённый настоящим lib/notify.sh, разбирается ботом.
//
// Каталог кладёт ci/events-e2e.sh из deploy-kit и передаёт сюда переменной: две
// половины конвейера живут в разных репозиториях, и это единственное место, где
// они встречаются по-настоящему — с файлами, которые написал писатель, а не
// тест. Без переменной случай пропускается вслух: молчаливый пропуск выглядел
// бы как зелёная проверка, которой не было.
func TestE2EReadsJournalFromNotifySh(t *testing.T) {
	dir := os.Getenv("DK_E2E_EVENTS_DIR")
	if dir == "" {
		t.Skip("нет DK_E2E_EVENTS_DIR: журнал от lib/notify.sh кладёт deploy-kit/ci/events-e2e.sh")
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("каталог %s не прочитан: %v", dir, err)
	}
	want := 0
	for _, e := range ents {
		if eventFileRe.MatchString(e.Name()) {
			want++
		}
	}
	if want == 0 {
		t.Fatalf("в %s нет ни одного события по шаблону §2 контракта", dir)
	}

	r := newE2E(t)
	r.dir = dir
	r.boot()
	if err := r.tick(); err != nil {
		t.Fatalf("такт по настоящему журналу не прошёл: %v", err)
	}
	// Служебные started в чат не идут, поэтому сообщений не больше, чем
	// событий, но хотя бы одно быть обязано: иначе писатель и читатель
	// разъехались молча — ровно то, ради чего этот случай и написан.
	if len(r.sent()) == 0 {
		t.Fatalf("бот не сказал ни слова о %d событиях из настоящего журнала", want)
	}
	t.Logf("событий в журнале %d, сообщений %d, правок %d", want, len(r.sent()), len(r.edits()))
}
