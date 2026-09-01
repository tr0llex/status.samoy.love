// Отправка событий выкатки в Telegram с гарантией доставки.
//
// # Что здесь гарантируется
//
// Событие рождается там, где происходит (docs/events.md в deploy-kit), и
// потерять его нельзя: тишина в чате читается как «не катились», а это ровно
// тот дефект, ради которого весь переход с наблюдения на события и затевался.
// Отсюда два правила, вокруг которых собран весь файл:
//
//  1. КУРСОР ДВИГАЕТСЯ ТОЛЬКО ПОСЛЕ ПОДТВЕРЖДЕНИЯ TELEGRAM. Отказ оставляет
//     событие первым в очереди и ничего не меняет в состоянии. Именно это
//     свойство даёт «Telegram лежал — сообщение придёт позже»: очередь стоит,
//     порядок сохраняется, ничего не пропадает.
//  2. ПОКАЗАТЬ ДВАЖДЫ — ХУЖЕ, ЧЕМ НЕ ПОКАЗАТЬ ВОВРЕМЯ. Доставка повторяется по
//     построению (три попытки транспорта, повтор прогона в CI, перечитывание
//     журнала после перезапуска бота), поэтому перед отправкой сверяемся с
//     recentIds: два файла с разными именами вполне могут нести один id, и
//     курсор от этого не спасает никак.
//
// # Почему очередь, а не отправка прямо из приёмника
//
// Приёмник читает каталог раз в секунду и обязан продвигаться по нему быстрее,
// чем Telegram отвечает. Разделив чтение и отправку, получаем два независимых
// темпа и одно место, где живёт вся работа с отказами: пауза между попытками,
// откат к новому сообщению вместо правки, дедупликация.
//
// # Что отправитель НЕ решает
//
// Форму сообщения (её задаёт форматтер, см. поле render) и то, какие файлы
// вообще попадают в очередь (это дело приёмника). Здесь — только доставка.
package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	// outboxGroupTTL — сколько бот помнит сообщение прогона (§6 контракта).
	// Десятикратный запас к самому долгому прогону хозяйства. Событие группы,
	// пришедшее позже, уходит НОВЫМ сообщением: править то, что давно
	// пролистали, бессмысленно, а Telegram старые сообщения править и не даст.
	outboxGroupTTL = 6 * time.Hour
	// outboxGroupsMax — сколько записей о прогонах держим в состоянии.
	// Сотня прогонов за шесть часов — уже аномалия, а не рабочий день.
	outboxGroupsMax = 100
	// outboxTargetsMax — целей в одном сообщении прогона. Больше в одном
	// прогоне не выкатывает ни один репозиторий хозяйства; событие сверх
	// предела уходит новым сообщением, а не теряется.
	outboxTargetsMax = 20
	// outboxRecentTTL — сколько помним id уже показанного. Ровно столько же
	// живёт файл события в журнале: пока файл может быть перечитан, его id
	// обязан помниться, иначе перезапуск бота повторит в чат весь каталог.
	outboxRecentTTL = 14 * 24 * time.Hour

	// Пауза между попытками растёт вдвое: Telegram отвечает отказом и на
	// секундной аварии, и на многочасовой, а долбиться в него каждые две
	// секунды сутки подряд — это лишний способ получить временный бан.
	outboxRetryBase = 2 * time.Second
	outboxRetryMax  = 5 * time.Minute
)

// outboxItem — разобранное событие выкатки, готовое к отправке.
//
// Это НАМЕРЕННО собственный тип отправителя, а не структура приёмника: у них
// разные задачи и разный срок жизни полей. Приёмник проверяет файл и отвергает
// мусор (§9 контракта), отправитель имеет дело только с тем, что проверку уже
// прошло, и знать про имена файлов ему нужно ровно одно — File как ключ
// курсора. Связывает их вызывающий; так стороны можно писать и менять по
// отдельности.
type outboxItem struct {
	// File — имя файла в журнале. Курсор — это имя, а не время: имя
	// лексикографически упорядочено (§2 контракта), и разбор мусорного файла не
	// имеет права влиять на продвижение по журналу.
	File string
	ID   string

	Group    string
	GroupSeq int

	Kind   string // started|success|failure|rolled_back|rollback|published
	App    string
	Source string // ci|local
	At     time.Time

	Version   string
	Previous  string
	Changelog []string
	CommitURL string
	RunURL    string
	Stage     string
	Reason    string
}

