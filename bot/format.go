package main

import (
	"fmt"
	"html"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Время во всех сообщениях — московское.
//
// Зона задана смещением, а не именем: с 2014 года в Москве нет переходов на
// летнее время, поэтому UTC+3 постоянен, а бот не зависит от наличия tzdata
// в системе. Агент пишет всё в UTC, показывать это владельцу неудобно.
var msk = time.FixedZone("MSK", 3*60*60)

const (
	up       = "🟢"
	down     = "🔴"
	degraded = "🟠"
	slowIcon = "🟡"
	unknown  = "⚪️"
)

// esc — чужой текст, годный для parse_mode=HTML.
//
// Экранирование здесь не единственная работа: перед ним из строки вырезается
// то, что текстом не является. Точка одна на весь бот нарочно — стоит завести
// вторую, и очистка неминуемо разойдётся с экранированием, а разойдясь, начнёт
// пропускать ровно там, где о ней забыли.
func esc(s string) string { return html.EscapeString(sanitizeText(s)) }

// bidiRunes — символы, переключающие направление письма, и невидимки рядом с
// ними.
//
// U+202E (RTL override) ничего не печатает, но переворачивает порядок всего,
// что идёт после него: «gnp.exe‮…» показывается читателю как «…exe.png».
// Строка в сообщении при этом ровно та, что записана в поле, — расходится
// только показ, и заметить подмену в чате нечем. Остальные из списка (метки
// направления, изоляты, ноль-ширинные, BOM и мягкий перенос) не переворачивают
// текст, но так же невидимы: ими режут слово пополам или клеят два разных
// адреса в один на вид.
func bidiRune(r rune) bool {
	switch {
	case r == 0x00AD, r == 0x061C, r == 0xFEFF:
		return true
	case r >= 0x200B && r <= 0x200F:
		return true
	case r >= 0x202A && r <= 0x202E:
		return true
	case r >= 0x2066 && r <= 0x2069:
		return true
	}
	return false
}

// sanitizeText вычищает из чужой строки всё, что не текст.
//
// Управляющие символы становятся ПРОБЕЛОМ, а не исчезают: «а\nб» без пробела
// склеилось бы в «аб». А исчезать они обязаны потому, что перевод строки в поле
// подделывает строку сообщения — «🔴 <b>Прод</b> недоступен» с новой строки
// читается как отдельное уведомление от бота, которому владелец доверяет по
// определению.
//
// Переключатели направления письма (bidiRune) удаляются целиком: показать
// пробел вместо невидимого символа было бы честнее, но он рвал бы слово.
//
// Битые байты UTF-8 превращаются в U+FFFD: результат этой функции уезжает в
// Telegram, а негодную кодировку он отвергает вместе со всем сообщением.
func sanitizeText(s string) string {
	if !strings.ContainsFunc(s, dirtyRune) && utf8.ValidString(s) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case bidiRune(r):
		case r < 0x20, r == 0x7F, r >= 0x80 && r <= 0x9F:
			b.WriteRune(' ')
		default:
			b.WriteRune(r) // невалидный байт уже пришёл сюда как U+FFFD
		}
	}
	return b.String()
}

func dirtyRune(r rune) bool {
	return r < 0x20 || r == 0x7F || (r >= 0x80 && r <= 0x9F) || bidiRune(r)
}

// logSafe — чужая строка, годная для записи в журнал.
//
// Отдельно от sanitizeText, потому что задача обратная. В сообщении управляющий
// символ надо СТЕРЕТЬ, чтобы читатель увидел текст; в журнале его надо
// ПОКАЗАТЬ, чтобы разбирающий инцидент увидел, что в поле лежала подделка.
// CRLF в поле события подделывает строку journald: одно поле превращается в
// две записи, и вторая выглядит как сообщение самого бота.
//
// Длина ограничена здесь же: поле приезжает файлом с диска, и его размер бот не
// выбирал.
func logSafe(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20, r == 0x7F, r >= 0x80 && r <= 0x9F, bidiRune(r):
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			b.WriteRune(r)
		}
	}
	return cutRunes(b.String(), logFieldMax)
}

// logFieldMax — сколько символов чужого поля попадает в одну запись журнала.
const logFieldMax = 200

func fmtTime(t time.Time) string {
	return t.In(msk).Format("02.01 15:04 MSK")
}

// humanDur — длительность по-русски.
//
// Единицы сокращённые («12 ч 30 мин»), чтобы не тащить в бот склонение
// числительных ради строки, которую всё равно читает один человек.
func humanDur(d time.Duration) string {
	if d < 0 {
		d = -d
	}
	switch {
	case d < time.Minute:
		return "меньше минуты"
	case d < time.Hour:
		return fmt.Sprintf("%d мин", int(d.Minutes()))
	case d < 24*time.Hour:
		h := int(d.Hours())
		m := int(d.Minutes()) - h*60
		if m == 0 {
			return fmt.Sprintf("%d ч", h)
		}
		return fmt.Sprintf("%d ч %d мин", h, m)
	default:
		days := int(d.Hours()) / 24
		h := int(d.Hours()) - days*24
		if h == 0 {
			return fmt.Sprintf("%d д", days)
		}
		return fmt.Sprintf("%d д %d ч", days, h)
	}
}

func statusIcon(status string) string {
	switch status {
	case "up", "operational":
		return up
	case "down":
		return down
	case "slow":
		// «Медленно» — не падение и не норма: сервис отвечает, но дольше
		// порога. Своя иконка, чтобы не путать ни с тем, ни с другим.
		return slowIcon
	case "degraded", "major":
		return degraded
	default:
		return unknown
	}
}

func formatHelp() string {
	return strings.Join([]string{
		"<b>Статус samoy.love</b>",
		"",
		"Кнопки под сообщением переключают экраны прямо здесь — новые",
		"сообщения при этом не плодятся, переписывается текущее.",
		"Верхний ряд — проекты: значок показывает состояние, нажатие",
		"раскрывает проверки, службы, версии и историю по дням.",
		"«Открыть» показывает страницу целиком, не выходя из Telegram.",
		"",
		"Если удобнее командами:",
		"/status — что живо, что лежит, аптайм",
		"/changelog — что менялось в последних выкатках",
		"/changelog metro — то же по одному сервису, с историей",
		"/incidents — последние падения",
		"/quiet 2h — помолчать (можно 8h, off — снять раньше)",
		"/help — эта справка",
		"",
		"Сам сообщу, когда что-то упадёт, поднимется или обновится.",
		"Про «медленно» будить не буду — это видно на экране статуса.",
		"",
		"<b>Полоска доступности</b> под проектом — две недели по суткам:",
		"🟩 без сбоев · 🟨 меньше 1% · 🟧 до 10% · 🟥 больше · ⬜ нет данных",
	}, "\n")
}

// formatStatus — ответ на /status.
//
// Порядок проектов не сортируется: он задан конфигом и совпадает с порядком
// на самой странице, чтобы взгляд искал сервис в одном и том же месте.
// stripDays — сколько суток показываем полоской. Четырнадцать умещаются в
// одну строку на телефоне; девяносто, как на странице, переносились бы и
// превращались в кашу.
const stripDays = 14

// Квадраты полоски. Порог тот же, что у ступеней на странице: сутки с одной
// сбойной минутой из 1440 не должны выглядеть как сутки, лежавшие наполовину.
func dayCell(d *Day) string {
	if d == nil || d.Total == 0 {
		return "⬜"
	}
	switch ratio := float64(d.Up) / float64(d.Total); {
	case ratio == 1:
		return "🟩"
	case ratio >= 0.99:
		return "🟨"
	case ratio >= 0.9:
		return "🟧"
	default:
		return "🟥"
	}
}

// worstDays — худшая ключевая проверка за каждые сутки.
//
// Считаем по ключевым проверкам и по худшей из них за сутки: если в этот день
// лежал игровой сервер, день плохой, даже когда сайт открывался. Та же логика
// живёт в src/lib/status.ts для страницы и мини-аппа — расхождение здесь
// означало бы, что бот и страница рисуют разную историю одного дня.
func worstDays(checks []Check, count int) []*Day {
	worst := make([]*Day, count)
	for _, c := range checks {
		if !c.Critical || len(c.Days) == 0 {
			continue
		}
		days := c.Days
		if len(days) > count {
			days = days[len(days)-count:]
		}
		for i, d := range days {
			slot := count - len(days) + i
			if slot < 0 || d == nil || d.Total == 0 {
				continue
			}
			cur := worst[slot]
			if cur == nil || float64(d.Up)/float64(d.Total) < float64(cur.Up)/float64(cur.Total) {
				worst[slot] = d
			}
		}
	}
	return worst
}

func strip(days []*Day) string {
	var b strings.Builder
	for _, d := range days {
		b.WriteString(dayCell(d))
	}
	return b.String()
}

// projectStrip — доступность проекта за две недели одной строкой.
func projectStrip(p Project) string { return strip(worstDays(p.Checks, stripDays)) }

// overallStrip — то же по всей экосистеме: одна строка вместо пяти
// одинаково зелёных полосок под каждым проектом.
func overallStrip(s *Summary) string {
	var all []Check
	for _, p := range s.Projects {
		all = append(all, p.Checks...)
	}
	return strip(worstDays(all, stripDays))
}

// hasHistory — есть ли в полоске хоть один день с данными. Полоска из
// четырнадцати белых квадратов не сообщает ничего и только занимает строку.
func hasHistory(s string) bool { return strings.ContainsAny(s, "🟩🟨🟧🟥") }

