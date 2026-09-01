// Агент статус-страницы: обходит сервисы, читает состояние systemd-юнитов и
// даты выкаток, копит историю и складывает готовый JSON для страницы.
//
// Запускается systemd-таймером раз в минуту. Работает на том же хосте, что и
// сами сервисы: это позволяет видеть юниты и версии, но означает, что при
// падении хоста страница будет недоступна — за внешнее наблюдение отвечает
// отдельный пробер в GitHub Actions, который шлёт уведомления в Telegram.
//
// Права root не нужны: systemctl show читается любым пользователем, каталоги
// с релизами доступны на чтение.
package main

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"flag"
	"fmt"
	"html"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	httpTimeout   = 12 * time.Second
	rawWindow     = 7 * 24 * time.Hour
	hourlyKeep    = 90 * 24
	dailyKeep     = 365
	incidentsKeep = 50
	daysOnPage    = 90
	releasesKeep  = 20

	// Пауза перед подтверждающим запросом: достаточно, чтобы разойтись с
	// мгновенной сетевой икотой, и мало, чтобы уложиться в TimeoutStartSec.
	confirmDelay = 2 * time.Second

	// Порог для «крупного сбоя»: доля лежащих критичных проверок, при которой
	// «частичный» перестаёт быть честным словом.
	majorShare = 0.5
)

// ------------------------------------------------------------------ конфиг

// DeployTarget — как называть цель выкатки в сообщении о релизе.
//
// Имя цели в событии выкатки — это её id (chillhub-api, samoylove-bot), и
// показывать его владельцу нельзя: в чате должно стоять то же имя, что на
// странице. Реестр для этого и существует, но собирался он только из проверок
// и проектов, а id цели выкатки совпадает с ними лишь по случайности — у
// метро и змеек совпал, у всех целей лаунчера нет. В ленте это выглядело как
// «🚀 chillhub-api» без ссылки, да ещё и в шапке карточки прогона, названной
// произвольной целью.
//
// Таблица заводится ЯВНОЙ, а не выводится из пути релиза: путь — это
// подробность установки, и завязывать на неё имя в чате значит переименовать
// цель первым же переездом каталога.
type DeployTarget struct {
	// Project — id проекта из projects. По нему цель получает имя проекта в
	// шапке карточки и адрес, если своего у неё нет.
	Project string `json:"project"`
	// Title — короткое имя цели: «Публичный API», «Установщик». Имя проекта
	// приписывается к нему читателем, второй раз писать его здесь не нужно.
	Title string `json:"title"`
	// URL — что открывать по имени цели. Пусто — адрес проекта.
	URL string `json:"url,omitempty"`
}

type Config struct {
	Projects []Project `json:"projects"`
	// Цели выкатки: id из события → как её называть. Ключей может не быть
	// вовсе — тогда цель покажется своим id, как и до появления таблицы.
	DeployTargets map[string]DeployTarget `json:"deployTargets,omitempty"`
}

type Project struct {
	ID       string  `json:"id"`
	Title    string  `json:"title"`
	Subtitle string  `json:"subtitle"`
	URL      string  `json:"url"`
	Accent   string  `json:"accent"`
	Checks   []Check `json:"checks"`
	Units    []Unit  `json:"units"`
	Builds   []Build `json:"builds"`
}

// Check описывает одну проверку.
//
// Признак «работает» здесь намеренно шире кода ответа: сервис, отвечающий 200
// пустым телом, HTML-заглушкой вместо скрипта или за восемь секунд, для
// пользователя не работает, а по одному лишь коду выглядит здоровым.
type Check struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Note string `json:"note"`
	// Impact — что падение значит для пользователя. Показывается в блоке
	// сбоев: именно за этим на статус-страницу и приходят.
	Impact string `json:"impact"`
	URL    string `json:"url"`
	Expect int    `json:"expect"`
	Cert   string `json:"cert"`

	// Critical: падение критичной проверки роняет вердикт проекта. Умолчание —
	// true, поэтому здесь указатель: у обычного bool «не задано» неотличимо от
	// «неважная», и забытое поле молча превращало бы сервис во второстепенный.
	Critical *bool `json:"critical"`

	// SlowMs: медленный ответ — не «упал», но и не «работает». 0 — без порога.
	SlowMs int64 `json:"slowMs"`

	BodyIncludes string `json:"bodyIncludes"`
	ExpectType   string `json:"expectType"`

	// AllowHosts — куда, кроме исходного хоста, разрешено уводить редиректом.
	AllowHosts []string `json:"allowHosts"`

	// Steps — сценарий из нескольких запросов вместо одиночного GET.
	Steps []Step `json:"steps"`
}

// IsCritical: умолчание — критичная. Второстепенность должна быть решением,
// записанным в конфиг, а не следствием незаполненного поля.
func (c Check) IsCritical() bool { return c.Critical == nil || *c.Critical }

// Step — шаг сценарной проверки. Capture кладёт поля JSON-ответа в переменные,
// которые следующий шаг подставляет в URL по имени в фигурных скобках.
type Step struct {
	Name         string            `json:"name"`
	URL          string            `json:"url"`
	Expect       int               `json:"expect"`
	BodyIncludes string            `json:"bodyIncludes"`
	ExpectType   string            `json:"expectType"`
	Capture      map[string]string `json:"capture"`
}

type Unit struct {
	Name  string `json:"name"`
	Title string `json:"title"`
}

// Build описывает, откуда брать дату выкатки и версию.
//   - type "url"     — /version.json, который кладёт общий пайплайн: самый
//     точный источник, потому что отвечает сам работающий сервис, а не диск
//   - type "release" — путь это симлинк на каталог релиза; его имя и есть версия
//   - type "file"    — только время изменения файла, версии нет
//
// "url" появился, когда все проекты переехали на deploy-kit: до этого версия
// была известна лишь там, где релизы разложены каталогами.
type Build struct {
	Title string `json:"title"`
	Type  string `json:"type"`
	Path  string `json:"path"`

	// App — id цели выкатки (APP из .deploy-kit/*.env): snakes, status-agent,
	// chillhub-site. Им подписаны события журнала выкаток, и только по нему
	// событие находит свою строку истории.
	//
	// Поле необязательное, потому что у целей типа "release" его видно из пути:
	// /opt/status-agent/current — это APP=status-agent (см. buildApp). Догадка
	// работает ровно там, где путь задаёт сама выкатка, и не работает у целей
	// типа "url": https://launcher.samoy.love/version.json ничего не говорит о
	// том, что цель называется chillhub-site. Таким и нужно App.
	//
	// Ошибиться в нём безопасно: событие с неизвестным id цели в историю не
	// попадёт и напишет об этом в журнал агента. Молча приписать выкатку чужой
	// цели нельзя — совпадение id проверяется, а не подбирается.
	App string `json:"app"`
}

// ------------------------------------------------------------------ история

type sample [4]int64 // [unix, ok, ms, code]
type bucket [4]any   // [ключ, up, total, avgMs]

type Incident struct {
	Service    string `json:"service"`
	Name       string `json:"name"`
	Start      string `json:"start"`
	End        string `json:"end"`
	Reason     string `json:"reason"`
	DurationMs int64  `json:"durationMs,omitempty"`
}

type CheckState struct {
	Status   string `json:"status"`
	Since    string `json:"since"`
	Ms       int64  `json:"ms"`
	Code     int    `json:"code"`
	CertDays *int   `json:"certDays"`
}

type State struct {
	Updated  string                 `json:"updated"`
	Services map[string]*CheckState `json:"services"`

	// EventsMs — курсор журнала выкаток: миллисекунда последнего разобранного
	// события. Курсор свой, отдельный от ботовского: каталог читают двое, и
	// удалять из него не имеет права никто — чистит писатель.
	//
	// ХРАНИТСЯ МИЛЛИСЕКУНДА, А НЕ ИМЯ ФАЙЛА, хотя имя было бы точнее. Причина в
	// том, где лежит этот файл: state.json пишется в DATA_DIR, который nginx
	// раздаёт наружу как /data/ (nginx/sites/status.samoy.love.conf). Имя
	// события — это «1785924102123-chillhub-api-failure.json», то есть цель и
	// исход её выкатки; ровно затем журнал и вынесен из раздаваемого каталога
	// (контракт, §1), и публиковать оттуда факт провала через курсор было бы
	// смешно. Миллисекунда не рассказывает ни о цели, ни об исходе.
	//
	// Потеря точности безвредна: события той же миллисекунды разбираются
	// повторно, а от повтора защищает id события в записи истории (applyEvent).
	// Ноль означает «курсора нет» — журнал считается прочитанным, см. readEvents.
	EventsMs int64 `json:"eventsMs,omitempty"`

	// Unexplained — версии, которые агент видит на проде, но о выкатке которых
	// события не было. Опрос /version.json остался, изменился его смысл: это
	// проверка, а не источник истории. Время первого такого наблюдения нужно,
	// чтобы не кричать на выкатку, которая ещё идёт (см. anomalyGrace).
	Unexplained map[string]Unexplained `json:"unexplained,omitempty"`
}

// Unexplained — необъявленная версия одной цели: что именно раздаёт прод и с
// какого момента об этом некому рассказать.
type Unexplained struct {
	Version string `json:"version"`
	Since   string `json:"since"`
}

// ------------------------------------------------------------------ выдача

type OutCheck struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Note     string `json:"note"`
	Impact   string `json:"impact"`
	URL      string `json:"url"`
	Critical bool   `json:"critical"`
	SlowMs   int64  `json:"slowMs,omitempty"`
	// Steps — сколько шагов в сценарии; странице нужно лишь показать, что
	// проверка составная, а не перечислять их.
	Steps     int            `json:"steps,omitempty"`
	Status    string         `json:"status"`
	Since     string         `json:"since"`
	Ms        int64          `json:"ms"`
	Code      int            `json:"code"`
	Error     string         `json:"error"`
	CertDays  *int           `json:"certDays"`
	CertState string         `json:"certState,omitempty"`
	Uptime    map[string]any `json:"uptime"`
	// Days — ровно 90 ячеек по календарю, null там, где замеров не было.
	Days []*OutDay `json:"days"`
	// Coverage — за сколько из этих 90 суток замеры вообще есть. Без него
	// «100%» по трём наблюдавшимся дням неотличимо от честных девяноста.
	Coverage int     `json:"coverage"`
	Spark    []int64 `json:"spark"`
}

type OutDay struct {
	D     string `json:"d"`
	Up    int64  `json:"up"`
	Total int64  `json:"total"`
	AvgMs int64  `json:"avgMs"`
}

type OutUnit struct {
	Name   string `json:"name"`
	Title  string `json:"title"`
	Active bool   `json:"active"`
	State  string `json:"state"`
	Since  string `json:"since"`
}

type OutBuild struct {
	Title   string `json:"title"`
	Version string `json:"version"`
	At      string `json:"at"`
	// URL — адрес самого выкаченного компонента, а не проекта целиком.
	// В сообщении о релизе полезно открыть именно то, что обновилось.
	URL string `json:"url,omitempty"`
	// CommitURL — адрес коммита, из которого собрана эта версия.
	//
	// Владелец просил однозначную связь «коммит — выкатка»: версия вида
	// release-<дата>-<sha> называет коммит, но не ведёт к нему. Здесь она
	// становится адресом, по которому виден сам диф.
	//
	// Поля может не быть, и это НОРМАЛЬНЫЙ исход, а не сбой: адрес
	// репозитория известен не про каждую цель (см. commitURL ниже). Читатель
	// обязан в этом случае показать версию обычным текстом, а не выдумывать
	// ссылку — неверная ссылка хуже отсутствующей, отсутствие видно сразу.
	//
	// Хранится ГОЛЫМ АДРЕСОМ, без разметки: файл читают двое — бот, который
	// шлёт HTML в чат, и страница, которая рисует HTML в браузере. Каждый
	// экранирует по-своему, и разметка в общих данных означала бы, что она
	// правильна ровно у одного из них.
	CommitURL string `json:"commitURL,omitempty"`
	// Changelog — что изменилось в этой версии: по строке на пункт, обычным
	// текстом. Агент его не составляет и составить не может: git-истории
	// выкаченных проектов на сервере нет. Список публикует сама выкатка в
	// version.json, здесь он только переносится дальше — на страницу и в
	// уведомление бота о релизе (bot/format.go, formatChangelog).
	Changelog []string  `json:"changelog,omitempty"`
	History   []Release `json:"history,omitempty"`

	// Anomaly — почему версии на проде нельзя верить как выкатке.
	//
	// История выкаток теперь приезжает событиями, и опрос /version.json из
	// источника истории стал ПРОВЕРКОЙ: версия, о которой не было события, —
	// это либо выкатка мимо пайплайна, либо потерянное событие, и оба случая
	// обязаны быть видимыми. Пустое поле — обычное состояние: версия объявлена.
	//
	// Значение — из перечисления (пока одно, anomalyNoEvent), а не готовая
	// фраза. Формулировка принадлежит читателю: файл читают страница и бот, и
	// каждый говорит на своём языке разметки. Заодно это то же правило, по
	// которому stage и reason в событии — перечисления, а не текст.
	Anomaly string `json:"anomaly,omitempty"`
	// AnomalySince — с какого момента версия остаётся необъявленной. Без него
	// «аномалия» неотличима от «аномалия уже сутки».
	AnomalySince string `json:"anomalySince,omitempty"`
}

