package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const owner = int64(173418650)

// recorder — поддельный Bot API, запоминающий отправленное целиком.
//
// Пишем весь запрос, а не только text: часть содержания уехала на кнопки
// (состояние проекта нарисовано на самой кнопке), и проверка «владелец это
// увидел» не должна зависеть от того, текст это или разметка.
func recorder(t *testing.T, sent *[]string) *Telegram {
	t.Helper()
	return testBot(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		*sent = append(*sent, string(raw))
		_, _ = w.Write([]byte(`{"ok":true,"result":{}}`))
	})
}

func writeSummary(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "summary.json")
	b, err := json.Marshal(summaryAt(time.Now().UTC(), "up", time.Now().UTC(), true, "v1"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func message(chat int64, text string) Update {
	u := Update{UpdateID: 1, Message: &Message{Text: text}}
	u.Message.Chat.ID = chat
	// В личной переписке id чата и id отправителя — одно и то же число.
	// Повторяем это здесь, чтобы проверка отправителя работала на настоящих
	// данных, а не проскакивала мимо пустого From.
	u.Message.From.ID = chat
	return u
}

func TestForeignChatIgnoredSilently(t *testing.T) {
	var sent []string
	tg := recorder(t, &sent)
	handleUpdate(context.Background(), tg, message(999, "/status"), owner, owner, "samoy_love_bot", writeSummary(t))
	if len(sent) != 0 {
		t.Fatalf("на чужое сообщение бот ответил: %v", sent)
	}
}

func TestReplyToOwner(t *testing.T) {
	summary := writeSummary(t)
	cases := []struct {
		name string
		text string
		want string
	}{
		{"справка", "/help", "/status"},
		{"состояние", "/status", "Snakes"},
		{"версии", "/versions", "v1"},
		{"инциденты", "/incidents", "Инцидентов не было"},
		{"короткий псевдоним", "/s", "Snakes"},
		{"неизвестная команда", "/deploy", "Не знаю такую команду"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var sent []string
			tg := recorder(t, &sent)
			handleUpdate(context.Background(), tg, message(owner, c.text), owner, owner, "samoy_love_bot", summary)
			if len(sent) != 1 {
				t.Fatalf("ожидали один ответ, получили %d: %v", len(sent), sent)
			}
			if !strings.Contains(sent[0], c.want) {
				t.Errorf("в ответе нет %q:\n%s", c.want, sent[0])
			}
		})
	}
}

// В группе id чата общий на всех: проверка «сообщение пришло в нужный чат»
// пропускает любого участника. Поэтому, когда владелец задан явно
// (TELEGRAM_OWNER_ID), сверяется и отправитель.
func TestGroupChatObeysOnlyOwner(t *testing.T) {
	const group = int64(-1001234567890)
	summary := writeSummary(t)

	stranger := message(group, "/status")
	stranger.Message.From.ID = 42
	var sent []string
	tg := recorder(t, &sent)
	handleUpdate(context.Background(), tg, stranger, group, owner, "samoy_love_bot", summary)
	if len(sent) != 0 {
		t.Fatalf("бот выполнил команду постороннего участника группы: %v", sent)
	}

	mine := message(group, "/status")
	mine.Message.From.ID = owner
	sent = nil
	handleUpdate(context.Background(), tg, mine, group, owner, "samoy_love_bot", summary)
	if len(sent) != 1 {
		t.Fatalf("владельцу бот обязан ответить, ответов %d", len(sent))
	}
}

func TestPlainTextGetsNoReply(t *testing.T) {
	var sent []string
	tg := recorder(t, &sent)
	// Обычная реплика в чате — не повод отвечать: бот не собеседник.
	handleUpdate(context.Background(), tg, message(owner, "как дела"), owner, owner, "samoy_love_bot", writeSummary(t))
	if len(sent) != 0 {
		t.Fatalf("бот ответил на обычный текст: %v", sent)
	}
}

func TestMissingDataIsReported(t *testing.T) {
	var sent []string
	tg := recorder(t, &sent)
	// Агент не создал файл — владелец должен узнать об этом, а не получить
	// пустой ответ.
	handleUpdate(context.Background(), tg, message(owner, "/status"), owner, owner, "", filepath.Join(t.TempDir(), "нет.json"))
	if len(sent) != 1 || !strings.Contains(sent[0], "данные агента") {
		t.Fatalf("о недоступных данных не сообщено: %v", sent)
	}
}

func TestSelfTestIsSilentForOwner(t *testing.T) {
	var paths []string
	tg := testBot(t, func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		_, _ = w.Write([]byte(`{"ok":true,"result":{}}`))
	})

	if err := selfTest(context.Background(), tg, owner, writeSummary(t)); err != nil {
		t.Fatalf("проверка канала не прошла: %v", err)
	}

	// Главное в этом тесте: выкатка бота НЕ пишет владельцу. Раньше selftest
	// слал полную сводку, и за вечер с десятком выкаток человек получал десяток
	// карточек «всё работает», которых не просил.
	for _, p := range paths {
		if strings.Contains(p, "sendMessage") {
			t.Fatalf("selftest отправил сообщение владельцу: %v", paths)
		}
	}
	if len(paths) != 1 || !strings.Contains(paths[0], "sendChatAction") {
		t.Fatalf("ожидали одну проверку канала через sendChatAction, получили %v", paths)
	}

	// Молчаливость не должна превратиться в «проверка ничего не проверяет»:
	// нет данных агента — провал, иначе выкатка сочтёт сломанного бота живым.
	if err := selfTest(context.Background(), tg, owner, filepath.Join(t.TempDir(), "нет.json")); err == nil {
		t.Fatal("без данных агента проверка не может считаться успешной")
	}
}