// muted и mutedUntil — состояние тишины. Раньше его вообще не было видно на
// экране статуса: факт «бот сейчас молчит» знал только тот, кто сам жал
// «Тихо 2 ч»/«До утра» и помнил об этом, а State.Muted читал только цикл
// уведомлений. Отсюда молчание, которое забыли снять, было незаметно ровно
// до следующей аварии.
func formatStatus(s *Summary, now time.Time, muted bool, mutedUntil time.Time) string {
	var b strings.Builder

	// Тот же принцип, что на странице: здоровое сворачивается, сломанное
	// поднимается наверх. Раньше бот печатал все проекты со всеми проверками,
	// службами и полосками — два десятка зелёных строк, среди которых
	// единственную красную приходилось искать глазами.
	switch s.Overall {
	case "operational":
		b.WriteString(up + " <b>Всё работает</b>")
	case "down":
		b.WriteString(down + " <b>Всё лежит</b>")
	case "major":
		b.WriteString(down + " <b>Крупный сбой</b>")
	default:
		b.WriteString(degraded + " <b>Частичный сбой</b>")
	}
	if muted {
		fmt.Fprintf(&b, "\n🔕 молчу до %s — /quiet off, чтобы снять раньше", fmtTime(mutedUntil))
	}

	var okCrit, totalCrit, auxBad int
	for _, p := range s.Projects {
		okCrit += p.Up
		totalCrit += p.Total
		auxBad += p.AuxDown + p.AuxSlow
	}
	fmt.Fprintf(&b, "\n<code>%d/%d</code> ключевых проверок в норме", okCrit, totalCrit)
	if auxBad > 0 {
		fmt.Fprintf(&b, " · <code>%d</code> второстеп. не в порядке", auxBad)
	}
	b.WriteString("\n")

	// Сломанное — единственное, что раскрывается подробно.
	for _, p := range s.Projects {
		for _, c := range p.Checks {
			if c.Status == "up" {
				continue
			}
			fmt.Fprintf(&b, "\n%s <b>%s · %s</b>%s",
				checkIcon(c), link(p.Title, p.URL), link(c.Name, c.URL), auxTail(c))
			if c.Impact != "" && c.Status == "down" {
				b.WriteString("\n   " + esc(c.Impact))
			}
			if c.Error != "" {
				b.WriteString("\n   <code>" + esc(c.Error) + "</code>")
			}
			if t, ok := parseTime(c.Since); ok {
				fmt.Fprintf(&b, "\n   %s", humanDur(now.Sub(t)))
			}
			b.WriteString("\n")
		}
	}

	// Службы показываем только когда с ними что-то не так: перечислять
	// работающие значит утопить в них неработающую.
	var dead []string
	for _, p := range s.Projects {
		for _, u := range p.Units {
			if !u.Active {
				dead = append(dead, fmt.Sprintf("%s %s · %s — %s", down, esc(p.Title), esc(u.Title), esc(u.State)))
			}
		}
	}
	if len(dead) > 0 {
		b.WriteString("\n" + strings.Join(dead, "\n") + "\n")
	}

	// Одна полоска на всю экосистему вместо пяти одинаковых под каждым
	// проектом. Разбивка по проектам — на кнопках и на экране проекта.
	if st := overallStrip(s); hasHistory(st) {
		fmt.Fprintf(&b, "\n%s\n<i>%d дней</i>\n", st, stripDays)
	}

	b.WriteString("\n" + freshness(s, now))
	return b.String()
}

// formatProject — экран одного проекта.
//
// Сюда переехали подробности, которые раньше печатались для всех проектов
// сразу: проверки с аптаймом, службы, версии, полоска. Общий экран от этого
// стал коротким, а разбор конкретного сервиса — полным.
func formatProject(p Project, s *Summary, now time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s <b>%s</b>", statusIcon(p.Status), link(p.Title, p.URL))
	fmt.Fprintf(&b, "\n<code>%d/%d</code> ключевых проверок в норме\n", p.Up, p.Total)

	// Сломанные проверки идут первыми: на экране проекта ищут именно их.
	sorted := append([]Check(nil), p.Checks...)
	sort.SliceStable(sorted, func(i, j int) bool {
		return checkRank(sorted[i]) < checkRank(sorted[j])
	})

	for _, c := range sorted {
		fmt.Fprintf(&b, "\n%s <b>%s</b>%s", checkIcon(c), link(c.Name, c.URL), auxTail(c))
		if c.Status == "up" {
			if u := uptimeText(c); u != "" {
				b.WriteString(" — " + u)
			}
			continue
		}
		if c.Impact != "" && c.Status == "down" {
			b.WriteString("\n   " + esc(c.Impact))
		}
		if c.Error != "" {
			b.WriteString("\n   <code>" + esc(c.Error) + "</code>")
		}
		if t, ok := parseTime(c.Since); ok {
			fmt.Fprintf(&b, "\n   %s", humanDur(now.Sub(t)))
		}
	}
	b.WriteString("\n")

	if len(p.Units) > 0 {
		b.WriteString("\n<b>Службы</b>")
		for _, u := range p.Units {
			icon := up
			if !u.Active {
				icon = down
			}
			fmt.Fprintf(&b, "\n%s %s — %s", icon, esc(u.Title), esc(u.State))
		}
		b.WriteString("\n")
	}

	if len(p.Builds) > 0 {
		b.WriteString("\n<b>Версии</b>")
		for _, bl := range p.Builds {
			version := bl.Version
			if version == "" {
				version = "неизвестна"
			}
			fmt.Fprintf(&b, "\n%s: <code>%s</code>", link(bl.Title, bl.URL), esc(version))
			if t, ok := parseTime(bl.At); ok {
				fmt.Fprintf(&b, " · %s назад", humanDur(now.Sub(t)))
			}
		}
		b.WriteString("\n")
	}

	if st := projectStrip(p); hasHistory(st) {
		fmt.Fprintf(&b, "\n%s\n<i>%d дней</i>\n", st, stripDays)
	}

	b.WriteString("\n" + freshness(s, now))
	return b.String()
}

// checkIcon — значок проверки.
//
// У второстепенной упавшей проверки значок оранжевый, а не красный: она не
// роняет вердикт проекта, и красный кружок рядом с работающим сервисом
// заставляет искать аварию там, где её нет. Функция одна на общий экран и на
// экран проекта — иначе одна и та же проверка выглядела бы на них по-разному.
func checkIcon(c Check) string {
	if !c.Critical && c.Status == "down" {
		return degraded
	}
	return statusIcon(c.Status)
}

func auxTail(c Check) string {
	if c.Critical {
		return ""
	}
	return " <i>(второстеп.)</i>"
}

// checkRank — порядок проверок на экране проекта: сначала лежащие, потом
// медленные, потом живые. Внутри группы порядок конфига сохраняется.
func checkRank(c Check) int {
	switch {
	case c.Status == "down" && c.Critical:
		return 0
	case c.Status == "down":
		return 1
	case c.Status == "slow":
		return 2
	default:
		return 3
	}
}

// uptimeText — доступность живой проверки короткой строкой.
//
// Агент отдаёт аптайм УЖЕ в процентах (agent/main.go, pct). Умножать здесь
// ещё на сто — как было в первой версии — значит показывать «9991.00%»:
// число выглядит настолько неправдоподобно, что читается как поломка бота,
// а не как доступность.
func uptimeText(c Check) string {
	v, ok := c.Uptime["d90"]
	if !ok || v == nil {
		if v, ok = c.Uptime["d7"]; !ok || v == nil {
			return ""
		}
		return fmt.Sprintf("%s за неделю", fmtPct(*v))
	}
	return fmt.Sprintf("%s за 90 дней", fmtPct(*v))
}

// fmtPct — процент без хвостовых нулей: «100%» вместо «100.00%», но
// «99.87%» целиком. То же правило, что на странице (src/lib/status.ts).
func fmtPct(v float64) string {
	s := strconv.FormatFloat(v, 'f', 2, 64)
	s = strings.TrimRight(s, "0")
	s = strings.TrimRight(s, ".")
	return s + "%"
}

// freshness — строка о свежести данных.
//
// Нужна отдельной строкой: если агент упал, все проверки в файле выглядят
// зелёными и сколь угодно старыми. Без отметки времени такой ответ вводит
// в заблуждение сильнее, чем отсутствие ответа.
func freshness(s *Summary, now time.Time) string {
	t, ok := parseTime(s.Updated)
	if !ok {
		return "<i>Время обновления данных неизвестно</i>"
	}
	age := now.Sub(t)
	line := fmt.Sprintf("<i>Данные агента: %s (%s назад)</i>", fmtTime(t), humanDur(age))
	if age >= staleAfter {
		line += "\n" + degraded + " <b>Данные устарели</b> — похоже, агент не обходит сервисы"
	}
	return line
}

func formatIncidents(s *Summary, now time.Time) string {
	if len(s.Incidents) == 0 {
		return up + " Инцидентов не было"
	}
	var b strings.Builder
	b.WriteString("<b>Последние инциденты</b>\n")
	for _, in := range s.Incidents {
		start, hasStart := parseTime(in.Start)
		fmt.Fprintf(&b, "\n%s <b>%s</b>\n", statusIcon(incidentStatus(in)), esc(in.Name))
		if hasStart {
			fmt.Fprintf(&b, "  начало: %s\n", fmtTime(start))
		}
		if in.End == "" {
			if hasStart {
				fmt.Fprintf(&b, "  идёт уже %s\n", humanDur(now.Sub(start)))
			} else {
				b.WriteString("  идёт\n")
			}
		} else {
			fmt.Fprintf(&b, "  длился %s\n", humanDur(time.Duration(in.DurationMs)*time.Millisecond))
		}
		if in.Reason != "" {
			fmt.Fprintf(&b, "  причина: %s\n", esc(in.Reason))
		}
	}
	return b.String()
}

func incidentStatus(in Incident) string {
	if in.End == "" {
		return "down"
	}
	return "up"
}