// Release — одна запись в истории выкаток.
//
// At — момент самой выкатки (у записей, сделанных из события, — поле at
// события; у старых записей — время сборки по данным сервиса или mtime
// симлинка), а Seen — когда агент эту запись сделал. Они расходятся: между
// выкаткой и запуском агента проходит до минуты, а если агент лежал — сколько
// угодно. Храним оба, потому что «когда выкатили» и «когда узнали» — разные
// вопросы, и подменять один другим значит врать в истории.
type Release struct {
	Version string `json:"version"`
	At      string `json:"at,omitempty"`
	Seen    string `json:"seen"`
	// EventID — id события выкатки, из которого сделана запись.
	//
	// Он и есть защита от повторов: доставка события повторяется по построению
	// (три попытки транспорта, повтор прогона одной кнопкой), и два файла с
	// разными именами несут один id. Курсор здесь не помогает никак — отличить
	// повтор можно только по id.
	//
	// Заодно поле отвечает на вопрос «откуда эта запись»: пусто — значит она
	// осталась от прежней механики, где историю писало наблюдение за версиями.
	// Публиковать его не страшно: это sha256, прообраз по нему не
	// восстанавливается (контракт, §5).
	EventID string `json:"eventId,omitempty"`
	// Changelog — что изменилось в этой версии, по строке на пункт.
	//
	// releases.json — единственное место, где список переживает следующую
	// выкатку: в summary.json changelog есть только у текущей версии, и на
	// вопрос «что было в релизе от прошлого вторника» ответить было нечем.
	// Раз история выкаток уже ведётся здесь, здесь же ей и место.
	//
	// Тип тот же changelogField, что и у version.json, ровно ради разбора:
	// releases.json читается с диска и переживает смены формата, а числом
	// или объектом в необязательном поле нельзя обрушить чтение всей карты
	// историй — это стоило бы истории всех сервисов сразу.
	//
	// Заполнен не у каждой записи: список хранится только у последних
	// releaseChangelogKeep релизов сервиса, у старых он вычищается.
	Changelog changelogField `json:"changelog,omitempty"`
}

type OutProject struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Subtitle string `json:"subtitle"`
	URL      string `json:"url"`
	Accent   string `json:"accent"`
	Status   string `json:"status"`
	// Up/Total — по критичным проверкам: именно они определяют вердикт.
	Up    int `json:"up"`
	Total int `json:"total"`
	// Сколько второстепенных проверок не в порядке. Вердикт они не роняют, но
	// умолчать о них тоже нельзя — иначе сломанная админка выглядит как ничто.
	AuxDown int `json:"auxDown"`
	AuxSlow int `json:"auxSlow"`
	Slow    int `json:"slow"`
	// Сколько служб не работает. Считается отдельно от проверок: юнит может
	// лежать, пока запросы всё ещё обслуживаются (кэш, вторая реплика,
	// фоновый обработчик без своего HTTP), — но состоянием «всё хорошо»
	// это уже не является.
	UnitsDown int        `json:"unitsDown"`
	Checks    []OutCheck `json:"checks"`
	Units     []OutUnit  `json:"units"`
	Builds    []OutBuild `json:"builds"`
}

type Summary struct {
	Updated   string       `json:"updated"`
	Overall   string       `json:"overall"`
	Projects  []OutProject `json:"projects"`
	Incidents []Incident   `json:"incidents"`
	// Таблица имён целей выкатки переносится из конфигурации как есть: агент
	// её не считает и не проверяет, он только доносит её до бота — тот
	// единственный, кому она нужна.
	DeployTargets map[string]DeployTarget `json:"deployTargets,omitempty"`
}

// ------------------------------------------------------------------ утилиты

func readJSON(path string, v any) bool {
	b, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return json.Unmarshal(b, v) == nil
}

// Пишем через временный файл и переименование: страница может читать data
// в любой момент, и половинчатый JSON ей достаться не должен.
func writeJSON(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func pct(up, total int64) *float64 {
	if total == 0 {
		return nil
	}
	v := float64(up) / float64(total) * 100
	v = float64(int(v*100+0.5)) / 100
	return &v
}

// ------------------------------------------------------------------ проверки

// Статусы проверки. «Медленно» — полноценное третье состояние, а не оттенок
// «работает»: сайт, отвечающий восемь секунд, для пользователя не работает,
// хотя код ответа безупречен.
const (
	statusUp   = "up"
	statusSlow = "slow"
	statusDown = "down"
)

// Тело читаем ради bodyIncludes и capture, но не целиком: манифест сборки
// бывает на мегабайты, а маркер и нужные поля лежат в начале.
const bodyReadLimit = 512 << 10

type result struct {
	check     Check
	status    string
	code      int
	ms        int64
	errText   string
	certDays  *int
	certState string
	attempts  int
}

func (r result) ok() bool { return r.status == statusUp || r.status == statusSlow }

// checkOnce — один проход проверки без повторов.
func checkOnce(c Check, client *http.Client) result {
	started := time.Now()
	var r result
	if len(c.Steps) > 0 {
		r = runSteps(c, client)
	} else {
		r = runStep(Step{
			URL:          c.URL,
			Expect:       c.Expect,
			BodyIncludes: c.BodyIncludes,
			ExpectType:   c.ExpectType,
		}, c, client, nil)
	}
	r.check = c
	r.ms = time.Since(started).Milliseconds()

	// Порог времени применяем только к успешному ответу: у упавшей проверки
	// «медленно» ничего не добавляет к «не работает».
	if r.status == statusUp && c.SlowMs > 0 && r.ms > c.SlowMs {
		r.status = statusSlow
		r.errText = fmt.Sprintf("ответ за %d мс при пороге %d мс", r.ms, c.SlowMs)
	}
	return r
}

// checkHTTP: сбой подтверждается вторым запросом.
//
// Одиночный оборванный коннект открывал инцидент, писал его в историю и слал
// тревогу в Telegram — а моргает не только сервис, но и дорога до него.
// Успех принимаем с первой попытки: подтверждать нечего.
func checkHTTP(c Check, client *http.Client) result {
	r := checkOnce(c, client)
	r.attempts = 1
	if r.status != statusDown {
		return r
	}
	time.Sleep(confirmDelay)
	second := checkOnce(c, client)
	second.attempts = 2
	if second.status != statusDown {
		// Первый заход соврал. В выдачу это не идёт — для страницы проверка
		// просто работает, — но в журнал попадает: если строка появляется
		// часто, моргает либо сервис, либо дорога до него, и это повод
		// разбираться, а не молча гасить повтором.
		log.Printf("проверка %s: первый заход дал %q, повтор прошёл", c.ID, r.errText)
	}
	return second
}

// runSteps — сценарий: несколько запросов подряд с передачей значений между
// ними. Одиночный GET проверяет, что endpoint отвечает; сценарий — что
// пользовательский путь проходим целиком.
func runSteps(c Check, client *http.Client) result {
	vars := map[string]string{}
	var last result
	for _, s := range c.Steps {
		last = runStep(s, c, client, vars)
		if last.status == statusDown {
			name := firstNonEmpty(s.Name, s.URL)
			last.errText = fmt.Sprintf("шаг «%s»: %s", name, last.errText)
			return last
		}
	}
	return last
}

// runStep выполняет один запрос и проверяет всё, что о нём известно: код,
// конечный хост после редиректов, тип содержимого и маркер в теле.
func runStep(s Step, c Check, client *http.Client, vars map[string]string) result {
	expect := s.Expect
	if expect == 0 {
		expect = 200
	}

	raw := s.URL
	for k, v := range vars {
		raw = strings.ReplaceAll(raw, "{"+k+"}", v)
	}
	if strings.Contains(raw, "{") && strings.Contains(raw, "}") {
		return result{status: statusDown, errText: "в URL осталась неподставленная переменная: " + raw}
	}

	req, err := http.NewRequest(http.MethodGet, raw, nil)
	if err != nil {
		return result{status: statusDown, errText: err.Error()}
	}
	req.Header.Set("User-Agent", "samoylove-status-agent (+https://status.samoy.love)")

	resp, err := client.Do(req)
	if err != nil {
		msg := err.Error()
		if strings.Contains(msg, "context deadline exceeded") || strings.Contains(msg, "Timeout") {
			msg = fmt.Sprintf("таймаут %ds", int(httpTimeout.Seconds()))
		}
		return result{status: statusDown, errText: msg}
	}
	defer resp.Body.Close()

	r := result{status: statusUp, code: resp.StatusCode}

	if resp.StatusCode != expect {
		r.status = statusDown
		r.errText = fmt.Sprintf("HTTP %d вместо %d", resp.StatusCode, expect)
		return r
	}

	// Куда в итоге привёл редирект. Угнанный домен или кривой конфиг nginx
	// иначе выглядят полным здоровьем: чужой сервер ответил 200, и ладно.
	if from, err := url.Parse(raw); err == nil && resp.Request != nil && resp.Request.URL != nil {
		if to := resp.Request.URL.Hostname(); to != from.Hostname() && !allowedHost(to, c.AllowHosts) {
			r.status = statusDown
			r.errText = fmt.Sprintf("редирект увёл на посторонний хост %s", to)
			return r
		}
	}

	if s.ExpectType != "" {
		ct := resp.Header.Get("Content-Type")
		if !strings.Contains(strings.ToLower(ct), strings.ToLower(s.ExpectType)) {
			r.status = statusDown
			r.errText = fmt.Sprintf("Content-Type %q вместо %q", ct, s.ExpectType)
			return r
		}
	}

	needBody := s.BodyIncludes != "" || len(s.Capture) > 0
	if !needBody {
		return r
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, bodyReadLimit))
	if err != nil {
		r.status = statusDown
		r.errText = "не удалось дочитать ответ: " + err.Error()
		return r
	}
	if s.BodyIncludes != "" && !strings.Contains(string(body), s.BodyIncludes) {
		r.status = statusDown
		r.errText = fmt.Sprintf("в ответе нет %q — код 200, но содержимое не то", s.BodyIncludes)
		return r
	}
	for name, field := range s.Capture {
		var doc map[string]any
		if json.Unmarshal(body, &doc) != nil {
			r.status = statusDown
			r.errText = "ответ не разбирается как JSON, брать " + field + " не из чего"
			return r
		}
		v, found := doc[field]
		if !found {
			r.status = statusDown
			r.errText = fmt.Sprintf("в ответе нет поля %q", field)
			return r
		}
		vars[name] = fmt.Sprint(v)
	}
	return r
}

func allowedHost(host string, allow []string) bool {
	for _, a := range allow {
		if strings.EqualFold(a, host) {
			return true
		}
	}
	return false
}

// Состояния сертификата. Раньше любая беда давала nil, и плашка TLS просто
// исчезала — ровно в тот момент, когда она нужнее всего. «Истёк» и «не смогли
// проверить» — разные новости, и путать их нельзя.
const (
	certOK          = "ok"
	certExpired     = "expired"
	certInvalid     = "invalid"
	certUnreachable = "unreachable"
)

// certDaysLeft: соединяемся без проверки цепочки, чтобы сертификат достался
// нам даже когда он негоден, и уже потом судим о его пригодности отдельно.
// Иначе про истёкший сертификат нельзя сказать ничего, кроме «не вышло».
func certDaysLeft(host string) (*int, string) {
	conn, err := tls.DialWithDialer(
		&net.Dialer{Timeout: httpTimeout},
		"tcp", host+":443",
		//nolint:gosec // G402: проверку делаем сами ниже — здесь она мешает узнать причину.
		&tls.Config{ServerName: host, InsecureSkipVerify: true},
	)
	if err != nil {
		return nil, certUnreachable
	}
	defer conn.Close()

	certs := conn.ConnectionState().PeerCertificates
	if len(certs) == 0 {
		return nil, certUnreachable
	}
	leaf := certs[0]
	days := int(time.Until(leaf.NotAfter).Hours() / 24)

	if time.Now().After(leaf.NotAfter) {
		return &days, certExpired
	}

	roots := x509.NewCertPool()
	inter := x509.NewCertPool()
	for _, c := range certs[1:] {
		inter.AddCert(c)
	}
	if sys, err := x509.SystemCertPool(); err == nil && sys != nil {
		roots = sys
	}
	if _, err := leaf.Verify(x509.VerifyOptions{
		DNSName:       host,
		Roots:         roots,
		Intermediates: inter,
	}); err != nil {
		return &days, certInvalid
	}
	return &days, certOK
}

