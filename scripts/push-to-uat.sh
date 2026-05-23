#!/bin/bash
# push-to-uat.sh — load UAT deploy settings and run deploy.sh.
# Settings live in scripts/.env.uat (gitignored). Copy scripts/.env.example
# and fill in your values.
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
ENV_FILE="${HERE}/.env.uat"

if [ ! -f "${ENV_FILE}" ]; then
  echo "missing ${ENV_FILE} — copy scripts/.env.example and fill it in" >&2
  exit 1
fi

set -a
# shellcheck disable=SC1090
source "${ENV_FILE}"
set +a

exec "${HERE}/deploy.sh" "$@"