// formatEvent — текст уведомления о событии.
//
// Порядок строк выбран под сценарий «пришло ночью, смотрю с телефона»:
// сначала ЧТО и для кого это плохо, потом техническая причина, потом время.
// Причина первой строкой заставляла бы разбирать «connect: connection
// refused» до того, как понятно, надо ли вообще вставать.
func formatEvent(e Event) string {
	switch e.Kind {
	case KindDown:
		// Дальше идёт последствие для пользователя, если оно описано в
		// конфиге, иначе техпричина.
		s := fmt.Sprintf("%s <b>%s</b> недоступен", down, link(e.Title, e.URL))
		if e.Reason != "" {
			s += "\n" + esc(e.Reason)
		}
		return s + "\n<i>" + fmtTime(e.At) + "</i>"

	case KindStillDown:
		// Напоминание короче первого сообщения: подробности уже приходили,
		// здесь важно только, сколько это тянется.
		return fmt.Sprintf("%s <b>%s</b> лежит уже %s",
			down, link(e.Title, e.URL), humanDur(e.Duration))

	case KindUp:
		return fmt.Sprintf("%s <b>%s</b> снова работает\nпростой: %s\n<i>%s</i>",
			up, link(e.Title, e.URL), humanDur(e.Duration), fmtTime(e.At))

	case KindRelease:
		s := fmt.Sprintf("🚀 <b>%s</b> обновлён\n%s",
			link(e.Title, e.URL), versionHTML(e.Version, e.CommitURL))
		if e.Previous != "" {
			s += fmt.Sprintf("\nбыла <code>%s</code>", esc(e.Previous))
		}
		s += "\n<i>собрано " + fmtTime(e.At) + "</i>"
		// Ссылка на ручное действие — сразу после времени сборки, а не в конце
		// после списка изменений: без неё релиз читается как завершённый, хотя
		// для части целей (см. deployAdminLinks) переключение канала ждёт
		// человека отдельным кликом в другой вкладке. Задвинуть её за длинный
		// changelog значило бы, что её не долистают.
		if e.AdminURL != "" {
			s += fmt.Sprintf("\n👉 <a href=\"%s\">переключить на неё в админке</a>", esc(e.AdminURL))
		}
		// Список изменений — последним и через пустую строку: он самый
		// длинный, самый необязательный и единственный, которого может не
		// быть. Всё, ради чего уведомление читают в первую очередь (что
		// обновилось и до какой версии), остаётся выше и на прежнем месте.
		s += changelogTail(e.Changelog, repoFromCommitURL(e.CommitURL), e.Previous, e.CommitURL)
		return s

	default:
		// Незнакомый вид наблюдения — не повод показать голый заголовок без
		// глагола: тот же случай в formatDeploy молчит, и здесь ему положено
		// вести себя одинаково.
		return ""
	}
}

// ------------------------------------------------------------ события выкатки

// Deploy — событие выкатки в том виде, в каком его читает форматтер.
//
// Тип отдельный от Event, и это не дублирование. Event описывает НАБЛЮДЕНИЕ за
// продом (упало, поднялось, сменилась версия), а Deploy — то, что выкатка
// сообщила о себе сама, и в нём есть поля, которых у наблюдения быть не может:
// стадия провала, причина отката, адрес прогона. Смысл и пределы полей заданы
// контрактом deploy-kit/docs/events.md и здесь не выдумываются заново.
//
// Title и URL приходят НЕ из события, а из реестра summary.json (контракт, §4):
// имя проекта в хозяйстве одно, и вторая правда о нём развела бы чат и
// страницу первым же переименованием.
type Deploy struct {
	Kind string // success | failure | rolled_back | rollback | started | published
	App  string // id цели из события; показывается, когда её нет в реестре
	// Title и URL — человеческое имя цели и адрес, куда идти смотреть.
	Title   string
	URL     string
	Project string
	// Version у rollback — это релиз, НА который откатились, у остальных видов
	// — тот, который выкатывали (контракт, §4).
	Version   string
	Previous  string
	Stage     string // только из перечисления контракта, §7
	Reason    string // только из перечисления контракта, §7
	CommitURL string
	RunURL    string
	Changelog []string
	At        time.Time
}

// Виды события. Строками, а не типом Kind: Kind описывает наблюдения бота, и
// смешивать в нём два разных перечисления — верный способ однажды сравнить
// «up» с «success».
const (
	deploySuccess    = "success"
	deployFailure    = "failure"
	deployRolledBack = "rolled_back"
	deployRollback   = "rollback"
	deployStarted    = "started"
	deployPublished  = "published"
)

const (
	iconFail = "❌"
	iconBack = "↩️"
	iconWait = "⏳"
	iconPub  = "📦"
	iconOK   = "✅"
	iconShip = "🚀"
)

// Потолки на поля события.
//
// Все они повторяют контракт (§8) и держатся здесь ВТОРОЙ раз намеренно.
// Событие приезжает файлом с диска, и проверить его обязан тот, кто отправляет
// сообщение: писатель, положивший в поле мегабайт, — это либо ошибка писателя,
// либо не тот писатель, и в обоих случаях чат не должен этого заметить.
const (
	deployTitleMax = 120 // столько же, сколько у темы коммита (CLAUDE.md)
	deployAppMax   = 64  // столько же, сколько у имени каталога релиза
	deployVerMax   = 128
	deployEnumMax  = 24
	// deployTargets — сколько целей печатается в сообщении прогона. Больше
	// шести не выкатывает ни один репозиторий хозяйства; двадцать — это уже
	// защита от вранья, а не формат.
	deployTargets = 20
	// deployChangelogMax — пунктов списка изменений в одном событии.
	//
	// Контракт разрешает писателю двадцать, здесь стоит сто, и это не
	// расхождение: БОТ НЕ СТРОЖЕ ТОГО, КТО ПИШЕТ. Двадцать первый пункт,
	// доехавший до бота, — это повод показать его, а не молча урезать релиз;
	// сто — тот же потолок против вранья, что стоит у агента.
	deployChangelogMax = 100
)

// deployStages — стадии провала по-русски. Ключи — закрытое перечисление
// контракта (§7).
//
// ЭТА КАРТА И ЕСТЬ ЗАЩИТА ОТ УТЕЧКИ. В сообщение попадает не значение поля, а
// то, что нашлось по нему в карте: сырой вывод лога, путь на сервере или кусок
// nginx.conf, положенный в stage вместо перечисления, не совпадёт ни с одним
// ключом и не покажется вовсе. Незнакомая стадия = стадии нет, а событие
// показывается: терять выкатку из-за неизвестного значения нельзя (контракт, §7).
var deployStages = map[string]string{
	"gates":      "проверки до сборки",
	"preflight":  "сборка и проверка цели",
	"upload":     "доставка на сервер",
	"switch":     "переключение релиза",
	"units":      "службы systemd",
	"health":     "healthcheck",
	"version":    "сверка версии на проде",
	"neighbours": "проверка соседних целей",
}

// deployReasons — причины отката по-русски. Правило то же, что у deployStages:
// в чат уезжает значение ИЗ КАРТЫ, а не из поля.
var deployReasons = map[string]string{
	"units_failed":      "службы не поднялись",
	"nginx_failed":      "nginx не принял конфигурацию",
	"health_failed":     "healthcheck не дождался ответа",
	"verify_failed":     "проверка verify не прошла",
	"version_mismatch":  "прод раздаёт не ту версию",
	"neighbours_failed": "не пережили соседние цели",
	"manual":            "откат запущен руками",
}

func stageText(s string) string  { return deployStages[s] }
func reasonText(s string) string { return deployReasons[s] }

// deployAdminLinks — цели, у которых выкатка не заканчивает дело сама.
//
// chillhub-installer — единственный сегодня пример: publish-file.sh кладёт
// сборку на сервер, но канал самообновления лаунчера (SELFUPDATE_URL) на неё
// НЕ переключается — решение «теперь обновляемся на эту версию» осознанно
// оставлено человеку (см. комментарий у SELFUPDATE_URL в
// chillhub/.deploy-kit/installer.env). Без прямой ссылки это решение легко
// забыть: релиз в чате выглядит завершённым, а переключатель ждёт в другой
// вкладке, о которой напоминает только память.
//
// Ключ — App из события (id цели), а не Project: одна и та же цель под
// разными App не бывает у одного проекта, но Project мог бы совпасть у двух
// репозиториев случайно. Ссылка — константа этого бота, не берётся из
// события: чужой писатель не может подставить сюда произвольный адрес.
var deployAdminLinks = map[string]string{
	"chillhub-installer": "https://launcher.samoy.love/admin/ui/admin.html#launcher",
}

// deployOutcome — одна строка единой таблицы "вид события выкатки → как о нём
// написать": значок и глагол.
//
// РАНЬШЕ ЭТОТ ЖЕ ИСХОД ОПИСЫВАЛИ ДВА СЛОВАРЯ ПОРОЗНЬ. formatDeploy держал
// мужской род («не выкачен», «откачен автоматически» — по имени цели),
// outcomeText — женский («провалена», «откачена — …» — про «цель»), и какой
// из них увидит владелец, решало число целей в прогоне, а не смысл события.
// Один прогон из шести целей мог показать один и тот же исход двумя разными
// словами в зависимости от того, был ли рядом ещё кто-то. Таблица ниже —
// единственный источник глагола для ОБЕИХ форм сообщения; род оставлен
// мужским, тем же, что был в formatDeploy, — она склоняется по имени цели
// («Админка не выкачен»), а не по смыслу слова «цель».
//
// deployStarted входит в таблицу ради значка и глагола, которые нужны
// ГРУППОВОМУ сообщению (цель, ещё не объявившая исход, но упомянутая рядом с
// теми, кто уже объявил). Одиночному сообщению это не нужно: started —
// служебный вид, который сам по себе в чат не идёт (контракт, §4, §12), и
// formatDeploy обрабатывает его отдельно, до обращения к таблице.
var deployOutcomeTable = map[string]struct{ icon, verb string }{
	deploySuccess:    {iconOK, "выкачен"},
	deployStarted:    {iconWait, "выкатывается…"},
	deployPublished:  {iconPub, "опубликован"},
	deployFailure:    {iconFail, "не выкачен"},
	deployRolledBack: {iconBack, "откачен автоматически"},
	deployRollback:   {iconBack, "откачен руками"},
}

// deployIcon — значок исхода. Незнакомый вид — тот же серый кружок, что и у
// незнакомого статуса проверки: не авария, а неизвестность.
func deployIcon(kind string) string {
	if o, ok := deployOutcomeTable[kind]; ok {
		return o.icon
	}
	return unknown
}

// deployVerb — глагол исхода без деталей. Пустая строка — незнакомый вид.
func deployVerb(kind string) string {
	return deployOutcomeTable[kind].verb
}

// deployDetailText — уточнение исхода: стадия провала или причина отката.
// Один источник для обеих форм сообщения — одиночной (отдельной строкой) и
// групповой (после тире); расхождение в тексте стадии между ними было бы той
// же болезнью, для которой заведена deployOutcomeTable.
func deployDetailText(d Deploy) string {
	switch d.Kind {
	case deployFailure:
		if st := stageText(d.Stage); st != "" {
			return "остановились на стадии: " + st
		}
	case deployRolledBack:
		// Причина точнее стадии и говорит о том же месте («healthcheck не
		// дождался ответа» вместо «остановились на healthcheck»), поэтому
		// стадия печатается, только когда причина незнакома.
		if r := reasonText(d.Reason); r != "" {
			return "причина: " + r
		}
		if st := stageText(d.Stage); st != "" {
			return "остановились на стадии: " + st
		}
	}
	return ""
}