// systemctl show читается без root. Пустой ActiveEnterTimestamp у остановленного
// юнита — норма, поэтому ошибка разбора времени не считается ошибкой проверки.
func unitState(name string) OutUnit {
	out := OutUnit{Name: name, State: "unknown"}
	cmd := exec.Command("systemctl", "show", name,
		"-p", "ActiveState", "-p", "SubState", "-p", "ActiveEnterTimestamp")
	b, err := cmd.Output()
	if err != nil {
		return out
	}
	fields := map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			fields[k] = v
		}
	}
	out.State = fields["ActiveState"]
	if sub := fields["SubState"]; sub != "" && sub != out.State {
		out.State = out.State + " / " + sub
	}
	out.Active = fields["ActiveState"] == "active"
	if ts := fields["ActiveEnterTimestamp"]; ts != "" {
		if t, err := time.Parse("Mon 2006-01-02 15:04:05 MST", ts); err == nil {
			out.Since = t.UTC().Format(time.RFC3339)
		}
	}
	return out
}

func buildInfo(b Build, client *http.Client) OutBuild {
	out := OutBuild{Title: b.Title}
	// Куда вести читателя за этим компонентом. У «url»-целей адрес известен
	// точно: version.json отдаёт сам сервис, значит его origin и есть адрес.
	// Для остальных ссылку подставит проект — здесь её взять неоткуда.
	if b.Type == "url" {
		if u, err := url.Parse(b.Path); err == nil && u.Scheme != "" && u.Host != "" {
			out.URL = u.Scheme + "://" + u.Host + "/"
		}
	}
	switch b.Type {
	case "url":
		// Отвечает сам сервис — значит, показана версия того, что реально
		// работает, а не того, что лежит на диске.
		resp, err := client.Get(b.Path)
		if err != nil {
			return out
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return out
		}
		var v struct {
			Version string `json:"version"`
			// Commit — короткий sha, из которого собран этот релиз. Кладут его
			// все три писателя version.json (оба переиспользуемых workflow и
			// bin/deploy), и лежит он рядом с версией с самого начала — до
			// сих пор дальше файла он никуда не шёл.
			Commit    string         `json:"commit"`
			BuiltAt   string         `json:"builtAt"`
			Changelog changelogField `json:"changelog"`
		}
		if json.NewDecoder(resp.Body).Decode(&v) == nil {
			out.Version = v.Version
			out.At = v.BuiltAt
			out.Changelog = normalizeChangelog(v.Changelog)
			// После нормализации, а не до: адрес репозитория берётся из уже
			// проверенных ссылок списка изменений.
			out.CommitURL = commitURL(repoBase(out.Changelog), v.Commit)
		}
	case "release":
		// Симлинк вида current -> releases/20260801-225039-5486b2d:
		// имя каталога и есть версия — дата плюс короткий коммит.
		if target, err := os.Readlink(b.Path); err == nil {
			out.Version = filepath.Base(target)
		}
		if fi, err := os.Lstat(b.Path); err == nil {
			out.At = fi.ModTime().UTC().Format(time.RFC3339)
		}
	default:
		if fi, err := os.Stat(b.Path); err == nil {
			out.At = fi.ModTime().UTC().Format(time.RFC3339)
		}
	}
	return out
}

// ------------------------------------------------------- список изменений

// ЕДИНИЦА ДЛИНЫ ВО ВСЕЙ ЭТОЙ ЦЕПОЧКЕ — СИМВОЛ, А НЕ БАЙТ.
//
// Потолок темы коммита задан владельцем и записан в CLAUDE.md: 120 СИМВОЛОВ.
// Ровно столько режет генератор (deploy-kit/bin/changelog, --width 120), и
// обрезка вообще должна быть видна читателю ровно в одном месте — там, где
// текст рисуется, то есть в боте.
//
// Раньше здесь стояли байты, и это была не придирка к единицам измерения.
// Кириллица — два байта на символ, поэтому прежние 100 байт генератора
// означали 50 символов: из 287 настоящих тем четырёх репозиториев хозяйства 43
// (15%) обрывались посреди фразы. Хуже того, каждый участник цепочки резал по
// своей мерке — генератор по 100 байт, агент по 300, бот снова по 100, — и
// одна и та же тема обрезалась дважды на разной длине, причём вторая обрезка
// приходилась уже на многоточие первой.
//
// Правило теперь одно: НИ АГЕНТ, НИ БОТ НЕ СТРОЖЕ ГЕНЕРАТОРА. Тема в 120
// символов обязана доехать до читателя ровно такой, какой её написали. Свои
// пределы у агента и бота остаются — оба читают чужой файл из сети и обязаны
// пережить враньё в нём, — но срабатывать они должны только на враньё.
const (
	// changelogKeep — пунктов на сборку в summary.json.
	//
	// БЫЛО 12, И ЭТО БЫЛА ПЕРВАЯ ИЗ ДВУХ ОБРЕЗОК, О КОТОРЫХ ПРОСИЛИ ЗАБЫТЬ.
	// Владелец хочет видеть ВСЕ выкаченные коммиты, а не «…и ещё N»; резать
	// список нельзя ни здесь, ни в боте. Причём первая обрезка решает всё:
	// чего агент не положил в summary.json, бота уже не спасёт.
	//
	// Сто — это не «сколько показываем», а «сколько вообще бывает». Медиана
	// релиза в хозяйстве — три коммита, самый крупный за год — сорок один.
	// Сотня оставляет двойной запас над любым настоящим релизом и при этом
	// остаётся потолком против вранья в чужом version.json.
	changelogKeep = 100

	// changelogChars — весь список одной сборки, в символах.
	//
	// Потолок на число пунктов сам по себе ничего не ограничивает: сто пунктов
	// по 240 символов — это 24 000 символов на КАЖДУЮ из девятнадцати целей, а
	// summary.json тянет страница на каждой загрузке.
	//
	// СЧИТАЕТСЯ ХРАНИМАЯ СТРОКА ЦЕЛИКОМ, ВМЕСТЕ С РАЗМЕТКОЙ ССЫЛКИ. Это не
	// оговорка: предел здесь про ПАМЯТЬ И РАЗМЕР ФАЙЛА, а разметка занимает и то
	// и другое ровно так же, как буквы. Читателю она невидима, и потому ширину
	// пункта (changelogLineChars) она не ест — но это другой предел и про другое.
	//
	// ЧИСЛО ПРИШЛОСЬ ПОДНЯТЬ, КОГДА ПОЯВИЛИСЬ ССЫЛКИ НА PR. Пункт со ссылкой
	// длиннее пункта без неё примерно на 63 символа («<a href="https://github.com/
	// владелец/репозиторий/pull/123">#123</a>»), то есть на треть. Прежние девять
	// тысяч были посчитаны для голого текста и означали ~73 пункта; со ссылками
	// тот же предел молча превратился в ~48. Молча — это худшее из свойств:
	// список обрывается без хвоста, и «больше ничего и не было» неотличимо от
	// «остальное не поместилось».
	//
	// Пятнадцать тысяч — это ~82 пункта со ссылками, вдвое больше самого
	// крупного релиза в истории хозяйства (41 коммит), и по-прежнему заведомо
	// меньше мегабайта на файл: 19 целей × 15 000 символов кириллицы ≈ 570 КБ в
	// пределе против ≈ 3 КБ на настоящих данных.
	changelogChars = 15000

	// changelogLineChars — символов на пункт по дороге в summary.json.
	//
	// Вдвое больше владельческих 120 намеренно. Агент здесь труба, а не
	// оформитель: режет по месту бот, там же и многоточие. Запас нужен потому,
	// что deploy-kit живёт своей выкаткой: ослабленный --width уедет на сервер
	// раньше, чем пересоберут этот бинарник, и агент не должен оказаться тем,
	// кто молча укоротит уже разрешённую тему.
	changelogLineChars = 240

	// Пределы для долгого хранения в releases.json. Файл лежит в вебруте
	// рядом с summary.json, переписывается раз в минуту и целиком читается
	// на каждом запуске — расти без предела ему нельзя.
	//
	// ЧТО ИМЕННО ХРАНИМ. «/changelog имя» показывает пять последних выкаток
	// цели, и показать он обязан ТОТ ЖЕ полный список, который приходил
	// сообщением: список, урезанный только в истории, читается как рассказ о
	// другой выкатке. Поэтому глубина осталась пятёркой, а вот сам список
	// больше не режется по десять пунктов — прежние 10 и 1200 символов теряли
	// уже двенадцатикоммитный релиз.
	//
	// Пять релизов на сервис: спрашивают «что приехало» про недавнее, а пять
	// выкаток одной цели — это неделя-две обычной работы. Остальные
	// releasesKeep записей остаются в истории как были, без списка: дата и
	// версия стоят дёшево, changelog — нет.
	//
	// Сто пунктов и 15 000 символов — ровно те же числа, что у summary.json
	// (changelogKeep, changelogChars), и это не совпадение: история хранит то
	// же самое, что уехало в чат, и второй, более строгий предел означал бы
	// расхождение двух рассказов об одной выкатке.
	releaseChangelogKeep  = 5
	releaseChangelogLines = changelogKeep
	releaseChangelogChars = changelogChars

	// releasesChangelogTotal — СКОЛЬКО СИМВОЛОВ СПИСКОВ ДЕРЖИТ ВЕСЬ ФАЙЛ.
	//
	// Предела на одну запись мало: он умножается на число целей, а их в
	// config уже девятнадцать и будет больше. 19 × 5 × 15 000 — это 1 425 000
	// символов, то есть под три мегабайта кириллицы в файле, который лежит в
	// вебруте и переписывается раз в минуту. Настоящих данных там на два
	// порядка меньше (медиана релиза — три темы по 50 символов, ≈ 20 КБ на всё
	// хозяйство), но предел, который держится на «столько не бывает», — не
	// предел.
	//
	// 150 000 символов ≈ 300 КБ кириллицы: в семь раз больше настоящего
	// объёма и почти вдесятеро меньше того, что дают одни только поштучные
	// пределы.
	// Когда потолок всё-таки достигнут, списки снимаются со СТАРЫХ записей
	// (capReleaseChangelogs): «что приехало вчера» спрашивают, «что приехало
	// в марте» — нет.
	releasesChangelogTotal = 150000
)

// changelogField — поле changelog из version.json.
//
// Принимаем и массив строк, и одну строку с переводами строк. Файл пишет
// шелл на стороне выкатки, и собрать там JSON-массив заметно труднее, чем
// одно текстовое поле: требовать массив значило бы получать поле пустым.
//
// Разбор чужой формы поля НЕ ошибка: version.json нужен прежде всего ради
// версии, и число вместо списка не повод потерять её вместе со всей выкаткой.
type changelogField []string

func (c *changelogField) UnmarshalJSON(b []byte) error {
	var arr []string
	if json.Unmarshal(b, &arr) == nil {
		*c = arr
		return nil
	}
	var s string
	if json.Unmarshal(b, &s) == nil {
		*c = strings.Split(s, "\n")
		return nil
	}
	*c = nil
	return nil
}

