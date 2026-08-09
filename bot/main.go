// Телеграм-бот статуса samoy.love: отвечает на команды владельца и сам
// сообщает о падениях, восстановлениях и новых версиях.
//
// Данные не собирает: единственный источник — summary.json, который раз в
// минуту пишет агент (agent/main.go). Второй независимый обход давал бы
// расхождения между страницей и ботом, а решать, кому из них верить,
// пришлось бы владельцу.
//
// Бот живёт на том же хосте, что и сервисы, поэтому про падение самого хоста
// он сообщить не может — это работа внешнего пробера в GitHub Actions
// (scripts/probe.mjs). Зато бот замечает, что данные перестали обновляться.
//
// Токен и chat id читаются из окружения (EnvironmentFile юнита), в
// репозитории их нет и быть не должно.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

// staleAfter — с какого возраста данные агента считаются несвежими.
// Глобальная переменная, потому что порог нужен и логике уведомлений, и
// форматированию ответов; задаётся один раз при старте.
var staleAfter = 5 * time.Minute

// Состояние и его замок — на уровне пакета: к ним обращаются оба цикла и
// обработчики нажатий. Раньше это были локальные переменные main, и добавить
// действие, меняющее состояние, было некуда.
var (
	mu        sync.Mutex
	botState  *State
	statePath string
)

// metrics — счётчики процесса. nil, пока main их не завёл: все методы
// безопасны на nil-приёмнике, поэтому тесты обработчиков ничего не настраивают.
var metrics *botMetrics

