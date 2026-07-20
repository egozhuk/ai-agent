#!/bin/sh
set -eu

ROOT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
SEARXNG_PID=""

cleanup() {
	if [ -n "$SEARXNG_PID" ] && kill -0 "$SEARXNG_PID" 2>/dev/null; then
		kill "$SEARXNG_PID" 2>/dev/null || true
		wait "$SEARXNG_PID" 2>/dev/null || true
	fi
}
trap cleanup EXIT INT TERM

if ! curl --max-time 1 -fsS "${SEARCH_API_BASE_URL:-http://127.0.0.1:8888}/search?q=test&format=json" >/dev/null 2>&1; then
	"$ROOT_DIR/scripts/start-searxng.sh" --foreground &
	SEARXNG_PID=$!

	ready=0
	for _ in $(seq 1 30); do
		if curl --max-time 1 -fsS "${SEARCH_API_BASE_URL:-http://127.0.0.1:8888}/search?q=test&format=json" >/dev/null 2>&1; then
			ready=1
			break
		fi
		sleep 1
	done
	if [ "$ready" -ne 1 ]; then
		echo "SearXNG не запустился на ${SEARCH_API_BASE_URL:-http://127.0.0.1:8888}" >&2
		exit 1
	fi
	echo "SearXNG запущен: ${SEARCH_API_BASE_URL:-http://127.0.0.1:8888}"
else
	echo "Использую уже запущенный SearXNG: ${SEARCH_API_BASE_URL:-http://127.0.0.1:8888}"
fi

cd "$ROOT_DIR"
set +e
go run .
status=$?
set -e
exit "$status"
