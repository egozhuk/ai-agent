#!/bin/sh
set -eu

ROOT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
export SEARXNG_SETTINGS_PATH="$ROOT_DIR/.local/searxng-config/settings.yml"

cd "$ROOT_DIR/.local/searxng"
exec "$ROOT_DIR/.local/searxng-venv/bin/granian" \
  --interface wsgi searx.webapp:app \
  --host 127.0.0.1 \
  --port 8888 \
  --workers 1 \
  --blocking-threads 16 \
  --runtime-threads 2 \
  --backpressure 32