func TestEnvDuration(t *testing.T) {
	t.Setenv("REMIND_INTERVAL", "3m")
	if got := envDuration("REMIND_INTERVAL", time.Hour); got != 3*time.Minute {
		t.Errorf("интервал из окружения не прочитан: %s", got)
	}
	// Опечатка в файле окружения не должна ронять бота: лучше значение по
	// умолчанию, чем служба, которая не стартует.
	t.Setenv("REMIND_INTERVAL", "пятнадцать минут")
	if got := envDuration("REMIND_INTERVAL", time.Hour); got != time.Hour {
		t.Errorf("при мусоре в окружении ожидали значение по умолчанию, получили %s", got)
	}
	t.Setenv("REMIND_INTERVAL", "")
	if got := envDuration("REMIND_INTERVAL", 15*time.Minute); got != 15*time.Minute {
		t.Errorf("без переменной ожидали значение по умолчанию, получили %s", got)
	}
}

// Владелец по умолчанию — тот же, что и чат: в личной переписке это одно и то
// же число, и настраивать отдельно нечего. Отдельная переменная нужна только
// групповому чату, где по chat id владельца не опознать.
func TestOwnerUserDefaultsToChat(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN", "не-настоящий")
	t.Setenv("TELEGRAM_CHAT_ID", "173418650")

	c, err := loadConfig()
	if err != nil {
		t.Fatalf("настройки не прочитаны: %v", err)
	}
	if c.OwnerUser != c.Owner {
		t.Errorf("в личной переписке владелец должен совпадать с чатом: %d и %d", c.OwnerUser, c.Owner)
	}

	// Группа без явного владельца: подтвердить права нечем, и бот об этом
	// говорит в журнале, а не делает вид, что всё в порядке.
	t.Setenv("TELEGRAM_CHAT_ID", "-1001234567890")
	if c, err = loadConfig(); err != nil {
		t.Fatalf("групповой чат не должен ронять бота: %v", err)
	}
	if c.OwnerUser != 0 {
		t.Errorf("по id группы владелец не определяется, получили %d", c.OwnerUser)
	}

	t.Setenv("TELEGRAM_OWNER_ID", "173418650")
	if c, err = loadConfig(); err != nil {
		t.Fatalf("настройки не прочитаны: %v", err)
	}
	if c.OwnerUser != 173418650 || c.Owner != -1001234567890 {
		t.Errorf("владелец и чат перепутаны: владелец %d, чат %d", c.OwnerUser, c.Owner)
	}

	t.Setenv("TELEGRAM_OWNER_ID", "не число")
	if _, err := loadConfig(); err == nil {
		t.Error("мусор в TELEGRAM_OWNER_ID должен быть виден сразу, а не молча отключать проверку")
	}
}

// writeSummaryOf кладёт готовую сводку в файл — для экранов, которым нужен
// не типовой набор из writeSummary, а своё состояние.
func writeSummaryOf(t *testing.T, s *Summary) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "summary.json")
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