func main() {
	// Единственный флаг — действие, а не настройка: всё остальное живёт в
	// окружении, одним местом (см. config.go).
	selftest := flag.Bool("selftest", false,
		"проверить данные, сборку сводки и канал в Telegram и выйти — без сообщения владельцу")
	flag.Parse()

	cfg, err := loadConfig()
	if err != nil {
		log.Fatal(err)
	}
	staleAfter = cfg.Stale
	applyConfig(cfg)

	// Счётчики — пакетная переменная, а не параметр каждой функции: наблюдение
	// не должно просачиваться в подписи обработчиков команд, которые про него
	// ничего знать не обязаны.
	metrics = newBotMetrics(cfg.Metrics, time.Now())
	if err := metrics.flush(time.Now()); err != nil {
		log.Printf("метрики не записаны (%s): %v", cfg.Metrics, err)
	}

	summaryPath := cfg.SummaryPath()
	pollTimeout := 30 * time.Second
	tg := newTelegram(cfg.Token, pollTimeout)

	// Состояние трогают оба цикла и нажатия на кнопки. Файл один — замок один.
	statePath = cfg.State
	botState = loadState(cfg.State)
	st := botState

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Имя бота нужно, чтобы отличать «/status» от «/status@сосед_по_чату» в
	// группе. TELEGRAM_BOT_USERNAME остаётся опциональным override (тесты,
	// узкий случай, когда getMe недоступен); по умолчанию бот спрашивает у
	// Telegram сам, а не полагается на переменную, забытую в файле окружения.
	self := cfg.Self
	if self == "" {
		if name, err := tg.GetMe(ctx); err != nil {
			log.Printf("getMe не удался, TELEGRAM_BOT_USERNAME не задан — бот будет отвечать на команды для ЛЮБОГО бота в группе: %v", err)
		} else {
			self = name
		}
	}

	if err := tg.SetMyCommands(ctx, botCommands()); err != nil {
		log.Printf("список команд Telegram не обновлён: %v", err)
	}

	// Проверка канала после выкатки. Молчащий бот неотличим от работающего,
	// пока что-нибудь не упадёт, — а выяснять это в момент аварии поздно.
	// Проверка молчалива для владельца, но не для выкатки: её код возврата
	// доезжает до release.sh через systemd-run --wait и роняет релиз.
	if *selftest {
		if err := selfTest(ctx, tg, cfg.Owner, summaryPath); err != nil {
			log.Fatalf("проверка не прошла: %v", err)
		}
		log.Print("проверка прошла: данные читаются, сводка строится, канал открыт")
		return
	}

	// Журнал выкаток: приёмник читает каталог, отправитель говорит в чат.
	//
	// Два цикла и два курсора, а не один: очередь отправки живёт в памяти, и
	// убитый бот теряет всё, что принял, но не отправил, — поэтому приём
	// продолжается с ПОДТВЕРЖДЁННОГО места, а не с прочитанного (контракт, §10).
	// Разделение даёт и второе: чтение каталога идёт своим темпом и не ждёт
	// Telegram.
	//
	// Замок тот же самый (mu), потому что state.json один. Порядок захвата
	// всегда «сначала замок отправителя, потом mu»: отсюда правило — методы
	// outbox не вызываются под mu, иначе получим взаимную блокировку с
	// отправителем, который под своим замком лезет в состояние.
	var (
		inbox  *Inbox
		sender *outbox
	)
	if cfg.EventsDir != "" {
		inbox = newInbox(cfg.EventsDir)
		store := &eventStore{st: st, inbox: inbox, groups: loadGroups(cfg.Groups)}
		sender = newOutbox(tg, cfg.Owner, store, func() error {
			mu.Lock()
			defer mu.Unlock()
			if err := saveState(statePath, st); err != nil {
				return err
			}
			st.dirty = false
			return saveGroups(cfg.Groups, store.groups)
		}, metrics, func(v groupView) string { return renderDeployGroup(summaryPath, v) })
		sender.keyboard = func(v groupView) *Keyboard { return deployKeyboardFor(summaryPath, v) }
		sender.log = log.Printf
	} else {
		log.Print("журнал выкаток не читается: EVENTS_DIR выключен")
	}

	var wg sync.WaitGroup
	wg.Add(2)

	// --- цикл уведомлений ---
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(cfg.Watch)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			s, err := loadSummary(summaryPath)
			if err != nil {
				log.Printf("данные агента не прочитаны: %v", err)
				continue
			}
			now := time.Now().UTC()
			mu.Lock()
			events := st.Apply(s, now, cfg.Remind, cfg.Stale)
			// Пока живы оба пути, одна выкатка видна дважды: событием (сразу) и
			// разницей снимков version.json (через минуту-другую). Событие
			// приходит первым и запоминает версию — наблюдение той же версии
			// после этого молчит. Обратный порядок (бот лежал, наблюдение
			// успело первым) даст дубль, и это осознанный размен: снос старого
			// пути идёт отдельной волной, а до неё лишнее сообщение лучше
			// пропавшего.
			events = dropAnnounced(events, now)
			// Пишем по факту изменения, а не по факту события: часть
			// изменений Apply делает молча (см. State.dirty). Флаг снимаем
			// только после удачной записи — иначе одна неудача потеряла бы
			// изменения навсегда.
			if st.dirty {
				if err := saveState(cfg.State, st); err != nil {
					log.Printf("состояние не сохранено: %v", err)
				} else {
					st.dirty = false
				}
			}
			muted, until := st.Muted(now)
			mu.Unlock()

			// Тишина глушит только шум: напоминания о том, что и так уже
			// известно. Само падение, восстановление и новая версия проходят
			// всегда — иначе «тихо до утра» означало бы «не сообщай мне о
			// новых авариях», а просили не этого.
			if muted {
				kept := events[:0]
				for _, e := range events {
					if e.Kind != KindStillDown {
						kept = append(kept, e)
					}
				}
				if len(kept) != len(events) {
					log.Printf("тишина до %s: придержал %d напоминаний",
						until.Format(time.RFC3339), len(events)-len(kept))
				}
				events = kept
			}

			for _, e := range events {
				// SendLong, а не SendWith: список выкаченных коммитов больше не
				// режется, и релиз на сорок тем в одно сообщение не влезает.
				// Частичная неудача внутри такой отправки — это ошибка целиком,
				// иначе владелец увидел бы половину списка и принял её за весь.
				if err := tg.SendLong(ctx, cfg.Owner, formatEvent(e), alertKeyboard(e.Project)); err != nil {
					metrics.sendFailed()
					log.Printf("уведомление не отправлено (%s %s): %v", e.Kind, e.Key, err)
					continue
				}
				metrics.notified(string(e.Kind), time.Now().UTC())
				log.Printf("уведомление: %s %s", e.Kind, e.Key)
			}

			// Файл переписывается на каждом обходе, а не только при событии:
			// по отметке heartbeat видно, что бот жив, даже когда всё спокойно
			// и уведомлять не о чем.
			if err := metrics.flush(time.Now()); err != nil {
				log.Printf("метрики не записаны: %v", err)
			}
		}
	}()

	// --- цикл команд ---
	go func() {
		defer wg.Done()
		for {
			if ctx.Err() != nil {
				return
			}
			mu.Lock()
			offset := st.Offset
			mu.Unlock()

			updates, err := tg.GetUpdates(ctx, offset, pollTimeout)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				// Сеть моргает, Telegram иногда отвечает 502. Пауза нужна,
				// чтобы при затяжном сбое не молотить запросами впустую.
				metrics.pollFailed()
				log.Printf("опрос Telegram не удался: %v", err)
				select {
				case <-ctx.Done():
					return
				case <-time.After(5 * time.Second):
				}
				continue
			}

			for _, u := range updates {
				mu.Lock()
				if u.UpdateID >= st.Offset {
					st.Offset = u.UpdateID + 1
				}
				mu.Unlock()
				handleUpdate(ctx, tg, u, cfg.Owner, cfg.OwnerUser, self, summaryPath)
			}
			if len(updates) > 0 {
				mu.Lock()
				err := saveState(cfg.State, st)
				mu.Unlock()
				if err != nil {
					log.Printf("состояние не сохранено: %v", err)
				}
			}
		}
	}()

	if sender != nil {
		wg.Add(2)

		// --- цикл чтения журнала выкаток ---
		go func() {
			defer wg.Done()
			ticker := time.NewTicker(cfg.Events)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
				}
				now := time.Now().UTC()
				mu.Lock()
				evs := inbox.Poll(st, now)
				for _, e := range evs {
					// Версия помечается объявленной ЗДЕСЬ, при приёме, а не
					// после отправки: наблюдение тикает раз в 30 секунд и
					// вполне может опередить лежачий Telegram, а два сообщения
					// об одном релизе — это ровно то, чего избегаем.
					rememberAnnouncedLocked(e, now)
				}
				if st.dirty {
					if err := saveState(cfg.State, st); err != nil {
						log.Printf("состояние не сохранено: %v", err)
					} else {
						st.dirty = false
					}
				}
				mu.Unlock()

				if len(evs) == 0 {
					continue
				}
				// Enqueue берёт замок отправителя, поэтому вызывается уже без
				// mu: порядок захвата в процессе один и обратный ему запрещён.
				items := make([]outboxItem, 0, len(evs))
				for _, e := range evs {
					items = append(items, outboxItemOf(e))
					log.Printf("событие выкатки: %s %s", logSafe(e.Kind), logSafe(e.App))
				}
				sender.Enqueue(items...)
			}
		}()

		// --- цикл отправки событий выкатки ---
		//
		// Отдельным циклом, потому что у отправки свой темп: отказ Telegram
		// оставляет событие первым в очереди и назначает паузу, а чтение
		// журнала в это время обязано продолжаться — иначе журнал встанет
		// целиком из-за одной недоставленной карточки.
		go func() {
			defer wg.Done()
			ticker := time.NewTicker(cfg.Events)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
				}
				// Ошибка уже в журнале (sender.log) вместе с номером попытки и
				// паузой: второй раз о ней писать нечего.
				_ = sender.Flush(ctx)
			}
		}()
	}

	log.Printf("бот запущен: данные %s, напоминание раз в %s", summaryPath, cfg.Remind)
	wg.Wait()
	log.Print("бот остановлен")
}

