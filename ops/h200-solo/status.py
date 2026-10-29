#!/usr/bin/env python3
"""Collect a redaction-safe status snapshot of the solo-mining stack.

--json  prints a JSON object (served by logstream at /api/status)
--line  prints one human-readable line (appended to logs/status.log)

Only public information is emitted: heights, GPU numbers, process liveness, a masked
mining address. RPC credentials never leave this process.
"""
import argparse
import json
import os
import re
import subprocess
import sys
import time
import urllib.request


def load_env(path):
    env = {}
    try:
        with open(path) as f:
            for line in f:
                line = line.strip()
                if not line or line.startswith("#") or "=" not in line:
                    continue
                k, v = line.split("=", 1)
                env[k.strip()] = v.strip().strip('"').strip("'")
    except FileNotFoundError:
        pass
    return env


def sh(cmd, timeout=10):
    try:
        return subprocess.run(cmd, capture_output=True, text=True, timeout=timeout).stdout.strip()
    except Exception:
        return ""


def rpc(env, method, *params):
    args = ["prlctl", "--notls", "-s", "127.0.0.1:44107",
            "-u", env.get("PEARLD_RPC_USER", "rpcuser"), "-P", env.get("PEARLD_RPC_PASSWORD", ""),
            method, *params]
    try:
        p = subprocess.run(args, capture_output=True, text=True, timeout=15)
    except Exception:
        return None, "prlctl unavailable"
    if p.returncode != 0:
        return None, (p.stderr or p.stdout).strip()[:200]
    out = p.stdout.strip()
    try:
        return json.loads(out), None
    except ValueError:
        return out, None


def mask_address(a):
    if not a or len(a) < 12:
        return "(unset)"
    return a[:9] + "…" + a[-4:]


def pid_alive(run_dir, name):
    try:
        with open(os.path.join(run_dir, name + ".pid")) as f:
            pid = int(f.read().strip())
        os.kill(pid, 0)
        return True
    except Exception:
        return False


def tail_lines(path, n=4000):
    try:
        with open(path, "rb") as f:
            f.seek(0, 2)
            size = f.tell()
            f.seek(max(0, size - 512 * 1024))
            data = f.read().decode("utf-8", "replace")
        return data.splitlines()[-n:]
    except FileNotFoundError:
        return []

ANSI = re.compile(r"\x1b\[[0-9;?]*[A-Za-z]")
_HTTP_CACHE = {}


def http_json(url, ttl=60):
    """GET JSON with a small in-process cache (status.py runs once per tick, so cache on disk)."""
    import hashlib
    key = os.path.join("/tmp", "status-cache-" + hashlib.sha1(url.encode()).hexdigest()[:12] + ".json")
    try:
        if time.time() - os.path.getmtime(key) < ttl:
            with open(key) as f:
                return json.load(f)
    except Exception:
        pass
    try:
        with urllib.request.urlopen(urllib.request.Request(url, headers={"User-Agent": "pearl-status/1"}), timeout=8) as r:
            data = json.load(r)
        with open(key, "w") as f:
            json.dump(data, f)
        return data
    except Exception:
        return None


def block_reward(height):
    S, E = 2.1e9, 650226
    return S * E / ((height + E) * (height - 1 + E)) if height else 0


