#!/usr/bin/env bash
# Pool mode: WildRig Multi on the Pearlhash pool (0 % dev fee there, 1 % pool fee, 1 PRL min payout),
# with our log shipper, status collector, watchdog and optional tunnel around it. No node, no model.
# Idempotent like run.sh. Rewards go to PEARLD_MINING_ADDRESS.
#
#   env: MODE=pool  POOL_URL (default stratum+tcp://pool.pearlhash.xyz:9000)  POOL_WORKER (default: rig id)
#        WILDRIG_VERSION (default 0.51.8)  WILDRIG_KERNEL (1 standard, 2 efficient)
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ENV_FILE="${ENV_FILE:-$HERE/env}"
# shellcheck disable=SC1090
set -a; . "$ENV_FILE"; set +a
WORKSPACE="${WORKSPACE:-/workspace}"
LOGS="$WORKSPACE/logs"; RUN="$WORKSPACE/run"; WR="$WORKSPACE/wildrig"
POOL_URL="${POOL_URL:-stratum+tcp://pool.pearlhash.xyz:9000}"
WILDRIG_VERSION="${WILDRIG_VERSION:-0.51.8}"
mkdir -p "$LOGS" "$RUN"
export PATH="/usr/local/bin:$PATH"

log() { printf '[run-pool %s] %s\n' "$(date -u +%H:%M:%S)" "$*" | tee -a "$LOGS/status.log"; }
alive() { [ -f "$RUN/$1.pid" ] && kill -0 "$(cat "$RUN/$1.pid")" 2>/dev/null; }
start() {
  local name="$1"; shift
  if alive "$name"; then log "$name already running (pid $(cat "$RUN/$name.pid"))"; return; fi
  log "starting $name"
  nohup "$@" >>"$LOGS/$name.log" 2>&1 &
  echo $! >"$RUN/$name.pid"
}

# the reference stack must not run alongside the pool miner (both want the GPU)
for n in watchdog loadgen vllm gateway pearld; do
  if alive "$n"; then log "stopping $n (pool mode)"; kill "$(cat "$RUN/$n.pid")" 2>/dev/null || true; rm -f "$RUN/$n.pid"; fi
done
pkill -f "venv/bin/[v]llm serve" 2>/dev/null || true

WORKER="${POOL_WORKER:-$(echo "${LOGS_PUSH_URL:-}" | sed -nE 's|.*/api/miners/([^/]+)/ingest.*|\1|p')}"
WORKER="${WORKER:-$(hostname -s | tr -cd '[:alnum:]_-')}"

# ---- 1. miner binary ------------------------------------------------------------------------------
if [ ! -x "$WR/wildrig-multi" ]; then
  log "downloading WildRig Multi $WILDRIG_VERSION"
  mkdir -p "$WR"
  curl -fsSL "https://github.com/andru-kun/wildrig-multi/releases/download/$WILDRIG_VERSION/wildrig-multi-linux-$WILDRIG_VERSION.tar.gz" -o "$WR/wr.tar.gz"
  tar -xzf "$WR/wr.tar.gz" -C "$WR" && rm -f "$WR/wr.tar.gz"
  chmod +x "$WR/wildrig-multi"
fi

# ---- 2. shipper, tunnel, status -------------------------------------------------------------------
PUSH_ARGS=()
if [ -n "${LOGS_PUSH_URL:-}" ] && [ -n "${LOGS_PUSH_TOKEN:-}" ]; then PUSH_ARGS=(--push-url "$LOGS_PUSH_URL" --push-token "$LOGS_PUSH_TOKEN"); fi
start logstream python3 "$HERE/logstream/logstream.py" --logs-dir "$LOGS" --port "${LOGSTREAM_PORT:-8080}" --bind 127.0.0.1 \
  --status-file "$RUN/status.json" --title "${LOGSTREAM_TITLE:-Pearl pool miner}" "${PUSH_ARGS[@]}"
if [ "${PUBLISH_TUNNEL:-0}" = "1" ]; then start tunnel cloudflared tunnel --no-autoupdate --url "http://127.0.0.1:${LOGSTREAM_PORT:-8080}"; fi
start status bash -c "while true; do
  python3 '$HERE/status.py' --env '$ENV_FILE' --json > '$RUN/status.json.tmp' && mv '$RUN/status.json.tmp' '$RUN/status.json'
  python3 '$HERE/status.py' --env '$ENV_FILE' --line >> '$LOGS/status.log'
  sleep 15; done"

# ---- 3. the miner ---------------------------------------------------------------------------------
KERNEL_ARGS=(); [ -n "${WILDRIG_KERNEL:-}" ] && KERNEL_ARGS=(--pearlhash-kernel "$WILDRIG_KERNEL")
start miner "$WR/wildrig-multi" --algo pearlhash --url "$POOL_URL" \
  --user "$PEARLD_MINING_ADDRESS.$WORKER" --pass x --watchdog "${KERNEL_ARGS[@]}"

# ---- 4. watchdog ----------------------------------------------------------------------------------
start watchdog bash -c "while true; do
  for n in miner logstream status; do
    if [ -f '$RUN'/\$n.pid ] && ! kill -0 \$(cat '$RUN'/\$n.pid) 2>/dev/null; then
      echo \"[watchdog \$(date -u +%H:%M:%S)] \$n died, restarting via run-pool.sh\" >> '$LOGS/status.log'
      rm -f '$RUN'/\$n.pid; '$HERE/run-pool.sh' >/dev/null 2>&1 || true
    fi
  done; sleep 20; done"

log "pool mode up: $PEARLD_MINING_ADDRESS.$WORKER -> $POOL_URL"
log "pool dashboard: https://pearlhash.xyz (search the address)"