// selfTest проверяет, что бот способен сделать свою работу, НЕ беспокоя
// владельца сообщением.
//
// Проверяются те же три звена, что и раньше:
//   - данные агента читаются (loadSummary);
//   - сводка строится (formatStatus вызывается, результат отбрасывается —
//     нужен сам факт, что форматирование не паникует на текущих данных);
//   - канал в Telegram открыт (Ping — sendChatAction, без записи в чат).
//
// Отправки карточки здесь больше нет: выкатка бота случается часто, а
// сообщение «всё работает» в ответ на неё владелец не просил. Подробности —
// в комментарии к Telegram.Ping.
func selfTest(ctx context.Context, tg *Telegram, owner int64, summaryPath string) error {
	s, err := loadSummary(summaryPath)
	if err != nil {
		return err
	}
	if formatStatus(s, time.Now().UTC(), false, time.Time{}) == "" {
		return fmt.Errorf("сводка построилась пустой: данные %s", summaryPath)
	}
	return tg.Ping(ctx, owner)
}

// handleUpdate отвечает на одно сообщение.
//
// Чужие чаты игнорируются молча: любой ответ незнакомцу — это подтверждение,
// что бот жив и слушает, и приглашение продолжать.
func handleUpdate(ctx context.Context, tg *Telegram, u Update, owner, ownerUser int64, self, summaryPath string) {
	// Нажатие на кнопку: перерисовываем тот же экран на месте.
	if q := u.CallbackQuery; q != nil {
		handleCallback(ctx, tg, q, owner, ownerUser, summaryPath)
		return
	}
	if u.Message == nil || u.Message.Chat.ID != owner {
		return
	}
	// Совпадения чата мало: в личной переписке это и есть владелец, а в группе
	// в тот же чат пишут все. Поэтому, когда владелец известен поимённо,
	// сверяем отправителя. Сообщение без From (посты в канале) судим
	// по-прежнему только по чату — иначе бот замолчал бы там, где раньше
	// отвечал.
	if ownerUser > 0 && u.Message.From.ID != 0 && u.Message.From.ID != ownerUser {
		return
	}
	word := parseCommand(u.Message.Text, self)
	if word == "" {
		return
	}
	cmd := resolveCommand(word)
	if cmd == "" {
		// Короткий ответ, а не полная справка (20 строк): незнакомая команда
		// — самый частый случай опечатки, и топить её в справке — не помощь.
		// Логируется и считается отдельно от команд, которые бот выполнил:
		// «команда пришла, но бот её не понял» — другая авария, чем «команды
		// не доходят вовсе».
		metrics.unknownCommand()
		log.Printf("неизвестная команда: /%s", logSafe(word))
		if err := tg.SendWith(ctx, owner, "Не знаю такую команду. /help — список команд.", navKeyboard(ViewHelp)); err != nil {
			log.Printf("ответ не отправлен: %v", err)
		}
		return
	}

	// Команды логируются: без этого не понять, дошло ли сообщение до бота,
	// когда владельцу кажется, что тот молчит. Текст не пишем — в журнале
	// ему делать нечего.
	metrics.command(cmd)
	log.Printf("команда /%s", cmd)

	// /quiet не листает экраны, а меняет (или показывает) тишину — та же
	// развилка, что у кнопок «Тихо 2 ч»/«До утра»/«Снова говорить» под
	// уведомлением об аварии, только доступная ДО аварии: перед плановыми
	// работами или ручной выкаткой глушить бота было нечем, кроме как
	// дождаться первого падения.
	if cmd == CmdQuiet {
		handleQuiet(ctx, tg, owner, commandArg(u.Message.Text))
		return
	}

	// Аргумент есть только у /changelog: «/changelog metro» — про одну цель,
	// «/changelog» — про всё хозяйство. Остальные команды его игнорируют, как
	// и раньше: «/status всё ли живо» — это /status.
	view := viewFor(cmd, commandArg(u.Message.Text))
	text, kb := renderView(view, summaryPath, time.Now().UTC())
	// SendLong: «/changelog» по всему хозяйству — самый длинный ответ бота, и с
	// полными списками изменений он в одно сообщение не помещается.
	if err := tg.SendLong(ctx, owner, text, kb); err != nil {
		metrics.sendFailed()
		log.Printf("ответ на /%s не отправлен: %v", cmd, err)
	}
}