def collect_pool(env, s, logs):
    """WildRig on a pool: hashrate/shares from miner.log, treasury + network from Blockbook."""
    s["mode"] = "pool"
    lines = [ANSI.sub("", l) for l in tail_lines(os.path.join(logs, "miner.log"), 600)]
    pool = {"name": "Pearlhash", "url": env.get("POOL_URL", "stratum+tcp://pool.pearlhash.xyz:9000"),
            "worker": env.get("POOL_WORKER") or (re.search(r"/api/miners/([^/]+)/", env.get("LOGS_PUSH_URL", "")) or [None, None])[1],
            "fee_pct": 1, "hashrate_ths": None, "accepted": 0, "rejected": 0, "connected": False,
            "last_share_ago_s": None, "network_hashps": None, "est_prl_day": None}
    for line in reversed(lines):
        m = re.search(r"^\s*10s:\s+([\d.]+)\s+TH/s", line) or re.search(r"#\d+\s+\S.*?\s([\d.]+)\s+TH/s", line)
        if m:
            pool["hashrate_ths"] = float(m.group(1))
            break
    for line in reversed(lines):
        m = re.search(r"\[(\d{2}:\d{2}:\d{2})\]\s+accepted\s+\((\d+)/(\d+)\)", line)
        if m:
            pool["accepted"], pool["rejected"] = int(m.group(2)), int(m.group(3))
            hh, mm, ss = map(int, m.group(1).split(":"))
            now = time.gmtime()
            ago = (now.tm_hour * 3600 + now.tm_min * 60 + now.tm_sec) - (hh * 3600 + mm * 60 + ss)
            pool["last_share_ago_s"] = ago if ago >= 0 else ago + 86400
            break
    pool["connected"] = any("use pool" in l or "new job from" in l for l in lines[-200:]) and not any(
        "disconnected" in l.lower() or "connection refused" in l.lower() for l in lines[-20:])
    bb = http_json("https://blockbook.pearlresearch.ai/api/", ttl=60) or {}
    backend = bb.get("backend", {})
    height = backend.get("blocks")
    diff = float(backend.get("difficulty") or 0)
    s["node"] = {"height": None, "network_height": height, "peers": None, "synced": None, "difficulty": diff or None,
                 "network_hashps": None, "error": None}
    if diff:
        # Pearl's getmininginfo networkhashps was ~1.44e12 x difficulty on our synced node (empirical).
        pool["network_hashps"] = diff * 1.44e12
        s["node"]["network_hashps"] = pool["network_hashps"]
    if pool["hashrate_ths"] and pool["network_hashps"] and height:
        share = pool["hashrate_ths"] * 1e12 / pool["network_hashps"]
        pool["est_prl_day"] = round(share * (86400 / 194) * block_reward(height) * (1 - pool["fee_pct"] / 100), 2)
        s["expected_blocks_per_day"] = round(share * 86400 / 194, 5)
    addr = env.get("PEARLD_MINING_ADDRESS", "")
    t = http_json(f"https://blockbook.pearlresearch.ai/api/v2/address/{addr}?details=basic", ttl=60) or {}
    grains = lambda v: int(v or 0) / 1e8
    s["treasury"] = {"balance_prl": grains(t.get("balance")), "pending_prl": grains(t.get("unconfirmedBalance")),
                     "received_prl": grains(t.get("totalReceived")), "txs": t.get("txs")} if t else {}
    s["pool"] = pool
    s["miner"] = {"work_rate_ths": None, "blocks_found": 0, "blocks_accepted": 0, "last_block_found": None}
    s["vllm"] = {"ready": False}
    s["loadgen"] = {}
    s["procs"] = {n: pid_alive(os.path.join(env.get("WORKSPACE", "/workspace"), "run"), n)
                  for n in ["miner", "logstream", "status", "watchdog", "tunnel"]}
    return s