// groupTarget — исход одной цели внутри прогона.
type groupTarget struct {
	App       string    `json:"app"`
	Kind      string    `json:"kind"`
	Version   string    `json:"version,omitempty"`
	Previous  string    `json:"previous,omitempty"`
	Stage     string    `json:"stage,omitempty"`
	Reason    string    `json:"reason,omitempty"`
	CommitURL string    `json:"commitURL,omitempty"`
	RunURL    string    `json:"runURL,omitempty"`
	At        time.Time `json:"at"`
}

// groupRecord — сообщение прогона в чате и всё, что нужно, чтобы перерисовать
// его целиком.
//
// Исходы целей и общий для прогона список изменений держатся ЗДЕСЬ, а не в
// памяти процесса: перезапуск бота посреди прогона chillhub превратил бы
// правку в затирание пяти уже объявленных строк одной. Сообщение правится
// целиком — значит, и знать о нём надо всё.
type groupRecord struct {
	MessageID int64         `json:"messageID"`
	At        time.Time     `json:"at"`
	Targets   []groupTarget `json:"targets,omitempty"`
	Changelog []string      `json:"changelog,omitempty"`
}

func (g *groupRecord) clone() *groupRecord {
	out := &groupRecord{MessageID: g.MessageID, At: g.At}
	out.Targets = append(out.Targets, g.Targets...)
	out.Changelog = append(out.Changelog, g.Changelog...)
	return out
}

// outboxStore — состояние, которым распоряжается отправитель (§10 контракта):
// курсор, недавние id, записи о прогонах.
//
// Интерфейсом, а не структурой, по одной причине: тот же курсор и то же
// множество id нужны приёмнику, и жить им положено в ОДНОМ state.json. Где
// именно они лежат — в своей структуре или полями общего State — решает
// связывание; отправителю важно только то, что перечислено ниже. Иначе одни и
// те же ключи (`outboxCursor`, `recentIds`) сериализовались бы из двух мест, и
// какое из них победит, зависело бы от порядка полей.
type outboxStore interface {
	// Cursor — имя последнего файла, по которому сообщение ПОДТВЕРЖДЕНО
	// Telegram. Всегда не больше inboxCursor приёмника: очередь живёт в
	// памяти, и убитый бот теряет всё, что принял, но не отправил. Ровно
	// поэтому на старте приёмник начинает с этого значения.
	Cursor() string
	// Advance двигает курсор вперёд и только вперёд.
	Advance(file string)
	// Seen — этот id уже показывали.
	Seen(id string) bool
	Remember(id string, at time.Time)
	// Group — сообщение прогона; nil означает незнакомый прогон.
	Group(key string) *groupRecord
	SetGroup(key string, rec *groupRecord)
	// Compact чистит обе карты по срокам и потолкам.
	Compact(now time.Time)
}

// outboxState — самостоятельная реализация outboxStore. Имена ключей ровно те,
// что заданы контрактом: структуру можно вложить в state.json как есть.
type outboxState struct {
	OutboxCursor string `json:"outboxCursor,omitempty"`
	// RecentIDs — id уже показанного и когда он был показан.
	RecentIDs map[string]time.Time `json:"recentIds,omitempty"`
	// Groups — прогон → его сообщение в чате.
	Groups map[string]*groupRecord `json:"groups,omitempty"`
}

func newOutboxState() *outboxState {
	return &outboxState{
		RecentIDs: map[string]time.Time{},
		Groups:    map[string]*groupRecord{},
	}
}

func (s *outboxState) Cursor() string { return s.OutboxCursor }

func (s *outboxState) Advance(file string) {
	if file > s.OutboxCursor {
		s.OutboxCursor = file
	}
}

func (s *outboxState) Seen(id string) bool {
	_, ok := s.RecentIDs[id]
	return ok
}

func (s *outboxState) Remember(id string, at time.Time) {
	if s.RecentIDs == nil {
		s.RecentIDs = map[string]time.Time{}
	}
	s.RecentIDs[id] = at
}

func (s *outboxState) Group(key string) *groupRecord { return s.Groups[key] }

func (s *outboxState) SetGroup(key string, rec *groupRecord) {
	if s.Groups == nil {
		s.Groups = map[string]*groupRecord{}
	}
	s.Groups[key] = rec
}