// handleQuiet отвечает на /quiet.
//
// Три формы: «/quiet 2h» (любая длительность, которую разбирает
// time.ParseDuration — «2h», «30m», «8h») задаёт тишину, «/quiet off» снимает
// её раньше срока, голый «/quiet» ничего не меняет и просто показывает,
// молчит ли бот сейчас, — то же самое, что владелец увидел бы строкой на
// /status, но без остального экрана.
func handleQuiet(ctx context.Context, tg *Telegram, owner int64, arg string) {
	arg = strings.ToLower(strings.TrimSpace(arg))
	now := time.Now().UTC()

	var text string
	switch arg {
	case "":
		muted, until := muteState(now)
		if muted {
			text = fmt.Sprintf("🔕 Молчу до %s", fmtTime(until))
		} else {
			text = "🔔 Не молчу"
		}
	case "off":
		text = applyMute(now, 0, true)
	default:
		d, err := time.ParseDuration(arg)
		if err != nil || d <= 0 {
			text = "Не понял длительность. Пример: <code>/quiet 2h</code>, <code>/quiet 8h</code>, <code>/quiet off</code>"
			break
		}
		text = applyMute(now, d, false)
	}

	if err := tg.SendWith(ctx, owner, text, mutedKeyboard()); err != nil {
		metrics.sendFailed()
		log.Printf("ответ на /quiet не отправлен: %v", err)
	}
}