// runURLRe — адрес прогона. Список разрешённого, собранный по тому же правилу,
// что и commitURLRe: буквальные схема и хост, буквальный путь GitHub Actions,
// номер прогона цифрами. Всё остальное ссылкой не станет и молча выбросится —
// «прогон», ведущий не в прогон, хуже отсутствующей ссылки.
var runURLRe = regexp.MustCompile(
	`^https://github\.com/[A-Za-z0-9][A-Za-z0-9._-]{0,63}/[A-Za-z0-9][A-Za-z0-9._-]{0,99}/actions/runs/[0-9]{1,20}(/attempts/[0-9]{1,4})?$`)

// runLinkHTML — ссылка «прогон» под сообщением о провале.
//
// Ради неё провал и сообщается: подробности провала в чат не едут (контракт,
// §7), и единственный честный ответ на «а что там случилось» — увести
// читателя туда, где лог лежит целиком.
func runLinkHTML(raw string) string {
	if !runURLRe.MatchString(raw) {
		return ""
	}
	return `<a href="` + esc(raw) + `">прогон</a>`
}

// sanitizeDeploy приводит чужое событие к тому, что можно показать.
//
// Порядок важен: сначала очистка, потом обрезка. Обрезать до очистки значило бы
// считать в длину невидимые символы, которые всё равно будут выброшены.
func sanitizeDeploy(d Deploy) Deploy {
	d.Kind = cutRunes(sanitizeText(d.Kind), deployEnumMax)
	d.Stage = cutRunes(sanitizeText(d.Stage), deployEnumMax)
	d.Reason = cutRunes(sanitizeText(d.Reason), deployEnumMax)
	d.App = cutRunes(sanitizeText(d.App), deployAppMax)
	d.Project = cutRunes(sanitizeText(d.Project), deployAppMax)
	d.Title = cutRunes(sanitizeText(d.Title), deployTitleMax)
	d.Version = cutRunes(sanitizeText(d.Version), deployVerMax)
	d.Previous = cutRunes(sanitizeText(d.Previous), deployVerMax)
	// Адреса не режутся, а отбрасываются целиком: обрезанный адрес ведёт не
	// туда, куда вёл исходный, и это хуже, чем текст без ссылки.
	if !allowedURL(d.URL) {
		d.URL = ""
	}
	if !commitURLRe.MatchString(d.CommitURL) {
		d.CommitURL = ""
	}
	if !runURLRe.MatchString(d.RunURL) {
		d.RunURL = ""
	}
	d.Changelog = capChangelog(d.Changelog)
	return d
}

// capChangelog зажимает число пунктов и говорит вслух, что зажал.
//
// Хвост дописывается строкой в том же виде, в каком его печатает
// deploy-kit/bin/changelog: разбор списка узнаёт его сам и пунктом не считает.
// Молча оборванный список читается как «больше ничего и не было».
func capChangelog(lines []string) []string {
	if len(lines) <= deployChangelogMax {
		return lines
	}
	rest := len(lines) - deployChangelogMax
	out := append([]string(nil), lines[:deployChangelogMax]...)
	return append(out, fmt.Sprintf("…и ещё %d %s — список не поместился", rest, pluralCommits(rest)))
}

// deployName — как называть цель в сообщении. Реестр summary.json главнее, но
// цели, которой в нём нет, показывается её id из события: видимое расхождение
// чинится, а тихо пропавшая строка — нет (контракт, §4).
func deployName(d Deploy) string {
	if d.Title != "" {
		return d.Title
	}
	return d.App
}

// formatDeploy — сообщение об ОДНОЙ цели.
//
// Успех отдаётся formatEvent без единой правки формы: ленту релизов читают
// годами, и переписать её заодно с транспортом значило бы сломать единственное,
// что в этой работе и так работало (контракт, §12).
//
// Пустая строка — законный ответ: started служебный и в чат не идёт, а
// незнакомый вид показывать нечем. Отправитель обязан такое сообщение
// пропустить, а не слать пустое.
// ВНИМАНИЕ: НА ПУТИ СООБЩЕНИЯ ЭТОЙ ФУНКЦИИ БОЛЬШЕ НЕТ.
//
// Она рисовала «прежнюю форму» для прогона из одной цели, и именно из-за неё
// один и тот же факт получал две вёрстки: карточку у прогона из двух целей и
// вот это — у прогона из одной. Развилку убрали, формат теперь один
// (formatDeployGroup), и вызывающих у formatDeploy в рабочем коде не осталось.
//
// Живёт она пока только ради своих же тестов. Править ЗДЕСЬ, ожидая увидеть
// правку в чате, бессмысленно: сообщение собирает formatDeployGroup.
func formatDeploy(d Deploy) string {
	d = sanitizeDeploy(d)
	name := link(deployName(d), d.URL)

	if d.Kind == deploySuccess {
		return formatEvent(Event{
			Kind: KindRelease, Title: deployName(d), URL: d.URL, Project: d.Project,
			Version: d.Version, Previous: d.Previous, CommitURL: d.CommitURL,
			Changelog: d.Changelog, At: d.At, AdminURL: deployAdminLinks[d.App],
		})
	}
	// started молчит, даже если он — единственная цель прогона: рассказывать
	// в чате «выкатывается» и ни о чём больше — значит слать сообщение без
	// исхода (контракт, §4, §12). В групповом сообщении та же цель наоборот
	// обязана остаться строкой: там она стоит рядом с уже объявленными
	// исходами других целей, и молчание о ней читалось бы как «эту цель не
	// катили» (см. outcomeText).
	if d.Kind == deployStarted {
		return ""
	}

	verb := deployVerb(d.Kind)
	if verb == "" {
		// Незнакомый вид — не повод терять единственную цель прогона молча:
		// но и выдумывать формулировку не из чего, поэтому сообщение пустое.
		return ""
	}

	// Сначала ЧТО произошло, потом версия, потом деталь (стадия/причина), и
	// только потом адрес прогона: порядок тот же, что у сообщения о падении,
	// — сперва то, ради чего читают, потом то, куда идти разбираться.
	s := fmt.Sprintf("%s <b>%s</b> %s", deployIcon(d.Kind), name, verb)
	if d.Version != "" {
		switch d.Kind {
		case deployRolledBack:
			s += "\nне удержался " + versionHTML(d.Version, d.CommitURL)
		case deployRollback:
			s += "\nвернули " + versionHTML(d.Version, d.CommitURL)
		default: // failure, published
			s += "\n" + versionHTML(d.Version, d.CommitURL)
		}
	}
	if det := deployDetailText(d); det != "" {
		s += "\n" + det
	}
	return s + runTail(d)
}

// runTail — общий хвост сообщения о выкатке: ссылка на прогон и время.
func runTail(d Deploy) string {
	s := ""
	if r := runLinkHTML(d.RunURL); r != "" {
		s += "\n" + r
	}
	if !d.At.IsZero() {
		s += "\n<i>" + fmtTime(d.At) + "</i>"
	}
	return s
}

// formatDeployGroup — ОДНО сообщение на прогон.
//
// Один пуш катит несколько целей одного репозитория (chillhub — шесть,
// status.samoy.love — три), и список изменений у них один и тот же: он считается
// по истории репозитория, а не по цели. Шесть сообщений с одинаковым блоком
// «Изменения» — это способ перестать читать чат: лента умирает не от одного
// лишнего сообщения, а от того, что полезное в ней тонет в повторах.
//
// Функция вызывается ЗАНОВО на каждое событие группы и рисует сообщение
// целиком: отправитель первым событием шлёт его, а дальше правит уже
// отправленное (контракт, §6). Поэтому здесь нет никакого «дописать строку» —
// есть текущее состояние прогона, собранное из всех его событий.
//
// project — заголовок прогона: имя проекта, а не первой попавшейся цели.
func formatDeployGroup(project string, ds []Deploy) string {
	targets := deployOutcomes(ds)
	if len(targets) == 0 {
		return ""
	}
	// Прогон, о котором нечего сказать: одни «выкатывается…». Сообщение без
	// исхода — это сообщение ни о чём (контракт, §4, §12).
	if !runHasOutcome(targets) {
		return ""
	}

	// ОДНА ВЁРСТКА НА ВСЕ РЕЛИЗЫ, СКОЛЬКО БЫ ЦЕЛЕЙ В ПРОГОНЕ НИ БЫЛО.
	//
	// Здесь стояла развилка: одна цель — «🚀 X обновлён» с версией отдельной
	// строкой, две и больше — карточка со списком. Один и тот же факт получал
	// две разные шапки, разное место версии, разную подпись времени и разный
	// текст ссылки на админку — а какую из двух увидит читатель, решало число
	// целей в прогоне, то есть обстоятельство, к сути релиза отношения не
	// имеющее. Читать такую ленту нельзя: глаз каждый раз заново ищет, где
	//version, где время и что вообще произошло.
	rawHead := sanitizeText(cutRunes(project, deployTitleMax))
	if rawHead == "" {
		rawHead = deployName(targets[0])
	}
	head := esc(rawHead)

	var b strings.Builder
	fmt.Fprintf(&b, "%s <b>%s</b>", iconShip, head)
	// В ШАПКЕ — КОММИТ, А НЕ ВЕРСИЯ.
	//
	// Версия у каждой цели своя: метку release-<дата>-<время>-<sha> каждая
	// задача считает сама, и совпадает в них только sha. Карточка chillhub от
	// 22.08 объявляла пять целей под версией одной из них, хотя настоящие были
	// …-105949, …-110126, …-110316 и 1.5.29. Общее у прогона — КОММИТ: пуш
	// один. Версии ушли в строки целей, где каждая своя и где она нужна для
	// `dk rollback`.
	//
	// Коммита нет (цель-файл со своей схемой версий, релиз до появления поля) —
	// показываем версию: пустая шапка хуже неточной.
	if c := runCommitHTML(targets); c != "" {
		b.WriteString(" · " + c)
	} else if v, commit := runVersion(targets); v != "" {
		b.WriteString(" · " + versionHTML(v, commit))
	}
	b.WriteString("\n")

	shown, rest := targets, 0
	if len(shown) > deployTargets {
		rest = len(shown) - deployTargets
		shown = shown[:deployTargets]
	}
	for _, t := range shown {
		fmt.Fprintf(&b, "\n%s %s — %s", outcomeIcon(t), link(cardTargetName(rawHead, t), t.URL), outcomeText(t))
		if t.Version != "" {
			b.WriteString(" · <code>" + esc(t.Version) + "</code>")
		}
	}
	if rest > 0 {
		fmt.Fprintf(&b, "\n…и ещё %d %s", rest, pluralTargets(rest))
	}
	// Ссылка на ручное действие: цель не перестаёт ждать переключения в
	// админке оттого, что приехала в одном прогоне с пятью другими.
	for _, t := range shown {
		if t.Kind != deploySuccess {
			continue
		}
		if url := deployAdminLinks[t.App]; url != "" {
			fmt.Fprintf(&b, "\n👉 <a href=\"%s\">переключить %s в админке</a>", esc(url), esc(cardTargetName(rawHead, t)))
		}
	}

	// АДРЕС ПРОГОНА — ТОЛЬКО У ПРОВАЛА, И ЭТО НЕ ЭКОНОМИЯ МЕСТА.
	//
	// У удачной выкатки идти в прогон незачем: всё, что о ней стоит знать,
	// уже в карточке. У провала наоборот: стадия называет, ГДЕ встало, а лог
	// прогона — единственное место, где написано, почему. Раньше эта ссылка
	// была только в одиночном сообщении и пропадала, стоило прогону выкатить
	// вторую цель.
	if runHasFailure(targets) {
		if r := runLinkHTML(runURLOf(targets)); r != "" {
			b.WriteString("\n" + r)
		}
	}

	// «Была …» — то, с чего ушли. Знает это только событие сервера: пайплайну
	// неоткуда узнать, на что показывал симлинк до переключения.
	if prev := runPrevious(targets); prev != "" {
		b.WriteString("\n\nбыла <code>" + esc(prev) + "</code>")
		if at := latestAt(targets); !at.IsZero() {
			b.WriteString("\n<i>" + fmtTime(at) + "</i>")
		}
	} else if at := latestAt(targets); !at.IsZero() {
		b.WriteString("\n\n<i>" + fmtTime(at) + "</i>")
	}

	// Список изменений — ОДИН РАЗ на прогон и последним блоком.
	//
	// Хвост со списком — только если в прогоне что-то ВЫКАТИЛОСЬ: строка
	// «изменений в этом релизе нет» описывает релиз, а у прогона, где все цели
	// провалились, релиза не было вовсе.
	if runHasSuccess(targets) {
		prev, commitURL := runCompare(targets)
		b.WriteString(changelogTail(runChangelog(targets), runRepoURL(targets), prev, commitURL))
	} else if cl := formatChangelog(runChangelog(targets), runRepoURL(targets)); cl != "" {
		b.WriteString("\n\n" + cl)
	}
	return b.String()
}