// Compact чистит обе карты. Делается при каждой записи состояния: записей у
// бота и так достаточно, отдельный таймер тут лишний.
//
// Сроки у groups и recentIds РАЗНЫЕ, и это намеренно. recentIds отвечает на
// вопрос «не показывали ли мы это уже» и обязан жить столько же, сколько файл
// события в журнале (14 суток), иначе перезапуск повторит в чат весь каталог.
// groups отвечает на вопрос «есть ли ещё смысл править то сообщение», и ответ
// на него перестаёт быть «да» через несколько часов.
func (s *outboxState) Compact(now time.Time) {
	for id, at := range s.RecentIDs {
		if now.Sub(at) > outboxRecentTTL {
			delete(s.RecentIDs, id)
		}
	}
	for g, rec := range s.Groups {
		if rec == nil || now.Sub(rec.At) > outboxGroupTTL {
			delete(s.Groups, g)
		}
	}
	if len(s.Groups) <= outboxGroupsMax {
		return
	}
	keys := make([]string, 0, len(s.Groups))
	for g := range s.Groups {
		keys = append(keys, g)
	}
	sort.Slice(keys, func(i, j int) bool {
		return s.Groups[keys[i]].At.Before(s.Groups[keys[j]].At)
	})
	for _, g := range keys[:len(s.Groups)-outboxGroupsMax] {
		delete(s.Groups, g)
	}
}

// groupView — то, из чего форматтер собирает сообщение прогона.
type groupView struct {
	// App первого события группы, Version — первая непустая версия прогона:
	// шапка сообщения. Заголовок цели и её адрес форматтер берёт из реестра
	// summary.json, в событии их нет намеренно (§4 контракта).
	App       string
	Version   string
	Source    string
	Changelog []string
	Targets   []groupTarget
}

// outboxTelegram — то немногое, что отправителю нужно от Telegram.
//
// Интерфейс здесь ради тестов: поддельный Telegram, который отказывает по
// команде, — единственный способ проверить главное свойство файла (курсор не
// двигается на отказе), не выходя в сеть.
type outboxTelegram interface {
	SendGroup(ctx context.Context, chatID int64, text string, kb *Keyboard) (int64, error)
	EditLong(ctx context.Context, chatID, messageID int64, text string, kb *Keyboard) error
}

// SendGroup — отправка с возвратом message_id.
//
// Send и SendLong его не возвращают, и до группировки это было незачем: бот
// отправлял и забывал. Правка сообщения прогона требует id, поэтому здесь тот
// же путь, что у SendLong (раскладка по частям, клавиатура на последней), но с
// разбором результата. Запоминается id ПЕРВОЙ части: именно она правится
// дальше, продолжения дописываются отдельными сообщениями.
func (t *Telegram) SendGroup(ctx context.Context, chatID int64, text string, kb *Keyboard) (int64, error) {
	parts := splitMessage(text, telegramTextLimit)
	if len(parts) == 0 {
		return 0, nil
	}
	var first int64
	for i, p := range parts {
		payload := map[string]any{
			"chat_id":                  chatID,
			"text":                     p,
			"parse_mode":               "HTML",
			"disable_web_page_preview": true,
		}
		if i == len(parts)-1 && kb != nil {
			payload["reply_markup"] = kb
		}
		var msg Message
		if err := t.call(ctx, "sendMessage", payload, &msg); err != nil {
			if i == 0 {
				return 0, err
			}
			// Первая часть уже в чате, и её id — это то, что позволит
			// дорисовать сообщение правкой. Отдаём его вместе с ошибкой:
			// потерять id значило бы на следующем событии прогона завести
			// вторую карточку рядом с уже отправленной.
			return first, fmt.Errorf("часть %d из %d: %w", i+1, len(parts), err)
		}
		if i == 0 {
			first = msg.MessageID
		}
	}
	return first, nil
}

// outbox — очередь событий с гарантией доставки.
type outbox struct {
	tg      outboxTelegram
	chatID  int64
	metrics *botMetrics

	// store и save разделены намеренно: отправитель распоряжается своей частью
	// состояния, но не знает, как оно ложится на диск. Запись делает тот, кто
	// держит весь state.json целиком.
	store outboxStore
	save  func() error

	// render и keyboard подменяемы: форма сообщения — дело форматтера, а не
	// доставки. Умолчание ниже даёт работоспособный вид, чтобы отправитель был
	// самодостаточен и проверяем до связывания.
	render   func(groupView) string
	keyboard func(groupView) *Keyboard

	now func() time.Time
	log func(format string, args ...any)

	mu      sync.Mutex
	queue   []outboxItem
	fails   int
	retryAt time.Time
}

