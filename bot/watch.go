package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type Kind string

const (
	KindDown      Kind = "down"
	KindStillDown Kind = "still"
	KindUp        Kind = "up"
	// KindRelease рождается не здесь: Apply версии больше не сравнивает, вид
	// остаётся ради формы сообщения о выкатке, которую собирает форматтер по
	// пришедшему событию (format.go, formatDeploy).
	KindRelease Kind = "release"
)

// Event — то, о чём стоит написать владельцу.
type Event struct {
	Key  string
	Kind Kind
	// Title и URL идут вместе: уведомление без ссылки заставляет искать адрес
	// руками ровно тогда, когда некогда.
	Title string
	URL   string
	// Project — id проекта, к которому относится событие. Нужен кнопке под
	// уведомлением: она ведёт сразу в упавший проект, а не на общий экран,
	// где о нём ещё надо вспомнить.
	Project  string
	Reason   string
	Duration time.Duration
	Version  string
	Previous string
	// CommitURL — адрес коммита этой версии. Пусто — версия в сообщении
	// остаётся обычным текстом; см. formatEvent.
	CommitURL string
	// Changelog — что изменилось в этой версии, по строке на пункт. Заполнен
	// только у релизов и только если список приехал в событии выкатки: сам бот
	// его составить не может, git-истории выкаченных проектов на сервере нет.
	// Пустой список означает прежнее сообщение о релизе, а не отсутствие
	// уведомления.
	Changelog []string
	At        time.Time
	// AdminURL — прямая ссылка на действие, которое ещё нужно сделать руками
	// после релиза (сегодня — переключить канал самообновления лаунчера в
	// админке). Пусто у всех обычных релизов: сборка сама решает, что
	// работает, и звать владельца никуда не нужно. Заполняется в formatDeploy
	// по таблице deployAdminLinks, не приезжает из события — источник
	// доверенный, поэтому не проходит через allowedURL().
	AdminURL string
}

// Item — что бот уже знает про одну наблюдаемую сущность.
//
// Notified хранится отдельно от Since: Since отвечает на вопрос «сколько
// лежит», Notified — «когда я последний раз про это писал». Слить их в одно
// поле нельзя, иначе напоминание о длительном простое либо не придёт, либо
// начнёт врать о длительности.
type Item struct {
	Down     bool   `json:"down"`
	Since    string `json:"since"`
	Notified string `json:"notified"`
}

// State переживает перезапуск бота: иначе рестарт службы (или выкатка новой
// версии) означал бы повторное уведомление обо всём, что лежит, и потерю
// места в журнале выкаток — весь непрочитанный журнал уехал бы в чат заново.
type State struct {
	// Offset — с какого update_id продолжать длинный опрос Telegram.
	Offset int64 `json:"offset"`
	// MutedUntil — до какого момента молчать о падениях.
	//
	// Ночью, когда сервис лежит и чинится, напоминания раз в 15 минут ничего
	// не добавляют, но мешают. Тишина ограничена по времени намеренно:
	// «выключить навсегда» превращает бота в неработающий, о чём вспоминают
	// в следующую аварию.
	MutedUntil string           `json:"mutedUntil,omitempty"`
	Items      map[string]*Item `json:"items"`

	// InboxCursor — имя последнего файла журнала выкаток, принятого в очередь.
	// OutboxCursor — имя последнего файла, по которому сообщение подтверждено
	// Telegram; он всегда не больше InboxCursor.
	//
	// Курсора два, потому что очередь отправки живёт в памяти. Убитый бот
	// теряет всё, что принял, но не отправил, поэтому на старте приём
	// продолжается с ПОДТВЕРЖДЁННОГО места, и неотправленное перечитывается с
	// диска (inbox.go, prime). Ровно за этим журнал и лежит на диске: он
	// переживает и лежачего бота, и лежачий Telegram.
	//
	// Имя файла, а не время: имя события устроено так, что
	// лексикографический порядок совпадает с хронологическим, и один проход по
	// отсортированному списку отвечает сразу и «что нового», и «в каком
	// порядке». Разбирать при этом ничего не нужно — испорченный файл не
	// должен влиять на продвижение по журналу.
	InboxCursor  string `json:"inboxCursor,omitempty"`
	OutboxCursor string `json:"outboxCursor,omitempty"`

	// RecentIDs — id недавних событий выкатки со временем приёма.
	//
	// Живёт столько же, сколько сам файл события (14 суток): пока файл может
	// быть перечитан, его id обязан помниться, иначе после перезапуска бот
	// повторит в чат всё, что лежит в каталоге. Курсор эту задачу не решает —
	// транспорт повторяет доставку, и два файла с РАЗНЫМИ именами могут нести
	// один id.
	RecentIDs map[string]string `json:"recentIds,omitempty"`

	// dirty — Apply изменил состояние, а на диск оно ещё не легло.
	//
	// Событие и изменение состояния — не одно и то же: первое наблюдение живой
	// цели запоминается молча. Пока цикл наблюдения писал файл только при
	// событии, такие записи не переживали перезапуск, и бот заново проходил
	// путь «вижу впервые» по всему, что и так знал.
	//
	// Поле не сериализуется намеренно: только что прочитанное с диска
	// состояние по определению чистое.
	dirty bool
}