// runHasOutcome — объявила ли хоть одна цель прогона свой исход.
func runHasOutcome(ds []Deploy) bool {
	for _, d := range ds {
		if d.Kind != deployStarted {
			return true
		}
	}
	return false
}

// runPrevious — релиз, с которого ушёл прогон. Первый непустой: у целей одного
// прогона он свой, а строка в карточке одна, и относится она к той цели, ради
// которой карточку читают (deployOutcomes ставит прод первым).
func runPrevious(ds []Deploy) string {
	for _, d := range ds {
		if d.Kind == deployRollback {
			continue
		}
		if d.Previous != "" {
			return d.Previous
		}
	}
	return ""
}

// deployOutcomes — текущий исход каждой цели прогона.
//
// Цель объявляется дважды (started, потом success или failure), и в сообщении
// ей полагается ОДНА строка. Порядок внутри каждой из двух групп (прод,
// остальные) — порядок первого появления цели: он совпадает с порядком
// выкатки, и строки не прыгают при каждой правке сообщения. Между группами
// порядок фиксирован: прод — то, ради чего читают карточку, — печатается
// первым и не должен зависеть от того, что в конкретном прогоне отчиталось
// раньше: вторичная цель бывает легче и финиширует раньше, и без этого
// правила прод оказывался бы в карточке вторым.
func deployOutcomes(ds []Deploy) []Deploy {
	var order []string
	byApp := map[string]Deploy{}
	for _, raw := range ds {
		d := sanitizeDeploy(raw)
		key := d.App
		if key == "" {
			key = d.Title
		}
		prev, seen := byApp[key]
		if !seen {
			order = append(order, key)
		}
		// Исход не откатывается назад в «выкатывается…»: события могут доехать
		// не по порядку (контракт, §6), и запоздавший started не имеет права
		// стереть уже объявленный итог.
		if seen && d.Kind == deployStarted && prev.Kind != deployStarted {
			continue
		}
		if seen {
			d = mergeDeploy(prev, d)
		}
		byApp[key] = d
	}
	out := make([]Deploy, 0, len(order))
	for _, k := range order {
		out = append(out, byApp[k])
	}
	sort.SliceStable(out, func(i, j int) bool {
		return isPrimaryTarget(out[i]) && !isPrimaryTarget(out[j])
	})
	return out
}

// isPrimaryTarget — цель того же имени, что и проект (APP совпадает с id
// проекта в реестре, конвенция всего хозяйства: metro, snakes, samoylove...).
// Такая цель — прод, а не одна из дополнительных (editor, admin-ui):
// вторичные цели именуются с суффиксом («metro-editor», «chillhub-admin-ui»)
// и этому условию не удовлетворяют.
func isPrimaryTarget(d Deploy) bool { return d.App != "" && d.App == d.Project }

// outcomeText — исход цели строкой в карточке прогона.
//
// Глагол и деталь берутся из той же таблицы и той же функции, что и у
// одиночного сообщения (deployOutcomeTable, deployDetailText): раньше здесь
// был свой, женский род («провалена», «откачена — …» про «цель»), и один и
// тот же исход читался по-разному в зависимости от числа целей в прогоне.
func outcomeText(d Deploy) string {
	verb := deployVerb(d.Kind)
	if verb == "" {
		// Незнакомый вид события — не повод потерять строку: пропавшая цель
		// читается как «её не катили», и это враньё.
		return "исход неизвестен"
	}
	if d.Kind == deployRollback && d.Version != "" {
		return verb + " на " + versionHTML(d.Version, d.CommitURL)
	}
	if det := deployDetailText(d); det != "" {
		return verb + " — " + det
	}
	return verb
}

func outcomeIcon(d Deploy) string { return deployIcon(d.Kind) }

// runVersion — версия прогона и адрес её коммита.
//
// Берётся первая объявленная: коммит у прогона один, значит и версия одна.
// Событие ручного отката пропускается — его version называет старый релиз.
func runVersion(ds []Deploy) (version, commitURL string) {
	for _, d := range ds {
		if d.Kind == deployRollback || d.Version == "" {
			continue
		}
		return d.Version, d.CommitURL
	}
	return "", ""
}

// runCommitHTML — коммит прогона ссылкой и коротким sha.
//
// Пуш один на весь прогон, поэтому коммит — единственное, что у целей общее и
// в шапке не соврёт. Показывается семь знаков: длиннее в заголовке не нужно, а
// ведёт ссылка на полный адрес.
func runCommitHTML(ds []Deploy) string {
	for _, d := range ds {
		if !commitURLRe.MatchString(d.CommitURL) {
			continue
		}
		_, sha, ok := strings.Cut(d.CommitURL, "/commit/")
		if !ok || len(sha) < 7 {
			continue
		}
		return `<a href="` + esc(d.CommitURL) + `">` + esc(sha[:7]) + `</a>`
	}
	return ""
}

// runCompare — пара «прошлый релиз, коммит этого» для ссылки на сравнение.
//
// Берётся первая цель, у которой есть обе половины. Порядок целей здесь не
// случайный: deployOutcomes ставит прод первым, то есть ссылка описывает ту
// цель, ради которой карточку и читают.
func runCompare(ds []Deploy) (previous, commitURL string) {
	for _, d := range ds {
		if d.Kind == deployRollback || d.Previous == "" || d.CommitURL == "" {
			continue
		}
		return d.Previous, d.CommitURL
	}
	return "", ""
}

// mergeDeploy склеивает два события ОДНОЙ цели одного прогона.
//
// Событий об одной выкатке приезжает два, и они дополняют друг друга, а не
// повторяют. Пайплайн знает адрес коммита, список изменений и адрес прогона;
// release.sh на сервере знает то, чего пайплайн знать не может, — на какой
// релиз в действительности показывал симлинк до переключения (previous) и чем
// закончился автооткат. Раньше сюда доезжало только последнее событие, и
// карточка теряла половину: «была …» или список изменений, смотря что пришло
// вторым.
//
// Правило простое: НОВОЕ событие главнее в том, что описывает исход (вид,
// время, стадия, причина), а поля-факты подхватываются у прежнего, если новое
// их не принесло. Затирать непустое пустым нельзя ни в одном поле — это и есть
// та потеря, ради которой функция заведена.
func mergeDeploy(prev, next Deploy) Deploy {
	out := next
	if out.Title == "" {
		out.Title = prev.Title
	}
	if out.URL == "" {
		out.URL = prev.URL
	}
	if out.Project == "" {
		out.Project = prev.Project
	}
	if out.Version == "" {
		out.Version = prev.Version
	}
	if out.Previous == "" {
		out.Previous = prev.Previous
	}
	if out.CommitURL == "" {
		out.CommitURL = prev.CommitURL
	}
	if out.RunURL == "" {
		out.RunURL = prev.RunURL
	}
	if len(out.Changelog) == 0 {
		out.Changelog = prev.Changelog
	}
	if out.At.IsZero() {
		out.At = prev.At
	}
	return out
}

// runChangelog — список изменений прогона. Тоже первый непустой: он общий для
// всех целей, и печатать его положено один раз.
func runChangelog(ds []Deploy) []string {
	for _, d := range ds {
		if len(d.Changelog) > 0 {
			return d.Changelog
		}
	}
	return nil
}

// runRepoURL — репозиторий прогона. Цели одного прогона живут в одном
// репозитории по построению (ключ группы включает github.repository), поэтому
// годится первый известный адрес коммита.
func runRepoURL(ds []Deploy) string {
	for _, d := range ds {
		if u := repoFromCommitURL(d.CommitURL); u != "" {
			return u
		}
	}
	return ""
}

func latestAt(ds []Deploy) time.Time {
	var at time.Time
	for _, d := range ds {
		if d.At.After(at) {
			at = d.At
		}
	}
	return at
}

// ------------------------------------------------------------ список изменений

