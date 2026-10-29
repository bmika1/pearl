#!/usr/bin/env bash
# Start the whole solo-mining stack in dependency order and keep it running.
# Order: logstream -> tunnel -> pearld -> (wait for sync) -> gateway -> vLLM -> (wait ready) -> loadgen.
# Every process logs to $WORKSPACE/logs/<name>.log; the log streamer serves those (redacted).
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ENV_FILE="${ENV_FILE:-$HERE/env}"
# shellcheck disable=SC1090
set -a; . "$ENV_FILE"; set +a
WORKSPACE="${WORKSPACE:-/workspace}"
VENV="$WORKSPACE/venv"
LOGS="$WORKSPACE/logs"
RUN="$WORKSPACE/run"
export HF_HOME="${HF_HOME:-$WORKSPACE/hf}"
export PATH="/usr/local/bin:$PATH"
mkdir -p "$LOGS" "$RUN"

RPC_ARGS=(--notls -s 127.0.0.1:44107 -u "$PEARLD_RPC_USER" -P "$PEARLD_RPC_PASSWORD")

log() { printf '[run %s] %s\n' "$(date -u +%H:%M:%S)" "$*" | tee -a "$LOGS/status.log"; }

alive() { [ -f "$RUN/$1.pid" ] && kill -0 "$(cat "$RUN/$1.pid")" 2>/dev/null; }

# start <name> <cmd...>: background, append to logs/<name>.log, record pid. No-op if alive.
start() {
  local name="$1"; shift
  if alive "$name"; then log "$name already running (pid $(cat "$RUN/$name.pid"))"; return; fi
  log "starting $name"
  nohup "$@" >>"$LOGS/$name.log" 2>&1 &
  echo $! >"$RUN/$name.pid"
}

# ---- 1. log streamer + public tunnel first, so viewers can watch the node sync -------------
PUSH_ARGS=()
if [ -n "${LOGS_PUSH_URL:-}" ] && [ -n "${LOGS_PUSH_TOKEN:-}" ]; then
  PUSH_ARGS=(--push-url "$LOGS_PUSH_URL" --push-token "$LOGS_PUSH_TOKEN")
fi
start logstream python3 "$HERE/logstream/logstream.py" \
  --logs-dir "$LOGS" --port "${LOGSTREAM_PORT:-8080}" --bind 127.0.0.1 \
  --status-file "$RUN/status.json" --title "${LOGSTREAM_TITLE:-Pearl solo miner}" "${PUSH_ARGS[@]}"

if [ "${PUBLISH_TUNNEL:-1}" = "1" ]; then
  start tunnel cloudflared tunnel --no-autoupdate --url "http://127.0.0.1:${LOGSTREAM_PORT:-8080}"
fi

# ---- 2. status collector (writes run/status.json every 15 s, one summary line to status.log) --
start status bash -c "while true; do
  python3 '$HERE/status.py' --env '$ENV_FILE' --json > '$RUN/status.json.tmp' && mv '$RUN/status.json.tmp' '$RUN/status.json'
  python3 '$HERE/status.py' --env '$ENV_FILE' --line >> '$LOGS/status.log'
  sleep 15; done"

# ---- 3. node ---------------------------------------------------------------------------------
start pearld pearld \
  --datadir="$WORKSPACE/pearld" --logdir="$WORKSPACE/pearld/logs" \
  --notls --rpclisten=127.0.0.1:44107 \
  --rpcuser="$PEARLD_RPC_USER" --rpcpass="$PEARLD_RPC_PASSWORD" \
  --miningaddr="$PEARLD_MINING_ADDRESS" --debuglevel=info

log "waiting for pearld RPC"
until prlctl "${RPC_ARGS[@]}" getblockcount >/dev/null 2>&1; do sleep 2; done

log "waiting for initial block download (getblocktemplate stops erroring once synced)"
while true; do
  out="$(prlctl "${RPC_ARGS[@]}" getblocktemplate 2>&1 || true)"
  if echo "$out" | grep -qiE 'downloading blocks|not connected|code: -(9|10)'; then
    h="$(prlctl "${RPC_ARGS[@]}" getblockcount 2>/dev/null || echo '?')"
    log "syncing... local height $h"
    sleep 30
  else
    break
  fi
done
log "node synced at height $(prlctl "${RPC_ARGS[@]}" getblockcount)"

# ---- 4. gateway (talks to pearld, exposes /tmp/pearlgw.sock to the miner) -------------------
export PEARLD_RPC_URL="http://127.0.0.1:44107" MINER_RPC_TRANSPORT=uds MINER_RPC_SOCKET_PATH=/tmp/pearlgw.sock
start gateway "$VENV/bin/pearl-gateway" start
until [ -S /tmp/pearlgw.sock ]; do sleep 1; done

# ---- 5. vLLM with the Pearl plugin (mining happens inside its forward passes) ----------------
# Miner settings stay at the tested defaults (MINER_* env vars override MinerSettings if ever needed).
start vllm "$VENV/bin/vllm" serve "$MODEL" \
  --host 127.0.0.1 --port 8000 \
  --max-model-len "${MAX_MODEL_LEN:-8192}" \
  --gpu-memory-utilization "${GPU_MEMORY_UTILIZATION:-0.9}" \
  --max-num-batched-tokens "${MAX_NUM_BATCHED_TOKENS:-16384}" \
  --max-num-seqs "${MAX_NUM_SEQS:-64}" \
  --no-enable-prefix-caching \
  --enforce-eager \
  --uvicorn-log-level warning

log "waiting for vLLM /health (model load takes a few minutes)"
until curl -fsS http://127.0.0.1:8000/health >/dev/null 2>&1; do sleep 5; done
log "vLLM ready"

# ---- 6. synthetic prefill load so the GPU is always hashing ---------------------------------
start loadgen python3 "$HERE/loadgen.py" \
  --url http://127.0.0.1:8000 \
  --concurrency "${LOADGEN_CONCURRENCY:-16}" \
  --prompt-tokens "${LOADGEN_PROMPT_TOKENS:-3000}"

# ---- 7. watchdog: restart light processes if they die; vLLM/pearld deaths are logged only ----
start watchdog bash -c "while true; do
  for n in logstream status loadgen gateway tunnel; do
    if [ -f '$RUN'/\$n.pid ] && ! kill -0 \$(cat '$RUN'/\$n.pid) 2>/dev/null; then
      echo \"[watchdog \$(date -u +%H:%M:%S)] \$n died, restarting via run.sh\" >> '$LOGS/status.log'
      rm -f '$RUN'/\$n.pid; '$HERE/run.sh' >/dev/null 2>&1 || true
    fi
  done
  for n in pearld vllm; do
    if [ -f '$RUN'/\$n.pid ] && ! kill -0 \$(cat '$RUN'/\$n.pid) 2>/dev/null; then
      echo \"[watchdog \$(date -u +%H:%M:%S)] \$n is DOWN; run stop.sh then run.sh to recover\" >> '$LOGS/status.log'
      rm -f '$RUN'/\$n.pid
    fi
  done
  sleep 20; done"

echo
log "stack is up. public page: $(grep -oE 'https://[a-z0-9-]+\.trycloudflare\.com' "$LOGS/tunnel.log" 2>/dev/null | tail -1 || echo '(tunnel disabled)')"
log "tail -f $LOGS/vllm.log | grep -E 'work rate|Block found'"
