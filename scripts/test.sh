#!/usr/bin/env bash
# Full check: build, vet, gofmt, and the test suite (with real engines when
# CHROMIUM_PATH / WEASYPRINT_PATH are set — sourced from .env.local if present).
set -euo pipefail

cd "$(dirname "$0")/.."

ENV_FILE="${ENV_FILE:-.env.local}"
if [[ -f "$ENV_FILE" ]]; then
  set -a
  # shellcheck disable=SC1090
  source "$ENV_FILE"
  set +a
fi

echo "== go build =="
go build ./...
echo "== go vet =="
go vet ./...
echo "== gofmt =="
unformatted="$(gofmt -l .)"
if [[ -n "$unformatted" ]]; then
  echo "gofmt needed on:" >&2
  echo "$unformatted" >&2
  exit 1
fi
echo "== go test =="
go test "$@" ./...