// Вид блока задан один раз — в deploy-kit/bin/changelog: заголовок
// «Изменения», пункты с «•», хвост «…и ещё 12 коммитов».
//
// Тот генератор — шелл-скрипт поверх git log, и здесь он неприменим: бот
// живёт на сервере, где нет ни одного из выкаченных репозиториев, и спросить
// «что изменилось» ему попросту не у кого. Поэтому список приезжает к боту
// данными: выкатка кладёт его в version.json рядом с версией, агент переносит
// в summary.json (agent/main.go, changelogField), а здесь остаётся только
// оформление.
//
// Оформление повторяет генератор намеренно, вплоть до склонения слова
// «коммит». Сообщений о релизе в хозяйстве несколько; как только формат
// разъезжается, ленту релизов становится невозможно читать. Числа ниже — его
// же умолчания.
//
// СЧИТАЕМ СИМВОЛЫ, А НЕ БАЙТЫ. Потолок темы задан владельцем и записан в
// CLAUDE.md: 120 символов. Ровно столько же режет генератор (--width 120), и
// бот — последний в цепочке, то есть единственное место, где читатель обрезку
// вообще должен увидеть.
//
// Байты стояли здесь не по недосмотру: раньше в байтах считал бюджет сам
// генератор, и разойтись с ним было бы хуже. Но для кириллицы байт вдвое
// строже символа, и прежние 100 байт означали 50 символов — из 287 настоящих
// тем хозяйства 43 (15%) обрывались посреди фразы. А поскольку своя мерка была
// у каждого — 100 байт у генератора, 300 у агента, 100 здесь, — одна и та же
// тема резалась дважды на разной длине, и второй рез приходился уже на
// многоточие первого.
//
// Правило простое: БОТ НЕ СТРОЖЕ ГЕНЕРАТОРА. Тема в 120 символов доезжает до
// читателя целиком. Свои пределы бот держит по-прежнему — текст приезжает из
// чужого файла по сети, — но срабатывают они только на враньё в этом файле.
//
// И ГЛАВНОЕ: СПИСОК БОЛЬШЕ НЕ ОБРЕЗАЕТСЯ ПО ЧИСЛУ ПУНКТОВ. Раньше здесь стояло
// changelogMax = 8, а всё сверх него сворачивалось в строку «…и ещё 1 коммит».
// Владелец сказал про неё прямо: она стоит строки и не сообщает ничего — какой
// именно коммит уехал, по ней не узнать. Настоящее ограничение одно — 4096
// единиц UTF-16 на сообщение у Telegram, и ответ на него не «показать меньше»,
// а «разложить по нескольким сообщениям» (splitMessage). Порядок при этом
// сохраняется, и не теряется ничего.
const (
	changelogWidth = 120 // СИМВОЛОВ на пункт, как и у генератора (--width 120)
	// changelogBudget — весь блок целиком, в символах.
	//
	// Это ЗАЩИТА ОТ ВРАНЬЯ В ЧУЖОМ ФАЙЛЕ, а не оформительский предел: срабатывать
	// на честном входе он не должен вовсе. Сорок тысяч символов — это около
	// десяти сообщений Telegram и втрое больше самого крупного релиза в истории
	// хозяйства (41 коммит, ≈ 5000 символов). Настоящий предел ставит агент —
	// 100 пунктов и 15 000 символов на сборку (agent/main.go, changelogChars);
	// эти числа выше нарочно, чтобы бот не оказался тем, кто урезал уже
	// разрешённое.
	changelogBudget = 40000
	// changelogReserve — место под строку о том, что упёрлись в потолок.
	// Резервируется заранее: дописывать её в уже израсходованный бюджет значило
	// бы врать про предел ровно в том сообщении, которое из-за него и урезано.
	changelogReserve = 60
	// changelogScan — сколько строк вообще разбираем. Поле приходит по сети,
	// и его длина ничем не ограничена; разбирать десять тысяч строк бот не
	// обязан. Пятьсот — впятеро больше того, что кладёт агент.
	changelogScan = 500
)

// ------------------------------------------------- «а что, собственно, уехало»

// releaseVersionRe — имя релиза deploy-kit: release-<дата>-<время>-<sha> или
// manual-… . Шаблон узкий намеренно: из версии достаётся КОММИТ, и принять за
// него хвост чужой схемы («1.5.28», «20260822») значит построить ссылку в
// никуда. Ссылка в никуда хуже её отсутствия: отсутствие видно сразу, а
// неверная — только по клику.
var releaseVersionRe = regexp.MustCompile(`^(?:release|manual)-[0-9]{8}-[0-9]{6}-([0-9a-f]{7,40})$`)

// commitOfVersion — короткий sha из имени релиза; пусто, если схема другая.
func commitOfVersion(v string) string {
	m := releaseVersionRe.FindStringSubmatch(v)
	if m == nil {
		return ""
	}
	return m[1]
}

// compareHTML — ссылка «посмотреть, что именно уехало».
//
// Сообщение о релизе называет версию и перечисляет темы коммитов, но проверить
// их было негде: тема — это то, что автор НАПИСАЛ, а не то, что он изменил.
// Ссылка на коммит показывает один коммит, а релиз почти всегда несёт больше.
// Диапазон «прошлый релиз → этот» — единственное место, где видно всё
// изменение целиком, и GitHub умеет показать его одним адресом.
//
// Обе половины диапазона у бота уже есть и обе проверены: previous — имя
// прошлого релиза из события сервера (только он знает, на что показывал
// симлинк до переключения), текущий коммит — из commitURL, который проходит
// commitURLRe. Ничего чужого в адрес не подставляется: repoURL получен из того
// же проверенного commitURL.
//
// Пусто — обычный исход, а не ошибка: у первой выкатки цели нет previous, у
// цели-файла версия своя и sha в ней нет вовсе.
func compareHTML(repoURL, previous, commitURL string) string {
	if repoURL == "" {
		return ""
	}
	prev := commitOfVersion(previous)
	if prev == "" || !commitURLRe.MatchString(commitURL) {
		return ""
	}
	_, cur, ok := strings.Cut(commitURL, "/commit/")
	if !ok || cur == "" {
		return ""
	}
	// Диапазон в одну точку показывать нечего: прод уже стоит на этом коммите.
	if strings.HasPrefix(cur, prev) {
		return ""
	}
	return `<a href="` + esc(repoURL+"/compare/"+prev+"..."+cur) + `">сравнить с предыдущим релизом</a>`
}

// changelogTail — общий хвост сообщения о релизе: список изменений и ссылка,
// по которой его можно проверить.
//
// ПУСТОЙ СПИСОК ТЕПЕРЬ НАЗЫВАЕТСЯ ВСЛУХ. Раньше блока «Изменения» просто не
// было, и три разных случая выглядели одинаково: изменений действительно нет;
// все коммиты отфильтрованы как шум (подъёмы версий, dependabot); генератор не
// отработал. Это ровно тот дефект, от которого предостерегает сам генератор:
// «пусто и непонятно почему» читается как «всё в порядке».
//
// Случай стал ЧАЩЕ после того, как список начали считать по путям цели
// (CHANGELOG_PATHS): цель, поехавшая в составе прогона и не менявшаяся сама,
// честно даёт пустой список. Раньше она показывала чужие коммиты — неправду,
// но заметную.
//
// Утверждение при этом подкреплено ссылкой: рядом с «изменений нет» стоит
// диапазон, по которому это видно. Бот не знает, почему список пуст, и не
// делает вид, что знает, — он даёт читателю проверить за один клик.
func changelogTail(lines []string, repoURL, previous, commitURL string) string {
	cl := formatChangelog(lines, repoURL)
	cmp := compareHTML(repoURL, previous, commitURL)
	switch {
	case cl != "" && cmp != "":
		return "\n\n" + cl + "\n" + cmp
	case cl != "":
		return "\n\n" + cl
	case cmp != "":
		return "\n\n<i>изменений в этом релизе нет</i> · " + cmp
	default:
		return "\n\n<i>изменений в этом релизе нет</i>"
	}
}

// formatChangelog собирает блок «Изменения» из простых текстовых строк.
//
// Пустой результат — нормальный исход и главное требование к этой функции:
// выкатка, которая ничего не положила в version.json, обязана дать ровно то
// же сообщение о релизе, что и раньше. Список изменений — украшение поверх
// уведомления, и его отсутствие не повод менять уведомление.
//
// Экранирование делается здесь и только здесь. Текст чужой: он приезжает из
// version.json по сети, а сообщение уходит с parse_mode=HTML, где одного «<»
// в теме коммита достаточно, чтобы разметка стала невалидной. На такую
// разметку Telegram отвечает ошибкой — то есть уведомление о релизе не
// приходит совсем, и вместо украшения получается потерянное сообщение.
func formatChangelog(lines []string, repoURL string) string {
	// Неразобранные строки всё равно считаются: хвост «…и ещё N» обязан
	// говорить про весь список, а не про ту его часть, до которой дошли руки.
	scanned := scanLines(lines)
	unscanned := len(lines) - len(scanned)
	items, tail := changelogItems(scanned)
	// Номер PR приезжает голым: разметку срезает доставка события. Ссылку
	// строим сами, из проверенного адреса репозитория.
	items = linkifyPullRefs(items, repoURL)
	if len(items) == 0 {
		return ""
	}

	var b strings.Builder
	head := "<b>Изменения</b>"
	b.WriteString(head)
	// Бюджет считается в символах, как и ширина пункта. Раньше здесь стояла
	// b.Len(), то есть БАЙТЫ, а ширина — тоже байты, и пока они совпадали,
	// расхождение было незаметно. Стоит перевести одно число в символы и
	// забыть про второе — и блок из восьми кириллических тем по 120 символов
	// (около 2100 байт) обрывался бы на третьем пункте.
	used := utf8.RuneCountInString(head)
	shown := 0
	for _, it := range items {
		line := "\n• " + it.render()
		n := utf8.RuneCountInString(line)
		if used+n+changelogReserve > changelogBudget {
			break
		}
		b.WriteString(line)
		used += n
		shown++
	}
	if shown == 0 {
		return ""
	}
	// Хвост про непоместившееся остаётся ровно для ненормального случая: список
	// упёрся в потолок против вранья (changelogBudget) или в потолок разбора
	// (changelogScan). На честных данных ни то ни другое не срабатывает, и
	// строки этой владелец больше не видит — за что её и не любили. А молчать,
	// когда потолок ВСЁ-ТАКИ сработал, нельзя: молча оборванный список читается
	// как «больше ничего и не было».
	if rest := len(items) - shown + unscanned; rest > 0 {
		tail = fmt.Sprintf("…и ещё %d %s — список не поместился", rest, pluralCommits(rest))
	}
	if tail != "" {
		b.WriteString("\n" + esc(cutRunes(tail, changelogWidth)))
	}
	return b.String()
}