// normalizeChangelog приводит поле к простому тексту: по строке на пункт, без
// разметки.
//
// Вид блока задан в deploy-kit/bin/changelog, и выкатке проще всего положить
// в version.json ровно то, что он напечатал: с заголовком «Изменения», с «•»
// в начале строк и уже экранированными & < >. Разбираем и это. Иначе на
// стороне выкатки завелось бы второе место, знающее формат, — а хранить
// чужую HTML-разметку в summary.json нельзя тем более: файл читает не только
// бот, но и страница, и экранирует каждый по-своему.
//
// Всё ограничено сверху. Поле приезжает по сети из чужого файла, а
// summary.json переписывается раз в минуту и целиком лежит в памяти у всех
// читателей.
//
// ЕДИНСТВЕННОЕ ИСКЛЮЧЕНИЕ ИЗ «БЕЗ РАЗМЕТКИ» — ссылка на PR в конце пункта.
// Генератор снимает с темы хвост «(#21)» (шум в 23% настоящих тем) и, если
// знает адрес репозитория, дорисовывает его ссылкой. Ссылку нельзя ни
// разэкранировать, ни обрезать, ни экранировать второй раз — любое из этого
// превращает её в мусор. Правила отбора — в splitRefLink: список
// РАЗРЕШЁННОГО, а не запрещённого.
func normalizeChangelog(in []string) []string {
	var out []string
	chars := changelogChars
	for _, raw := range in {
		if len(out) >= changelogKeep {
			break
		}
		// Пункт обязан быть однострочным: перевод строки внутри темы коммита
		// разорвал бы список пополам уже в сообщении.
		s := strings.Join(strings.Fields(raw), " ")
		// Заголовок блока рисует читатель — бот добавляет его сам.
		if s == "" || s == "<b>Изменения</b>" || s == "Изменения" {
			continue
		}
		// Маркеры и правило их снятия — те же, что в боте (bot/format.go,
		// trimMarker): расхождение означало бы, что на одном пути выкатки маркер
		// снимается, а на другом уезжает в сообщение. За маркером обязан идти
		// пробел (или ничего): без этого условия «-Wall в CFLAGS» превратилось
		// бы в «Wall в CFLAGS». Голый маркер темой не является и отсеивается
		// ниже как пустая строка — иначе он занял бы место настоящего пункта.
		for _, marker := range []string{"•", "-", "*", "–", "—"} {
			rest, ok := strings.CutPrefix(s, marker)
			if !ok {
				continue
			}
			if rest == "" || strings.HasPrefix(rest, " ") {
				s = strings.TrimSpace(rest)
			}
			break
		}
		// Ссылку на PR отделяем ДО всего остального: и разэкранирование, и
		// обрезка для неё яд. Дальше по конвейеру идёт только текст пункта.
		text, href, label, hasRef := splitRefLink(s)
		// Генератор экранирует вывод для Telegram, а бот экранирует ещё раз
		// при отправке. Без разэкранирования здесь «go 1.22 <-- важно»
		// доехало бы до читателя как «go 1.22 &amp;lt;-- важно».
		text = html.UnescapeString(text)
		// Режем ПОСЛЕ разэкранирования. Порядок здесь важнее, чем кажется:
		// генератор присылает уже экранированный текст, и обрезка сырой строки
		// могла бы остановиться посреди «&amp;» — в summary.json уехало бы
		// «&am», а на странице и в чате читатель увидел бы огрызок сущности
		// вместо символа.
		text = cutRunes(text, changelogLineChars)
		// А вот теперь — то, ради чего разэкранирование опасно. Тема коммита
		// пишется человеком с правом мержа, и написать в ней <a href="…">#1</a>
		// текстом ему никто не мешает: генератор это экранирует, а мы бы
		// РАЗэкранировали обратно и положили в summary.json уже разметкой.
		// Бот, читая её, увидел бы настоящую ссылку — то есть чужой https-адрес
		// под видом номера PR. Ссылку в пункт ставит только агент и только
		// проверенную; чужую снимаем, номер оставляем видимым.
		//
		// ПОСЛЕ обрезки, а не до: обрезка отбрасывает хвост строки и способна
		// открыть концом пункта то, что раньше стояло в середине.
		text = dropRefLinkMarkup(text)
		if text == "" {
			continue
		}
		if hasRef {
			// Собираем заново из проверенных кусков, а не переносим чужую
			// строку: так в разметку физически нечему просочиться, кроме
			// адреса из разрешённого алфавита и цифр номера.
			text += " " + refLinkHTML(href, label)
		}
		// Общий потолок списка проверяем ДО добавления и целым пунктом:
		// обрезать пункт наполовину значило бы разрезать ссылку, а половина
		// тега — это отказ Telegram, то есть не пришедшее уведомление. Первый
		// пункт проходит всегда: список из одной строки не бывает «слишком
		// длинным», он бывает единственным, что вообще есть сказать.
		n := utf8.RuneCountInString(text)
		if n > chars && len(out) > 0 {
			break
		}
		chars -= n
		out = append(out, text)
	}
	return out
}

// ------------------------------------------------- ссылка на PR внутри пункта

// refLinkRe — ЕДИНСТВЕННАЯ разметка, которой позволено проехать сквозь
// нормализацию. Всё, что не совпало с этим выражением, остаётся текстом и
// экранируется как текст.
//
// Выражение — список разрешённого, и каждая его часть отвечает за свою угрозу:
//
//   - «^(.*?) ?» и «$» — якорь только В КОНЦЕ пункта. Ссылка в середине темы
//     генератором не ставится, значит это чужая разметка;
//   - «<a href="…">» без пробела перед «>» — атрибут ровно один. Ни onclick,
//     ни target, ни title сюда не пролезут;
//   - «https://» буквально — ни javascript:, ни data:, ни http:;
//   - алфавит адреса без «"», «<», «>», «&» и пробела — из атрибута нельзя
//     выйти, нельзя открыть второй тег и нельзя оборвать сущность;
//   - «#[0-9]{1,12}» — текстом ссылки может быть только номер. Вложенный тег
//     («<b>») содержит «<», цифрой не является и совпадения не даст;
//   - «{1,200}» и «{1,12}» — длина ограничена: адрес приезжает из чужого
//     файла, а summary.json лежит в памяти у каждого читателя.
//
// Отдельно про «(.*?)»: «.» в Go не совпадает с переводом строки, а пункт к
// этому моменту уже склеен в одну строку — разорвать выражение переносом
// нельзя.
var refLinkRe = regexp.MustCompile(`^(.*?) ?<a href="(https://[A-Za-z0-9._~:/%+-]{1,200})">#([0-9]{1,12})</a>$`)

// splitRefLink отделяет от пункта разрешённую ссылку на PR.
//
// Возвращает текст без ссылки, адрес, подпись («#21») и признак того, что
// ссылка вообще нашлась. Не нашлась — текст возвращается неизменным, и это
// нормальный, а не исключительный исход: ссылку генератор ставит, только когда
// знает адрес репозитория.
//
// Вход здесь — вывод генератора, то есть текст с УЖЕ экранированными & < >.
// Поэтому «<» в тексте пункта означает разметку, а не букву, и любая разметка
// перед ссылкой — повод не поверить всей строке целиком. Это и отсеивает
// вторую ссылку, незакрытый тег и «<script>» перед ссылкой: разбирать такое
// незачем, текстом оно безопасно.
func splitRefLink(s string) (text, href, label string, ok bool) {
	m := refLinkRe.FindStringSubmatch(s)
	if m == nil {
		return s, "", "", false
	}
	if strings.ContainsAny(m[1], "<>") {
		return s, "", "", false
	}
	// Пункт из одной ссылки — не пункт: генератор так не печатает (его
	// выражение требует непустой темы), а читателю «#21» без темы не говорит
	// ничего.
	if strings.TrimSpace(m[1]) == "" {
		return s, "", "", false
	}
	return m[1], m[2], "#" + m[3], true
}

// refLinkHTML собирает ссылку обратно.
//
// Адрес всё равно проходит через экранирование, хотя разрешённый алфавит и так
// не содержит «"», «&», «<» и «>». Это стоит ноль и переживает правку алфавита
// в refLinkRe: забыть здесь про экранирование дороже, чем сделать его дважды.
func refLinkHTML(href, label string) string {
	return `<a href="` + html.EscapeString(href) + `">` + html.EscapeString(label) + `</a>`
}

// anchorTagRe — тег <a>, открывающий или закрывающий, ГДЕ БЫ ОН НИ СТОЯЛ.
//
// Нарочно шире, чем refLinkRe: тот отвечает на вопрос «это наша ссылка?» и
// потому обязан быть узким, а этот — на вопрос «это вообще якорь?», и здесь
// узость была бы дырой. Регистр не важен: разбор HTML к регистру тега
// нечувствителен, а «<A HREF=…>» пишется так же легко, как «<a href=…>».
// «[^<>]*» без вложенных скобок — выражение линейное, чужой ввод его не
// раскрутит.
var anchorTagRe = regexp.MustCompile(`(?i)</?a(?:\s[^<>]*)?>`)

// dropRefLinkMarkup снимает с УЖЕ разэкранированного текста то, что читатель
// вниз по конвейеру принял бы за ссылку.
//
// Нужна ровно из-за отмывания: тема коммита с текстовым «<a href="https://…">
// #1</a>» приезжает сюда экранированной, splitRefLink её (правильно) не
// признаёт, а разэкранирование превращает её в настоящую разметку. После этой
// функции в summary.json действует простое правило: якорь в пункте есть
// только тот, который поставил агент.
//
// СНИМАЕТСЯ ЯКОРЬ В ЛЮБОМ МЕСТЕ СТРОКИ, А НЕ ТОЛЬКО В КОНЦЕ. Здесь стоял разбор
// по refLinkRe, а он привязан к концу пункта, — и правило выше держалось только
// для хвоста. Тема вида «Якорь <a href="https://чужой/х">#1</a> в середине
// темы» проезжала насквозь и ложилась в summary.json и releases.json НАСТОЯЩЕЙ
// разметкой с чужим адресом. Читателям это сегодня не вредит: бот такую строку
// целиком не признаёт ссылкой и экранирует, а страница список изменений вовсе
// не рисует. Но и то и другое — свойство ЧУЖОГО кода, а правило записано здесь
// и должно держаться здесь: файл лежит в вебруте, читателей у него больше
// одного, и появиться новый может без единой правки в этом файле.
//
// Номер сохраняем — он часть темы, и терять его не за что: снимаются теги, а не
// то, что между ними. Другие теги (<b>, <script>) отсюда НЕ снимаются и уезжают
// в файл текстом с угловыми скобками — так же, как «go 1.22 <-- важно», которое
// снимать нельзя. Разметкой они не станут: и бот, и страница экранируют всё,
// кроме проверенного якоря.
func dropRefLinkMarkup(s string) string {
	return strings.TrimSpace(anchorTagRe.ReplaceAllString(s, ""))
}

// ------------------------------------------------------- ссылка на коммит
//
// ОТКУДА БЕРЁТСЯ АДРЕС РЕПОЗИТОРИЯ. Это главный вопрос всей затеи, и ответ на
// него не «из таблицы».
//
// Таблицу «сервис → репозиторий» пришлось бы вести руками в config/status.json
// рядом с девятнадцатью целями, и она разъезжалась бы молча: переименовали
// репозиторий — ссылка ведёт в никуда, а узнать об этом можно только кликнув.
// Хуже того, таблица утверждает связь, которую ничем не подтверждает: она
// говорит, из какого репозитория сервис СОБИРАЛСЯ БЫ, а не из какого он
// действительно собран.
//
// Настоящий источник уже приезжает в том же version.json, что и версия с
// коммитом, — в списке изменений. Выкатка ставит там ссылки на PR, и базу для
// них берёт из github.repository того самого прогона, который этот релиз и
// собрал (deploy-kit, go-service.yml: LINK_BASE), а при локальной выкатке — из
// remote того самого рабочего дерева (bin/deploy, normalize_remote). То есть
// адрес приходит ОТ САМОЙ ВЫКАТКИ, вместе с релизом, и относится ровно к тому
// коду, который сейчас работает. Соврать в нём можно только соврав в релизе.
//
// Отсюда и поведение при нехватке данных: списка изменений нет (цель типа
// release читается симлинком, version.json у неё нет вовсе), ссылок в списке
// нет (--link-base none, релиз без PR), поле commit пустое или не похоже на
// sha — ссылки НЕТ. Не «примерная», не «наверное этот репозиторий» — нет.
// Версия в этом случае показывается обычным текстом, как и показывалась.

// repoLinkRe — из проверенной ссылки на PR достаём владельца и репозиторий.
//
// Хост зафиксирован буквально: github.com и только он. Алфавит имён — без «"»,
// «<», «>», «&» и пробела, поэтому собранный ниже адрес физически не может
// выйти за пределы атрибута. Первый символ имени обязан быть буквой или
// цифрой: иначе «..» прошло бы как имя владельца и дало бы адрес с выходом
// вверх по пути.
var repoLinkRe = regexp.MustCompile(
	`^https://github\.com/([A-Za-z0-9][A-Za-z0-9._-]{0,63}/[A-Za-z0-9][A-Za-z0-9._-]{0,99})/pull/[0-9]{1,12}$`)

// commitShaRe — короткий или полный sha и ничего кроме.
//
// Строгость тут дешёвая и небесполезная: bin/deploy при отсутствии git пишет
// в это поле «local», и такой «коммит» не должен превращаться в ссылку.
var commitShaRe = regexp.MustCompile(`^[0-9a-f]{7,40}$`)

// repoBase возвращает https://github.com/владелец/репозиторий по ссылкам на PR
// внутри уже нормализованного списка изменений.
//
// Пункты пришли из чужого файла, поэтому разбираются тем же splitRefLink, что
// и всё остальное, а не поиском подстроки. Разные базы в одном списке — повод
// не поверить никакой: у релиза один репозиторий, и два разных адреса означают
// либо подделку, либо склеенный вручную файл.
func repoBase(changelog []string) string {
	base := ""
	for _, s := range changelog {
		_, href, _, ok := splitRefLink(s)
		if !ok {
			continue
		}
		m := repoLinkRe.FindStringSubmatch(href)
		if m == nil {
			continue
		}
		b := "https://github.com/" + m[1]
		if base != "" && base != b {
			return ""
		}
		base = b
	}
	return base
}

// commitURL собирает адрес коммита из проверенных кусков — ровно так же, как
// refLinkHTML собирает ссылку на PR: не переносит чужую строку, а строит новую.
// Пустая строка на выходе — «ссылки нет», и это законный ответ.
func commitURL(base, sha string) string {
	if base == "" || !commitShaRe.MatchString(sha) {
		return ""
	}
	return base + "/commit/" + sha
}