// render обязателен: форма сообщения — дело форматтера (см. пакетный
// комментарий), а не запасного текста внутри отправителя. Прежде здесь стоял
// renderGroupFallback как умолчание «на случай, если забыли подключить» — и
// именно оно печатало сырые stage/reason в чат в обход карт
// deployStages/deployReasons (§7 контракта), потому что подстановка
// настоящего форматтера в main.go шла ПОСЛЕ newOutbox, а не через него.
// Обязательный параметр убирает саму возможность забыть.
func newOutbox(tg outboxTelegram, chatID int64, store outboxStore, save func() error, m *botMetrics, render func(groupView) string) *outbox {
	if store == nil {
		store = newOutboxState()
	}
	if save == nil {
		save = func() error { return nil }
	}
	if render == nil {
		panic("newOutbox: render обязателен, форма сообщения не может быть запасной")
	}
	return &outbox{
		tg:      tg,
		chatID:  chatID,
		metrics: m,
		store:   store,
		save:    save,
		render:  render,
		now:     func() time.Time { return time.Now().UTC() },
		log:     func(string, ...any) {},
	}
}

// Cursor — имя последнего подтверждённого Telegram файла. Приёмник начинает с
// него после перезапуска.
func (o *outbox) Cursor() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.store.Cursor()
}

// Pending — сколько событий принято, но ещё не отправлено.
func (o *outbox) Pending() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.queue)
}

// Enqueue кладёт разобранные события в очередь.
//
// Порядок сохраняется: очередь — FIFO, а лента релизов, в которой выкатка
// опередила свой же старт, врёт о том, что происходило.
func (o *outbox) Enqueue(items ...outboxItem) {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, it := range items {
		o.queue = append(o.queue, it)
		o.metrics.eventAccepted()
	}
	o.syncPendingLocked()
}

// Flush отправляет всё, что успевает, и останавливается на первой неудаче.
//
// ОСТАНОВКА — ЭТО ФУНКЦИЯ, А НЕ ОГРАНИЧЕНИЕ. Перескочить упавшее событие и
// отправить следующее значило бы поменять порядок ленты и, что хуже,
// потребовать второго курсора для дырки в середине. Событие остаётся первым в
// очереди, курсор стоит, следующий вызов начнёт с него же.
func (o *outbox) Flush(ctx context.Context) error {
	for {
		o.mu.Lock()
		now := o.now()
		if now.Before(o.retryAt) {
			o.mu.Unlock()
			return nil
		}
		if len(o.queue) == 0 {
			o.syncPendingLocked()
			o.mu.Unlock()
			return nil
		}
		// Голову только СМОТРИМ. Убрать её из очереди можно лишь после того,
		// как Telegram подтвердил приём; Enqueue дописывает в хвост, поэтому
		// пока идёт отправка, голова остаётся головой.
		it := o.queue[0]
		o.mu.Unlock()

		if skip, why := o.skippable(it); skip {
			o.commit(it, nil, why)
			continue
		}

		rec, err := o.deliver(ctx, it)
		if err != nil {
			o.metrics.sendFailed()
			o.mu.Lock()
			o.fails++
			o.retryAt = o.now().Add(backoff(o.fails))
			fails, until := o.fails, o.retryAt
			o.mu.Unlock()
			o.log("событие выкатки не отправлено (%s %s), попытка %d, пауза до %s: %v",
				it.Kind, it.App, fails, until.Format(time.RFC3339), err)
			return err
		}
		o.commit(it, rec, "")
		o.metrics.eventSent()
	}
}

