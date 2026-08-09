// Счётчики бота в формате Prometheus.
//
// # Зачем боту метрики
//
// Молчащий бот неотличим от работающего до первой аварии — ровно та же
// причина, по которой у выкатки есть -selftest. Счётчики отвечают на вопросы,
// которые иначе выясняются в самый неудачный момент: доходят ли уведомления,
// не отвечает ли Telegram отказами, шлём ли мы напоминания об одном и том же
// простое чаще, чем договаривались.
//
// # Почему файл, а не эндпоинт
//
// Бот принципиально никуда не слушает: он сам ходит в Telegram длинным
// опросом (см. .deploy-kit/bot.env — HEALTH у цели нет и не будет). Открывать
// ради метрик первый в его жизни порт значит завести новую поверхность там,
// где её сознательно не было. Файл для textfile-коллектора node_exporter даёт
// то же самое и ничего не открывает.
//
// Счётчики живут в памяти и обнуляются при перезапуске — для counter это
// штатно, rate() распознаёт сброс. Отдельная метка времени heartbeat нужна
// потому, что файл переживает сам процесс: без неё остановленный бот выглядел
// бы вечно живым с последними значениями.
package main

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// defaultBotMetricsPath — каталог textfile-коллектора node_exporter.
const defaultBotMetricsPath = "/var/lib/node_exporter/textfile/samoylove-bot.prom"

// botMetrics — счётчики одного процесса.
//
// Все методы безопасны на nil-приёмнике: бот должен работать и там, где
// выгрузка выключена (локальный запуск, хост без node_exporter), не обрастая
// проверками на каждой строке вызова.
type botMetrics struct {
	path string

	mu             sync.Mutex
	notifications  map[string]float64
	commands       map[string]float64
	unknownCmds    float64
	sendFailures   float64
	pollFailures   float64
	lastNotifiedAt int64
	startedAt      int64

	// События выкатки. Три счётчика вместо одного, потому что расходятся они
	// по разным авариям: «принято, но не отправлено» — лежачий Telegram,
	// «отброшено» без роста «принято» — заклинивший на дубле журнал.
	eventsAccepted float64
	eventsSent     float64
	eventsDropped  map[string]float64
	// oldestPendingAt — момент самого старого неотправленного события, 0 —
	// очередь пуста. Хранится МОМЕНТ, а не возраст: возраст растёт сам по
	// себе, и посчитанный при записи в очередь он врал бы ровно тогда, когда
	// нужен, — пока Telegram лежит и никто ничего не пишет.
	oldestPendingAt int64
}

func newBotMetrics(path string, now time.Time) *botMetrics {
	if path == "" {
		return nil
	}
	return &botMetrics{
		path:          path,
		notifications: map[string]float64{},
		commands:      map[string]float64{},
		eventsDropped: map[string]float64{},
		startedAt:     now.Unix(),
	}
}

// eventAccepted — событие выкатки принято в очередь отправки.
func (m *botMetrics) eventAccepted() {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.eventsAccepted++
	m.mu.Unlock()
}

// eventSent — сообщение о событии подтверждено Telegram, курсор сдвинут.
func (m *botMetrics) eventSent() {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.eventsSent++
	m.mu.Unlock()
}

// eventDropped — событие снято с очереди без сообщения. Причина меткой: дубль
// по id — это штатная работа транспорта, а «служебный started» — вообще не
// повод беспокоиться, и складывать их в одно число значит потерять и то, и
// другое.
func (m *botMetrics) eventDropped(reason string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.eventsDropped[reason]++
	m.mu.Unlock()
}

// pendingSince — момент самого старого неотправленного события. Нулевое время
// означает пустую очередь.
func (m *botMetrics) pendingSince(at time.Time) {
	if m == nil {
		return
	}
	m.mu.Lock()
	if at.IsZero() {
		m.oldestPendingAt = 0
	} else {
		m.oldestPendingAt = at.Unix()
	}
	m.mu.Unlock()
}

// notified — уведомление ушло владельцу. kind повторяет вид события
// (down/still/up/release), а не текст: текст пересказывать метрике незачем.
func (m *botMetrics) notified(kind string, now time.Time) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.notifications[kind]++
	m.lastNotifiedAt = now.Unix()
	m.mu.Unlock()
}

// sendFailed — Telegram не принял сообщение. Считается отдельно от отправок:
// «уведомлений ноль» и «уведомления не уходят» — разные аварии.
func (m *botMetrics) sendFailed() {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.sendFailures++
	m.mu.Unlock()
}

// command — владелец что-то спросил у бота.
func (m *botMetrics) command(name string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.commands[name]++
	m.mu.Unlock()
}