// ------------------------------------------- связывание журнала выкаток

// eventStore склеивает две половины состояния в одну.
//
// Приёмник держит курсор приёма и недавние id прямо в State (watch.go),
// отправитель просит от состояния интерфейс outboxStore. Здесь они сходятся:
// курсор отправки и recentIds — те же самые поля State, и никакой второй их
// копии в процессе нет. Иначе один и тот же ключ state.json писался бы из двух
// мест, и какое победит, зависело бы от порядка записи.
//
// Всё, что здесь есть, живёт под общим замком mu: под ним же работает приёмник,
// а Confirmed трогает его внутреннюю память о неподтверждённых id.
type eventStore struct {
	st    *State
	inbox *Inbox
	// groups — память о сообщениях прогонов. Отдельным файлом, потому что поля
	// Groups в State нет (см. Config.Groups).
	groups map[string]*groupRecord
}

func (s *eventStore) Cursor() string {
	mu.Lock()
	defer mu.Unlock()
	return s.st.OutboxCursor
}

// Advance двигает курсор вперёд и только вперёд: назад его двигает лишь
// приёмник на старте, и то свой собственный.
func (s *eventStore) Advance(file string) {
	mu.Lock()
	defer mu.Unlock()
	if file > s.st.OutboxCursor {
		s.st.OutboxCursor = file
		s.st.dirty = true
	}
}

func (s *eventStore) Seen(id string) bool {
	mu.Lock()
	defer mu.Unlock()
	_, ok := s.st.RecentIDs[id]
	return ok
}

// Remember идёт через Inbox.Confirmed, а не пишет в карту напрямую: приёмник
// держит ещё и список отданных, но не подтверждённых id, и снимать его обязан
// тот же вызов, что кладёт id в долгую память.
func (s *eventStore) Remember(id string, at time.Time) {
	mu.Lock()
	defer mu.Unlock()
	s.inbox.Confirmed(s.st, id, at)
}

func (s *eventStore) Group(key string) *groupRecord {
	mu.Lock()
	defer mu.Unlock()
	return s.groups[key]
}

func (s *eventStore) SetGroup(key string, rec *groupRecord) {
	mu.Lock()
	defer mu.Unlock()
	s.groups[key] = rec
}