// skippable — событие, которое отправлять не надо, но курсор по нему сдвинуть
// обязательно: иначе журнал встанет на первом же дубле.
func (o *outbox) skippable(it outboxItem) (bool, string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if it.ID != "" && o.store.Seen(it.ID) {
		return true, "дубль"
	}
	// Файл не больше курсора — он уже пройден. На честном входе такого не
	// бывает, но приёмник и отправитель переживают перезапуск порознь, и
	// повторная выдача уже отправленного не должна приводить ко второму
	// сообщению.
	if it.File != "" && it.File <= o.store.Cursor() {
		return true, "пройден курсором"
	}
	// started сам по себе в чат не идёт (§4, §12 контракта): он служебный,
	// нужен детектору аномалий и метрике времени выкатки. Но если карточка
	// прогона уже висит, строка «выкатывается…» в ней полезна — этот случай
	// разбирает deliver, сюда он не попадает.
	if it.Kind == "started" && !o.groupAliveLocked(it.Group) {
		return true, "служебный started"
	}
	return false, ""
}

func (o *outbox) groupAliveLocked(group string) bool {
	rec := o.store.Group(group)
	return rec != nil && o.now().Sub(rec.At) < outboxGroupTTL && len(rec.Targets) < outboxTargetsMax
}

// deliver отправляет одно событие и возвращает новую запись о его прогоне.
//
// Состояние здесь НЕ меняется: работаем на копии записи и отдаём её наверх,
// чтобы вся мутация происходила в одном месте и только после успеха.
func (o *outbox) deliver(ctx context.Context, it outboxItem) (*groupRecord, error) {
	o.mu.Lock()
	var rec *groupRecord
	if o.groupAliveLocked(it.Group) {
		rec = o.store.Group(it.Group).clone()
	}
	o.mu.Unlock()

	if rec == nil {
		// Незнакомый (или просроченный, или переполненный) прогон — новое
		// сообщение НЕМЕДЛЕННО. Ждать конца прогона нельзя: он идёт минутами,
		// а у desktop-artifact — десятками минут, и всё это время в чате было
		// бы пусто. Хуже того, «конца прогона» у писателя не существует:
		// упавший на середине не сообщает о себе ничего.
		next := &groupRecord{At: o.now(), Targets: []groupTarget{targetOf(it)}, Changelog: it.Changelog}
		view := groupViewOf(it, next)
		id, err := o.tg.SendGroup(ctx, o.chatID, o.render(view), o.keyboardFor(view))
		if err != nil {
			return nil, err
		}
		next.MessageID = id
		return next, nil
	}

	next := rec.clone()
	next.upsert(targetOf(it))
	if len(next.Changelog) == 0 {
		// Список изменений у прогона ОДИН: он считается по истории
		// репозитория, а не по цели. Печатается один раз и берётся у первого
		// события, которое его принесло.
		next.Changelog = it.Changelog
	}
	view := groupViewOf(it, next)
	text := o.render(view)
	err := o.editGroup(ctx, next.MessageID, text, view)
	if err == nil {
		return next, nil
	}
	// Правка не удалась окончательно: карточку прогона удалили руками,
	// сообщение слишком старое, Telegram лежит дольше наших повторов. Молчать
	// тут недопустимо — цель выкачена, а в чате об этом не сказано. Шлём новым
	// сообщением и перепривязываем message_id: лишнее сообщение переживём,
	// потерянное уведомление — нет.
	o.log("правка сообщения прогона не удалась, шлю новым: %v", err)
	next.At = o.now()
	id, sendErr := o.tg.SendGroup(ctx, o.chatID, text, o.keyboardFor(view))
	if sendErr != nil {
		return nil, sendErr
	}
	next.MessageID = id
	return next, nil
}

func (o *outbox) keyboardFor(v groupView) *Keyboard {
	if o.keyboard == nil {
		return nil
	}
	return o.keyboard(v)
}

// commit снимает событие с очереди и двигает курсор — только это и означает
// «доставлено». rec == nil означает пропуск (дубль или служебное событие):
// сообщения не было, но журнал обязан продвинуться.
func (o *outbox) commit(it outboxItem, rec *groupRecord, skipped string) {
	o.mu.Lock()
	if len(o.queue) > 0 {
		o.queue = o.queue[1:]
	}
	if rec != nil {
		o.store.SetGroup(it.Group, rec)
	}
	o.store.Advance(it.File)
	if it.ID != "" {
		// recentIds пополняется ВМЕСТЕ со сдвигом курсора и в той же записи
		// состояния. Пометив id при приёме, мы получили бы потерю сообщения
		// при падении между приёмом и отправкой; пометив после — редкий дубль
		// при падении ровно между ответом Telegram и записью состояния.
		// Транзакции с чужим сервисом не бывает, и выбран тот отказ, который
		// громкий.
		o.store.Remember(it.ID, o.now())
	}
	o.fails, o.retryAt = 0, time.Time{}
	o.store.Compact(o.now())
	o.syncPendingLocked()
	o.mu.Unlock()

	if skipped != "" {
		o.metrics.eventDropped(skipped)
		o.log("событие выкатки пропущено (%s): %s %s", skipped, it.Kind, it.App)
	}
	// Память — источник правды, диск только догоняет: отправленное уже в чате,
	// и неудачная запись состояния не повод повторить сообщение.
	if err := o.save(); err != nil {
		o.log("состояние отправителя не сохранено: %v", err)
	}
}