// scanLines — сколько строк вообще разбираем.
//
// Поле приходит по сети, и его длина ничем не ограничена; разбирать десять
// тысяч строк ради восьми пунктов бот не обязан.
func scanLines(lines []string) []string {
	if len(lines) > changelogScan {
		return lines[:changelogScan]
	}
	return lines
}

// changelogMarkers — маркеры пунктов, которые ставит генератор. Список тот же,
// что в agent/main.go, normalizeChangelog: расхождение здесь означало бы, что
// на одном пути выкатки маркер снимается, а на другом уезжает в сообщение.
var changelogMarkers = []string{"•", "-", "*", "–", "—"}

// trimMarker снимает маркер пункта.
//
// За маркером обязан идти пробел (или ничего): без этого условия «-Wall в
// CFLAGS» превратилось бы в «Wall в CFLAGS». Голый маркер без темы — пустой
// пункт, и он отсеивается дальше как пустая строка.
func trimMarker(s string) string {
	for _, m := range changelogMarkers {
		rest, ok := strings.CutPrefix(s, m)
		if !ok {
			continue
		}
		if rest == "" || strings.HasPrefix(rest, " ") {
			return strings.TrimSpace(rest)
		}
		break
	}
	return s
}

// changelogItems приводит чужие строки к пунктам: по строке на пункт, без
// заголовка, без маркеров и без HTML-подстановок. Возвращает пункты и хвост
// генератора («…и ещё 12 коммитов»), если он был.
//
// Разбор намеренно повторяет agent/main.go, normalizeChangelog, а не полагается
// на него. Комментарий к summary.json описывает путь «файл собран мимо агента»
// как рабочий, и на нём сюда приезжает ровно то, что напечатал
// deploy-kit/bin/changelog: с «<b>Изменения</b>» первой строкой, с «•» в начале
// пунктов и с уже экранированными & < >. Пока разбор был односторонним, этот
// заголовок доезжал до владельца ПУНКТОМ СПИСКА — под настоящим заголовком,
// который бот рисует сам, да ещё и экранированным во второй раз:
//
//	<b>Изменения</b>
//	• &lt;b&gt;Изменения&lt;/b&gt;
//	• обновить nginx до 1.24
//
// Бот — читатель чужого файла, и его дело не рассчитывать на чистый вход, а
// приводить к своему виду то, что пришло.
//
// Разэкранирование парное к esc() при выводе: без него «go 1.22 &lt;-- важно»
// доехало бы как «go 1.22 &amp;lt;-- важно». Для строк, которые агент уже
// разэкранировал, шаг вхолостую — обычный текст он не трогает.
func changelogItems(lines []string) (items []clItem, tail string) {
	for _, raw := range lines {
		// Пункт списка обязан быть однострочным: перевод строки внутри темы
		// коммита разорвал бы список пополам.
		s := strings.Join(strings.Fields(raw), " ")
		s = trimMarker(s)
		// Ссылку на PR отделяем ПЕРВОЙ и от сырой строки. Разэкранирование для
		// неё яд, обрезка — тоже, а экранирование на выводе превратило бы её в
		// текст «<a href=…>». Всё, что не совпало с единственным разрешённым
		// видом ссылки (splitRefLink), остаётся текстом и экранируется как
		// текст — то есть чужая разметка не становится разметкой никогда.
		text, href, label, hasRef := splitRefLink(s)
		text = strings.TrimSpace(html.UnescapeString(text))
		switch {
		case text == "":
			// Пустая строка — не пункт. Голая ссылка тоже: splitRefLink её не
			// признаёт, а пустой текст пунктом не является.
		case isChangelogHeader(text):
			// Заголовок блока рисует бот, и второй раз он не нужен.
		case strings.HasPrefix(text, "…"), strings.HasPrefix(text, "..."):
			// Хвост генератора («…и ещё 12 коммитов») доезжает сюда обычной
			// строкой. Пунктом списка он не является и маркер получить не
			// должен.
			tail = text
		default:
			it := clItem{text: text}
			if hasRef {
				it.href, it.label = href, label
			}
			items = append(items, it)
		}
	}
	return items, tail
}

// clItem — один пункт списка изменений: текст темы и, если она была, ссылка на
// PR.
//
// Разделены нарочно. Текст — чужой и до последнего момента остаётся текстом:
// его режут по ширине и экранируют на выводе. Ссылка — наоборот: её нельзя ни
// резать, ни экранировать, зато её адрес и подпись уже проверены по списку
// разрешённого. Пока они лежали в одной строке, любое из двух правил ломало
// другое.
type clItem struct {
	text  string
	href  string // пусто — ссылки нет
	label string // «#21»
}

// render — готовый к отправке HTML одного пункта.
//
// Обрезаем ДО экранирования: иначе рез мог бы прийтись на середину «&amp;», и
// в сообщение уехал бы огрызок сущности — Telegram отвечает на негодную
// разметку ошибкой, то есть уведомление молча не приходит.
//
// Ширину считает только текст. Ссылка занимает в сообщении место, но не
// занимает его на экране: читатель видит «#21», а не адрес. Резать тему из-за
// длины невидимого адреса значило бы наказывать её за него.
func (it clItem) render() string {
	s := esc(cutRunes(it.text, changelogWidth))
	if it.href != "" {
		s += " " + refLinkHTML(it.href, it.label)
	}
	return s
}

// pullRefRe — хвост «#21», который GitHub дописывает к теме при сквош-мерже.
// Только в конце строки: «#5» посреди темы («починить #5 из списка») номером
// PR не является, и уводить читателя по нему некуда.
var pullRefRe = regexp.MustCompile(`\s+#([0-9]{1,9})$`)

// repoFromCommitURL достаёт адрес репозитория из адреса коммита.
//
// Пусто, если адрес не прошёл commitURLRe: доверять здесь можно только тому,
// что уже проверено, иначе ссылка на PR стала бы способом увести читателя
// куда угодно чужой строкой из события.
func repoFromCommitURL(commitURL string) string {
	if !commitURLRe.MatchString(commitURL) {
		return ""
	}
	base, _, ok := strings.Cut(commitURL, "/commit/")
	if !ok {
		return ""
	}
	return base
}

// linkifyPullRefs возвращает номеру PR ссылку, которую он потерял по дороге.
//
// Генератор списка (deploy-kit/bin/changelog) отдаёт «тема <a …>#21</a>», но
// доставка события срезает разметку намеренно: в событии обязан лежать простой
// текст, иначе бот получает недоверенный HTML (docs/events.md §4). До читателя
// номер доезжал голым, и из чата в PR было не уйти.
//
// Поэтому ссылка не ПРОВОЗИТСЯ, а СТРОИТСЯ здесь — из адреса репозитория,
// который бот и так знает по проверенному адресу коммита. Ничего чужого в
// href не попадает: от строки события берётся только число.
//
// splitRefLink остаётся на месте и разбирает случай, когда ссылка всё же
// приехала: путь «файл собран мимо агента» (summary.json) её сохраняет.
func linkifyPullRefs(items []clItem, repoURL string) []clItem {
	if repoURL == "" {
		return items
	}
	for i, it := range items {
		if it.href != "" {
			continue
		}
		m := pullRefRe.FindStringSubmatch(it.text)
		if m == nil {
			continue
		}
		items[i].text = strings.TrimSpace(it.text[:len(it.text)-len(m[0])])
		items[i].href = repoURL + "/pull/" + m[1]
		items[i].label = "#" + m[1]
	}
	return items
}

// isChangelogHeader — эта строка и есть заголовок блока.
//
// Проверяем после разэкранирования, поэтому одной проверки хватает на обе
// формы: «<b>Изменения</b>» и «&lt;b&gt;Изменения&lt;/b&gt;» к этому моменту
// уже одно и то же.
func isChangelogHeader(s string) bool {
	for _, h := range []string{"<b>Изменения</b>", "Изменения"} {
		if strings.EqualFold(s, h) {
			return true
		}
	}
	return false
}

// ------------------------------------------------- ссылка на PR внутри пункта

// refLinkRe — ЕДИНСТВЕННАЯ разметка, которой позволено проехать из данных в
// сообщение. Всё, что не совпало с этим выражением, экранируется как текст.
//
// Выражение и splitRefLink дословно повторены в агенте (agent/main.go), и это
// не небрежность, а условие: агент и бот — отдельные модули без общего пакета,
// а решать «ссылка это или нет» они обязаны одинаково. Разойдутся правила —
// разойдётся и вывод, причём в сторону, которую видно только у читателя.
// Правите здесь — правьте там же.
//
// БОТ ПРОВЕРЯЕТ САМ, А НЕ ДОВЕРЯЕТ АГЕНТУ. Путь «summary.json собран мимо
// агента» описан в summary.go как рабочий, а releases.json бот и вовсе читает
// с диска: положиться на то, что кто-то выше по течению уже всё проверил, —
// это положиться на предположение, а не на код.
//
// Выражение — список разрешённого, и каждая его часть отвечает за свою угрозу:
//
//   - «^(.*?) ?» и «$» — якорь только В КОНЦЕ пункта;
//   - «<a href="…">» без пробела перед «>» — атрибут ровно один: ни onclick,
//     ни target, ни title сюда не пролезут;
//   - «https://» буквально — ни javascript:, ни data:, ни http:;
//   - алфавит адреса без «"», «<», «>», «&» и пробела — из атрибута нельзя
//     выйти и нельзя открыть второй тег;
//   - «#[0-9]{1,12}» — подписью может быть только номер: вложенный тег
//     содержит «<», цифрой не является и совпадения не даст;
//   - «{1,200}» и «{1,12}» — длина ограничена: текст приезжает из чужого файла.
var refLinkRe = regexp.MustCompile(`^(.*?) ?<a href="(https://[A-Za-z0-9._~:/%+-]{1,200})">#([0-9]{1,12})</a>$`)