// cutRunes обрезает строку до n СИМВОЛОВ, никогда не разрезая символ UTF-8.
//
// Символ, а не байт: потолок темы владелец задал в символах (CLAUDE.md), а
// счёт в байтах означал бы для кириллицы вдвое более строгий предел, чем
// написано, — ровно та беда, из-за которой темы обрывались на полуслове.
//
// Функция дословно повторена в боте (bot/format.go, cutRunes), и это не
// небрежность, а условие: агент и бот — отдельные модули, общего пакета у них
// нет, а одна и та же тема обязана получиться одинаковой на обоих путях
// выкатки. Разойдутся реализации — разойдутся списки, и увидит это только
// читатель. Правите здесь — правьте там же.
func cutRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	// Многоточие тоже занимает место — один символ, — и результат обязан
	// укладываться в заявленный предел вместе с ним. На совсем тесном пределе
	// многоточие съедает больше смысла, чем сообщает, и там его нет вовсе.
	tail := "…"
	if n <= 12 {
		tail = ""
	} else {
		n--
	}
	cut := len(s)
	seen := 0
	for i := range s {
		if seen == n {
			cut = i
			break
		}
		seen++
	}
	t := s[:cut]
	// Обрыв посреди слова читается хуже, чем чуть более короткая строка.
	// Граница слова — пробел, то есть ASCII: символ UTF-8 этим не разрезать.
	if i := strings.LastIndexByte(t, ' '); i > 0 && utf8.RuneCountInString(t[:i])*2 >= n {
		t = t[:i]
	}
	return strings.TrimRight(t, " ") + tail
}

// ------------------------------------------------------------------ агрегация

// bucketKey приводит ключ корзины к строке независимо от того, откуда он взят.
//
// Часовой ключ это unix-время: в коде он int64, а после json.Unmarshal
// (bucket это [4]any) возвращается float64. fmt.Sprint даёт для них разное —
// "1785618000" против "1.785618e+09" — и корзина текущего часа, прочитанная с
// диска, не сливалась с новым замером: агент дописывал по корзине на запуск,
// то есть раз в минуту. Из-за этого hourlyKeep=2160 покрывал не 90 суток, а
// полтора, d7 считался по одной корзине (всегда 100%), а спарклайн показывал
// последние 24 запуска вместо 24 часов.
func bucketKey(v any) string {
	if f, ok := v.(float64); ok {
		return fmt.Sprint(int64(f))
	}
	return fmt.Sprint(v)
}

func bumpBucket(buckets []bucket, key any, ok bool, ms int64, keep int) []bucket {
	if n := len(buckets); n > 0 && bucketKey(buckets[n-1][0]) == bucketKey(key) {
		last := &buckets[n-1]
		up := toInt((*last)[1])
		total := toInt((*last)[2])
		avg := toInt((*last)[3])
		if ok {
			up++
		}
		total++
		avg = (avg*(total-1) + ms) / total
		(*last)[1], (*last)[2], (*last)[3] = up, total, avg
	} else {
		var up int64
		if ok {
			up = 1
		}
		buckets = append(buckets, bucket{key, up, int64(1), ms})
	}
	if len(buckets) > keep {
		buckets = buckets[len(buckets)-keep:]
	}
	return buckets
}

func toInt(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case float64:
		return int64(n)
	}
	return 0
}

// Аптайм считаем по календарному окну, а не по числу корзин.
//
// Корзина заводится только когда агент отработал. Раньше «за 90 дней» брало
// девяносто ПОСЛЕДНИХ корзин: если агент неделю лежал, окно незаметно
// растягивалось на девяносто семь суток, а сам простой агента ещё и улучшал
// цифру, потому что плохих замеров в нём не было. Теперь окно задаёт
// календарь, а дни без замеров честно остаются дырками.

// dayWindow возвращает ключи последних n суток, свежий — последним.
func dayWindow(now time.Time, n int) []string {
	keys := make([]string, 0, n)
	for i := n - 1; i >= 0; i-- {
		keys = append(keys, now.AddDate(0, 0, -i).Format("2006-01-02"))
	}
	return keys
}

// indexBuckets раскладывает корзины по нормализованному ключу. Корзины с
// одинаковым ключом складываются, а не затирают друг друга: в уже записанных
// файлах истории такие дубли есть — их наплодил bumpBucket, пока сравнивал
// ключи разных типов, — и брать из них только последнюю значило бы выкинуть
// почти все замеры того часа.
func indexBuckets(buckets []bucket) map[string]bucket {
	idx := make(map[string]bucket, len(buckets))
	for _, b := range buckets {
		key := bucketKey(b[0])
		prev, ok := idx[key]
		if !ok {
			idx[key] = b
			continue
		}
		up := toInt(prev[1]) + toInt(b[1])
		total := toInt(prev[2]) + toInt(b[2])
		avg := int64(0)
		if total > 0 {
			avg = (toInt(prev[3])*toInt(prev[2]) + toInt(b[3])*toInt(b[2])) / total
		}
		idx[key] = bucket{prev[0], up, total, avg}
	}
	return idx
}

// uptimeOverDays — доступность за последние n календарных суток.
func uptimeOverDays(daily []bucket, now time.Time, n int) *float64 {
	idx := indexBuckets(daily)
	var up, total int64
	for _, key := range dayWindow(now, n) {
		if b, ok := idx[key]; ok {
			up += toInt(b[1])
			total += toInt(b[2])
		}
	}
	return pct(up, total)
}

// uptimeOverHours — то же для часового окна: ключ часовой корзины это unix
// начала часа.
func uptimeOverHours(hourly []bucket, now time.Time, hours int) *float64 {
	idx := indexBuckets(hourly)
	var up, total int64
	for i := 0; i < hours; i++ {
		key := fmt.Sprint(now.Add(-time.Duration(i) * time.Hour).Truncate(time.Hour).Unix())
		if b, ok := idx[key]; ok {
			up += toInt(b[1])
			total += toInt(b[2])
		}
	}
	return pct(up, total)
}

// daysForPage — ровно n ячеек по календарю; сутки без замеров это nil, и на
// шкале они выглядят дыркой, а не сдвигают всю историю влево.
func daysForPage(daily []bucket, now time.Time, n int) []*OutDay {
	idx := indexBuckets(daily)
	out := make([]*OutDay, 0, n)
	for _, key := range dayWindow(now, n) {
		b, ok := idx[key]
		if !ok || toInt(b[2]) == 0 {
			out = append(out, nil)
			continue
		}
		out = append(out, &OutDay{D: key, Up: toInt(b[1]), Total: toInt(b[2]), AvgMs: toInt(b[3])})
	}
	return out
}

func uptimeFromRaw(raw []sample, hours int) *float64 {
	from := time.Now().Add(-time.Duration(hours) * time.Hour).Unix()
	var up, total int64
	for _, s := range raw {
		if s[0] >= from {
			total++
			up += s[1]
		}
	}
	return pct(up, total)
}

// ------------------------------------------------------------------ main

// validateCheckIDs требует, чтобы id проверки был уникален во всём конфиге, а
// не в пределах проекта. Под этим ключом лежит вообще всё, что агент копит:
// raw/hourly/daily, state.Services, инциденты и карта, из которой проекты
// разбирают результаты обратно. Два одинаковых id — это две проверки, которые
// каждую минуту затирают историю друг друга, показываются в обоих проектах
// одинаково и сливают свои инциденты в один. Конфиг правится руками, поэтому
// падаем сразу: молча склеенную историю уже не расклеить.
func validateCheckIDs(cfg Config) error {
	owner := map[string]string{}
	for _, p := range cfg.Projects {
		for _, c := range p.Checks {
			if prev, ok := owner[c.ID]; ok {
				return fmt.Errorf("id проверки %q повторяется: проекты %q и %q", c.ID, prev, p.ID)
			}
			owner[c.ID] = p.ID
		}
	}
	return nil
}