// syncPendingLocked обновляет возраст самого старого неотправленного события.
// Считается от момента САМОГО события, а не от момента приёма: «принято, но не
// отправлено пять минут» должно срабатывать и на событии, пролежавшем в
// журнале, пока бот был мёртв.
func (o *outbox) syncPendingLocked() {
	if len(o.queue) == 0 {
		o.metrics.pendingSince(time.Time{})
		return
	}
	oldest := o.queue[0].At
	for _, it := range o.queue[1:] {
		if !it.At.IsZero() && (oldest.IsZero() || it.At.Before(oldest)) {
			oldest = it.At
		}
	}
	o.metrics.pendingSince(oldest)
}

func backoff(fails int) time.Duration {
	if fails < 1 {
		return 0
	}
	d := outboxRetryBase
	for i := 1; i < fails && d < outboxRetryMax; i++ {
		d *= 2
	}
	if d > outboxRetryMax {
		return outboxRetryMax
	}
	return d
}

func targetOf(it outboxItem) groupTarget {
	return groupTarget{
		App: it.App, Kind: it.Kind, Version: it.Version, Previous: it.Previous,
		Stage: it.Stage, Reason: it.Reason,
		CommitURL: it.CommitURL, RunURL: it.RunURL, At: it.At,
	}
}

// upsert заменяет строку цели на месте, а не дописывает.
//
// Порядок строк — это порядок, в котором цели ЗАЯВИЛИ о себе: started цели
// ставит её в список, success той же цели меняет исход в той же строке.
// Дописыванием мы получили бы «бот выкатывается…» и «бот выкачен» рядом.
func (g *groupRecord) upsert(t groupTarget) {
	for i := range g.Targets {
		if g.Targets[i].App == t.App {
			g.Targets[i] = mergeTarget(g.Targets[i], t)
			return
		}
	}
	g.Targets = append(g.Targets, t)
}

// mergeTarget склеивает два события ОДНОЙ цели одного прогона.
//
// СКЛЕИВАТЬ НАДО ЗДЕСЬ, А НЕ У ФОРМАТТЕРА. Строка цели живёт в состоянии, и
// пока здесь стояло `g.Targets[i] = t`, до форматтера доезжало только
// последнее событие — остальное стиралось до того, как его кто-либо увидел.
//
// А событий об одной выкатке приезжает два, и они дополняют друг друга:
// пайплайн знает адрес коммита, список изменений и адрес прогона; release.sh
// на сервере знает то, чего пайплайн знать не может, — на какой релиз
// показывал симлинк до переключения. Порядок между ними не гарантирован:
// сервер обычно на секунду-две раньше.
//
// Живой пример из журнала (метро, 01.09):
//
//	04:55:45  success  src=local  previous=release-20260829-055631-ae6e43d
//	04:55:48  success  src=ci     previous=—
//
// Второе стирало первое, и в чат уходил релиз без строки «была …» и без
// ссылки на сравнение — той самой, которой подкреплена строка «изменений в
// этом релизе нет». Утверждение оставалось, а способ его проверить пропадал.
//
// Правило: новое событие главнее в том, что описывает ИСХОД, а поля-факты
// подхватываются у прежнего, если новое их не принесло. Затирать непустое
// пустым нельзя ни в одном поле.
func mergeTarget(prev, next groupTarget) groupTarget {
	// ЗАПОЗДАВШИЙ started НЕ СТИРАЕТ УЖЕ ОБЪЯВЛЕННЫЙ ИСХОД. События могут
	// доехать не по порядку (контракт, §6), и «выкачен» не имеет права
	// откатиться в «выкатывается…». У форматтера такая проверка есть, но сюда
	// она не успевала: строка к тому моменту была уже перезаписана.
	if next.Kind == evStarted && prev.Kind != "" && prev.Kind != evStarted {
		out := prev
		out.Version = firstNonEmptyStr(prev.Version, next.Version)
		out.Previous = firstNonEmptyStr(prev.Previous, next.Previous)
		out.CommitURL = firstNonEmptyStr(prev.CommitURL, next.CommitURL)
		out.RunURL = firstNonEmptyStr(prev.RunURL, next.RunURL)
		return out
	}
	out := next
	out.Version = firstNonEmptyStr(next.Version, prev.Version)
	out.Previous = firstNonEmptyStr(next.Previous, prev.Previous)
	out.CommitURL = firstNonEmptyStr(next.CommitURL, prev.CommitURL)
	out.RunURL = firstNonEmptyStr(next.RunURL, prev.RunURL)
	if out.At.IsZero() {
		out.At = prev.At
	}
	return out
}

