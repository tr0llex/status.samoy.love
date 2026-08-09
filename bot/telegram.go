package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Bot API — обычный HTTP с JSON, поэтому библиотека не нужна: из всего
// протокола боту требуются два метода, getUpdates и sendMessage.
//
// Работаем длинным опросом, а не вебхуком: вебхук потребовал бы публичного
// эндпоинта, места в конфиге nginx и сертификата ради одного чата с одним
// человеком. Опрос ничего наружу не открывает.
type Telegram struct {
	token  string
	base   string // вынесен в поле, чтобы тесты подставляли свой сервер
	client *http.Client
}

func newTelegram(token string, pollTimeout time.Duration) *Telegram {
	return &Telegram{
		token: token,
		base:  "https://api.telegram.org",
		// Таймаут клиента заведомо больше таймаута длинного опроса, иначе
		// каждый холостой опрос обрывался бы ошибкой.
		client: &http.Client{Timeout: pollTimeout + 30*time.Second},
	}
}

type Update struct {
	UpdateID      int64          `json:"update_id"`
	Message       *Message       `json:"message"`
	CallbackQuery *CallbackQuery `json:"callback_query"`
}

type Message struct {
	MessageID int64 `json:"message_id"`
	Chat      struct {
		ID int64 `json:"id"`
	} `json:"chat"`
	// From — кто написал. В личной переписке совпадает с Chat.ID, в группе —
	// нет: там по chat id владельца не отличить от остальных участников.
	// Поле необязательное: у постов в канале отправителя нет.
	From struct {
		ID int64 `json:"id"`
	} `json:"from"`
	Text string `json:"text"`
}

// CallbackQuery — нажатие на инлайн-кнопку.
//
// Telegram ждёт ответа на каждое нажатие: пока его нет, у пользователя
// крутится часик на кнопке. Отвечать надо даже когда делать нечего.
type CallbackQuery struct {
	ID      string   `json:"id"`
	Data    string   `json:"data"`
	Message *Message `json:"message"`
	From    struct {
		ID int64 `json:"id"`
	} `json:"from"`
}

// Кнопки. Кнопка либо шлёт callback_data обратно боту, либо открывает
// мини-приложение прямо внутри Telegram — второе требует https-адреса.
type Button struct {
	Text         string  `json:"text"`
	CallbackData string  `json:"callback_data,omitempty"`
	WebApp       *WebApp `json:"web_app,omitempty"`
	URL          string  `json:"url,omitempty"`
}

type WebApp struct {
	URL string `json:"url"`
}

type Keyboard struct {
	InlineKeyboard [][]Button `json:"inline_keyboard"`
}

type apiResponse struct {
	OK          bool            `json:"ok"`
	Result      json.RawMessage `json:"result"`
	Description string          `json:"description"`
}

// redactedError — сетевая ошибка без адреса запроса.
//
// Причина сохраняется через Unwrap: errors.Is по context.Canceled и по
// сетевым ошибкам продолжает работать, наружу не выводится только текст.
type redactedError struct {
	msg   string
	cause error
}

func (e *redactedError) Error() string { return e.msg }
func (e *redactedError) Unwrap() error { return e.cause }

// redact срезает с ошибки адрес запроса.
//
// Токен лежит в самом пути (/bot<token>/<method>), а http.Client заворачивает
// транспортные сбои в *url.Error, чей Error() печатает адрес целиком. Такая
// ошибка попадает в журнал на каждом моргании сети и каждом 502 от Telegram
// (цикл команд в main.go пишет её по своему же комментарию регулярно) — и
// токен оказывается в journald открытым текстом. Прочитавший журнал получает
// полный контроль над ботом, поэтому в тексте ошибки остаются только метод и
// причина. Токен на всякий случай вычищается и из причины: она приходит из
// чужого кода, и ручаться за её содержимое нельзя.
func (t *Telegram) redact(method string, err error) error {
	if err == nil {
		return nil
	}
	cause := err
	var ue *url.Error
	if errors.As(err, &ue) && ue.Err != nil {
		cause = ue.Err
	}
	msg := cause.Error()
	if t.token != "" {
		msg = strings.ReplaceAll(msg, t.token, "<токен>")
	}
	return &redactedError{msg: method + ": " + msg, cause: cause}
}