func newState() *State {
	return &State{Items: map[string]*Item{}}
}

func loadState(path string) *State {
	st := newState()
	b, err := os.ReadFile(path)
	if err != nil {
		return st
	}
	if json.Unmarshal(b, st) != nil {
		return newState()
	}
	if st.Items == nil {
		st.Items = map[string]*Item{}
	}
	// Ключ "versions" в старых файлах состояния молча игнорируется: снимок
	// версий больше ни на что не влияет, а падать из-за него — значит потерять
	// вместе с ним и курсоры журнала, и историю падений.
	return st
}

// saveState пишет через временный файл: бот может быть убит в любой момент,
// а недочитанный state.json означал бы шквал повторных уведомлений.
func saveState(path string, st *State) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// target — одна наблюдаемая сущность в плоском виде: проверка, юнит или
// свежесть самих данных. Приводим их к общему виду, чтобы логика дедупликации
// была одна на всех и не расползалась по трём похожим веткам.
type target struct {
	key     string
	title   string
	url     string
	project string
	reason  string
	down    bool
	since   time.Time
}

func targets(s *Summary, now time.Time, stale time.Duration) []target {
	var out []target
	for _, p := range s.Projects {
		for _, c := range p.Checks {
			since, ok := parseTime(c.Since)
			if !ok {
				since = now
			}
			// Падением считаем ТОЛЬКО "down". Появившееся состояние "slow"
			// означает, что сервис отвечает, просто дольше порога, — будить
			// этим владельца и открывать инцидент нельзя. Иначе любая
			// сетевая просадка ночью читалась бы как авария.
			title := p.Title + " · " + c.Name
			if !c.Critical {
				title += " (второстепенная)"
			}
			out = append(out, target{
				key:     "check:" + c.ID,
				title:   title,
				url:     firstNonEmptyStr(c.URL, p.URL),
				project: p.ID,
				reason:  firstNonEmptyStr(c.Impact, c.Error),
				down:    c.Status == "down",
				since:   since,
			})
		}
		for _, u := range p.Units {
			since, ok := parseTime(u.Since)
			if !ok {
				since = now
			}
			out = append(out, target{
				key:     "unit:" + u.Name,
				title:   p.Title + " · " + u.Title,
				url:     p.URL,
				project: p.ID,
				reason:  "состояние юнита: " + u.State,
				down:    !u.Active,
				since:   since,
			})
		}
	}

	// Свежесть данных — такая же наблюдаемая сущность.
	//
	// Если агент перестал ходить по сервисам, все проверки в файле остаются
	// зелёными навсегда. Молчащий бот в этот момент выглядит как «всё
	// хорошо», хотя на деле никто уже не смотрит.
	updated, ok := parseTime(s.Updated)
	if !ok {
		updated = now
	}
	age := now.Sub(updated)
	out = append(out, target{
		key:    "data",
		title:  "Данные статуса",
		url:    statusPageURL,
		reason: fmt.Sprintf("агент не обновлял summary.json %s", humanDur(age)),
		down:   age >= stale,
		since:  updated,
	})
	return out
}

