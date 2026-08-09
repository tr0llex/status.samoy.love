package main

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"
)

// Экран — это то, что нарисовано в сообщении сейчас. Команда и нажатие на
// кнопку приводят к одному и тому же экрану, поэтому отрисовка одна на оба
// пути: иначе /status и кнопка «Статус» со временем разъехались бы.

func viewOf(cmd string) string {
	switch cmd {
	case CmdIncidents:
		return ViewIncidents
	case CmdChangelog:
		return ViewChangelog
	case CmdHelp:
		return ViewHelp
	default:
		return ViewStatus
	}
}

// viewFor — экран для команды с аргументом.
//
// Аргумент уезжает прямо в ключ экрана, потому что тот же ключ становится
// callback_data кнопки «Обновить». Там 64 БАЙТА на всё, и слишком длинное
// слово владельца не должно превращать ответ в ошибку Telegram — сообщение с
// негодной кнопкой не отправляется целиком. Поэтому имя обрезается заранее:
// названия целей короткие, и обрезанное имя всё равно приведёт к ответу
// «не знаю такой цели» со списком тех, что есть.
func viewFor(cmd, arg string) string {
	if cmd != CmdChangelog || arg == "" {
		return viewOf(cmd)
	}
	return ViewChangelogOf + cutBytes(strings.ToLower(arg), callbackDataMax-len(ViewChangelogOf))
}

// renderView собирает текст экрана и клавиатуру под ним. Данные читаются на
// каждый показ: между нажатиями кнопок агент успевает записать новое
// состояние, и показывать закэшированное значило бы врать в ответ на
// «Обновить».
//
// Клавиатура возвращается вместе с текстом, потому что зависит от тех же
// данных: на кнопке проекта нарисовано его состояние. Собирать её отдельно
// значило бы прочитать файл дважды и получить кнопку от одного момента
// времени поверх текста от другого.
func renderView(view, summaryPath string, now time.Time) (string, *Keyboard) {
	if view == ViewHelp {
		return formatHelp(), navKeyboard(ViewHelp)
	}
	s, err := loadSummary(summaryPath)
	if err != nil {
		log.Printf("данные агента не прочитаны: %v", err)
		return "🔴 Не могу прочитать данные агента — похоже, он не работает", navKeyboard(view)
	}
	if q, ok := changelogOfView(view); ok {
		return renderChangelog(s, summaryPath, q, now)
	}
	if id, ok := projectOfView(view); ok {
		for _, p := range s.Projects {
			if p.ID == id {
				return formatProject(p, s, now), projectKeyboard(s, view)
			}
		}
		// Проект исчез из конфига, а кнопка на него осталась в старом
		// сообщении. Молча возвращаем на общий экран.
		return statusText(s, now), statusKeyboard(s)
	}
	switch view {
	case ViewIncidents:
		return formatIncidents(s, now), navKeyboard(view)
	case ViewChangelog:
		return renderChangelog(s, summaryPath, "", now)
	default:
		return statusText(s, now), statusKeyboard(s)
	}
}

// statusText — экран статуса с состоянием тишины.
//
// Тишину раньше нельзя было увидеть на самом экране: включалась она только
// кнопкой под уведомлением об аварии, то есть уже ПОСЛЕ того, как что-то
// упало, а факт «бот сейчас молчит» никак не отображался. Здесь и только
// здесь читается botState — единственный источник правды о тишине (тот же,
// что у /quiet и у цикла уведомлений).
func statusText(s *Summary, now time.Time) string {
	muted, until := muteState(now)
	return formatStatus(s, now, muted, until)
}

// muteState — тишина сейчас, под общим замком: тот же State, что меняют
// /quiet и кнопки «Тихо 2 ч»/«До утра»/«Снова говорить».
func muteState(now time.Time) (bool, time.Time) {
	mu.Lock()
	defer mu.Unlock()
	if botState == nil {
		return false, time.Time{}
	}
	return botState.Muted(now)
}