func main() {
	cfgPath := flag.String("config", "/etc/status-agent/status.json", "путь к конфигу")
	dataDir := flag.String("data", "/var/www/status/data", "куда складывать данные")
	metricsPath := flag.String("metrics", defaultMetricsPath,
		"куда класть .prom для textfile-коллектора node_exporter; пусто — не писать")
	eventsDir := flag.String("events", defaultEventsDir,
		"журнал событий выкатки, только на чтение; пусто — не читать")
	flag.Parse()

	runStart := time.Now()

	var cfg Config
	if !readJSON(*cfgPath, &cfg) {
		log.Fatalf("не удалось прочитать конфиг %s", *cfgPath)
	}
	if err := validateCheckIDs(cfg); err != nil {
		log.Fatalf("конфиг %s: %v", *cfgPath, err)
	}

	now := time.Now().UTC()
	client := &http.Client{Timeout: httpTimeout}

	state := State{Services: map[string]*CheckState{}}
	readJSON(filepath.Join(*dataDir, "state.json"), &state)
	if state.Services == nil {
		state.Services = map[string]*CheckState{}
	}
	var incidents []Incident
	readJSON(filepath.Join(*dataDir, "incidents.json"), &incidents)

	// История выкаток. Ключ — проект плюс название цели, потому что у проекта
	// целей несколько (сайт, API, админка) и версии у них свои.
	releases := map[string][]Release{}
	readJSON(filepath.Join(*dataDir, "releases.json"), &releases)
	// Пределы применяем сразу после чтения, ко всей карте. События приходят
	// только к целям из конфига, а в файле остаются и ключи исчезнувших целей —
	// раз ключ уже никогда не обновится, его список изменений иначе останется в
	// файле навсегда.
	for key := range releases {
		releases[key] = trimReleaseChangelogs(releases[key])
	}

	// История пополняется журналом выкаток: о выкатке рассказывает она сама, а
	// не разница двух снимков /version.json. Читается это ДО обхода целей —
	// дальше в summary.json уезжает уже пополненная история, и запись о выкатке
	// не отстаёт от версии на минуту.
	if *eventsDir != "" {
		keys := eventKeys(cfg)
		events, cursor := readEvents(*eventsDir, state.EventsMs, eventsMaxPerRun)
		added := 0
		for _, ev := range events {
			key, ok := keys[ev.App]
			if !ok {
				// Цель выкатывается, но статус-страница о ней не знает. Это не
				// повод молчать: событие уже случилось, и о расхождении конфигов
				// должно быть сказано вслух — иначе история цели просто не
				// ведётся, и заметить это можно будет только по пустому
				// /changelog.
				log.Printf("событие %s: цель %q не найдена в конфиге, история не пополнена", ev.File, ev.App)
				continue
			}
			before := len(releases[key])
			releases[key] = applyEvent(releases[key], ev, now)
			if len(releases[key]) != before {
				added++
			}
		}
		// Курсор двигаем и тогда, когда ни одно событие не легло в историю:
		// перечитывать журнал заново незачем, а испорченный или чужой файл не
		// имеет права остановить его навсегда.
		state.EventsMs = cursor
		if len(events) > 0 {
			log.Printf("журнал выкаток: разобрано событий %d, записей истории добавлено %d, курсор %d",
				len(events), added, cursor)
		}
	}
	anomalies := newAnomalyTracker(state.Unexplained)

	// Все проверки — параллельно: последовательный обход десятка эндпоинтов
	// растянул бы запуск на секунды и смазал бы измерение времени ответа.
	type job struct {
		project *Project
		check   Check
	}
	var jobs []job
	for i := range cfg.Projects {
		for _, c := range cfg.Projects[i].Checks {
			jobs = append(jobs, job{&cfg.Projects[i], c})
		}
	}

	results := make([]result, len(jobs))
	var wg sync.WaitGroup
	for i, j := range jobs {
		wg.Add(1)
		go func(i int, j job) {
			defer wg.Done()
			r := checkHTTP(j.check, client)
			if j.check.Cert != "" {
				r.certDays, r.certState = certDaysLeft(j.check.Cert)
			}
			results[i] = r
		}(i, j)
	}
	wg.Wait()

	byCheckID := map[string]OutCheck{}

	for i, j := range jobs {
		r := results[i]
		id := j.check.ID
		status := r.status

		rawPath := filepath.Join(*dataDir, "raw", id+".json")
		hourlyPath := filepath.Join(*dataDir, "hourly", id+".json")
		dailyPath := filepath.Join(*dataDir, "daily", id+".json")

		// В историю доступности «медленно» идёт как доступность: аптайм должен
		// оставаться про доступность, иначе он перестанет быть сравнимым с
		// собой же за прошлые месяцы. Деградацию видно по времени ответа.
		var raw []sample
		readJSON(rawPath, &raw)
		var okInt int64
		if r.ok() {
			okInt = 1
		}
		raw = append(raw, sample{now.Unix(), okInt, r.ms, int64(r.code)})
		cutoff := now.Add(-rawWindow).Unix()
		for len(raw) > 0 && raw[0][0] < cutoff {
			raw = raw[1:]
		}

		var hourly, daily []bucket
		readJSON(hourlyPath, &hourly)
		readJSON(dailyPath, &daily)
		hourly = bumpBucket(hourly, now.Truncate(time.Hour).Unix(), r.ok(), r.ms, hourlyKeep)
		daily = bumpBucket(daily, now.Format("2006-01-02"), r.ok(), r.ms, dailyKeep)

		// История не критична для текущего запуска, ронять из-за неё агент не
		// станем — но и молчать нельзя: незаписанная история это тихо тающие
		// аптайм и девяностодневная шкала.
		if err := writeJSON(rawPath, raw); err != nil {
			log.Printf("история %s не записана: %v", rawPath, err)
		}
		if err := writeJSON(hourlyPath, hourly); err != nil {
			log.Printf("история %s не записана: %v", hourlyPath, err)
		}
		if err := writeJSON(dailyPath, daily); err != nil {
			log.Printf("история %s не записана: %v", dailyPath, err)
		}

		prev := state.Services[id]
		since := now.Format(time.RFC3339)
		if prev != nil && prev.Status == status {
			since = prev.Since
		} else {
			incidents = applyIncident(incidents, incidentChange{
				id:      id,
				name:    incidentName(j.project.Title, j.check.Name),
				prev:    prev,
				status:  status,
				errText: r.errText,
				at:      since,
			}, now)
		}
		state.Services[id] = &CheckState{
			Status: status, Since: since, Ms: r.ms, Code: r.code, CertDays: r.certDays,
		}

		days := daysForPage(daily, now, daysOnPage)
		coverage := 0
		for _, d := range days {
			if d != nil {
				coverage++
			}
		}

		spark := []int64{}
		sfrom := 0
		if len(hourly) > 24 {
			sfrom = len(hourly) - 24
		}
		for _, b := range hourly[sfrom:] {
			spark = append(spark, toInt(b[3]))
		}

		byCheckID[id] = OutCheck{
			ID: id, Name: j.check.Name, Note: j.check.Note, Impact: j.check.Impact,
			URL:      firstNonEmpty(j.check.URL, stepsEntryURL(j.check)),
			Critical: j.check.IsCritical(), SlowMs: j.check.SlowMs, Steps: len(j.check.Steps),
			Status: status, Since: since, Ms: r.ms, Code: r.code, Error: r.errText,
			CertDays: r.certDays, CertState: r.certState,
			Uptime: map[string]any{
				"d1":  uptimeFromRaw(raw, 24),
				"d7":  uptimeOverHours(hourly, now, 24*7),
				"d90": uptimeOverDays(daily, now, 90),
			},
			Days: days, Coverage: coverage, Spark: spark,
		}
	}

	out := Summary{Updated: now.Format(time.RFC3339), DeployTargets: cfg.DeployTargets}
	names := map[string]string{}
	for _, p := range cfg.Projects {
		op := OutProject{
			ID: p.ID, Title: p.Title, Subtitle: p.Subtitle, URL: p.URL, Accent: p.Accent,
		}
		// Вердикт проекта считаем по критичным проверкам. Раньше все проверки
		// весили одинаково, и падение внутренней админки описывалось теми же
		// словами, что и падение игрового сервера, без которого не идут матчи.
		for _, c := range p.Checks {
			oc := byCheckID[c.ID]
			op.Checks = append(op.Checks, oc)
			names[c.ID] = incidentName(p.Title, c.Name)
			switch {
			case !oc.Critical:
				switch oc.Status {
				case statusDown:
					op.AuxDown++
				case statusSlow:
					op.AuxSlow++
				}
			default:
				op.Total++
				switch oc.Status {
				case statusUp:
					op.Up++
				case statusSlow:
					op.Up++
					op.Slow++
				}
			}
		}
		for _, u := range p.Units {
			ou := unitState(u.Name)
			ou.Title = u.Title
			if !ou.Active {
				op.UnitsDown++
			}
			op.Units = append(op.Units, ou)
		}
		for _, b := range p.Builds {
			ob := buildInfo(b, client)
			if ob.URL == "" {
				ob.URL = p.URL
			}
			key := p.ID + "::" + b.Title
			// Опрос версии остался, но стал проверкой: версия на проде, о
			// которой не было события, — аномалия, а не запись в историю.
			ob.Anomaly, ob.AnomalySince = anomalies.check(key, ob.Version, releases[key], now)
			ob.History = historyForPage(releases[key])
			op.Builds = append(op.Builds, ob)
		}
		op.Status = projectStatus(op)
		out.Projects = append(out.Projects, op)
	}

	out.Overall = overallStatus(out.Projects)

	// Общий потолок на списки изменений — после того, как все цели дописали
	// свои записи. Поштучные пределы уже применены (trimReleaseChangelogs), но
	// они не знают друг о друге: девятнадцать целей по своему потолку — это
	// файл, которого никто не заказывал.
	capReleaseChangelogs(releases)

	renameIncidents(incidents, names)
	sort.SliceStable(incidents, func(i, j int) bool { return incidents[i].Start > incidents[j].Start })
	if len(incidents) > incidentsKeep {
		incidents = incidents[:incidentsKeep]
	}
	out.Incidents = incidents
	if len(out.Incidents) > 10 {
		out.Incidents = out.Incidents[:10]
	}

	state.Updated = out.Updated
	// Карта необъявленных версий пересобирается целиком: цели, исчезнувшие из
	// конфига, из неё уходят сами, а не копятся до конца времён.
	state.Unexplained = anomalies.next
	if len(state.Unexplained) == 0 {
		state.Unexplained = nil
	}
	if err := writeJSON(filepath.Join(*dataDir, "state.json"), state); err != nil {
		log.Fatalf("не удалось записать state.json: %v", err)
	}
	incidentsPath := filepath.Join(*dataDir, "incidents.json")
	if err := writeJSON(incidentsPath, incidents); err != nil {
		log.Printf("не удалось записать %s: %v", incidentsPath, err)
	}
	releasesPath := filepath.Join(*dataDir, "releases.json")
	if err := writeJSON(releasesPath, releases); err != nil {
		log.Printf("не удалось записать %s: %v", releasesPath, err)
	}
	if err := writeJSON(filepath.Join(*dataDir, "summary.json"), out); err != nil {
		log.Fatalf("не удалось записать summary.json: %v", err)
	}

	var down, slow, aux int
	for _, p := range out.Projects {
		down += p.Total - p.Up
		slow += p.Slow
		aux += p.AuxDown
	}
	log.Printf(
		"проверок: %d, критичных недоступно: %d, медленных: %d, второстепенных недоступно: %d, состояние: %s",
		len(jobs), down, slow, aux, out.Overall,
	)

	// Метрики пишутся последними и не могут сорвать запуск: данные страницы
	// уже на диске, и падать из-за наблюдения за собой было бы обидно.
	if err := writeMetrics(*metricsPath, buildMetrics(out, incidents, time.Since(runStart), now)); err != nil {
		log.Printf("метрики не записаны (%s): %v", *metricsPath, err)
	}
}

// incidentChange — смена состояния одной проверки, из которой рождается или
// закрывается инцидент.
type incidentChange struct {
	id      string
	name    string
	prev    *CheckState
	status  string
	errText string
	at      string
}

// applyIncident ведёт историю падений.
//
// Инцидент — только про недоступность. Переход «работает» → «медленно»
// состояние меняет, но инцидентом не является: иначе история засорится
// деградациями и в ней утонут настоящие падения.
//
// Первое наблюдение (prev == nil) лежащей проверки инцидент ОТКРЫВАЕТ. Раньше
// оно молчало, и падение, заставшее агента без истории — первый запуск, новый
// сервер, вычищенный data/, — не попадало в историю вовсе: до самого
// восстановления его как будто не было. Бот в такой ситуации владельцу пишет
// (см. bot/watch.go, ветка !seen), и молчащая при этом страница ему
// противоречила.
func incidentName(projectTitle, checkName string) string {
	return projectTitle + " · " + checkName
}

// Имя инцидента записывается один раз, в момент падения, и живёт в
// incidents.json до конца срока хранения. Из-за этого переименование проекта
// или проверки оставляло в истории название, которого больше нигде нет:
// в конфиге давно другое, а страница годами показывает старое. Показываем то,
// как проверка называется сейчас; у исчезнувших из конфига проверок остаётся
// записанное когда-то — иначе инцидент лишится имени вовсе.
func renameIncidents(incidents []Incident, names map[string]string) {
	for i := range incidents {
		if name, ok := names[incidents[i].Service]; ok {
			incidents[i].Name = name
		}
	}
}

func applyIncident(incidents []Incident, c incidentChange, now time.Time) []Incident {
	switch {
	case c.status == statusDown && (c.prev == nil || c.prev.Status != statusDown):
		return append([]Incident{{
			Service: c.id, Name: c.name, Start: c.at,
			Reason: firstNonEmpty(c.errText, "недоступен"),
		}}, incidents...)

	case c.prev != nil && c.prev.Status == statusDown && c.status != statusDown:
		// Закрываем самый свежий незакрытый инцидент этой проверки: список
		// отсортирован по убыванию времени начала, поэтому первый найденный
		// он и есть.
		for k := range incidents {
			if incidents[k].Service == c.id && incidents[k].End == "" {
				incidents[k].End = c.at
				if t0, err := time.Parse(time.RFC3339, incidents[k].Start); err == nil {
					incidents[k].DurationMs = now.Sub(t0).Milliseconds()
				}
				break
			}
		}
	}
	return incidents
}

// projectStatus — вердикт по критичным проверкам и состоянию служб.
//
// Второстепенные проверки статус не роняют до «лежит»: они видны на странице
// и считаются в AuxDown, но админка, недоступная для публикации, не должна
// выглядеть как проблема у пользователей, которых она не касается.
//
// Мёртвая служба, наоборот, вердикт меняет. Раньше юниты в него не входили
// вовсе, и получалось так: бот будил владельца сообщением «служба упала»,
// а страница в ту же минуту показывала «Все системы работают». Два ответа на
// один вопрос — худшее, что может делать статус-страница. Роняем именно до
// «частично»: запросы в этот момент часто ещё обслуживаются, и говорить
// «лежит» было бы таким же враньём, только в другую сторону.
func projectStatus(op OutProject) string {
	switch {
	case op.Total == 0:
		// Критичных проверок нет вовсе — судить не по чему.
		if op.AuxDown > 0 || op.UnitsDown > 0 {
			return "degraded"
		}
		return statusUp
	case op.Up == 0:
		return statusDown
	case op.Up < op.Total:
		return "degraded"
	case op.Slow > 0:
		return "degraded"
	case op.UnitsDown > 0:
		return "degraded"
	}
	return statusUp
}

// Общий статус: между «частичным» и «массовым» появилась ступень.
//
// Раньше «массовый сбой» требовал, чтобы легли ВСЕ проверки до единой: три
// полностью мёртвых проекта из четырёх описывались тем же словом, что и
// моргнувшая второстепенная проверка. Теперь смотрим на долю лежащих
// критичных проверок.
func overallStatus(projects []OutProject) string {
	var critical, down, slow, auxDown, unitsDown int
	for _, p := range projects {
		critical += p.Total
		down += p.Total - p.Up
		slow += p.Slow
		auxDown += p.AuxDown
		unitsDown += p.UnitsDown
	}
	switch {
	case critical == 0:
		if unitsDown > 0 || auxDown > 0 {
			return "degraded"
		}
		return "operational"
	case down == critical:
		return "down"
	case float64(down)/float64(critical) >= majorShare:
		return "major"
	case down > 0:
		return "degraded"
	case slow > 0 || auxDown > 0 || unitsDown > 0:
		return "degraded"
	}
	return "operational"
}

// stepsEntryURL — адрес первого шага сценария: у составной проверки своего
// URL нет, а ссылка на странице нужна.
func stepsEntryURL(c Check) string {
	if len(c.Steps) > 0 {
		return c.Steps[0].URL
	}
	return ""
}

// ------------------------------------------------------- журнал выкаток
//
// ИСТОРИЯ РЕЛИЗОВ ДЕЛАЕТСЯ ИЗ СОБЫТИЙ, А НЕ ИЗ НАБЛЮДЕНИЯ.
//
// Раньше запись в releases.json рождалась из разницы двух снимков
// /version.json: агент замечал, что версия стала другой, и дописывал строку.
// Разница существует не всегда, и терялось ровно то же, что терялось в чате:
// три выкатки за минуту давали одну запись, выкатка с откатом за ту же минуту —
// ни одной (версия вернулась на место), провал выкатки — ни одной (версия не
// менялась, сравнивать нечего), автооткат — ни одной. Историю писало
// последствие, а не сама выкатка.
//
// Теперь пишет выкатка: каждая кладёт событие в журнал на диске, а агент его
// читает своим курсором. Контракт журнала — deploy-kit/docs/events.md, ссылки
// на разделы ниже ведут туда; расходиться с ним нельзя, по нему же собран
// писатель и второй читатель (bot/inbox.go).
//
// КАТАЛОГ ЧИТАЕТСЯ ТОЛЬКО НА ЧТЕНИЕ. Удалять обработанное не имеет права ни
// один читатель: их двое, и первый же, кто «уберёт за собой», унесёт
// необработанное у второго. Чистит писатель, по возрасту имени (§11).
//
// Проверки чужого файла — те же, что у бота, и по той же причине: каталог
// наполняет другой пользователь, и разбирать его содержимое положено так, будто
// файл положил кто угодно (§9).