// Compact чистит только записи о прогонах: recentIds чистится приёмником и по
// своему, куда более долгому сроку — 14 суток против шести часов. Сроки разные
// намеренно: «не показывали ли мы это уже» и «есть ли ещё смысл править то
// сообщение» — разные вопросы, и ответы на них перестают быть верными в разное
// время.
func (s *eventStore) Compact(now time.Time) {
	mu.Lock()
	defer mu.Unlock()
	for g, rec := range s.groups {
		if rec == nil || now.Sub(rec.At) > outboxGroupTTL {
			delete(s.groups, g)
		}
	}
	if len(s.groups) <= outboxGroupsMax {
		return
	}
	keys := make([]string, 0, len(s.groups))
	for g := range s.groups {
		keys = append(keys, g)
	}
	sort.Slice(keys, func(i, j int) bool { return s.groups[keys[i]].At.Before(s.groups[keys[j]].At) })
	for _, g := range keys[:len(s.groups)-outboxGroupsMax] {
		delete(s.groups, g)
	}
}

// loadGroups читает память о прогонах. Битый или пропавший файл — не авария:
// потеряв её, бот заведёт новые карточки вместо правки старых. Это хуже, чем
// правка, но несравнимо лучше молчания.
func loadGroups(path string) map[string]*groupRecord {
	out := map[string]*groupRecord{}
	b, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	if err := json.Unmarshal(b, &out); err != nil {
		log.Printf("память о прогонах не разобрана (%s): %v", path, err)
		return map[string]*groupRecord{}
	}
	return out
}

// saveGroups пишет через временный файл и rename — по той же причине, что и
// saveState: бота могут убить в любой момент, а недочитанный файл означал бы
// вторую карточку рядом с уже отправленной.
func saveGroups(path string, groups map[string]*groupRecord) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(groups)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// outboxItemOf — переходник между проверенным событием приёмника и очередью
// отправителя. Типы у них разные намеренно (граница «проверено» видна в
// системе типов), и склейка — единственное место, где о них знают оба.
func outboxItemOf(e DeployEvent) outboxItem {
	return outboxItem{
		File: e.File, ID: e.ID,
		Group: e.Group, GroupSeq: e.GroupSeq,
		Kind: e.Kind, App: e.App, Source: e.Source, At: e.At,
		Version: e.Version, Previous: e.Previous, Changelog: e.Changelog,
		CommitURL: e.CommitURL, RunURL: e.RunURL,
		Stage: e.Stage, Reason: e.Reason,
	}
}

// renderDeployGroup рисует сообщение прогона.
//
// Заголовок цели и её адрес берутся из реестра summary.json, а не из события:
// имя проекта в хозяйстве одно, и вторая правда о нём развела бы чат и
// страницу первым же переименованием (контракт, §4). Реестр читается на каждое
// сообщение, а не кэшируется: сообщений о выкатках единицы в день, а вот
// устаревший в памяти реестр после переименования пришлось бы отлаживать.
func renderDeployGroup(summaryPath string, v groupView) string {
	reg := loadRegistry(summaryPath)
	ds := make([]Deploy, 0, len(v.Targets))
	for _, t := range v.Targets {
		d := Deploy{
			Kind: t.Kind, App: t.App,
			Version: t.Version, Previous: t.Previous,
			Stage: t.Stage, Reason: t.Reason,
			CommitURL: t.CommitURL, RunURL: t.RunURL,
			At: t.At,
			// Список изменений у прогона ОДИН, и печатается он один раз; какой
			// именно цели его отдать, решает форматтер, поэтому он есть у всех.
			Changelog: v.Changelog,
		}
		d.Title, d.URL, d.Project = reg.target(t.App)
		ds = append(ds, d)
	}
	return formatDeployGroup(reg.project(ds), ds)
}

// deployKeyboardFor — клавиатура карточки прогона (deployKeyboard,
// keyboard.go) с проектом и адресом прогона, найденными по её целям.
//
// Проект узнаём из реестра summary.json, а не из события: имя проекта в
// хозяйстве одно (контракт, §4). Адрес прогона одинаков у всех целей одного
// прогона (один пуш — один CI-run), поэтому годится первый непустой.
func deployKeyboardFor(summaryPath string, v groupView) *Keyboard {
	reg := loadRegistry(summaryPath)
	var projectID string
	for _, t := range v.Targets {
		if _, _, project := reg.target(t.App); project != "" {
			projectID = project
			break
		}
	}
	var runURL string
	for _, t := range v.Targets {
		if t.RunURL != "" {
			runURL = t.RunURL
			break
		}
	}
	return deployKeyboard(projectID, runURL)
}

