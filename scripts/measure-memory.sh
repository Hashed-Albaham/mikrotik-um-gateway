#!/usr/bin/env bash
#
# Measures the resident memory of the gateway while it serves real requests
# against the bundled mock portal. Requires a Go toolchain and python3.
#
# Usage: ./scripts/measure-memory.sh [requests]
#
# The gateway is started with the same runtime tuning the container image sets
# (GOGC and GOMEMLIMIT); override them in the environment to compare settings.

set -euo pipefail

REQUESTS=${1:-${REQUESTS:-50}}
PORT=${PORT:-8099}
LISTEN_PORT=${LISTEN_PORT:-18080}
LOGOUT_DELAY=${LOGOUT_DELAY:-2}
SETTLE_SECONDS=${SETTLE_SECONDS:-5}

ROOT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$ROOT_DIR"

export PATH=$PATH:/usr/local/go/bin
mkdir -p bin
CGO_ENABLED=0 go build -trimpath -buildvcs=false -ldflags="-s -w" -o bin/um-status-gateway .

binary_size=$(stat -c %s bin/um-status-gateway 2>/dev/null || stat -f %z bin/um-status-gateway)

python3 scripts/mock-um-portal.py "$PORT" >/tmp/mock-um-portal.log 2>&1 &
mock_pid=$!

# Wait until the mock portal accepts connections before starting the gateway.
for _ in $(seq 1 50); do
  if curl -s -o /dev/null -m 1 "http://127.0.0.1:${PORT}/um/api/getProfiles"; then
    break
  fi
  sleep 0.2
done

UM_HOST="http://127.0.0.1:${PORT}" \
LISTEN_ADDR="127.0.0.1:${LISTEN_PORT}" \
LOGOUT_DELAY_SECONDS="${LOGOUT_DELAY}" \
RELEASE_MEMORY_ON_SESSION_END="${RELEASE_MEMORY_ON_SESSION_END:-true}" \
GOGC="${GOGC:-20}" \
GOMEMLIMIT="${GOMEMLIMIT:-4MiB}" \
bin/um-status-gateway >/tmp/um-status-gateway.log 2>&1 &
gateway_pid=$!

cleanup() {
  kill "$gateway_pid" "$mock_pid" 2>/dev/null || true
  wait "$gateway_pid" "$mock_pid" 2>/dev/null || true
}
trap cleanup EXIT

rss_kb() {
  awk '/VmRSS/ {print $2}' "/proc/${gateway_pid}/status"
}

for _ in $(seq 1 50); do
  if curl -fsS "http://127.0.0.1:${LISTEN_PORT}/healthz" >/dev/null 2>&1; then
    break
  fi
  sleep 0.2
done

sleep 1
rss_idle=$(rss_kb)

first_request=$(curl -fsS "http://127.0.0.1:${LISTEN_PORT}/api/status?user=0018067158&pass=demo")
burst_start=$(date +%s.%N)
for _ in $(seq 1 "$REQUESTS"); do
  curl -fsS "http://127.0.0.1:${LISTEN_PORT}/api/status?user=0018067158&pass=demo" >/dev/null
done
burst_end=$(date +%s.%N)

# Allow the scheduled logouts of the last requests to complete, then let the
# runtime scavenger return idle memory to the operating system.
sleep "$((LOGOUT_DELAY + SETTLE_SECONDS))"
rss_loaded=$(rss_kb)

printf 'sample response : %s\n' "$first_request"
printf 'binary size     : %.2f MiB (%s bytes)\n' \
  "$(awk -v b="$binary_size" 'BEGIN {print b/1048576}')" "$binary_size"
printf 'requests served : %s\n' "$REQUESTS"
printf 'average latency : %.1f ms/request\n' \
  "$(awk -v s="$burst_start" -v e="$burst_end" -v n="$REQUESTS" 'BEGIN {print (e-s)*1000/n}')"
printf 'RSS after start : %s kB (%.2f MiB)\n' \
  "$rss_idle" "$(awk -v k="$rss_idle" 'BEGIN {print k/1024}')"
printf 'RSS after load  : %s kB (%.2f MiB)\n' \
  "$rss_loaded" "$(awk -v k="$rss_loaded" 'BEGIN {print k/1024}')"
