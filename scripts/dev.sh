#!/usr/bin/env bash
# Run the service locally with .env.local loaded.
#
#   cp .env.local.example .env.local   # then edit CHROMIUM_PATH
#   ./scripts/dev.sh
#
# The binary reads plain env vars (no dotenv dependency) so it behaves the
# same here as under systemd/Docker; this script only sources the file.
set -euo pipefail

cd "$(dirname "$0")/.."

ENV_FILE="${ENV_FILE:-.env.local}"
if [[ -f "$ENV_FILE" ]]; then
  set -a
  # shellcheck disable=SC1090
  source "$ENV_FILE"
  set +a
else
  echo "no $ENV_FILE found — copy .env.local.example and set CHROMIUM_PATH" >&2
  exit 1
fi

exec go run ./cmd/api
