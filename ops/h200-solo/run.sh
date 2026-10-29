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
export LD_LIBRARY_PATH="/usr/local/cuda/lib64:$VENV/lib/python3.12/site-packages/torch/lib:${LD_LIBRARY_PATH:-}"
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
# shellcheck disable=SC1091
. "$HERE/lib.sh"
# shellcheck disable=SC2046
start pearld $(pearld_cmd)

log "waiting for pearld RPC"
until prlctl "${RPC_ARGS[@]}" getblockcount >/dev/null 2>&1; do sleep 2; done

log "waiting for initial block download (peers connected, local height at the network tip, getblocktemplate OK)"
while true; do
  read -r peers local tip <<<"$(python3 - "$PEARLD_RPC_USER" "$PEARLD_RPC_PASSWORD" <<'PYEOF'
import json, subprocess, sys
u, p = sys.argv[1], sys.argv[2]
def rpc(m):
    r = subprocess.run(["prlctl", "--notls", "-s", "127.0.0.1:44107", "-u", u, "-P", p, m], capture_output=True, text=True, timeout=20)
    return json.loads(r.stdout) if r.returncode == 0 and r.stdout.strip() else None
peers = rpc("getpeerinfo") or []
tip = max([max(x.get("currentheight", 0) or 0, x.get("startingheight", 0) or 0) for x in peers] or [0])
print(len(peers), rpc("getblockcount") or 0, tip)
PYEOF
)"
  gbt_ok=0
  prlctl "${RPC_ARGS[@]}" getblocktemplate >/dev/null 2>&1 && gbt_ok=1
  if [ "${peers:-0}" -ge 3 ] && [ "${tip:-0}" -gt 1000 ] && [ "${local:-0}" -ge $((tip - 2)) ] && [ "$gbt_ok" = 1 ]; then
    break
  fi
  log "syncing... local height ${local:-?} / network ${tip:-?} (${peers:-0} peers, template ok=$gbt_ok)"
  sleep 30
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
