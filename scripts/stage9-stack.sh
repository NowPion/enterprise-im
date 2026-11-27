#!/usr/bin/env bash
set -euo pipefail

# Stage 9 isolated stack launcher (NO plaintext secrets stored here).
# Required environment variables before calling:
#   MYSQL_PASSWORD
#   REDIS_PASSWORD
# Optional overrides (defaults shown for stage9-admin-qa):
export SERVER_ADDR="${SERVER_ADDR:-:8084}"
export IM_VITE_PORT="${IM_VITE_PORT:-5177}"
export IM_BACKEND_ORIGIN="${IM_BACKEND_ORIGIN:-http://127.0.0.1:8084}"
export MYSQL_HOST="${MYSQL_HOST:-127.0.0.1}"
export MYSQL_PORT="${MYSQL_PORT:-3306}"
export MYSQL_USER="${MYSQL_USER:-root}"
export MYSQL_DATABASE="${MYSQL_DATABASE:-im_stage9_admin}"
export REDIS_ADDR="${REDIS_ADDR:-127.0.0.1:6379}"
export REDIS_DB="${REDIS_DB:-4}"
export SNOWFLAKE_NODE_ID="${SNOWFLAKE_NODE_ID:-14}"
export CORS_ALLOWED_ORIGINS="${CORS_ALLOWED_ORIGINS:-http://localhost:${IM_VITE_PORT},http://127.0.0.1:${IM_VITE_PORT}}"

if [[ -z "${MYSQL_PASSWORD:-}" || -z "${REDIS_PASSWORD:-}" ]]; then
  echo "ERROR: MYSQL_PASSWORD and REDIS_PASSWORD must be exported in the environment." >&2
  exit 1
fi

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BACKEND_PID=""
VITE_PID=""

cleanup() {
  if [[ -n "${VITE_PID}" ]]; then
    kill "${VITE_PID}" 2>/dev/null || true
  fi
  if [[ -n "${BACKEND_PID}" ]]; then
    kill "${BACKEND_PID}" 2>/dev/null || true
  fi
}
trap cleanup EXIT INT TERM

(
  cd "${ROOT_DIR}/backend"
  exec go run ./cmd/server
) &
BACKEND_PID=$!

(
  cd "${ROOT_DIR}/frontend"
  exec npm run dev -- --host 127.0.0.1 --port "${IM_VITE_PORT}"
) &
VITE_PID=$!

echo "Started backend (PID ${BACKEND_PID}) on ${SERVER_ADDR} and Vite (PID ${VITE_PID}) on ${IM_VITE_PORT}"
wait "${BACKEND_PID}" "${VITE_PID}"
