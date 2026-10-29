#!/usr/bin/env bash
# Local checks, no GPU needed: redaction unit tests + an end-to-end smoke test of the log page.
# Writes fake logs containing secrets, starts logstream on a free loopback port, and asserts that
# the API serves redacted lines, drops payload lines, serves status JSON, and the page renders.
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TMP="${TMPDIR:-/tmp}/pearl-logstream-smoke.$$"
mkdir -p "$TMP/logs" "$TMP/run"
trap '{ kill ${LSPID:-} 2>/dev/null; wait ${LSPID:-} 2>/dev/null; rm -rf "$TMP"; } || true' EXIT

echo "== redaction unit tests"
(cd "$HERE/logstream" && python3 -m unittest -q test_redact)

echo "== smoke test"
printf '%s\n' \
  '2026-10-28 20:00:01 [INF] SRVR: Starting pearld --rpcpass=hunter2 --miningaddr=prl1p5cyxnuxmeuwuvkwfem96lqzszd02n6xdcjrs20cac6yqjjwudpxqkedrcr' \
  '2026-10-28 20:00:02 [INF] SYNC: New valid peer 203.0.113.9:44108 (outbound)' \
  '2026-10-28 20:00:03 [INF] SYNC: Syncing to block height 135200 from peer 203.0.113.9:44108' > "$TMP/logs/pearld.log"
printf '%s\n' \
  '2026-10-28 20:00:05 - INFO - Updating block template to height 135201 - work_cache.py:35' \
  '2026-10-28 20:00:09 - INFO - Block accepted by node! - submission_service.py:62' > "$TMP/logs/gateway.log"
printf '%s\n' \
  'INFO 10-28 20:00:06 Received request cmpl-1: prompt: "payload that must be dropped"' \
  '2026-10-28 20:00:07 - INFO - miner work rate: 731.20 TH/s (inner hashes) | 1900.0 T int8-MAC/s | 812 noisy GEMMs in 10.0s | avg m=4096 - gemm_operators.py:140' \
  'INFO 10-28 20:00:08 Avg prompt throughput: 25000.0 tokens/s, Avg generation throughput: 8.0 tokens/s' \
  '2026-10-28 20:00:08 - INFO - Block found! header=... - callbacks.py:33' > "$TMP/logs/vllm.log"
cat > "$TMP/run/status.json" <<'JSON'
{"ts":1,"address":"prl1p5cyx…drcr","node":{"height":135201,"network_height":135201,"peers":8,"synced":true,"difficulty":49200000,"network_hashps":7.1e19},
 "gpu":{"name":"NVIDIA H200","util_pct":97,"mem_used_mb":120000,"mem_total_mb":143771,"power_w":655,"temp_c":61},
 "vllm":{"ready":true,"prompt_tps":25000,"gen_tps":8},"miner":{"work_rate_ths":731.2,"blocks_found":1,"blocks_accepted":1,"last_block_found":"2026-10-28 20:00:09"},
 "loadgen":{"rps":8.1,"prompt_tps":24300,"errors":0},"procs":{"pearld":true,"gateway":true,"vllm":true,"loadgen":true,"logstream":true,"tunnel":true,"status":true,"watchdog":true},
 "public_url":"https://x.trycloudflare.com","uptime_s":5400,"expected_blocks_per_day":0.0046,"leak":"rpcpass=hunter2"}
JSON

PORT="$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()')"
python3 "$HERE/logstream/logstream.py" --logs-dir "$TMP/logs" --status-file "$TMP/run/status.json" \
  --port "$PORT" --title "smoke test" > "$TMP/ls.out" 2>&1 &
LSPID=$!
for _ in $(seq 1 20); do curl -fsS "http://127.0.0.1:$PORT/healthz" >/dev/null 2>&1 && break; sleep 0.25; done
# a line appended after startup must show up within the poll interval
echo '2026-10-28 20:00:30 [INF] SYNC: Verified downloaded block e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855 at height 135202' >> "$TMP/logs/pearld.log"
sleep 1.5

PORT="$PORT" python3 - <<'PY'
import json, os, sys, urllib.request
base = f"http://127.0.0.1:{os.environ['PORT']}"
get = lambda p: urllib.request.urlopen(base + p, timeout=5)
lines = json.load(get("/api/lines?after=0"))
texts = [l["text"] for l in lines["lines"]]
joined = "\n".join(texts)
for l in lines["lines"]:
    print(f"  {l['seq']:>3} {l['src']:<9} {l['kind']:<6} {l['text'][:110]}")
def check(cond, msg):
    print(("  ok   " if cond else "  FAIL ") + msg)
    if not cond: sys.exit(1)
check("hunter2" not in joined, "rpc password masked")
check("prl1p5cyxnuxmeuwuvkwfem96lqzszd02n6xdcjrs20cac6yqjjwudpxqkedrcr" not in joined and "prl1p5cyx…drcr" in joined, "mining address masked")
check("203.0.113.9" not in joined and "203.x.x.x" in joined, "peer IP masked")
check("payload that must be dropped" not in joined, "request payload line dropped")
check(any(l["kind"] == "block" for l in lines["lines"]), "block events tagged")
check("at height 135202" in joined, "line appended after start was picked up")
check("e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" not in joined, "64-hex shortened")
st = json.load(get("/api/status"))
check(st["address"] == "prl1p5cyx…drcr" and "served_at" in st, "status served")
check("hunter2" not in json.dumps(st), "status passes through redactor")
page = get("/").read().decode()
check("<title>smoke test</title>" in page and "api/lines" in page, "page renders with title")
try:
    urllib.request.urlopen(base + "/etc/passwd", timeout=5); check(False, "unknown path 404")
except urllib.error.HTTPError as e:
    check(e.code == 404, "unknown path 404")
print("  all smoke checks passed")
PY
