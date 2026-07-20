#!/bin/sh
set -eu

ROOT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
SOURCE_DIR="$ROOT_DIR/.local/searxng"
VENV_DIR="$ROOT_DIR/.local/searxng-venv"
PID_FILE="$SOURCE_DIR/searxng.pid"
LOG_FILE="$SOURCE_DIR/searxng.log"
export SEARXNG_SETTINGS_PATH="$ROOT_DIR/.local/searxng-config/settings.yml"

if [ ! -x "$VENV_DIR/bin/granian" ] || [ ! -f "$SEARXNG_SETTINGS_PATH" ]; then
	echo "SearXNG не установлен. Сначала выполните: $ROOT_DIR/scripts/install-searxng.sh" >&2
	exit 1
fi

if [ -f "$PID_FILE" ] && kill -0 "$(cat "$PID_FILE")" 2>/dev/null; then
	echo "SearXNG уже запущен (PID $(cat "$PID_FILE"))."
	exit 0
fi
rm -f "$PID_FILE"

cd "$SOURCE_DIR"

if [ "${1:-}" = "--foreground" ]; then
	exec "$VENV_DIR/bin/granian" \
		--interface wsgi searx.webapp:app \
		--host 127.0.0.1 \
		--port 8888 \
		--workers 1 \
		--blocking-threads 16 \
		--runtime-threads 2 \
		--backpressure 32
fi

nohup "$VENV_DIR/bin/granian" \
	--interface wsgi searx.webapp:app \
	--host 127.0.0.1 \
	--port 8888 \
  --workers 1 \
	--blocking-threads 16 \
	--runtime-threads 2 \
	--backpressure 32 \
	>"$LOG_FILE" 2>&1 &
PID=$!
printf '%s\n' "$PID" > "$PID_FILE"
sleep 1
if ! kill -0 "$PID" 2>/dev/null; then
	rm -f "$PID_FILE"
	echo "SearXNG не запустился. Лог: $LOG_FILE" >&2
	tail -n 30 "$LOG_FILE" >&2 || true
	exit 1
fi
printf '%s\n' "SearXNG запущен (PID $PID), лог: $LOG_FILE"