def collect(env):
    ws = env.get("WORKSPACE", "/workspace")
    logs, run = os.path.join(ws, "logs"), os.path.join(ws, "run")
    now = time.time()
    s = {"ts": int(now), "address": mask_address(env.get("PEARLD_MINING_ADDRESS", "")),
         "model": env.get("MODEL", ""), "version": env.get("PEARL_VERSION", "")}

    s["mode"] = env.get("MODE", "solo")
    # gpu first (both modes)
    q0 = sh(["nvidia-smi", "--query-gpu=name,utilization.gpu,memory.used,memory.total,power.draw,temperature.gpu",
             "--format=csv,noheader,nounits"])
    if q0:
        parts = [x.strip() for x in q0.splitlines()[0].split(",")]
        if len(parts) >= 6:
            s["gpu"] = {"name": parts[0], "util_pct": float(parts[1]), "mem_used_mb": float(parts[2]),
                        "mem_total_mb": float(parts[3]), "power_w": float(parts[4]), "temp_c": float(parts[5])}
    if s["mode"] == "pool":
        collect_pool(env, s, logs)
        try:
            s["uptime_s"] = int(now - os.path.getmtime(os.path.join(run, "miner.pid")))
        except Exception:
            s["uptime_s"] = None
        return s

    # processes
    s["procs"] = {n: pid_alive(run, n) for n in
                  ["pearld", "gateway", "vllm", "loadgen", "logstream", "tunnel", "status", "watchdog"]}

    # node
    node = {"height": None, "network_height": None, "peers": None, "synced": False,
            "difficulty": None, "network_hashps": None, "error": None}
    bc, err = rpc(env, "getblockchaininfo")
    if isinstance(bc, dict):
        node["height"] = bc.get("blocks")
        node["difficulty"] = bc.get("difficulty")
    else:
        node["error"] = err
    peers, _ = rpc(env, "getpeerinfo")
    if isinstance(peers, list):
        node["peers"] = len(peers)
        hs = [max(p.get("currentheight", 0) or 0, p.get("startingheight", 0) or 0) for p in peers]
        node["network_height"] = max(hs) if hs else None
    mi, _ = rpc(env, "getmininginfo")
    if isinstance(mi, dict):
        node["network_hashps"] = mi.get("networkhashps")
        node["difficulty"] = mi.get("difficulty", node["difficulty"])
    gbt, gerr = rpc(env, "getblocktemplate")
    node["synced"] = isinstance(gbt, dict)
    if gerr and not node["error"]:
        node["error"] = gerr
    s["node"] = node

    # gpu
    q = sh(["nvidia-smi", "--query-gpu=name,utilization.gpu,memory.used,memory.total,power.draw,temperature.gpu",
            "--format=csv,noheader,nounits"])
    gpu = {}
    if q:
        parts = [x.strip() for x in q.splitlines()[0].split(",")]
        if len(parts) >= 6:
            gpu = {"name": parts[0], "util_pct": float(parts[1]), "mem_used_mb": float(parts[2]),
                   "mem_total_mb": float(parts[3]), "power_w": float(parts[4]), "temp_c": float(parts[5])}
    s["gpu"] = gpu

    # vllm
    vllm = {"ready": False, "prompt_tps": None, "gen_tps": None}
    try:
        urllib.request.urlopen("http://127.0.0.1:8000/health", timeout=3)
        vllm["ready"] = True
    except Exception:
        pass
    for line in reversed(tail_lines(os.path.join(logs, "vllm.log"), 400)):
        m = re.search(r"Avg prompt throughput: ([\d.]+) tokens/s, Avg generation throughput: ([\d.]+)", line)
        if m:
            vllm["prompt_tps"], vllm["gen_tps"] = float(m.group(1)), float(m.group(2))
            break
    s["vllm"] = vllm

    # miner (from the work-rate patch) and block events
    miner = {"work_rate_ths": None, "work_rate_line": None, "blocks_found": 0, "blocks_accepted": 0,
             "last_block_found": None}
    vl = tail_lines(os.path.join(logs, "vllm.log"))
    for line in reversed(vl):
        m = re.search(r"miner work rate: ([\d.]+) TH/s", line)
        if m:
            miner["work_rate_ths"] = float(m.group(1))
            miner["work_rate_line"] = line[-160:]
            break
    miner["blocks_found"] = sum(1 for l in vl if "Block found!" in l or "MoE block found!" in l)
    gl = tail_lines(os.path.join(logs, "gateway.log"))
    miner["blocks_accepted"] = sum(1 for l in gl if "Block accepted by node!" in l)
    for line in reversed(gl):
        if "Block accepted by node!" in line:
            miner["last_block_found"] = line[:19]
            break
    s["miner"] = miner

    # loadgen
    lg = {"rps": None, "prompt_tps": None, "errors": None}
    for line in reversed(tail_lines(os.path.join(logs, "loadgen.log"), 50)):
        m = re.search(r"loadgen: ([\d.]+) req/s, ([\d.]+) prompt tok/s, .*errors=(\d+)", line)
        if m:
            lg = {"rps": float(m.group(1)), "prompt_tps": float(m.group(2)), "errors": int(m.group(3))}
            break
    s["loadgen"] = lg

    # tunnel
    url = None
    for line in reversed(tail_lines(os.path.join(logs, "tunnel.log"), 200)):
        m = re.search(r"https://[a-z0-9-]+\.trycloudflare\.com", line)
        if m:
            url = m.group(0)
            break
    s["public_url"] = url

    # uptime of the stack = age of logstream pid file
    try:
        s["uptime_s"] = int(now - os.path.getmtime(os.path.join(run, "logstream.pid")))
    except Exception:
        s["uptime_s"] = None

    # expected solo odds, if we know both rates
    if miner["work_rate_ths"] and node["network_hashps"]:
        share = miner["work_rate_ths"] * 1e12 / float(node["network_hashps"])
        s["expected_blocks_per_day"] = round(share * 86400 / 194, 5)
    return s


def one_line(s):
    if s.get("mode") == "pool":
        p, g, t = s.get("pool", {}), s.get("gpu", {}), s.get("treasury", {})
        return ("status: pool {name} worker={w} | {h} TH/s | shares {a} ok / {r} rej | gpu {u}% {pw}W | "
                "treasury {b} PRL (+{pend} pending) | est {e} PRL/day").format(
            name=p.get("name"), w=p.get("worker"), h=p.get("hashrate_ths"), a=p.get("accepted"), r=p.get("rejected"),
            u=g.get("util_pct"), pw=g.get("power_w"), b=t.get("balance_prl"), pend=t.get("pending_prl"), e=p.get("est_prl_day"))
    n, g, m, v = s["node"], s["gpu"], s["miner"], s["vllm"]
    return ("status: height {h}/{nh} peers={p} synced={sy} | gpu {u}% {pw}W | work {w} TH/s | "
            "vllm ready={vr} prompt {pt} tok/s | blocks found={bf} accepted={ba}").format(
        h=n["height"], nh=n["network_height"], p=n["peers"], sy=n["synced"],
        u=g.get("util_pct"), pw=g.get("power_w"), w=m["work_rate_ths"], vr=v["ready"],
        pt=v["prompt_tps"], bf=m["blocks_found"], ba=m["blocks_accepted"])


if __name__ == "__main__":
    ap = argparse.ArgumentParser()
    ap.add_argument("--env", required=True)
    ap.add_argument("--json", action="store_true")
    ap.add_argument("--line", action="store_true")
    a = ap.parse_args()
    snap = collect(load_env(a.env))
    if a.json:
        print(json.dumps(snap))
    if a.line or not a.json:
        print(time.strftime("%Y-%m-%d %H:%M:%S", time.gmtime()) + " " + one_line(snap))
    sys.exit(0)