const (
	// defaultEventsDir — журнал событий выкатки (§1). Путь тот же, что у бота:
	// каталог один, читателей двое, и второй адрес означал бы историю не того
	// хозяйства, чей статус показывается.
	defaultEventsDir = "/var/lib/deploy-kit/events"

	// eventsMaxFileBytes — предел §8. Размер смотрится через stat ДО открытия,
	// иначе «предел» означал бы «сначала прочитать гигабайт, потом отказаться».
	eventsMaxFileBytes = 8 << 10

	// eventsMaxPerRun — сколько файлов разбирается за один запуск. Не потолок
	// журнала, а защита такта: агент oneshot, живёт секунды и держит
	// TimeoutStartSec=90, а всё, что не влезло, честно доедет следующей минутой —
	// курсор для этого и нужен.
	eventsMaxPerRun = 1000

	// eventsChangelogKeep — пунктов из одного события. Контракт разрешает
	// писателю двадцать, здесь стоит тот же changelogKeep, что и у version.json:
	// АГЕНТ НЕ СТРОЖЕ ТОГО, КТО ПИШЕТ. Двадцать первый пункт, доехавший до
	// агента, — повод показать его, а не молча укоротить релиз; сотня остаётся
	// потолком против вранья.
	eventsChangelogKeep = changelogKeep

	// anomalyNoEvent — единственная пока аномалия: прод раздаёт версию, о
	// выкатке которой события не было.
	anomalyNoEvent = "no_event"

	// anomalyGrace — сколько версии позволено оставаться необъявленной, прежде
	// чем это назовут аномалией.
	//
	// Пауза не для мягкости, а против ложной тревоги: version.json на проде
	// меняется в момент переключения релиза, а событие об успехе пишется ПОСЛЕ
	// проверок — health, verify (до 120 с), сверка версии, соседние цели. Агент
	// в это окно попадает регулярно, потому что ходит раз в минуту. Десять минут
	// длиннее самой долгой выкатки хозяйства и заметно короче того, за что можно
	// упустить настоящую выкатку мимо пайплайна.
	anomalyGrace = 10 * time.Minute
)

// eventFileRe — единственный вход в журнал: всё, что не подошло, для читателя
// не существует (§3). Недописанный «.json.tmp» не подходит ни при каком
// раскладе, поэтому файл посреди записи невидим, даже если писателю однажды
// заменят rename на cp.
//
// Тринадцать цифр фиксированной ширины — то, на чём держится курсор:
// лексикографический порядок имён совпадает с хронологическим только при
// одинаковой длине числа (§2).
var eventFileRe = regexp.MustCompile(
	`^([0-9]{13})-([a-z0-9][a-z0-9._-]{0,63})-(started|success|failure|rolled_back|rollback|published)\.json$`)

// eventHex64Re — форма id (§5). Одна на CI и на локальную выкатку: внутрь никто
// не заглядывает, прообраз остаётся у писателя.
var eventHex64Re = regexp.MustCompile(`^[0-9a-f]{64}$`)