// splitRefLink отделяет от пункта разрешённую ссылку на PR.
//
// Не нашлась — текст возвращается неизменным, и это нормальный, а не
// исключительный исход: ссылку генератор ставит, только когда знает адрес
// репозитория, а до недавнего времени не ставил вовсе.
func splitRefLink(s string) (text, href, label string, ok bool) {
	m := refLinkRe.FindStringSubmatch(s)
	if m == nil {
		return s, "", "", false
	}
	// Разметка перед ссылкой — повод не поверить строке целиком. Здесь же
	// отсеиваются вторая ссылка, незакрытый тег и «<script>» перед ссылкой:
	// разбирать такое незачем, текстом оно безопасно.
	if strings.ContainsAny(m[1], "<>") {
		return s, "", "", false
	}
	// Пункт из одной ссылки — не пункт: читателю «#21» без темы не говорит
	// ничего.
	if strings.TrimSpace(m[1]) == "" {
		return s, "", "", false
	}
	return m[1], m[2], "#" + m[3], true
}

// refLinkHTML собирает ссылку обратно из проверенных кусков.
//
// Именно собирает, а не переносит чужую строку: так в разметку физически
// нечему просочиться, кроме адреса из разрешённого алфавита и цифр номера.
// Экранирование поверх этого стоит ноль и переживает правку алфавита в
// refLinkRe.
func refLinkHTML(href, label string) string {
	return `<a href="` + esc(href) + `">` + esc(label) + `</a>`
}

// ------------------------------------------------- версия как ссылка на коммит

// commitURLRe — вторая, независимая проверка адреса коммита.
//
// Первую делает агент (agent/main.go, commitURL), и её мало ровно по той же
// причине, по какой мало её для ссылок на PR: summary.json может быть собран
// мимо агента — это записано в summary.go как рабочий путь, — а сообщение
// уходит с parse_mode=HTML. Доверять полю в чужом файле только потому, что
// кто-то выше по течению обещал его проверить, здесь нельзя.
//
// Список разрешённого, а не запрещённого:
//
//   - «https://github.com/» буквально — ни другой схемы, ни другого хоста;
//   - алфавит имён без «"», «<», «>», «&» и пробела — из атрибута не выйти и
//     второй тег не открыть;
//   - первый символ имени — буква или цифра: «..» именем не станет;
//   - «[0-9a-f]{7,40}» — sha и ничего кроме: «local», которое пишет bin/deploy
//     без git, ссылкой не станет;
//   - длины ограничены сверху: строка приезжает из файла на диске.
var commitURLRe = regexp.MustCompile(
	`^https://github\.com/[A-Za-z0-9][A-Za-z0-9._-]{0,63}/[A-Za-z0-9][A-Za-z0-9._-]{0,99}/commit/[0-9a-f]{7,40}$`)

// versionHTML рисует версию релиза: ссылкой на коммит, если он известен, и
// обычным моноширинным текстом, если нет.
//
// Ради чего: версия вида release-<дата>-<sha> НАЗЫВАЕТ коммит, но не ведёт к
// нему — чтобы увидеть, что именно уехало, приходилось искать sha руками. Со
// ссылкой вопрос «что в этом релизе» решается одним касанием прямо из чата.
//
// ССЫЛКА БЕЗ <code>, А НЕ ВОКРУГ НЕГО. Моноширинный текст внутри ссылки — это
// вложенные сущности Telegram; они поддерживаются не везде и не всегда, а цена
// ошибки здесь несоразмерна: отвергнутое сообщение — это НЕ ПРИШЕДШЕЕ
// уведомление о релизе, то есть ровно то, ради чего бот и существует. Ссылка и
// так выделена цветом, различить её в сообщении нечем не хуже.
func versionHTML(version, commitURL string) string {
	if !commitURLRe.MatchString(commitURL) {
		return "<code>" + esc(version) + "</code>"
	}
	// Собираем из проверенных кусков и всё равно экранируем: экранирование
	// поверх проверки стоит ноль и переживает правку алфавита в commitURLRe.
	return `<a href="` + esc(commitURL) + `">` + esc(version) + `</a>`
}

// pluralCommits — склонение слова «коммит». Копия правила из
// deploy-kit/bin/changelog: «…и ещё 2 коммитов» в ленте релизов выглядит как
// поломка формата, а не как список изменений.
func pluralCommits(n int) string {
	if n < 0 {
		n = -n
	}
	switch h, t := n%100, n%10; {
	case h >= 11 && h <= 14:
		return "коммитов"
	case t == 1:
		return "коммит"
	case t >= 2 && t <= 4:
		return "коммита"
	default:
		return "коммитов"
	}
}

// cutRunes обрезает строку до n СИМВОЛОВ, НИКОГДА не разрезая символ UTF-8.
//
// Этим режется всё, что читает человек: тема коммита, хвост списка, имя цели в
// ответе «не знаю такую». Символ, а не байт, потому что в символах задан
// потолок темы (CLAUDE.md) и в символах же считает генератор; счёт в байтах
// означал бы для кириллицы вдвое более строгий предел, чем написано.
//
// Функция дословно повторена в агенте (agent/main.go, cutRunes), и это не
// небрежность, а условие: агент и бот — отдельные модули, общего пакета у них
// нет, а одна и та же тема обязана получиться одинаковой на обоих путях
// выкатки. Правите здесь — правьте там же.
//
// Резать посреди руны нельзя: битую строку Telegram отвергает целиком, и
// сообщение молча не приходит. Обрыв посреди слова читается хуже чуть более
// короткой строки, поэтому по возможности отступаем до пробела.
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
	// Граница слова — пробел, то есть ASCII: символ UTF-8 этим не разрезать.
	if i := strings.LastIndexByte(t, ' '); i > 0 && utf8.RuneCountInString(t[:i])*2 >= n {
		t = t[:i]
	}
	return strings.TrimRight(t, " ") + tail
}

// cutBytes обрезает строку до n БАЙТОВ, НИКОГДА не разрезая символ UTF-8.
//
// Байты здесь остались ровно для одного места, где предел байтовый на самом
// деле: callback_data кнопки, у которой Telegram считает 64 БАЙТА (view.go,
// viewFor). Для текста, который читает человек, есть cutRunes — путать их
// нельзя, иначе кириллическое имя цели в кнопке уедет вдвое длиннее лимита, а
// сообщение с негодной кнопкой Telegram не примет целиком.
//
// Многоточия здесь нет намеренно: обрезанное имя цели не показывается, оно
// возвращается обратно ключом экрана.
func cutBytes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// linkHosts — хосты, на которые боту позволено уводить читателя.
//
// СПИСОК РАЗРЕШЁННОГО, А НЕ ЗАПРЕЩЁННОГО. Хозяйство целиком живёт на двух
// именах, и третьего адреса в сообщении бота взяться неоткуда — а если он
// всё-таки взялся, значит взялся не оттуда.
var linkHosts = []string{"samoy.love", "github.com"}

// linkAlphabet — алфавит адреса, попадающего в href.
//
// Ни «"», ни «<», ни «>», ни пробела, ни управляющих символов: из атрибута
// нельзя выйти и нельзя открыть второй тег. Экранирование поверх этого всё
// равно остаётся, но опираться на него одно нельзя — оно чинит разметку, а не
// назначение ссылки.
var linkAlphabet = regexp.MustCompile(`^https://[A-Za-z0-9._~:/?#@!$&'()*+,;=%-]{1,300}$`)

// allowedURL — можно ли вести читателя по этому адресу.
//
// Проверяются ТРИ вещи, и каждая закрывает свою дыру:
//
//   - схема ровно «https»: «javascript:», «data:» и «http:» ссылкой не станут;
//   - алфавит адреса: см. linkAlphabet;
//   - хост из списка, причём разобранный, а не найденный подстрокой.
//     «https://github.com@evil.example/» ведёт на evil.example, а
//     «https://github.com.evil.example/» — тем более; обе строки содержат
//     «github.com» и обе обязаны быть отвергнуты.
//
// Зачем вообще: сообщения бота — это то, чему владелец доверяет по
// определению, и ссылка в них открывается не глядя. Адреса приезжают из
// summary.json на диске и из события выкатки, то есть из файлов, которые бот не
// писал; фишинговая ссылка от имени бота стоит дешевле любой другой.
func allowedURL(raw string) bool {
	if !linkAlphabet.MatchString(raw) {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	for _, h := range linkHosts {
		if host == h || strings.HasSuffix(host, "."+h) {
			return true
		}
	}
	return false
}

// link — подпись со ссылкой на сам сервис.
//
// Уведомление без ссылки заставляет владельца искать адрес руками ровно в тот
// момент, когда некогда: увидел «недоступен» — хочешь открыть и посмотреть.
// Непрошедший проверку адрес — не повод потерять подпись: она остаётся
// текстом, ровно как у цели, для которой адрес не настроен вовсе.
func link(text, url string) string {
	if !allowedURL(url) {
		return esc(text)
	}
	return fmt.Sprintf(`<a href="%s">%s</a>`, esc(url), esc(text))
}

// runHasSuccess — выкатилась ли в прогоне хоть одна цель.
//
// Нужно ровно одному месту: строке «изменений в этом релизе нет». Она говорит
// о релизе, а у прогона, где всё провалилось, релиза не было.
func runHasSuccess(ds []Deploy) bool {
	for _, d := range ds {
		if d.Kind == deploySuccess {
			return true
		}
	}
	return false
}

// runHasFailure — провалилась ли в прогоне хоть одна цель.
func runHasFailure(ds []Deploy) bool {
	for _, d := range ds {
		switch d.Kind {
		case deployFailure, deployRolledBack:
			return true
		}
	}
	return false
}

// runURLOf — адрес прогона. Он один на все цели (один пуш — один прогон),
// поэтому годится первый непустой.
func runURLOf(ds []Deploy) string {
	for _, d := range ds {
		if d.RunURL != "" {
			return d.RunURL
		}
	}
	return ""
}

// cardTargetName — как назвать цель ВНУТРИ карточки.
//
// Реестр отдаёт полное имя, «Лаунчер · Публичный API»: оно верное там, где
// цель стоит сама по себе. В карточке проект уже написан в шапке, и полное
// имя превращало каждую строку в повтор — «Лаунчер» в шапке и следом пять
// строк, начинающихся с того же слова.
//
// Срезается ТОЛЬКО совпадающий префикс и только если после него что-то
// осталось: у цели, названной именем проекта («Метро»), срезать нечего, и
// пустая строка вместо имени была бы хуже повтора.
func cardTargetName(head string, d Deploy) string {
	name := deployName(d)
	if p := head + " · "; strings.HasPrefix(name, p) && len(name) > len(p) {
		return name[len(p):]
	}
	return name
}
