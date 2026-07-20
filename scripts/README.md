# Скрипты проекта

## SearXNG без Docker

Однократно установить SearXNG и зависимости:

```bash
./scripts/install-searxng.sh
```

Запустить в фоне:

```bash
./scripts/start-searxng.sh
```

Остановить:

```bash
./scripts/stop-searxng.sh
```

Лог сервиса находится в `.local/searxng/searxng.log`. `start-dev.sh` запускает SearXNG во foreground и останавливает его вместе с ботом.
