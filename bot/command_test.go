package main

import "testing"

func TestParseCommand(t *testing.T) {
	cases := []struct {
		name string
		text string
		self string
		want string
	}{
		{"обычная команда", "/status", "samoy_love_bot", "status"},
		{"с аргументами", "/status всё ли живо", "samoy_love_bot", "status"},
		{"аргумент не меняет команду", "/changelog metro", "samoy_love_bot", "changelog"},
		{"регистр не важен", "/Status", "samoy_love_bot", "status"},
		{"пробелы по краям", "  /help  ", "samoy_love_bot", "help"},
		{"обращение к нам", "/status@samoy_love_bot", "samoy_love_bot", "status"},
		{"обращение к нам в другом регистре", "/status@Samoy_Love_Bot", "samoy_love_bot", "status"},
		{"обращение к чужому боту", "/status@other_bot", "samoy_love_bot", ""},
		{"имя бота не задано — отвечаем на любое", "/status@any_bot", "", "status"},
		{"обычный текст", "привет", "samoy_love_bot", ""},
		{"пусто", "", "samoy_love_bot", ""},
		{"только слеш", "/", "samoy_love_bot", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := parseCommand(c.text, c.self); got != c.want {
				t.Errorf("parseCommand(%q) = %q, ожидали %q", c.text, got, c.want)
			}
		})
	}
}

func TestResolveCommand(t *testing.T) {
	cases := map[string]string{
		"help":      CmdHelp,
		"start":     CmdHelp,
		"s":         CmdStatus,
		"status":    CmdStatus,
		"i":         CmdIncidents,
		"incidents": CmdIncidents,
		"c":         CmdChangelog,
		"changelog": CmdChangelog,
		"q":         CmdQuiet,
		"quiet":     CmdQuiet,
		// Псевдонимы, снесённые сведением к одному короткому на команду:
		// каждой команде положена ровно одна короткая форма, и «cl»/«changes»
		// у /changelog, «state» у /status и «log» у /incidents были лишними.
		// /versions снесён целиком: версия и список изменений теперь
		// приходят одной карточкой при выкатке.
		"cl":       "",
		"changes":  "",
		"state":    "",
		"log":      "",
		"v":        "",
		"versions": "",
		"выкатка":  "",
		"":         "",
	}
	for word, want := range cases {
		if got := resolveCommand(word); got != want {
			t.Errorf("resolveCommand(%q) = %q, ожидали %q", word, got, want)
		}
	}
}