// Muted — молчим ли сейчас, и до какого времени.
func (st *State) Muted(now time.Time) (bool, time.Time) {
	until, ok := parseTime(st.MutedUntil)
	return ok && now.Before(until), until
}

// Mute включает тишину. Возвращает момент, до которого она действует.
func (st *State) Mute(now time.Time, d time.Duration) time.Time {
	until := now.Add(d)
	st.MutedUntil = until.UTC().Format(time.RFC3339)
	return until
}

func (st *State) Unmute() { st.MutedUntil = "" }

// Apply сверяет свежий summary.json с тем, что бот уже знает, и возвращает
// события доступности, о которых стоит написать. Состояние меняется прямо
// здесь: событие считается доставленным, как только оно попало в список.
//
// Правила:
//   - первое наблюдение молчит, если всё хорошо, но кричит, если уже лежит:
//     после перезапуска бота владелец должен узнать о текущем сбое, но не
//     получить простыню «все сервисы живы»;
//   - повторное сообщение об одном и том же простое приходит не чаще
//     remind — иначе при часовом падении придёт сотня одинаковых строк.
//
// О выкатках Apply не говорит ничего. Раньше релиз выводился из разницы двух
// соседних снимков summary.json, и такой вывод терял ровно то, ради чего
// уведомление и нужно: три выкатки за минуту схлопывались в одну, выкатка с
// откатом внутри минуты не давала вообще ничего, а провал и автооткат были
// невидимы — версия на проде не менялась, сравнивать было нечего. Теперь о
// выкатке сообщает событие от того, кто её делал (inbox.go/outbox.go), а
// опрос версий остаётся только источником данных для проверки: версия на
// проде, о которой события не было, — аномалия, а не повод для сообщения о
// релизе.
func (st *State) Apply(s *Summary, now time.Time, remind, stale time.Duration) []Event {
	var events []Event

	for _, t := range targets(s, now, stale) {
		prev, seen := st.Items[t.key]
		switch {
		case !seen:
			item := &Item{Down: t.down, Since: t.since.UTC().Format(time.RFC3339)}
			if t.down {
				item.Notified = now.UTC().Format(time.RFC3339)
				events = append(events, Event{
					Key: t.key, Kind: KindDown, Title: t.title, URL: t.url,
					Project: t.project, Reason: t.reason, At: now,
				})
			}
			st.Items[t.key] = item
			st.dirty = true

		case prev.Down != t.down:
			since, ok := parseTime(prev.Since)
			if !ok {
				since = t.since
			}
			kind := KindUp
			if t.down {
				kind = KindDown
			}
			events = append(events, Event{
				Key: t.key, Kind: kind, Title: t.title, URL: t.url,
				Project: t.project, Reason: t.reason,
				Duration: now.Sub(since), At: now,
			})
			prev.Down = t.down
			prev.Since = t.since.UTC().Format(time.RFC3339)
			prev.Notified = now.UTC().Format(time.RFC3339)
			st.dirty = true

		case t.down:
			last, ok := parseTime(prev.Notified)
			if !ok || now.Sub(last) >= remind {
				since, ok := parseTime(prev.Since)
				if !ok {
					since = t.since
				}
				events = append(events, Event{
					Key: t.key, Kind: KindStillDown, Title: t.title, URL: t.url,
					Project: t.project, Reason: t.reason,
					Duration: now.Sub(since), At: now,
				})
				prev.Notified = now.UTC().Format(time.RFC3339)
				st.dirty = true
			}
		}
	}

	return events
}

// firstNonEmptyStr — первое непустое из перечисленного.
//
// В уведомлении полезнее человеческая формулировка последствия («матчи не
// идут»), чем текст ошибки; но если impact в конфиге не заполнен, показать
// нужно хоть что-то.
func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
