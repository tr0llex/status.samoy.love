package main

import "strings"

// Кнопки под сообщениями.
//
// Инлайн-клавиатура здесь не украшение: без неё каждый повторный взгляд на
// статус — это набрать команду с телефона, а каждый ответ — новое сообщение
// в ленте. С кнопками владелец жмёт «Обновить», и карточка переписывается
// на месте.
//
// callback_data ограничен 64 байтами, поэтому в него кладём короткий ключ
// экрана, а не состояние: всё, что нужно для отрисовки, и так лежит в
// summary.json.
// Адреса приходят из конфига (config.go) — единственного места настроек.
// Пакетные переменные, а не параметр каждой функции: клавиатура рисуется в
// десятке мест, и таскать через все них две строки незачем.
var (
	statusPageURL = "https://status.samoy.love/"
	miniApp       = "https://status.samoy.love/tg/"
	// groupChat — отрицательный Owner (см. config.go) уже отличает группу от
	// личной переписки для прав, но до этой правки то же число никак не
	// влияло на openButton(). Telegram документирует web_app как «доступно
	// только в личных чатах»: попытка отправить его в группе — это отказ
	// sendMessage целиком, а не пропавшая кнопка, то есть немота бота во
	// ВСЕХ клавиатурах разом (openButton стоит в navRows и alertKeyboard).
	groupChat = false
)

// applyConfig задаёт адреса один раз при старте.
func applyConfig(c Config) {
	statusPageURL = c.StatusURL
	miniApp = c.MiniApp
	groupChat = c.Owner < 0
}

// Действия под уведомлением. Ночью нужна не навигация по экранам, а способ
// прекратить поток: сервис уже чинится, а напоминания продолжают приходить.
const (
	ActMute2h = "a:mute:2h"
	ActMute8h = "a:mute:8h"
	ActUnmute = "a:unmute"
)

// ActWhatNowPrefix — «Что сейчас» на сообщении, которое НЕ является экраном:
// уведомление о падении, карточка прогона выкатки, подтверждение тишины.
//
// Правило владения сообщением: EditLong (перерисовка на месте) годится
// только для сообщения, которое бот сам нарисовал КАК ЭКРАН, — навигационные
// Статус/Инциденты/Изменения/Справка, где кнопки внизу и так листают то же
// самое сообщение. Уведомление, карточка прогона и подтверждение
// тишины — это СОБЫТИЯ, а не экраны: их перерисовка стирает саму причину,
// по которой сообщение в переписке, — текст аварии, кнопки тишины под ним,
// факт, что цель катилась. Нажатие «Что сейчас» на таком сообщении обязано
// открыть НОВЫЙ экран, а не заменить собой уже написанное.
//
// Префикс, а не своя константа под каждый экран: кнопка ведёт то на общий
// статус, то на экран конкретного проекта, а заводить под каждый случай
// отдельные действие и обработчик значило бы дублировать handleCallback ради
// одной и той же развилки «открыть новым сообщением».
const ActWhatNowPrefix = "a:now:"

// whatNowButton — «Что сейчас», открывающее view НОВЫМ сообщением.
func whatNowButton(view string) Button {
	return Button{Text: "Что сейчас", CallbackData: ActWhatNowPrefix + view}
}

const (
	ViewStatus    = "v:status"
	ViewIncidents = "v:incidents"
	ViewHelp      = "v:help"
	// ViewProject — экран одного проекта, ключ вида "v:p:metro".
	// Подробности переехали сюда с общего экрана: раньше /status печатал все
	// проекты со всеми проверками, службами и полосками, и единственную
	// красную строку приходилось искать глазами среди двух десятков зелёных.
	ViewProject = "v:p:"
	// ViewChangelog — что менялось по всему хозяйству, последняя выкатка каждой
	// цели. ViewChangelogOf — то же по одной цели, ключ вида "v:cl:metro".
	//
	// Раньше список изменений можно было увидеть ровно один раз — в уведомлении
	// о релизе в момент выкатки. Пропустил сообщение (или спишь, или его
	// придержала тишина) — и узнать, что уехало, уже негде: бот живёт на
	// сервере, где нет ни одного из выкаченных репозиториев. Журнал выкаток
	// агента (releases.json) хранит это и после отправки.
	ViewChangelog   = "v:cl"
	ViewChangelogOf = "v:cl:"
)

