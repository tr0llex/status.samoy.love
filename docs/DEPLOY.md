# Выкатка и настройка

Операционные подробности вынесены сюда: в README им место занимало каждую
пятую строку, а нужны они одному человеку на одном сервере.

## Выкатка

Общим пайплайном [deploy-kit](https://github.com/samoy-love/deploy-kit). Три
независимые цели: страница, агент и бот. Правка страницы не перезапускает сбор
метрик, обновление агента не ждёт пересборки статики, перезапуск бота не
задевает ни то, ни другое.

```bash
dk deploy status-site status-agent samoylove-bot   # локально, тем же путём, что и CI
dk rollback status-site --list
```

Раскладка на сервере: релизы в `<корень>/releases/<версия>`, рабочая версия —
симлинк `current`. Страница живёт в `/var/www/status`, агент в
`/opt/status-agent`, systemd-юнит запускает его через `current`.

Юниты едут вместе с релизом: всё, что попадает в артефакт под `systemd/`,
выкатка ставит в `/etc/systemd/system` — но только если файл изменился.
Раньше юнит правился в git и переустанавливался руками, и файл в репозитории
с файлом на машине расходились молча.

Откат идёт без пересборки: релизы уже лежат на сервере.

Данные проверок лежат в `/var/www/status/data` — **вне** каталога релизов,
поэтому история переживает любую выкатку. Там же журнал выкаток
`releases.json`, из которого бот отвечает на `/changelog`: как список изменений
туда попадает — в [RELEASES.md](RELEASES.md).

## Первичная настройка (один раз)

```bash
sudo useradd --system --no-create-home --shell /usr/sbin/nologin status
sudo mkdir -p /var/www/status/data /var/www/status-acme
sudo chown -R status:status /var/www/status/data
sudo install -m 0644 deploy/systemd/status-agent.{service,timer} /etc/systemd/system/
sudo systemctl daemon-reload && sudo systemctl enable --now status-agent.timer

sudo certbot certonly --webroot -w /var/www/status-acme -d status.samoy.love
```

Бот — отдельный пользователь: данные статуса ему нужны только на чтение,
и юнит закрепляет это через `ReadOnlyPaths`.

```bash
sudo useradd --system --no-create-home --shell /usr/sbin/nologin samoylove-bot
sudo install -d -m 0755 -o samoylove-bot -g samoylove-bot /var/lib/samoylove-bot
sudo install -d -m 0755 /etc/samoylove-bot
sudo install -m 0600 /dev/null /etc/samoylove-bot/env   # дальше вписать токен и chat id
sudo install -m 0644 deploy/systemd/samoylove-bot.service /etc/systemd/system/
sudo systemctl daemon-reload && sudo systemctl enable --now samoylove-bot.service
```

## Конфиг проверок едет с релизом

`config/status.json` попадает в артефакт выкатки и лежит рядом с бинарником:
`/opt/status-agent/current/status.json`. Отдельной копии в `/etc` больше нет —
она молча отставала, и заметить это можно было только по пустому `impact` на
странице.

Путь к конфигу задан в `ExecStart`, поэтому раньше правки конфига — названия
проектов, `impact`, пороги — до прода не доезжали, хотя код там уже новый:
юниты выкаткой не доставлялись. Теперь доставляются, и первая же выкатка
агента приводит юнит в соответствие с репозиторием сама.

Старую копию конфига можно убрать — она больше не читается:

```bash
sudo rm -rf /etc/status-agent
```

Проверить, что доехало: в `summary.json` на проде у проверок должен появиться
непустой `impact`.

```bash
curl -s https://status.samoy.love/data/summary.json | grep -o '"impact":"[^"]*"' | head -3
```

## Переезд бота на новое имя (один раз)

Бот переименован из `samoy-bot` в `samoylove-bot`. Пути, юнит, пользователь и
каталог секретов сменились вместе с ним, а на хосте остались старые — выкатка
падает на `chown: invalid group`. Один раз нужно сделать это руками:

```bash
sudo useradd --system --no-create-home --shell /usr/sbin/nologin samoylove-bot
sudo install -d -m 0755 -o samoylove-bot -g samoylove-bot /var/lib/samoylove-bot
sudo install -d -m 0755 /etc/samoylove-bot

# Секреты переносим, а не заводим заново: chat id и токен те же.
sudo mv /etc/samoy-bot/env /etc/samoylove-bot/env
sudo chmod 0600 /etc/samoylove-bot/env

# Историю уведомлений тоже: без неё бот при старте объявит заново всё,
# что сейчас лежит, и повторит все известные ему версии.
sudo mv /var/lib/samoy-bot/state.json /var/lib/samoylove-bot/state.json
sudo chown samoylove-bot:samoylove-bot /var/lib/samoylove-bot/state.json

sudo install -m 0644 deploy/systemd/samoylove-bot.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl disable --now samoy-bot.service
sudo systemctl enable --now samoylove-bot.service
```

После этого выкатка бота проходит обычным порядком. Старое можно убрать:

```bash
sudo rm -rf /opt/samoy-bot /var/lib/samoy-bot /etc/samoy-bot
sudo rm -f /etc/systemd/system/samoy-bot.service
sudo userdel samoy-bot
```

## Бот: секреты и проверка канала

Токен и chat id — в `/etc/samoylove-bot/env` (0600, root), образец:
`deploy/samoylove-bot.env.example`. В репозитории значений нет.

История уведомлений и `offset` Telegram лежат в
`/var/lib/samoylove-bot/state.json` — перезапуск службы не превращается в
повторную рассылку обо всём, что лежит.

Проверить канал после выкатки:

```bash
sudo systemd-run --pipe --uid=samoylove-bot \
  --property=EnvironmentFile=/etc/samoylove-bot/env \
  /opt/samoylove-bot/current/samoylove-bot -selftest
```

Придёт та же сводка, что и по `/status`. Молчащий бот неотличим от
работающего, пока что-нибудь не упадёт, — а выяснять это в момент аварии
поздно.

Адрес мини-приложения переопределяется переменной `MINIAPP_URL`: пригодится,
чтобы проверить сборку через туннель. Telegram открывает мини-приложение
только по https; если адрес другой, кнопка остаётся, но становится обычной
ссылкой.

## Бот: журнал выкаток

О релизе бот узнаёт не по разнице снимков `version.json`, а из журнала
событий, который пишет сама выкатка. Разница снимков существует не всегда:
три выкатки за минуту дают одно сообщение, выкатка с откатом — ноль, провал
выкатки — ноль (версия не менялась, сравнивать нечего), автооткат — ноль.
Контракт журнала — [`docs/events.md`](https://github.com/samoy-love/deploy-kit/blob/main/docs/events.md)
в deploy-kit; форма сообщения о релизе при этом не изменилась.

Каталог `/var/lib/deploy-kit/events` заводит `bin/install-server` из
deploy-kit: он же создаёт группу `deploy-events` и вписывает в неё читателей —
бота и агента. Отдельно руками ничего делать не нужно, кроме одного:

```bash
sudo systemctl restart samoylove-bot   # членство в группе берётся при старте
```

Пока бот не перезапущен после первого `install-server`, каталог для него
недоступен, и в журнале службы будет `permission denied`.

Проверить, что читается:

```bash
sudo -u samoylove-bot ls -l /var/lib/deploy-kit/events | tail -5
journalctl -u samoylove-bot -n 50 | grep 'событие выкатки'
```

Курсоры и уже показанные `id` лежат в `state.json`, память о карточках
прогонов — в `deploy-groups.json` рядом. Курсора два: принято и подтверждено
Telegram. Убитый бот теряет очередь отправки, но не события — он перечитывает
журнал с подтверждённого места, поэтому лежачий бот и лежачий Telegram
означают задержку, а не потерю.

Удалять файлы из журнала боту нельзя и незачем: чистит их писатель (14 суток),
а «что я уже видел» помнит курсор. Юнит закрепляет это через `ReadOnlyPaths`.

Выключить новый путь — одна строка в `/etc/samoylove-bot/env` и перезапуск:

```
EVENTS_DIR=off
```

После этого бот снова объявляет релизы по разнице версий, как раньше.

## Внешний сторож

Секреты репозитория: `TELEGRAM_BOT_TOKEN`, `TELEGRAM_CHAT_ID`. Без них сторож
работает молча и только пишет данные.

Проверить канал: запустить workflow `probe` вручную с галкой `notify_test` —
сводка приходит всегда, независимо от того, чей сейчас голос.

Прогнать дед-мэн и режим «сторож говорит», не дожидаясь настоящей аварии,
можно подсунув заведомо протухший или недоступный ответ:

```bash
STATUS_SUMMARY_URL=http://127.0.0.1:9999/ node scripts/probe.mjs
```

## Локально

```bash
npm install
npm run dev
cd agent && go run . -config ../config/status.json -data ../tmp-data

# Боту нужны токен и chat id в окружении и данные, собранные агентом.
# Настройки только переменными: флагов у бота нет, кроме -selftest.
cd bot && TELEGRAM_BOT_TOKEN=… TELEGRAM_CHAT_ID=… \
  DATA_DIR=../tmp-data STATE_FILE=../tmp-data/bot-state.json \
  EVENTS_DIR=../tmp-data/events go run .
```

Каталога `events` локально может не быть — это не ошибка и не повод шуметь в
журнал: бот просто ничего оттуда не читает. Чтобы проверить сообщение о выкатке, туда
достаточно положить файл по контракту (образцы — `docs/events/` в deploy-kit).
