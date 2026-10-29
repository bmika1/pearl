#!/usr/bin/env bash
# Stop everything started by run.sh (reverse order). Chain data, model cache and logs are kept.
set -uo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ENV_FILE="${ENV_FILE:-$HERE/env}"
# shellcheck disable=SC1090
set -a; . "$ENV_FILE"; set +a
RUN="${WORKSPACE:-/workspace}/run"
for n in watchdog miner loadgen vllm gateway pearld status tunnel logstream; do
  if [ -f "$RUN/$n.pid" ]; then
    pid="$(cat "$RUN/$n.pid")"
    if kill -0 "$pid" 2>/dev/null; then
      echo "stopping $n ($pid)"; kill "$pid" 2>/dev/null
      for _ in $(seq 1 30); do kill -0 "$pid" 2>/dev/null || break; sleep 1; done
      kill -9 "$pid" 2>/dev/null || true
    fi
    rm -f "$RUN/$n.pid"
  fi
done
pkill -f 'vllm serve' 2>/dev/null || true
echo "stopped"