// callbackDataMax — предел Telegram на callback_data кнопки.
//
// Считается в БАЙТАХ, и превышение — не молчаливая обрезка, а отказ отправить
// сообщение целиком: ответ на /changelog с длинным аргументом просто не пришёл
// бы. Поэтому аргумент, уезжающий в ключ экрана, обрезается заранее.
const callbackDataMax = 64

// changelogOfView возвращает запрошенную цель, если это экран одной цели.
func changelogOfView(view string) (string, bool) {
	if !strings.HasPrefix(view, ViewChangelogOf) {
		return "", false
	}
	q := strings.TrimPrefix(view, ViewChangelogOf)
	return q, q != ""
}

// projectOfView возвращает id проекта, если это экран проекта.
func projectOfView(view string) (string, bool) {
	if !strings.HasPrefix(view, ViewProject) {
		return "", false
	}
	id := strings.TrimPrefix(view, ViewProject)
	return id, id != ""
}

// Кнопка мини-приложения работает только в личной переписке и только по
// https. Если адрес почему-то не https, или чат — группа, отдаём обычную
// ссылку: пусть откроется в браузере, но кнопка не пропадёт, а сообщение с
// ней не откажется отправляться целиком.
func openButton() Button {
	if !groupChat && strings.HasPrefix(miniApp, "https://") {
		return Button{Text: "Открыть", WebApp: &WebApp{URL: miniApp}}
	}
	return Button{Text: "Открыть", URL: miniApp}
}

// Пометка текущего экрана. Точка перед подписью вместо обрамления с двух
// сторон: Telegram и так центрирует текст кнопки, а симметричные точки
// читались как часть названия.
func mark(view, current, label string) string {
	if view == current {
		return "· " + label
	}
	return label
}

// navKeyboard — клавиатура под экраном.
//
// Эмодзи убраны из навигации намеренно: значок несёт смысл там, где сообщает
// состояние (кнопка проекта, строка проверки). В кнопке «Версии» он ничего не
// сообщает и превращает панель в рябь.
func navKeyboard(current string) *Keyboard {
	return &Keyboard{InlineKeyboard: navRows(current)}
}

func navRows(current string) [][]Button {
	return [][]Button{
		{
			{Text: mark(ViewStatus, current, "Статус"), CallbackData: ViewStatus},
			{Text: mark(ViewIncidents, current, "Инциденты"), CallbackData: ViewIncidents},
		},
		{
			{Text: "Обновить", CallbackData: current},
			openButton(),
		},
	}
}

// statusKeyboard — навигация плюс ряд проектов.
//
// Кнопка проекта несёт его состояние значком, поэтому на общем экране больше
// не нужно печатать строку про каждый живой сервис: и так видно, что все
// зелёные, а подробности — по нажатию.
func statusKeyboard(s *Summary) *Keyboard {
	rows := projectRows(s, "")
	return &Keyboard{InlineKeyboard: append(rows, navRows(ViewStatus)...)}
}

// projectKeyboard — экран одного проекта: соседние проекты остаются под рукой,
// чтобы обойти их не возвращаясь каждый раз назад.
func projectKeyboard(s *Summary, current string) *Keyboard {
	rows := projectRows(s, current)
	return &Keyboard{InlineKeyboard: append(rows, navRows(current)...)}
}

// По три в ряд: названия проектов короткие, и на телефоне три кнопки со
// значком помещаются без переноса.
const projectsPerRow = 3

// buttonIcon — значок для КНОПКИ проекта, и он намеренно не такой, как в тексте.
//
// В тексте строка идёт одна на проверку, и зелёный кружок там отмечает
// конкретную строку. В ряду кнопок всё иначе: проектов шесть, в норме зелёные
// все шесть, и панель превращается в стену одинаковых ярких пятен. Яркость при
// этом ничего не сообщает — она означает «как всегда».
//
// Поэтому норма молчит, а значок остаётся только у того, что требует внимания:
// одна красная кнопка среди пяти спокойных видна сразу, среди пяти
// ярко-зелёных — нет.
func buttonIcon(status string) string {
	switch status {
	case "up", "operational":
		return ""
	default:
		return statusIcon(status) + " "
	}
}