func (t *Telegram) call(ctx context.Context, method string, payload any, result any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	// Токен лежит прямо в адресе, поэтому ЛЮБАЯ ошибка отсюда и ниже уходит
	// наружу только через redact: см. комментарий к нему.
	endpoint := fmt.Sprintf("%s/bot%s/%s", t.base, t.token, method)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return t.redact(method, err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := t.client.Do(req)
	if err != nil {
		return t.redact(method, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return t.redact(method, err)
	}
	var out apiResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return fmt.Errorf("%s: неожиданный ответ (%d)", method, resp.StatusCode)
	}
	if !out.OK {
		return fmt.Errorf("%s: telegram отказал: %s", method, out.Description)
	}
	if result != nil {
		return json.Unmarshal(out.Result, result)
	}
	return nil
}

// Send отправляет сообщение владельцу. Разметка HTML, а не Markdown:
// в версиях и причинах сбоев попадаются подчёркивания и звёздочки, и
// экранировать их в Markdown больнее, чем три спецсимвола HTML.
func (t *Telegram) Send(ctx context.Context, chatID int64, text string) error {
	return t.SendWith(ctx, chatID, text, nil)
}

// Ping проверяет канал, НИЧЕГО не отправляя в переписку.
//
// sendChatAction выбран именно поэтому: он проходит ровно тот же путь, что и
// настоящее сообщение — токен, сеть, доступ бота в этот чат, — но не оставляет
// после себя записи. Если токен протух, чат недоступен или бота заблокировали,
// Telegram ответит ошибкой, и проверка это увидит.
//
// Раньше на этом месте была отправка полной сводки. Она честно проверяла
// канал, но платила за это сообщением владельцу при КАЖДОЙ выкатке бота: за
// один вечер выкаток набирается с десяток, и человек получает столько же
// карточек «всё работает», которых не просил. Уведомление, которое приходит
// без события, обесценивает те, что приходят с событием.
func (t *Telegram) Ping(ctx context.Context, chatID int64) error {
	return t.call(ctx, "sendChatAction", map[string]any{
		"chat_id": chatID,
		"action":  "typing",
	}, nil)
}

// SendWith — сообщение с кнопками под ним.
func (t *Telegram) SendWith(ctx context.Context, chatID int64, text string, kb *Keyboard) error {
	payload := map[string]any{
		"chat_id":                  chatID,
		"text":                     text,
		"parse_mode":               "HTML",
		"disable_web_page_preview": true,
	}
	if kb != nil {
		payload["reply_markup"] = kb
	}
	return t.call(ctx, "sendMessage", payload, nil)
}

// ------------------------------------------------ длинный ответ по частям

const (
	// telegramTextLimit — 4096 единиц UTF-16 на текст сообщения. Предел
	// жёсткий и односторонний: сверх него Telegram не обрезает, а ОТВЕРГАЕТ
	// сообщение целиком, то есть владелец получает молчание вместо ответа.
	telegramTextLimit = 4096
	// partHeaderReserve — место под шапку «продолжение (2/3)».
	//
	// Шапка дописывается уже после раскладки, поэтому её место резервируется
	// заранее: посчитать части по полному лимиту, а потом дописать в них ещё по
	// строке — это ровно тот способ упереться в предел, от которого раскладка и
	// заводится. Шестьдесят четыре единицы с запасом покрывают «продолжение
	// (99/99)» вместе с тегами и переводом строки.
	partHeaderReserve = 64
)

// splitMessage раскладывает готовый текст по сообщениям Telegram.
//
// ЗАЧЕМ. Владелец попросил не резать список выкаченных коммитов: строка «…и ещё
// 1 коммит» стоит места и не сообщает ничего. Но 4096 единиц UTF-16 — предел
// чужой и непреодолимый, а релиз на сорок тем по 120 символов — это около 4800.
// Единственный ответ, при котором не теряется ничего, — несколько сообщений
// подряд.
//
// КАК. Режем ТОЛЬКО по границам строк: строка здесь — это пункт списка или
// заголовок блока, и разрыв внутри неё разорвал бы и разметку, и фразу. Порядок
// сохраняется, части нумеруются, каждая следующая узнаётся по шапке — иначе
// вторая половина списка выглядит как отдельное сообщение о другой выкатке.
//
// Пустой текст даёт пустой список частей: отправлять пустое сообщение Telegram
// всё равно откажется, а вызывающему проще не иметь такого случая вовсе.
func splitMessage(text string, limit int) []string {
	if text == "" {
		return nil
	}
	if utf16Len(text) <= limit {
		return []string{text}
	}
	room := limit - partHeaderReserve

	var parts []string
	var cur strings.Builder
	used := 0
	flush := func() {
		// Пустые строки на стыке частей срезаем: перевод строки, разделявший
		// два блока внутри одного сообщения, в начале следующего превращается
		// в пустую строку под шапкой «продолжение».
		s := strings.Trim(cur.String(), "\n")
		cur.Reset()
		used = 0
		if s == "" {
			return
		}
		parts = append(parts, s)
	}
	for i, line := range strings.Split(text, "\n") {
		if i > 0 {
			// Перевод строки принадлежит предыдущей строке, но считается
			// вместе со следующей: иначе часть, забитая ровно под предел,
			// вылезет за него на один символ.
			line = "\n" + line
		}
		n := utf16Len(line)
		if n > room {
			// Одна строка длиннее целой части. На честных данных этого не
			// бывает: и пункт списка, и строка версии обрезаны по
			// changelogWidth. Значит, это чужой файл — и лучше показать текст
			// без разметки, чем не показать ничего. Разметку снимаем целиком:
			// разрезать её посередине — это негодный HTML, то есть отказ
			// Telegram, то есть молчание в ответ на команду.
			flush()
			parts = append(parts, flatChunks(line, room)...)
			continue
		}
		if used+n > room {
			flush()
			line = strings.TrimPrefix(line, "\n")
			n = utf16Len(line)
		}
		cur.WriteString(line)
		used += n
	}
	flush()

	// Шапка — второй проход, когда общее число частей уже известно. Без «из
	// скольких» продолжение неотличимо от нового сообщения, а владелец не
	// должен гадать, всё ли пришло.
	for i := 1; i < len(parts); i++ {
		parts[i] = fmt.Sprintf("<i>продолжение (%d/%d)</i>\n", i+1, len(parts)) + parts[i]
	}
	return parts
}

// tagRe — разметка Telegram в готовой строке. Нужна ровно для одного:
// снять её со строки, которую придётся резать посередине.
var tagRe = regexp.MustCompile(`</?[A-Za-z][^>]*>`)

// flatChunks режет одну слишком длинную строку, не оставляя негодной разметки.
//
// Порядок такой: снять теги, разэкранировать, порезать по символам,
// экранировать каждый кусок заново. Так каждый кусок — законченный текст без
// разметки, а не половина тега и не огрызок «&am». Теряется оформление одной
// строки; текст не теряется, и ради него всё и делается.
func flatChunks(s string, limit int) []string {
	plain := html.UnescapeString(tagRe.ReplaceAllString(s, ""))
	plain = strings.TrimSpace(plain)
	var out []string
	var cur []rune
	used := 0
	for _, r := range plain {
		// Экранирование удлиняет: «&» стоит пять единиц, а не одну, и эмодзи
		// вне BMP — две. Считаем по экранированному виду, иначе кусок вылезет
		// за предел уже после сборки.
		n := utf16Len(esc(string(r)))
		if used+n > limit && len(cur) > 0 {
			out = append(out, esc(string(cur)))
			cur, used = nil, 0
		}
		cur = append(cur, r)
		used += n
	}
	if len(cur) > 0 {
		out = append(out, esc(string(cur)))
	}
	return out
}

// SendLong отправляет ответ, не помещающийся в одно сообщение, несколькими
// подряд.
//
// Клавиатура вешается на ПОСЛЕДНЮЮ часть: кнопки относятся ко всему ответу, а
// не к его первому куску, и висеть им положено под концом текста.
//
// Ошибка на любой части — ошибка всей отправки, и в ней сказано, на какой
// именно. Считать отправку удачной, когда владелец увидел половину списка,
// нельзя: половина списка выглядит как весь список, и заметить пропажу
// невозможно. Дальше по циклу такая ошибка попадает в журнал и в счётчик
// несостоявшихся отправок ровно так же, как отказ на одном сообщении.
func (t *Telegram) SendLong(ctx context.Context, chatID int64, text string, kb *Keyboard) error {
	parts := splitMessage(text, telegramTextLimit)
	if len(parts) == 0 {
		return nil
	}
	for i, p := range parts {
		var k *Keyboard
		if i == len(parts)-1 {
			k = kb
		}
		if err := t.SendWith(ctx, chatID, p, k); err != nil {
			if len(parts) == 1 {
				return err
			}
			return fmt.Errorf("часть %d из %d: %w", i+1, len(parts), err)
		}
	}
	return nil
}

// EditLong перерисовывает экран на месте, дописывая продолжение отдельными
// сообщениями.
//
// «Обновить» правит то сообщение, которое владелец уже читает, — ради этого
// правка и существует. Но заменить одно сообщение на три Telegram не умеет, а
// урезать ответ до одного нельзя: именно от урезания и уходим. Поэтому первая
// часть уезжает правкой, остальные — вдогонку.
func (t *Telegram) EditLong(ctx context.Context, chatID, messageID int64, text string, kb *Keyboard) error {
	parts := splitMessage(text, telegramTextLimit)
	if len(parts) == 0 {
		return nil
	}
	var head *Keyboard
	if len(parts) == 1 {
		head = kb
	}
	if err := t.Edit(ctx, chatID, messageID, parts[0], head); err != nil {
		return err
	}
	for i, p := range parts[1:] {
		var k *Keyboard
		if i == len(parts)-2 {
			k = kb
		}
		if err := t.SendWith(ctx, chatID, p, k); err != nil {
			return fmt.Errorf("часть %d из %d: %w", i+2, len(parts), err)
		}
	}
	return nil
}

// Edit переписывает уже отправленное сообщение.
//
// Благодаря этому «Обновить» не плодит новые сообщения: статус меняется прямо
// в том, которое владелец уже читает, — переписка не превращается в ленту из
// двадцати почти одинаковых карточек.
func (t *Telegram) Edit(ctx context.Context, chatID, messageID int64, text string, kb *Keyboard) error {
	payload := map[string]any{
		"chat_id":                  chatID,
		"message_id":               messageID,
		"text":                     text,
		"parse_mode":               "HTML",
		"disable_web_page_preview": true,
	}
	if kb != nil {
		payload["reply_markup"] = kb
	}
	err := t.call(ctx, "editMessageText", payload, nil)
	// Если содержимое не изменилось, Telegram отвечает ошибкой. Это не сбой:
	// владелец нажал «Обновить», а с прошлого раза ничего не поменялось.
	if err != nil && strings.Contains(err.Error(), "message is not modified") {
		return nil
	}
	return err
}

// AnswerCallback гасит «часики» на нажатой кнопке. Текст, если он задан,
// всплывает короткой плашкой поверх чата.
func (t *Telegram) AnswerCallback(ctx context.Context, id, text string) error {
	return t.call(ctx, "answerCallbackQuery", map[string]any{
		"callback_query_id": id,
		"text":              text,
	}, nil)
}

// GetMe отдаёт имя пользователя (username) самого бота.
//
// Раньше это имя бралось ТОЛЬКО из TELEGRAM_BOT_USERNAME, и при пустой
// переменной parseCommand переставал отсекать обращения к другим ботам в
// группе (self == "" — «отвечаем на любое имя»): бот начинал реагировать на
// «/status@other_bot», адресованное соседу по чату. getMe знает своё имя
// без настройки; переменная окружения остаётся опциональным override — им
// пользуются тесты и те, кто хочет обойтись без лишнего запроса при старте.
func (t *Telegram) GetMe(ctx context.Context) (string, error) {
	var me struct {
		Username string `json:"username"`
	}
	if err := t.call(ctx, "getMe", map[string]any{}, &me); err != nil {
		return "", err
	}
	return me.Username, nil
}

// BotCommand — одна строка синего меню команд Telegram.
type BotCommand struct {
	Command     string `json:"command"`
	Description string `json:"description"`
}

// SetMyCommands наполняет синее меню команд рядом с полем ввода.
//
// Вызывается один раз при старте: меню не привязано к чату и не меняется
// между запусками, кроме как при правке этого же списка в коде.
func (t *Telegram) SetMyCommands(ctx context.Context, cmds []BotCommand) error {
	return t.call(ctx, "setMyCommands", map[string]any{"commands": cmds}, nil)
}

func (t *Telegram) GetUpdates(ctx context.Context, offset int64, timeout time.Duration) ([]Update, error) {
	var updates []Update
	err := t.call(ctx, "getUpdates", map[string]any{
		"offset":  offset,
		"timeout": int(timeout.Seconds()),
		// Кроме сообщений нужны нажатия на кнопки; на остальные типы событий
		// не подписываемся, чтобы не тратить трафик впустую.
		"allowed_updates": []string{"message", "callback_query"},
	}, &updates)
	return updates, err
}
