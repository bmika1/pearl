#!/usr/bin/env python3
"""Synthetic prefill load for the Pearl vLLM miner. Stdlib only.

The miner only hashes GEMMs whose token dimension m >= 1024, and vLLM only runs GEMMs
when it has requests. This keeps N long-prompt completions in flight with max_tokens=1,
so nearly all GPU time is prefill (large m) and almost none is decode (m = #sequences).
Prompts are random word soup with a nonce, so prefix caching could never skip the work.
"""
import argparse
import json
import random
import threading
import time
import urllib.error
import urllib.request

WORDS = ("pearl oyster matrix tensor lattice kernel epoch nonce ledger merkle salt noise rank tile "
         "hopper cutlass stream event header proof circuit field prime root commit hash block chain "
         "peer sync node wallet taproot grain reward target bits time span filter drift anchor cable "
         "harbor glacier meadow canyon ember quartz basalt cedar maple willow falcon heron otter lynx "
         "copper silver cobalt nickel zinc argon neon xenon krypton radon helium carbon boron silicon "
         "monday tuesday thursday friday sunday january march april june august october december "
         "north south east west upper lower inner outer left right near far early late quick slow "
         "red orange yellow green blue indigo violet amber crimson teal olive navy coral ivory slate").split()


def make_prompt(n_tokens, rng):
    # ~1.3 tokens per English word on Llama tokenizers; aim slightly high.
    n_words = int(n_tokens / 1.25)
    body = " ".join(rng.choice(WORDS) for _ in range(n_words))
    return f"[{rng.getrandbits(64):016x}] Continue the sequence: {body}"


class Stats:
    def __init__(self):
        self.lock = threading.Lock()
        self.reset()

    def reset(self):
        self.n = 0
        self.prompt_tokens = 0
        self.errors = 0
        self.lat = []

    def add(self, ok, ptoks, latency):
        with self.lock:
            if ok:
                self.n += 1
                self.prompt_tokens += ptoks
                self.lat.append(latency)
            else:
                self.errors += 1

    def snapshot_and_reset(self):
        with self.lock:
            s = (self.n, self.prompt_tokens, self.errors, sorted(self.lat))
            self.reset()
        return s


def get_model_id(url):
    while True:
        try:
            with urllib.request.urlopen(f"{url}/v1/models", timeout=5) as r:
                data = json.load(r)
            return data["data"][0]["id"]
        except Exception as e:
            print(f"loadgen: waiting for vLLM ({e})", flush=True)
            time.sleep(5)


def worker(url, model, n_tokens, stats, seed):
    rng = random.Random(seed)
    while True:
        body = json.dumps({"model": model, "prompt": make_prompt(n_tokens, rng),
                           "max_tokens": 1, "temperature": 0}).encode()
        req = urllib.request.Request(f"{url}/v1/completions", data=body,
                                     headers={"Content-Type": "application/json"})
        t0 = time.time()
        try:
            with urllib.request.urlopen(req, timeout=300) as r:
                data = json.load(r)
            stats.add(True, data.get("usage", {}).get("prompt_tokens", 0), time.time() - t0)
        except urllib.error.HTTPError as e:
            stats.add(False, 0, 0)
            if e.code == 400:
                print(f"loadgen: 400 from server, prompt probably exceeds max_model_len: {e.read()[:200]!r}", flush=True)
                time.sleep(5)
        except Exception:
            stats.add(False, 0, 0)
            time.sleep(1)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--url", default="http://127.0.0.1:8000")
    ap.add_argument("--concurrency", type=int, default=16)
    ap.add_argument("--prompt-tokens", type=int, default=3000)
    ap.add_argument("--report-every", type=float, default=10.0)
    a = ap.parse_args()

    model = get_model_id(a.url)
    print(f"loadgen: model={model} concurrency={a.concurrency} prompt_tokens~{a.prompt_tokens}", flush=True)
    stats = Stats()
    for i in range(a.concurrency):
        threading.Thread(target=worker, args=(a.url, model, a.prompt_tokens, stats, 1000 + i), daemon=True).start()

    while True:
        time.sleep(a.report_every)
        n, ptoks, errs, lat = stats.snapshot_and_reset()
        p50 = lat[len(lat) // 2] if lat else 0.0
        print(f"loadgen: {n / a.report_every:.2f} req/s, {ptoks / a.report_every:.0f} prompt tok/s, "
              f"p50 latency {p50:.2f}s, inflight={a.concurrency}, errors={errs}", flush=True)


if __name__ == "__main__":
    main()