func groupViewOf(it outboxItem, rec *groupRecord) groupView {
	v := groupView{App: it.App, Source: it.Source, Changelog: rec.Changelog, Targets: rec.Targets}
	if len(rec.Targets) > 0 {
		v.App = rec.Targets[0].App
	}
	for _, t := range rec.Targets {
		if t.Version != "" {
			v.Version = t.Version
			break
		}
	}
	return v
}

// editRetries — паузы между попытками правки карточки прогона. Значение живёт
// переменной, а не константой, чтобы тест не ждал наяву.
var editRetries = []time.Duration{2 * time.Second, 5 * time.Second}

// editGroup правит карточку прогона, повторяя ВРЕМЕННЫЕ отказы Telegram.
//
// ЗАЧЕМ ПОВТОРЫ. Не сумев поправить сообщение, отправитель шлёт новое — иначе
// выкатка осталась бы необъявленной. Пока временный отказ и постоянный
// обрабатывались одинаково, одна секунда недоступности Telegram превращалась в
// ДУБЛЬ в ленте. Так и вышло 31.08:
//
//	01:12:38 правка не удалась, шлю новым: editMessageText: telegram отказал: Bad Gateway
//	01:13:46 правка не удалась, шлю новым: editMessageText: context deadline exceeded
//
// В чате это дало два одинаковых сообщения о релизе 1.6.37 с разницей в две
// минуты. Ни одной новой правды во втором не было.
//
// Таймаут клиента опаснее прочего: ответ не дошёл, но правка на стороне
// Telegram могла и пройти — тогда «шлю новым» даёт дубль гарантированно.
// Поэтому повтор, а не немедленный запасной путь.
//
// ПОСТОЯННЫЙ ОТКАЗ НЕ ПОВТОРЯЕТСЯ: карточку удалили или её нельзя править —
// во второй раз ответ будет тот же, а пауза задержит рассказ о выкатке.
func (o *outbox) editGroup(ctx context.Context, messageID int64, text string, view groupView) error {
	var err error
	for attempt := 0; ; attempt++ {
		err = o.tg.EditLong(ctx, o.chatID, messageID, text, o.keyboardFor(view))
		if err == nil || editIsPermanent(err) {
			return err
		}
		if attempt >= len(editRetries) {
			return err
		}
		o.log("правка сообщения прогона не удалась (%v) — повтор через %s", err, editRetries[attempt])
		select {
		case <-ctx.Done():
			return err
		case <-time.After(editRetries[attempt]):
		}
	}
}

// editIsPermanent — отказ, который не изменится от повтора.
//
// Разбор по тексту описания, потому что другого признака у Telegram нет: код
// ответа один и тот же (200 с ok: false), а машиночитаемого кода ошибки в
// ответе нет вовсе. Список узкий НАМЕРЕННО: всё незнакомое считается временным
// и повторяется. Ошибиться в эту сторону дёшево — три лишние попытки; ошибиться
// в другую значит вернуть дубль, ради устранения которого всё и написано.
func editIsPermanent(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	for _, sig := range []string{
		"message to edit not found",
		"message can't be edited",
		"message_id_invalid",
		"chat not found",
		"bot was blocked",
		// «не изменилось» — это успех, переодетый в отказ: текст карточки уже
		// такой, каким мы хотим его видеть. Повторять нечего, и слать новое
		// сообщение тем более: оно было бы копией уже висящего.
		"message is not modified",
	} {
		if strings.Contains(s, sig) {
			return true
		}
	}
	return false
}