// handleCallback обрабатывает нажатие на инлайн-кнопку.
//
// Отвечать Telegram надо всегда и как можно раньше: пока ответа нет, на
// кнопке у владельца крутятся часики. Поэтому сначала гасим их, а потом уже
// перерисовываем сообщение.
//
// Владелец здесь проверяется по ownerUser, а не по owner: нажатие приносит id
// человека, а chat id — это адрес, куда отвечать. Если владелец не задан
// (в TELEGRAM_CHAT_ID группа, TELEGRAM_OWNER_ID пуст), подтвердить право
// нажимающего нечем, и кнопка не срабатывает — ровно как и раньше.
func handleCallback(ctx context.Context, tg *Telegram, q *CallbackQuery, owner, ownerUser int64, summaryPath string) {
	if ownerUser <= 0 || q.From.ID != ownerUser {
		// Чужому не отвечаем содержимым, но часики гасим: иначе кнопка у него
		// будет «висеть», и это само по себе подсказка, что бот живой.
		_ = tg.AnswerCallback(ctx, q.ID, "")
		return
	}
	if err := tg.AnswerCallback(ctx, q.ID, ""); err != nil {
		log.Printf("нажатие не подтверждено: %v", err)
	}
	if q.Message == nil {
		return
	}

	// Действия обрабатываются отдельно: они меняют состояние (тишина) или
	// открывают экран новым сообщением («Что сейчас»), а не листают текущее.
	if handled := handleAction(ctx, tg, q, owner, summaryPath); handled {
		return
	}

	view := q.Data
	_, isProject := projectOfView(view)
	_, isService := changelogOfView(view)
	if !isProject && !isService {
		switch view {
		case ViewStatus, ViewIncidents, ViewChangelog, ViewHelp:
		default:
			// Кнопка из сообщения, отправленного прошлой версией бота.
			view = ViewStatus
		}
	}

	log.Printf("кнопка %s", view)
	text, kb := renderView(view, summaryPath, time.Now().UTC())
	if err := tg.EditLong(ctx, q.Message.Chat.ID, q.Message.MessageID, text, kb); err != nil {
		log.Printf("экран %s не перерисован: %v", view, err)
	}
}

// handleAction — нажатия, которые что-то делают, а не листают текущий экран.
//
// Сюда же попадает «Что сейчас» с уведомления, карточки прогона и
// подтверждения тишины (ActWhatNowPrefix): по правилу владения сообщением
// (см. комментарий к ActWhatNowPrefix в keyboard.go) такое сообщение — не
// экран, и его нельзя перерисовывать. Ответ уходит НОВЫМ сообщением, а не
// правкой: уведомление о падении, карточка выкатки и подтверждение тишины
// должны остаться в переписке как были — по ним потом восстанавливают
// картину, и затирать их значит терять историю.
func handleAction(ctx context.Context, tg *Telegram, q *CallbackQuery, owner int64, summaryPath string) bool {
	if view, ok := strings.CutPrefix(q.Data, ActWhatNowPrefix); ok {
		text, kb := renderView(view, summaryPath, time.Now().UTC())
		if err := tg.SendWith(ctx, q.Message.Chat.ID, text, kb); err != nil {
			log.Printf("экран %s не открыт новым сообщением: %v", view, err)
		}
		return true
	}

	var d time.Duration
	switch q.Data {
	case ActMute2h:
		d = 2 * time.Hour
	case ActMute8h:
		d = 8 * time.Hour
	case ActUnmute:
	default:
		return false
	}

	text := applyMute(time.Now().UTC(), d, q.Data == ActUnmute)
	if err := tg.SendWith(ctx, owner, text, mutedKeyboard()); err != nil {
		log.Printf("подтверждение тишины не отправлено: %v", err)
	}
	return true
}

// applyMute задаёт или снимает тишину и сохраняет состояние.
//
// Общая точка для кнопок «Тихо 2 ч»/«До утра»/«Снова говорить» и команды
// /quiet: раньше тишину можно было включить ТОЛЬКО кнопкой под уведомлением
// об аварии, то есть уже после того, как что-то упало. Заранее заглушить
// бота на плановые работы или ручную выкатку было нечем. Здесь — тот же
// State, тот же замок и тот же текст подтверждения, что и у кнопок, лишь бы
// не завести тишине вторую реализацию.
func applyMute(now time.Time, d time.Duration, unmute bool) string {
	mu.Lock()
	var text string
	if unmute {
		botState.Unmute()
		text = "🔔 Снова сообщаю о падениях"
	} else {
		until := botState.Mute(now, d)
		// Говорим, до какого времени, а не «на 2 часа»: под утро разница
		// между «через два часа» и «в 07:40» существенная.
		text = fmt.Sprintf("🔕 Молчу до %s\nПадения всё это время записываются — увижу в /incidents.", fmtTime(until))
	}
	if err := saveState(statePath, botState); err != nil {
		log.Printf("состояние не сохранено: %v", err)
	}
	mu.Unlock()
	return text
}