// eventAppRe — тот же набор, что у имени каталога релиза: app попадает в имя
// файла, и это единственное, что стоит между чужой строкой и записью в соседний
// каталог (§4).
var eventAppRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// eventVersionRe — набор app плюс «+»: release-20260805-101502-1a2b3c4,
// manual-…, собственная версия проекта у целей с VERSION_CMD. Версия уезжает и
// в историю на публичной странице, поэтому алфавит закрытый, а не «любая строка
// до 128 байт».
var eventVersionRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._+-]{0,127}$`)

// rawEvent — событие как оно лежит на диске, то есть недоверенные данные.
// Полей меньше, чем в контракте: агенту нужно ровно то, из чего делается запись
// истории. Лишний ключ разбор не роняет (иначе новое поле требовало бы
// одновременного обновления обеих сторон), но и в файл агента не попадает.
type rawEvent struct {
	V         int      `json:"v"`
	ID        string   `json:"id"`
	Kind      string   `json:"kind"`
	App       string   `json:"app"`
	At        string   `json:"at"`
	Version   string   `json:"version"`
	Changelog []string `json:"changelog"`
}

// deployEvent — проверенное событие выкатки. Всё, что здесь лежит, прошло
// проверку типа, длины, перечисления и очистку от управляющих символов.
type deployEvent struct {
	File    string
	ID      string
	Kind    string
	App     string
	At      string
	Version string
	// Changelog — простой текст: разметки в событии не бывает по контракту
	// (§4), писатель разворачивает её обратно в текст. Разметку навешивает
	// читатель — бот при отправке, страница при рисовании.
	Changelog []string
}

// Виды события (§4). Историей становятся не все: в releases.json попадает то,
// после чего прод раздаёт названную версию.
const (
	evSuccess    = "success"     // выкатилось и проверилось
	evPublished  = "published"   // опубликован файл
	evRollback   = "rollback"    // ручной откат: version — релиз, НА который откатились
	evRolledBack = "rolled_back" // автооткат: version — та, которую выкатывали и сняли
)

// eventMakesHistory — попадает ли событие в историю выкаток.
//
// started и failure не попадают: первый служебный, второй говорит, что на проде
// НЕ появилось ничего нового, — истории выкаток нечего о нём записать (в чате об
// этом говорит бот, и это его работа, а не работа журнала релизов).
//
// rolled_back тоже не попадает, и это не оплошность: версия в нём — та, которую
// выкатывали и сняли автооткатом, а история отвечает на вопрос «что работало на
// проде». Приняв её за релиз, страница показала бы версию, которой на проде не
// было ни минуты. Про возврат на прежний релиз расскажет rollback от того же
// release.sh.
func eventMakesHistory(kind string) bool {
	switch kind {
	case evSuccess, evPublished, evRollback:
		return true
	}
	return false
}

// buildApp — id цели выкатки, которой принадлежит сборка.
//
// Явное App из конфига главнее всего. Догадка для целей типа "release" честнее,
// чем кажется: путь задаёт сама выкатка, /opt/<APP>/current — это её раскладка
// (server/release.sh), и производное от неё имя совпадает с APP по построению, а
// не по совпадению. Не совпало — событие просто не найдёт цель и скажет об этом
// в журнал.
func buildApp(b Build) string {
	if b.App != "" {
		return b.App
	}
	if b.Type != "release" {
		return ""
	}
	app := filepath.Base(filepath.Dir(filepath.Clean(b.Path)))
	if !eventAppRe.MatchString(app) {
		return ""
	}
	return app
}

// eventKeys — карта «id цели выкатки → ключ истории».
//
// Ключ истории остаётся прежним, «проект::цель»: по нему живут и страница, и
// экран /changelog в боте (bot/changelog.go, services). Событие приносит другой
// id — id цели выкатки, — и связать их можно только здесь.
//
// Одинаковый id у двух целей — это не «возьмём первую»: две цели одного APP
// затирали бы историю друг друга, и заметить это можно было бы только по
// расходящимся спискам изменений. Обе снимаются с карты, о чём говорится вслух.
func eventKeys(cfg Config) map[string]string {
	keys := map[string]string{}
	dup := map[string]bool{}
	for _, p := range cfg.Projects {
		for _, b := range p.Builds {
			app := buildApp(b)
			if app == "" {
				continue
			}
			key := p.ID + "::" + b.Title
			if prev, ok := keys[app]; ok && prev != key {
				log.Printf("id цели %q встречается дважды: %q и %q — события этой цели в историю не пойдут",
					app, prev, key)
				dup[app] = true
				continue
			}
			keys[app] = key
		}
	}
	for app := range dup {
		delete(keys, app)
	}
	return keys
}

// readEvents — что нового в журнале после курсора.
//
// Второе значение — новый курсор. Он двигается и на пропущенном файле:
// испорченное событие не имеет права остановить журнал, иначе одна опечатка
// писателя навсегда останавливает историю выкаток.
func readEvents(dir string, sinceMs int64, limit int) ([]deployEvent, int64) {
	// Каталога может не быть вовсе: агент обязан работать на машине, где
	// deploy-kit ещё не раскладывали, и на своей же машине разработчика.
	ents, err := os.ReadDir(dir)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("журнал выкаток не прочитан (%s): %v", dir, err)
		}
		return nil, sinceMs
	}

	// os.ReadDir отдаёт записи отсортированными по имени, а имя события
	// устроено так, что лексикографический порядок совпадает с хронологическим
	// (§2). Поэтому сортировки по времени из содержимого здесь нет и быть не
	// должно: разбор мусорного файла не влияет на продвижение по журналу.
	var names []string
	for _, e := range ents {
		if eventFileRe.MatchString(e.Name()) {
			names = append(names, e.Name())
		}
	}
	if len(names) == 0 {
		return nil, sinceMs
	}

	if sinceMs <= 0 {
		// Курсора нет: новая установка, потерянный state.json или первый запуск
		// после перехода на события. Разобрать журнал целиком значило бы
		// дописать в историю две недели выкаток, о которых записи уже есть, —
		// сделанные прежней механикой, без id события, то есть неотличимые от
		// новых. Считаем журнал прочитанным и говорим об этом в лог.
		last := eventMs(names[len(names)-1])
		log.Printf("журнал выкаток: курсора нет, %d файлов приняты за прочитанные до %d", len(names), last)
		return nil, last
	}

	// os.Root вместо голого пути — то же, что O_NOFOLLOW, только переносимо и
	// на весь путь сразу: каталог открывается один раз, а имена внутри него
	// разрешаются без выхода наружу. Симлинк, положенный в каталог событий, не
	// должен превращаться в чтение файла, до которого агенту дела нет.
	root, err := os.OpenRoot(dir)
	if err != nil {
		log.Printf("журнал выкаток не открыт (%s): %v", dir, err)
		return nil, sinceMs
	}
	defer func() { _ = root.Close() }()

	cursor := sinceMs
	taken := 0
	var out []deployEvent
	for _, name := range names {
		ms := eventMs(name)
		// Не «строго больше»: курсор помнит миллисекунду, а не имя, и события
		// той же миллисекунды у другой цели иначе потерялись бы навсегда.
		// Повторный разбор безвреден — от повтора защищает id (applyEvent).
		if ms < sinceMs {
			continue
		}
		if taken >= limit {
			break
		}
		taken++
		if ms > cursor {
			cursor = ms
		}
		if ev, ok := readEvent(root, name); ok {
			out = append(out, ev)
		}
	}
	return out, cursor
}

// eventMs — миллисекунда из имени файла. Имя уже проверено выражением, поэтому
// разбор не может провалиться, а ошибку всё равно не игнорируем молча: ноль
// поставит файл в начало и заставит перечитать его следующим запуском.
func eventMs(name string) int64 {
	m := eventFileRe.FindStringSubmatch(name)
	if m == nil {
		return 0
	}
	ms, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return 0
	}
	return ms
}

// readEvent разбирает один файл журнала. Второе значение — годится ли событие
// для истории; ошибка здесь никогда не останавливает обход.
func readEvent(root *os.Root, name string) (deployEvent, bool) {
	m := eventFileRe.FindStringSubmatch(name)
	if m == nil {
		return deployEvent{}, false
	}

	// Размер смотрится ДО открытия, и заодно проверяется, что это обычный файл:
	// Lstat не идёт по симлинку, поэтому подсунутая ссылка отсеивается здесь, а
	// не после того, как агент прочитал, куда она ведёт.
	fi, err := root.Lstat(name)
	if err != nil {
		log.Printf("событие %s не прочитано: %v", name, err)
		return deployEvent{}, false
	}
	if !fi.Mode().IsRegular() {
		log.Printf("событие %s пропущено: не обычный файл (%s)", name, fi.Mode())
		return deployEvent{}, false
	}
	if fi.Size() > eventsMaxFileBytes {
		log.Printf("событие %s пропущено: %d байт при пределе %d", name, fi.Size(), eventsMaxFileBytes)
		return deployEvent{}, false
	}

	f, err := root.Open(name)
	if err != nil {
		log.Printf("событие %s не открыто: %v", name, err)
		return deployEvent{}, false
	}
	defer func() { _ = f.Close() }()

	// Файл мог подрасти между stat и open, поэтому читается на байт больше
	// предела: чтение до конца при обмане в stat означало бы предел, которого
	// нет.
	b, err := io.ReadAll(io.LimitReader(f, eventsMaxFileBytes+1))
	if err != nil {
		log.Printf("событие %s не дочитано: %v", name, err)
		return deployEvent{}, false
	}
	if len(b) > eventsMaxFileBytes {
		log.Printf("событие %s пропущено: больше %d байт", name, eventsMaxFileBytes)
		return deployEvent{}, false
	}

	var raw rawEvent
	if err := json.Unmarshal(b, &raw); err != nil {
		// Обрезанный файл, мусор, чужой формат — обычный вход, а не
		// исключительный. Разбор без паники, пропуск и запись в лог.
		log.Printf("событие %s не разобрано: %v", name, cleanEventText(err.Error()))
		return deployEvent{}, false
	}
	return cleanEvent(raw, name, m[2], m[3])
}

// cleanEvent проверяет разобранное событие и вычищает из него всё, что не
// является текстом.
//
// Событие отвергается только тогда, когда без поля запись истории невозможна
// или была бы неверной: не та версия схемы, не sha256 в id, несовпадение имени
// файла с содержимым, отсутствующая или непохожая на версию версия. Всё
// описательное — список изменений, время — при непрохождении проверки просто
// исчезает.
func cleanEvent(raw rawEvent, file, fileApp, fileKind string) (deployEvent, bool) {
	if raw.V != 1 {
		log.Printf("событие %s пропущено: версия схемы %d", file, raw.V)
		return deployEvent{}, false
	}
	if !eventHex64Re.MatchString(raw.ID) {
		log.Printf("событие %s пропущено: id не sha256", file)
		return deployEvent{}, false
	}
	// Имя файла и содержимое обязаны говорить одно и то же. Разойдясь, они
	// сломали бы всё, что считается по имени: порядок и курсор. Дешёвая
	// проверка на месте вместо дорогого расследования потом.
	if raw.App != fileApp || raw.Kind != fileKind {
		log.Printf("событие %s пропущено: имя файла не совпадает с содержимым", file)
		return deployEvent{}, false
	}
	if !eventAppRe.MatchString(raw.App) {
		log.Printf("событие %s пропущено: app=%q", file, cleanEventText(raw.App))
		return deployEvent{}, false
	}

	ev := deployEvent{File: file, ID: raw.ID, Kind: raw.Kind, App: raw.App}

	// Версия — то единственное, ради чего запись существует: запись истории без
	// версии не отвечает ни на один вопрос, который к истории задают.
	if !eventVersionRe.MatchString(raw.Version) {
		log.Printf("событие %s пропущено: version=%q", file, cleanEventText(raw.Version))
		return deployEvent{}, false
	}
	ev.Version = raw.Version

	// Время события полезно, но не обязательно: без него запись останется с
	// одним Seen, и это лучше, чем потерянная выкатка.
	if t, err := time.Parse(time.RFC3339, raw.At); err == nil {
		ev.At = t.UTC().Format(time.RFC3339)
	} else if raw.At != "" {
		log.Printf("событие %s: at=%q не RFC3339, запись останется без времени выкатки",
			file, cleanEventText(raw.At))
	}

	ev.Changelog = cleanEventChangelog(raw.Changelog)
	return ev, true
}

// cleanEventChangelog приводит список изменений из события к тому, что можно
// положить в файл, который раздаётся по HTTP.
//
// Разметки в событии не бывает по контракту, но проверяется это здесь, а не там:
// releases.json читают бот и страница, и бот признаёт ссылкой пункт, который
// выглядит как ссылка на PR (bot/format.go, refLink). Пропустив чужой «<a
// href="https://…">#1</a>» из события, агент подарил бы кому угодно кликабельный
// адрес от имени бота, которому владелец доверяет по определению.
func cleanEventChangelog(items []string) []string {
	var out []string
	chars := changelogChars
	for _, raw := range items {
		if len(out) >= eventsChangelogKeep {
			break
		}
		s := cutRunes(cleanEventText(raw), changelogLineChars)
		// Якорь снимается ПОСЛЕ обрезки, а не до: обрезка отбрасывает хвост и
		// способна открыть концом пункта то, что раньше стояло в середине.
		s = dropRefLinkMarkup(s)
		if s == "" {
			continue
		}
		// Общий потолок списка проверяется целым пунктом и ДО добавления:
		// полпункта в истории читаются как рассказ о другой выкатке. Первый
		// пункт проходит всегда — список из одной строки не бывает слишком
		// длинным, он бывает единственным, что есть сказать.
		n := utf8.RuneCountInString(s)
		if n > chars && len(out) > 0 {
			break
		}
		chars -= n
		out = append(out, s)
	}
	return out
}

// cleanEventText делает из недоверенной строки текст.
//
// Убирается три класса символов, и каждый — не гигиена, а известный приём:
//   - битые последовательности UTF-8: такую строку Telegram отвергает целиком,
//     то есть выкатка молча остаётся необъявленной;
//   - управляющие символы: CR и LF в поле подделывают строки journald, то есть
//     позволяют написать в журнал агента что угодно от его имени, а на странице
//     разрывают пункт пополам;
//   - U+202E и соседи: переворот направления текста показывает не то, что
//     написано, — этим маскируют и адреса, и имена.
//
// Правила те же, что у бота (bot/inbox.go, cleanEventText): расхождение означало
// бы, что в чате и в истории один и тот же пункт выглядит по-разному.
func cleanEventText(s string) string {
	s = strings.ToValidUTF8(s, "")
	s = strings.Map(func(r rune) rune {
		switch {
		case r == '\t' || r == '\n' || r == '\r':
			return ' '
		case r < 0x20 || r == 0x7f:
			return -1
		case r >= 0x80 && r <= 0x9f:
			return -1
		case r >= 0x202a && r <= 0x202e: // LRE, RLE, PDF, LRO, RLO
			return -1
		case r >= 0x2066 && r <= 0x2069: // LRI, RLI, FSI, PDI
			return -1
		case r == 0x200e || r == 0x200f: // LRM, RLM
			return -1
		case r == 0xfeff:
			return -1
		}
		return r
	}, s)
	// Пункт обязан быть однострочным и без двойных пробелов: перевод строки
	// внутри темы разорвал бы список пополам уже в сообщении.
	return strings.Join(strings.Fields(s), " ")
}

// applyEvent дописывает событие выкатки в историю цели.
//
// Дедупликация — по id события, и только по нему. Ни версия, ни время для этого
// не годятся: две разные выкатки одной цели могут нести одну и ту же версию
// (перевыкатка после отката, повтор прогона после починки инфраструктуры), и
// схлопнув их, мы потеряли бы настоящую выкатку — то самое, ради чего события и
// заводились.
func applyEvent(history []Release, ev deployEvent, now time.Time) []Release {
	if !eventMakesHistory(ev.Kind) || ev.Version == "" || ev.ID == "" {
		return history
	}
	for _, r := range history {
		if r.EventID == ev.ID {
			return history
		}
	}
	history = append([]Release{{
		Version:   ev.Version,
		At:        ev.At,
		Seen:      now.Format(time.RFC3339),
		EventID:   ev.ID,
		Changelog: changelogField(ev.Changelog),
	}}, history...)
	if len(history) > releasesKeep {
		history = history[:releasesKeep]
	}
	return trimReleaseChangelogs(history)
}

// historyHasVersion — есть ли в истории запись об этой версии.
//
// Смотрится вся история, а не головная запись: между выкаткой и запуском агента
// могла случиться вторая выкатка, и версия, которую прод раздаёт сейчас, вполне
// может лежать не первой. Записи без id события тоже считаются — их сделала
// прежняя механика, и объявлять аномалией то, что она успела записать, значило
// бы поднять тревогу на всём хозяйстве в день перехода.
func historyHasVersion(history []Release, version string) bool {
	for _, r := range history {
		if r.Version == version {
			return true
		}
	}
	return false
}

// anomalyTracker — опрос /version.json как ПРОВЕРКА.
//
// Опрос остался, но перестал быть источником истории. Его новый смысл: версия
// на проде, о которой не было события, — это аномалия. Либо выкатили мимо
// пайплайна (руками на сервере), либо событие потерялось; различить эти два
// случая агент не может и не пытается — важно, что оба видны.
//
// Прежнее состояние читается, новое собирается заново: так из state.json уходят
// цели, которых больше нет в конфиге, — иначе карта копила бы их вечно.
type anomalyTracker struct {
	prev map[string]Unexplained
	next map[string]Unexplained
}

func newAnomalyTracker(prev map[string]Unexplained) *anomalyTracker {
	return &anomalyTracker{prev: prev, next: map[string]Unexplained{}}
}

// check возвращает аномалию для цели: код и момент, с которого версия остаётся
// необъявленной. Пустой код — обычное состояние.
func (a *anomalyTracker) check(key, version string, history []Release, now time.Time) (string, string) {
	if version == "" || historyHasVersion(history, version) {
		return "", ""
	}
	since := now
	if p, ok := a.prev[key]; ok && p.Version == version {
		// Версия та же, что и в прошлый раз: считаем от первого наблюдения, а
		// не от текущего, иначе отсчёт обнулялся бы каждую минуту и аномалия не
		// наступала бы никогда.
		if t, err := time.Parse(time.RFC3339, p.Since); err == nil {
			since = t.UTC()
		}
	}
	rec := Unexplained{Version: version, Since: since.Format(time.RFC3339)}
	a.next[key] = rec
	if now.Sub(since) < anomalyGrace {
		// Выкатка, возможно, ещё идёт: version.json меняется до того, как
		// событие об успехе написано. Молчим, но помним с какого момента.
		return "", ""
	}
	log.Printf("аномалия: %s раздаёт версию %s с %s, события о её выкатке не было — выкатили мимо пайплайна или событие потерялось",
		key, version, rec.Since)
	return anomalyNoEvent, rec.Since
}

// trimReleaseChangelogs удерживает releases.json в берегах: список изменений
// остаётся только у свежих записей и не длиннее отведённого ему места.
//
// Обрезаем всю историю на каждом запуске, а не только что дописанную запись.
// Файл читается с диска: его мог записать агент другой версии, с другими
// пределами, или чинили руками. Предел, который держится только на записи, —
// не предел, а договорённость с самим собой.
func trimReleaseChangelogs(history []Release) []Release {
	for i := range history {
		if i >= releaseChangelogKeep {
			// Сама запись остаётся: «когда что выкатывали» страница и бот
			// показывают на всю глубину releasesKeep. Дорог только текст.
			history[i].Changelog = nil
			continue
		}
		history[i].Changelog = cutChangelog(history[i].Changelog)
	}
	return history
}

// cutChangelog режет один список до releaseChangelogLines пунктов и
// releaseChangelogChars символов суммарно, не разрезая символ UTF-8.
//
// Бюджет считается в символах, как и всё остальное в этой цепочке: в байтах он
// для кириллицы был вдвое строже написанного и обрывал в истории тот самый
// список, который целиком ушёл в чат.
//
// Пункт либо влезает целиком, либо не берётся вовсе. Раньше он дорезался по
// остатку бюджета, и это было безопасно, пока в пункте не было разметки:
// теперь в конце может стоять ссылка на PR, а половина тега — это отказ
// Telegram, то есть молча не пришедшее сообщение. Первый пункт берём всегда.
func cutChangelog(in changelogField) changelogField {
	var out changelogField
	budget := releaseChangelogChars
	for _, s := range in {
		if len(out) >= releaseChangelogLines {
			break
		}
		if s == "" {
			continue
		}
		n := utf8.RuneCountInString(s)
		if n > budget && len(out) > 0 {
			break
		}
		budget -= n
		out = append(out, s)
	}
	return out
}

// capReleaseChangelogs удерживает в берегах ВЕСЬ файл, а не каждую запись по
// отдельности.
//
// Поштучных пределов мало: они умножаются на число целей, а releases.json
// лежит в вебруте и переписывается раз в минуту. Считаем суммарную длину всех
// сохранённых списков и, пока она выше releasesChangelogTotal, снимаем список
// со САМОЙ СТАРОЙ записи. Порядок обхода задан явной сортировкой, потому что
// обход карты в Go случаен: без этого файл при каждом запуске терял бы разные
// записи, и «что приехало вчера» пропадало бы через раз.
//
// Сама запись остаётся: дата и версия стоят дёшево, и история выкаток на всю
// глубину releasesKeep никуда не девается. Дорог только текст.
func capReleaseChangelogs(releases map[string][]Release) {
	type entry struct {
		key   string
		i     int
		when  string
		chars int
	}
	var all []entry
	total := 0
	for key, history := range releases {
		for i, r := range history {
			n := 0
			for _, s := range r.Changelog {
				n += utf8.RuneCountInString(s)
			}
			if n == 0 {
				continue
			}
			total += n
			all = append(all, entry{key: key, i: i, when: firstNonEmpty(r.Seen, r.At), chars: n})
		}
	}
	if total <= releasesChangelogTotal {
		return
	}
	// Сначала самые старые. Ключ и позиция — не для красоты: у записей одного
	// запуска время совпадает до секунды, и без них порядок снова стал бы
	// случайным.
	sort.Slice(all, func(a, b int) bool {
		x, y := all[a], all[b]
		if x.when != y.when {
			return x.when < y.when
		}
		if x.key != y.key {
			return x.key < y.key
		}
		return x.i > y.i
	})
	for _, e := range all {
		if total <= releasesChangelogTotal {
			return
		}
		releases[e.key][e.i].Changelog = nil
		total -= e.chars
	}
}

// historyForPage — та же история, но без списков изменений.
//
// summary.json страница тянет целиком раз в минуту, а из истории показывает
// только версию и дату (src/pages/index.astro, buildRow). Список изменений
// нужен там ровно один — у текущей версии, и он лежит рядом, в
// OutBuild.Changelog. Класть в summary.json ещё и пять changelog'ов на каждую
// цель значило бы платить трафиком каждой загрузки страницы за то, что с неё
// никогда не спрашивают: за этим ходят в releases.json и только по запросу.
func historyForPage(history []Release) []Release {
	if len(history) == 0 {
		return nil
	}
	out := make([]Release, len(history))
	for i, r := range history {
		r.Changelog = nil
		out[i] = r
	}
	return out
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