func projectRows(s *Summary, current string) [][]Button {
	if s == nil {
		return nil
	}
	var rows [][]Button
	var row []Button
	for _, p := range s.Projects {
		label := buttonIcon(p.Status) + p.Title
		row = append(row, Button{
			Text:         mark(ViewProject+p.ID, current, label),
			CallbackData: ViewProject + p.ID,
		})
		if len(row) == projectsPerRow {
			rows = append(rows, row)
			row = nil
		}
	}
	if len(row) > 0 {
		rows = append(rows, row)
	}
	return rows
}

// changelogKeyboard — под экраном изменений: ряд проектов, чтобы провалиться
// в конкретный, не набирая его имя руками.
//
// Значка состояния на этих кнопках нет намеренно, в отличие от экрана статуса:
// здесь речь о выкатках, а не о здоровье, и красный кружок рядом с названием
// читался бы как «этот релиз сломан».
func changelogKeyboard(s *Summary, current string) *Keyboard {
	rows := changelogRows(s, current)
	return &Keyboard{InlineKeyboard: append(rows, navRows(current)...)}
}

func changelogRows(s *Summary, current string) [][]Button {
	if s == nil {
		return nil
	}
	var rows [][]Button
	var row []Button
	seen := map[string]bool{}
	for _, p := range s.Projects {
		// У проекта целей бывает несколько (сайт, API, админка), а кнопка
		// ведёт в проект целиком: экран цели покажет их все.
		if p.ID == "" || seen[p.ID] || len(ViewChangelogOf+p.ID) > callbackDataMax {
			continue
		}
		seen[p.ID] = true
		row = append(row, Button{
			Text:         mark(ViewChangelogOf+p.ID, current, p.Title),
			CallbackData: ViewChangelogOf + p.ID,
		})
		if len(row) == projectsPerRow {
			rows = append(rows, row)
			row = nil
		}
	}
	if len(row) > 0 {
		rows = append(rows, row)
	}
	return rows
}

// alertKeyboard — под уведомлением о падении.
//
// Здесь не до навигации: нужно понять масштаб, открыть сервис — или сказать
// боту помолчать, пока чинишь. Без последнего владелец либо терпит
// напоминания, либо глушит чат целиком и пропускает следующую аварию.
//
// projectID — если известно, какой проект упал, первая кнопка ведёт сразу в
// него: иначе владелец попадает на общий экран и ищет там то, о чём ему
// только что написали.
func alertKeyboard(projectID string) *Keyboard {
	what := whatNowButton(ViewStatus)
	if projectID != "" {
		what = whatNowButton(ViewProject + projectID)
	}
	return &Keyboard{InlineKeyboard: [][]Button{
		{what, openButton()},
		{
			{Text: "Тихо 2 ч", CallbackData: ActMute2h},
			{Text: "До утра", CallbackData: ActMute8h},
		},
	}}
}

// mutedKeyboard — под подтверждением тишины: единственное осмысленное
// действие здесь — передумать; «Что сейчас» открывает статус новым
// сообщением, не заменяя собой это подтверждение (см. ActWhatNowPrefix).
func mutedKeyboard() *Keyboard {
	return &Keyboard{InlineKeyboard: [][]Button{
		{
			{Text: "Снова говорить", CallbackData: ActUnmute},
			whatNowButton(ViewStatus),
		},
	}}
}

// deployKeyboard — под карточкой прогона выкатки.
//
// Кнопок тишины здесь НЕТ, хотя раньше они были — deployKeyboard попросту
// возвращал alertKeyboard как есть. Тишина глушит ТОЛЬКО напоминания цикла
// наблюдения (KindStillDown); события выкатки идут через outbox, минуя
// фильтр тишины, и «До утра» под успешной карточкой не глушит ничего, а под
// провалившейся не останавливает поток карточек — кнопка врала. Вместо них —
// «Прогон» (адрес CI, если пришёл и прошёл проверку runURLRe) и «Что
// менялось» по проекту: то, что под карточкой выкатки действительно имеет
// смысл.
func deployKeyboard(projectID, runURL string) *Keyboard {
	what := whatNowButton(ViewStatus)
	if projectID != "" {
		what = whatNowButton(ViewProject + projectID)
	}
	row := []Button{what, openButton()}
	if runURLRe.MatchString(runURL) {
		row = append(row, Button{Text: "Прогон", URL: runURL})
	}
	rows := [][]Button{row}
	if projectID != "" {
		rows = append(rows, []Button{{Text: "Что менялось", CallbackData: ViewChangelogOf + projectID}})
	}
	return &Keyboard{InlineKeyboard: rows}
}