// registry — реестр проектов из summary.json: по id цели выкатки даёт
// человеческое имя, адрес и проект.
type registry struct {
	apps     map[string][3]string // app → title, url, project
	projects map[string]string    // project → заголовок
}

func loadRegistry(summaryPath string) registry {
	r := registry{apps: map[string][3]string{}, projects: map[string]string{}}
	s, err := loadSummary(summaryPath)
	if err != nil {
		// Реестра нет — не повод молчать о выкатке: цель покажется своим id и
		// без ссылки. Это видно и чинится, в отличие от пропавшего сообщения.
		return r
	}
	for _, p := range s.Projects {
		// Проверки заводятся первыми, проект — последним и поверх: id цели
		// выкатки чаще совпадает с id проекта, и «Лаунчер» в шапке читается
		// лучше, чем «Лаунчер · Сайт».
		for _, c := range p.Checks {
			r.apps[c.ID] = [3]string{p.Title + " · " + c.Name, firstNonEmptyStr(c.URL, p.URL), p.ID}
		}
		r.apps[p.ID] = [3]string{p.Title, p.URL, p.ID}
		r.projects[p.ID] = p.Title
	}
	return r
}

func (r registry) target(app string) (title, url, project string) {
	v, ok := r.apps[app]
	if !ok {
		return "", "", ""
	}
	return v[0], v[1], v[2]
}

// project — заголовок прогона. Имя проекта, а не первой попавшейся цели: в
// шапке стоит то, что катилось целиком.
func (r registry) project(ds []Deploy) string {
	for _, d := range ds {
		if t := r.projects[d.Project]; t != "" {
			return t
		}
	}
	if len(ds) > 0 {
		return ds[0].App
	}
	return ""
}

// ------------------------------------------- защита от двух голосов на переходе

// announcedTTL — сколько версия считается уже объявленной событием.
//
// Шесть часов: наблюдение тикает раз в 30 секунд, и разрыв между событием и
// первым же обходом измеряется минутами. Запас взят на лежачего агента,
// который мог не переписать summary.json сразу.
const announcedTTL = 6 * time.Hour

// announced — версии, о которых уже сказало событие выкатки. Живёт в памяти
// намеренно: после перезапуска бота старый путь и так молчит — версии в
// State.Versions к тому моменту записаны.
//
// Читается и пишется только под mu.
var announced = map[string]time.Time{}

// rememberAnnouncedLocked помечает версию события объявленной. Вызывается под
// mu.
func rememberAnnouncedLocked(e DeployEvent, now time.Time) {
	if e.Version == "" {
		return
	}
	switch e.Kind {
	case evSuccess, evRollback, evRolledBack, evPublished:
		announced[e.Version] = now
	}
	for v, at := range announced {
		if now.Sub(at) > announcedTTL {
			delete(announced, v)
		}
	}
}

// dropAnnounced выбрасывает сообщение о релизе, о котором уже сказало событие.
// Вызывается под mu.
//
// Выбрасывается ТОЛЬКО KindRelease: падения и восстановления к выкатке
// отношения не имеют и глушить их нельзя ни при каких обстоятельствах.
func dropAnnounced(events []Event, now time.Time) []Event {
	if len(announced) == 0 {
		return events
	}
	kept := events[:0]
	for _, e := range events {
		if e.Kind == KindRelease && e.Version != "" {
			if at, ok := announced[e.Version]; ok && now.Sub(at) <= announcedTTL {
				log.Printf("о версии %s уже сказало событие выкатки: наблюдение молчит", logSafe(e.Version))
				continue
			}
		}
		kept = append(kept, e)
	}
	return kept
}