// unknownCommand — владелец отправил слово, начинающееся с «/», которое не
// разрешилось ни в одну команду. Отдельный счётчик, а не общий с command():
// «команда пришла, но бот её не понял» — другая авария, чем «команды не
// доходят вовсе», и слитые в одно число они неразличимы на графике.
func (m *botMetrics) unknownCommand() {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.unknownCmds++
	m.mu.Unlock()
}

// pollFailed — не удался длинный опрос Telegram.
func (m *botMetrics) pollFailed() {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.pollFailures++
	m.mu.Unlock()
}

// render собирает документ экспозиции. Значения одного семейства идут подряд
// и после своих # HELP/# TYPE: иначе парсер вправе отбросить весь файл.
func (m *botMetrics) render(now time.Time) string {
	if m == nil {
		return ""
	}
	m.mu.Lock()
	notifications := copyCounts(m.notifications)
	commands := copyCounts(m.commands)
	eventsDropped := copyCounts(m.eventsDropped)
	unknownCmds := m.unknownCmds
	sendFailures, pollFailures := m.sendFailures, m.pollFailures
	lastNotifiedAt, startedAt := m.lastNotifiedAt, m.startedAt
	eventsAccepted, eventsSent := m.eventsAccepted, m.eventsSent
	oldestPendingAt := m.oldestPendingAt
	m.mu.Unlock()

	// Возраст считается на месте, от момента записи файла: очередь, стоящая
	// из-за лежачего Telegram, не порождает ни одного вызова, а показывать
	// именно её и нужно.
	pendingAge := float64(0)
	if oldestPendingAt != 0 {
		pendingAge = float64(now.Unix() - oldestPendingAt)
	}

	var b strings.Builder
	writeFamily(&b, "statusbot_notifications_total",
		"Отправленные уведомления по виду события", "counter", "kind", notifications)
	writeFamily(&b, "statusbot_commands_total",
		"Обработанные команды владельца", "counter", "command", commands)
	writeSingle(&b, "statusbot_unknown_commands_total",
		"Команды, которые бот не распознал", "counter", unknownCmds)

	writeFamily(&b, "statusbot_deploy_events_dropped_total",
		"События выкатки, снятые с очереди без сообщения", "counter", "reason", eventsDropped)

	writeSingle(&b, "statusbot_deploy_events_accepted_total",
		"События выкатки, принятые в очередь отправки", "counter", eventsAccepted)
	writeSingle(&b, "statusbot_deploy_events_sent_total",
		"События выкатки, подтверждённые Telegram", "counter", eventsSent)
	writeSingle(&b, "statusbot_deploy_events_pending_age_seconds",
		"Возраст самого старого неотправленного события (0 — очередь пуста)", "gauge", pendingAge)

	writeSingle(&b, "statusbot_send_failures_total",
		"Сообщения, которые Telegram не принял", "counter", sendFailures)
	writeSingle(&b, "statusbot_poll_failures_total",
		"Неудачные опросы Telegram", "counter", pollFailures)
	writeSingle(&b, "statusbot_last_notification_timestamp_seconds",
		"Когда ушло последнее уведомление (0 — ни одного с запуска)", "gauge", float64(lastNotifiedAt))
	writeSingle(&b, "statusbot_start_timestamp_seconds",
		"Когда процесс запустился", "gauge", float64(startedAt))
	writeSingle(&b, "statusbot_heartbeat_timestamp_seconds",
		"Когда бот последний раз обновлял этот файл", "gauge", float64(now.Unix()))
	return b.String()
}

func copyCounts(src map[string]float64) map[string]float64 {
	out := make(map[string]float64, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out
}

func writeFamily(b *strings.Builder, name, help, typ, labelName string, values map[string]float64) {
	fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s %s\n", name, escapeHelp(help), name, typ)
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	// Сортировка не косметика: файл переписывается каждые полминуты, и без
	// неё его невозможно ни сравнить глазами, ни проверить тестом.
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(b, "%s{%s=\"%s\"} %s\n", name, labelName, escapeLabel(k), formatFloat(values[k]))
	}
}

func writeSingle(b *strings.Builder, name, help, typ string, v float64) {
	fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s %s\n%s %s\n",
		name, escapeHelp(help), name, typ, name, formatFloat(v))
}

func escapeHelp(s string) string {
	return strings.NewReplacer(`\`, `\\`, "\n", `\n`).Replace(s)
}

func escapeLabel(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(s)
}

func formatFloat(v float64) string {
	if v == math.Trunc(v) && math.Abs(v) < 1e15 {
		return strconv.FormatInt(int64(v), 10)
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// flush пишет файл атомарно: коллектор читает каталог по своему расписанию и
// вполне может попасть в середину записи, а половина документа — это
// отброшенный файл и дырка в графике.
func (m *botMetrics) flush(now time.Time) error {
	if m == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(m.path), 0o755); err != nil {
		return err
	}
	tmp := m.path + ".tmp"
	if err := os.WriteFile(tmp, []byte(m.render(now)), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, m.path)
}
