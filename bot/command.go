package main

import "strings"

// Команды бота. Короткие псевдонимы есть у каждой: команды набирают с
// телефона, и /v вместо /versions экономит больше, чем кажется.
const (
	CmdHelp      = "help"
	CmdStatus    = "status"
	CmdVersions  = "versions"
	CmdIncidents = "incidents"
	CmdChangelog = "changelog"
	// CmdQuiet — тишина по требованию, а не только по кнопке под уже
	// случившейся аварией: «/quiet 2h» перед плановыми работами или ручной
	// выкаткой, «/quiet off», чтобы снять раньше срока, голый «/quiet» —
	// посмотреть, молчит ли бот сейчас.
	CmdQuiet = "quiet"
)

var aliases = map[string]string{
	"help":      CmdHelp,
	"start":     CmdHelp,
	"h":         CmdHelp,
	"status":    CmdStatus,
	"s":         CmdStatus,
	"state":     CmdStatus,
	"versions":  CmdVersions,
	"version":   CmdVersions,
	"v":         CmdVersions,
	"incidents": CmdIncidents,
	"i":         CmdIncidents,
	"log":       CmdIncidents,
	"changelog": CmdChangelog,
	"changes":   CmdChangelog,
	"cl":        CmdChangelog,
	"c":         CmdChangelog,
	"quiet":     CmdQuiet,
	"q":         CmdQuiet,
}

// parseCommand достаёт команду из текста сообщения.
//
// Возвращает пустую строку, если это не команда вовсе или команда адресована
// другому боту: в группе с несколькими ботами сообщение «/status@other_bot»
// приходит всем, и отвечать на чужое обращение — верный способ мешать.
//
// self передаётся без «@» и сравнивается без учёта регистра, потому что
// Telegram сохраняет регистр так, как его набрал отправитель.
func parseCommand(text, self string) string {
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, "/") {
		return ""
	}
	word := strings.Fields(text)[0]
	word = strings.TrimPrefix(word, "/")
	if name, at, found := strings.Cut(word, "@"); found {
		if self != "" && !strings.EqualFold(at, self) {
			return ""
		}
		word = name
	}
	return strings.ToLower(word)
}

// resolveCommand приводит псевдоним к канонической команде.
// Пустая строка означает «не наша команда».
func resolveCommand(word string) string {
	return aliases[word]
}

// commandArg — то, что владелец дописал после команды.
//
// Отдельная функция, а не второе значение parseCommand: команда есть у каждого
// сообщения, а аргумент нужен ровно одной из них (/changelog), и менять
// подпись, которую зовут из трёх мест, ради одного вызова незачем.
//
// Пробелы схлопываются вместе с переводами строк: аргумент набирают с телефона,
// где автозамена легко добавляет лишний пробел, а «/changelog  metro» и
// «/changelog metro» обязаны означать одно и то же.
func commandArg(text string) string {
	f := strings.Fields(strings.TrimSpace(text))
	if len(f) < 2 {
		return ""
	}
	return strings.Join(f[1:], " ")
}
