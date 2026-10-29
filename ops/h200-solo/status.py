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


def collect(env):
    ws = env.get("WORKSPACE", "/workspace")
    logs, run = os.path.join(ws, "logs"), os.path.join(ws, "run")
    now = time.time()
    s = {"ts": int(now), "address": mask_address(env.get("PEARLD_MINING_ADDRESS", "")),
         "model": env.get("MODEL", ""), "version": env.get("PEARL_VERSION", "")}

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
