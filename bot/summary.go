package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// Структуры повторяют формат summary.json, который пишет агент (agent/main.go).
//
// Бот сознательно не делает собственных проверок: два независимых обхода
// расходились бы во времени, и на странице и в телеграме было бы разное
// состояние одного и того же сервиса. Единственный источник правды — файл
// агента, бот его только читает и пересказывает.
//
// Описаны не все поля: спарклайны времени ответа нужны только странице.
// А вот история по дням пригодилась — из неё бот рисует полоску доступности
// цветными квадратами, и две недели умещаются в одну строку сообщения.

type Summary struct {
	Updated   string     `json:"updated"`
	Overall   string     `json:"overall"`
	Projects  []Project  `json:"projects"`
	Incidents []Incident `json:"incidents"`
	// DeployTargets — как называть цели выкатки: id из события → имя, адрес и
	// проект. Заводится в config/status.json и переносится агентом как есть.
	//
	// Без неё цель показывалась своим id: реестр собирался из проверок и
	// проектов, а id цели выкатки совпадает с ними лишь по случайности. У
	// целей лаунчера не совпадал ни один, и в ленте стояло «chillhub-api» без
	// ссылки — в том числе в шапке карточки прогона.
	DeployTargets map[string]DeployTarget `json:"deployTargets,omitempty"`
}

type Project struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	URL    string `json:"url"`
	Status string `json:"status"`
	// Up/Total — по критичным проверкам: они определяют вердикт проекта.
	Up    int `json:"up"`
	Total int `json:"total"`
	// Второстепенные проверки не в порядке. Вердикт они не роняют, но
	// умолчать о них нельзя: иначе сломанная админка исчезает из отчёта.
	AuxDown int     `json:"auxDown"`
	AuxSlow int     `json:"auxSlow"`
	Slow    int     `json:"slow"`
	Checks  []Check `json:"checks"`
	Units   []Unit  `json:"units"`
	Builds  []Build `json:"builds"`
}

type Check struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	URL  string `json:"url"`
	// Status: "up", "slow" или "down". «Медленно» — не падение: сервис
	// отвечает, просто дольше порога, и будить этим владельца нельзя.
	Status string `json:"status"`
	// Critical: падение второстепенной проверки не роняет вердикт проекта.
	Critical bool   `json:"critical"`
	Impact   string `json:"impact"`
	Since    string `json:"since"`
	Ms       int64  `json:"ms"`
	Code     int    `json:"code"`
	Error    string `json:"error"`
	// Уровень доступности за сутки/неделю/90 дней. Значение может быть null,
	// пока замеров нет, поэтому указатель, а не число.
	Uptime    map[string]*float64 `json:"uptime"`
	CertDays  *int                `json:"certDays"`
	CertState string              `json:"certState"`
	// Days — 90 ячеек по календарю; null там, где замеров за сутки нет.
	// Указатель именно поэтому: «нет данных» и «ноль доступности» — разное.
	Days []*Day `json:"days"`
}

type Day struct {
	D     string `json:"d"`
	Up    int64  `json:"up"`
	Total int64  `json:"total"`
}

type Unit struct {
	Name   string `json:"name"`
	Title  string `json:"title"`
	Active bool   `json:"active"`
	State  string `json:"state"`
	Since  string `json:"since"`
}

type Build struct {
	Title   string `json:"title"`
	Version string `json:"version"`
	At      string `json:"at"`
	// Адрес самого компонента: в сообщении о релизе открывать надо то, что
	// обновилось, а не проект целиком.
	URL string `json:"url"`
	// CommitURL — адрес коммита, из которого собрана эта версия. Его кладёт
	// агент (agent/main.go, commitURL), выведя из данных самой выкатки.
	//
	// Поля может не быть, и это нормальный случай: у цели, чья версия читается
	// симлинком релиза, version.json нет вовсе, а значит нет ни коммита, ни
	// адреса репозитория. Сообщение о релизе в этом случае обязано выйти
	// прежним — с версией обычным текстом.
	//
	// Приезжает ГОЛЫМ АДРЕСОМ, без разметки, и проверяется здесь ещё раз
	// (format.go, commitURLRe): summary.json может быть собран мимо агента —
	// это описано выше как рабочий путь.
	CommitURL string `json:"commitURL,omitempty"`
	// Changelog — короткий список изменений этой версии, по строке на пункт,
	// обычным текстом и без разметки. Его кладёт выкатка в version.json, а
	// агент переносит сюда (agent/main.go, changelogField).
	//
	// Поля может не быть, и это нормальный случай: цель, чья выкатка ничего
	// не публикует, обязана давать ровно то же уведомление о релизе, что и
	// раньше. Текст остаётся чужим до самой отправки — экранирует его
	// formatChangelog.
	Changelog changelogField `json:"changelog,omitempty"`
}

// changelogField — список изменений, разбор которого не может провалиться.
//
// Тип нужен ради одного: чужая форма необязательного поля не должна стоить
// владельцу ВСЕХ уведомлений. Ошибка разбора здесь — это ошибка loadSummary,
// то есть цикл наблюдения на каждом обходе пишет «данные агента не прочитаны»
// и молчит обо всём: о падениях, о восстановлениях, о релизах. Цена
// украшения в этот момент — весь бот, а не строчка в сообщении.
//
// Обычная форма — массив строк, его пишет агент. Строку с переводами строк
// принимаем на случай, когда summary.json собран мимо агента.
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

type Incident struct {
	Service    string `json:"service"`
	Name       string `json:"name"`
	Start      string `json:"start"`
	End        string `json:"end"`
	Reason     string `json:"reason"`
	DurationMs int64  `json:"durationMs"`
}

func loadSummary(path string) (*Summary, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s Summary
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &s, nil
}

// parseTime разбирает время в формате, которым пишет агент (RFC3339).
// Пустая или битая строка — не ошибка: у остановленного юнита времени
// запуска просто нет, и сообщение должно собраться без него.
func parseTime(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, false
	}
	return t.UTC(), true
}

// DeployTarget — строка таблицы имён целей выкатки. Смысл полей — там же, где
// она заводится: agent/main.go.
type DeployTarget struct {
	Project string `json:"project"`
	Title   string `json:"title"`
	URL     string `json:"url,omitempty"`
}
