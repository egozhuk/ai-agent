# MAX AI Agent on Mistral

Персональный бот для MAX: отвечает через Mistral API, доступен только владельцу, ведет учет токенов и подготовлен к подключению интеграций.

## Что уже есть

- MAX Long Polling для разработки и Webhook для production.
- Доступ `ACCESS_MODE=owner` только для `OWNER_MAX_USER_ID`; `ACCESS_MODE=all` для публичного запуска.
- Mistral Chat Completions с tool calling.
- Веб-поиск через локальный SearXNG JSON API; встроенный Mistral `web_search` для обычных запросов не используется.
- Автоматическое переключение темы: агент очищает историю только когда понимает, что новый запрос явно не связан с предыдущим.
- Многошаговое выполнение: агент может последовательно искать данные, анализировать результат и выполнять следующее действие, обновляя единое статусное сообщение.
- Документы: PDF и другие файлы сохраняются как активный документ конкретного пользователя; вопросы по нему передаются в Document Q&A.
- Google OAuth подключение аккаунта пользователя.
- Gmail read-only: просмотр последних писем после подключения.
- Google Calendar: создание событий после подключения.
- SQLite-хранилище `data/agent.db` со статистикой токенов, историей диалогов, аккаунтами и задачами.
- Access/refresh tokens хранятся зашифрованно через AES-GCM.

## Запуск для разработки

```bash
export MAX_BOT_TOKEN="ваш токен MAX"
export MISTRAL_API_KEY="ваш ключ Mistral"
export OWNER_MAX_USER_ID="ваш user_id в MAX"
export BOT_MODE="polling"

# SearXNG, установленный на этом же сервере без Docker
export SEARCH_API_BASE_URL="http://127.0.0.1:8888"

go run .
```

## Локальный поиск через SearXNG без Docker

Бот ожидает SearXNG на `SEARCH_API_BASE_URL` и обращается к endpoint `/search?format=json`.
SearXNG можно установить напрямую на Linux через официальный installation script:

```bash
git clone https://github.com/searxng/searxng.git
cd searxng
sudo ./utils/searxng.sh install all
```

После установки проверьте, что в `/etc/searxng/settings.yml` включен JSON-формат:

```yaml
search:
  formats:
    - html
    - json
```

Проверка перед запуском бота:

```bash
curl 'http://127.0.0.1:8888/search?q=чемпионат+мира&format=json'
```

В этом проекте локальный SearXNG уже установлен в `.local/`. Запуск на macOS:

```bash
./scripts/start-searxng.sh
```

Оставьте этот процесс работающим в отдельном окне терминала, а бота запускайте в другом.
Либо запускайте оба процесса одной командой:

```bash
./scripts/start-dev.sh
```

При остановке бота через `Ctrl+C` этот скрипт завершит запущенный им SearXNG.

Официальная инструкция: <https://docs.searxng.org/admin/installation-scripts.html>.
Публичные SearXNG-инстансы для постоянной работы бота не рекомендуется использовать.

Если MAX возвращает `x509: certificate signed by unknown authority`, укажите сертификат Минцифры. В проекте уже есть подходящий PEM bundle:

```bash
export MAX_CA_CERT_PATH="russiantrustedca/russiantrustedca2024.pem"
go run .
```

Также поддерживаются DER `.cer` файлы и список путей через `:` на macOS/Linux, например:

```bash
export MAX_CA_CERT_PATH="russiantrustedca/russian_trusted_root_ca_gost_2025.cer:russiantrustedca/russian_trusted_sub_ca_gost_2025.cer"
go run .
```

Команды в боте:

- `/start` - краткая справка.
- `/connect_google` - выдать ссылку для подключения Gmail/Google Calendar.
- `/oauth_google_info` - показать redirect URI, который должен быть указан в Google Cloud Console.
- `/connections` - показать подключенные аккаунты.
- `/disconnect_google` - удалить сохраненные Google tokens.
- `/usage` - расход токенов владельца.
- `/remind 10m текст` - одноразовое напоминание через указанный интервал.
- `/remind 2026-07-20 10:00 текст` - напоминание на конкретное время по локальным часам.
- `/watch https://example.com daily` - проверять страницу раз в день и сообщать об изменениях.
- `/watch https://example.com 6h` - проверять страницу с произвольным интервалом от 1 минуты.
- «Каждый день в 10 утра присылай мои задачи» - включить ежедневную сводку задач на текущий день.
- `/reminders` - список активных напоминаний и наблюдений.
- `/clear_reminders` - удалить все активные одноразовые напоминания текущего пользователя.
- `/cancel ID` - отменить задачу по ID.

## Google OAuth

Создайте OAuth Client типа Web application в Google Cloud Console и добавьте redirect URI:

```text
https://your-domain.com/oauth/google/callback
```

Для локального теста можно использовать публичный HTTPS tunnel и указать его как `PUBLIC_BASE_URL`.

```bash
export PUBLIC_BASE_URL="https://your-domain.com"
export GOOGLE_CLIENT_ID="..."
export GOOGLE_CLIENT_SECRET="..."
export TOKEN_ENCRYPTION_KEY="$(openssl rand -base64 32)"
go run .
```

Пользователь может подключиться командой `/connect_google`, но это не обязательный ручной шаг: если он впервые попросит действие с Gmail или Calendar, агент сам вернет персональную OAuth-ссылку и после callback будет использовать сохраненный токен этого MAX-пользователя.

Запрошенные scopes:

- `https://www.googleapis.com/auth/gmail.readonly`
- `https://www.googleapis.com/auth/calendar.events`

Не храните `TOKEN_ENCRYPTION_KEY` в репозитории. Если потерять этот ключ, старые OAuth-токены из `data/agent.db` нельзя будет расшифровать, пользователям придется подключить аккаунты заново.

## Production через Webhook

MAX рекомендует Webhook для production. Endpoint должен быть HTTPS, с доверенным сертификатом.

```bash
export BOT_MODE="webhook"
export WEBHOOK_ADDR=":8080"
export WEBHOOK_SECRET="случайная_строка_5_символов_или_длиннее"
go run .
```

После деплоя нужно создать подписку в MAX:

```bash
curl -X POST "https://platform-api2.max.ru/subscriptions" \
  -H "Authorization: $MAX_BOT_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "url": "https://your-domain.com/webhook",
    "update_types": ["message_created", "bot_started"],
    "secret": "'"$WEBHOOK_SECRET"'"
  }'
```

## Агентские действия

Напоминания, ежедневные сводки и наблюдения за страницами хранятся в `data/agent.db` и выполняются фоновым планировщиком. Mistral распознает естественную просьбу и вызывает локальное действие `create_reminder`, `schedule_daily_digest` или `watch_url`, после чего бот сохраняет задачу в базе. Ежедневная сводка выбирает только задачи текущего пользователя на текущую дату. При первом запуске наблюдения текущая версия страницы запоминается как исходная, а при последующем изменении содержимое старой и новой версии отправляется в Mistral для краткого анализа.

## Следующие интеграции

- Картинки в поисковых ответах: сейчас бот возвращает текст и источники; изображения можно добавить отдельным Mistral image tool или парсингом ссылок из найденных страниц.
- Лимиты: добавить дневной/месячный бюджет токенов на пользователя.
- Естественные формулировки задач: распознавать даты вроде «напомни завтра в 10:00».
