# KNOWHOW

Working notes for agents/tools picking up this repo. Keep it factual; date every external number.

## What Pearl is (from the repo)

- L1 chain, Proof-of-Useful-Work: PoW is a by-product of int8 "NoisyGEMM" matrix multiplies run
  inside vLLM inference (paper: arXiv 2504.09971). Monorepo: Go node (`node/`, btcd fork),
  wallet (`wallet/`), SPV (`spv/`), Rust ZK prover (`zk-pow/`, Plonky2/STARKy), CUDA kernels
  (`miner/pearl-gemm/`, CUTLASS, **sm90 only = H100/H200**), vLLM plugin (`miner/vllm-miner/`),
  node<->miner bridge (`miner/pearl-gateway/`).
- Consensus params (`node/chaincfg/params.go`): target block time 3m14s (194 s), ~445 blocks/day.
  Difficulty uses a WTEMA filter. Mainnet forks: MoE at height 71935, salted-seed (V3 certs) at
  99000 — both already active in Oct 2026, so miners must be on node v1.4.1+/pearl-mining 0.3.1+.
- Emission (`node/blockchain/validate.go` CalcBlockSubsidy): total supply 2.1e9 PRL, smooth
  decay `S*E/((h+E)(h-1+E))`, E = 650226. Reward ≈ 2215 PRL at h=135k, ≈1889 at 200k,
  ≈1325 at 365k.

## How the reference miner actually mines

- `vllm-miner` swaps vLLM's quantized linear ops for NoisyGEMM. A PoW "hit" is detected in the
  GEMM epilogue (`callbacks.py`), the opened block info goes to `pearl-gateway`, which runs the
  CPU ZK prover and calls `submitblock` on pearld. The gateway is **solo-only** (no stratum/pool
  protocol in the repo).
- Mining only happens inside real forward passes, and only when the GEMM is big enough:
  `miner/vllm-miner/src/vllm_miner/config.yaml` requires m ≥ 1024, n ≥ 256, k ≥ 1024. There is
  no built-in load generator: an idle `vllm serve` mines nothing. You need steady batched
  prefill traffic (≥1024 tokens per forward) to keep the GPU hashing.
- Supported reference models: `pearl-ai/Llama-3.1-8B-Instruct-pearl`,
  `pearl-ai/Llama-3.3-70B-Instruct-pearl`, `pearl-ai/Qwen3-30B-A3B-Instruct-2507-pearl` (MoE).
- Public pools ship their own GEMM-only miners (Oyster Pool "Hopper tensor-kernel path",
  Pearlhash recommends WildRig, Kryptex). That is the practical way to mine; the repo stack is
  the reference/solo path.

## Network + economics snapshot (2026-10-28, external, unverified beyond the sources listed)

- Height ~135.1k, difficulty ~49.2M, network ~71 EH/s (WhatToMine 70,975 PH/s; minerstat 71.77;
  Lord of Pearls 68.5). Block reward 2,214 PRL. PRL ≈ $1.86 (CoinGecko), ATH $2.46, circ 361M.
- Per-GPU hashrate: H100 SXM 610 TH/s, H200 SXM 742 TH/s @680 W (Oyster Pool benchmarks);
  an older affiliate blog claims 800–1000 TH/s. RTX 5090 ≈ 415 TH/s on community miners.
- hashrate.no revenue: ~$0.0268/TH/s/day → H200 ≈ $20/day. Own arithmetic agrees:
  742e12/71e18 × 445 × 2214 ≈ 10.3 PRL/day ≈ $19/day.
- Solo on one H200: expected ~1 block per ~215 days (2.2k PRL ≈ $4k lump). Use a pool.
- Pools: Oyster 3% PPLNS, Pearlhash 1%, Kryptex 3% PPS+, AlphaMine 3% PPLNS/solo.
- Sources: whattomine.com/coins/469-prl-pearl, hashrate.no/coins/PRL/benchmarks, oysterpool.com,
  minerstat.com/coin/prl, lordofpearls.xyz/stats, coingecko.com/en/coins/pearl-research.

## Open questions

- CPU ZK proof time per block (gateway `proof_generator.py`) — not measured; matters for solo
  orphan risk only.
- Whether any pool accepts shares from the reference vllm-miner (none document it).

## Per-card economics (2026-10-28, revenue ≈ $0.027/TH/s/day per hashrate.no; WhatToMine table)

| Card | TH/s | W | Gross $/day | Rental $/day | Owned net $/day @0.10–0.20/kWh |
|---|---|---|---|---|---|
| H200 SXM | 742 | 680 | ~20 | 60–100 | 17–18 (card ~$30k) |
| H100 SXM | 610 | 610 | ~16 | ~48 | 13–15 |
| B200 (Kryptex, unverified) | 775 | 950 | ~21 | 100+ | 16–19 |
| RTX 5090 | 415 | 560 | ~10–11 | 5–17 ($0.20–0.69/h) | 7.4–8.7 (card ~$3k) |
| RTX 4090 | 308–330 | 370 | ~8 | 7–10 | 6.3–7.1 (used ~$1.5–2k) |
| RTX 5080 | 210 | 220 | ~5 | — | 4.0–4.5 |
| RTX 5070 Ti | 175 | 200 | ~4.2 | — | 3.3–3.7 |

- No rented card is clearly profitable; 5090 at Vast.ai floor (~$0.20–0.36/h) is the only one
  with a positive gross margin, and those instances are interruptible.
- Owned consumer cards (4090/5090) pay back in ~230–400 days at today's reward, but per-TH
  revenue has been falling (5090 revenue halved within weeks of mainnet; hashrate.no shows
  −9.5% over its last window). Datacenter cards never pay back on Pearl alone.
- Consumer cards need third-party miners (WildRig, Oyster prl-miner, Kryptex); the repo's
  reference miner is sm90-only.

## Rented datacenter cards vs Pearl revenue (user's provider price list, 2026-10-28)

Revenue ≈ $0.027/TH/s/day. Hashrates for non-Hopper cards are estimates from int8 tensor
throughput relative to benchmarked cards; no public PearlHash benchmark exists for A100/GH200/A10/A6000.

| Card | $/h | $/day | Est. TH/s | Rev $/day | Net $/day | Reference miner? |
|---|---|---|---|---|---|---|
| B200 SXM6 | 6.99 | 168 | ~775 (Kryptex, unverified) | ~21 | -147 | no (sm100) |
| GH200 | 2.29 | 55 | ~600–700 | ~16–19 | -36 to -39 | yes (sm90) |
| H100 SXM | 4.29 | 103 | 610 | ~16 | -87 | yes |
| H100 PCIe | 3.29 | 79 | ~500 | ~13 | -66 | yes |
| A100 40GB | 1.99 | 48 | ~250–300 | ~7–8 | -40 | no (sm80, third-party miner) |
| A10 | 1.29 | 31 | ~100 | ~2.7 | -28 | no |
| A6000 | 1.09 | 26 | ~130 | ~3.5 | -23 | no |
| Quadro RTX 6000 | 0.69 | 17 | ~90 | ~2.4 | -14 | no |

Every row loses ≥70% of rent. GH200 is the least bad (rev ≈ 30% of rent). Rented Pearl mining
only clears at ≲ $0.40/h for a 5090-class card.

## ops/h200-solo: self-hosted solo mining kit with public redacted log page (2026-10-28)

- Location: `ops/h200-solo/` (README is the runbook). Target: one rented Hopper box (Vast.ai,
  image `nvidia/cuda:13.0.3-devel-ubuntu24.04`, >= 300 GB disk). Wallet is created locally;
  the box only receives the public Taproot address.
- Why source build: the GHCR `vllm-miner` image is tagged v1.1.0 (pre salted-seed fork), so
  `bootstrap.sh` clones the repo at `PEARL_VERSION` (v1.5.5) and runs `uv sync --package vllm-miner`
  on the box (CUTLASS + plonky2 compile, 20–45 min). pearld/prlctl come from the release installer.
- Start order in `run.sh`: logstream -> cloudflared quick tunnel -> status loop -> pearld -> wait
  until `getblocktemplate` stops returning "downloading blocks" -> pearl-gateway (UDS) -> vllm serve
  (`--no-enable-prefix-caching --enforce-eager --max-num-batched-tokens 16384`) -> loadgen -> watchdog.
- Load generator is required: the miner only hashes GEMMs with m >= 1024 (`vllm_miner/config.yaml`),
  i.e. prefill of long prompts. `loadgen.py` keeps 16 x ~3000-token `max_tokens=1` requests in flight.
- Miner patch `patches/0001-miner-work-rate-log.patch` (also applied to this checkout in
  `miner/vllm-miner/src/vllm_miner/gemm_operators.py`): logs `miner work rate: X TH/s (inner hashes)`
  every 10 s from the vLLM worker via loguru; `status.py` parses it. Disable with MINER_WORK_LOG_INTERVAL=0.
- Log page: `logstream/logstream.py` (stdlib) tails `logs/*.log`, applies `redact.py` rules before
  buffering (creds, tokens, seeds, 64-hex, prl1 addresses, IPs, home dirs; drops request payload
  lines), serves `/`, `/api/lines?after=`, `/api/status`. Page polls every 3 s. Bound to loopback,
  published via `cloudflared tunnel --url` (random trycloudflare.com URL, no SLA).
- Local verification: `ops/h200-solo/test_local.sh` (unit tests + end-to-end smoke of the page).
  Nothing GPU-side has been executed yet; first real run will validate bootstrap timing and the
  vllm CLI flags against vllm 0.20.2.
- Static review (2026-10-29): Pearl HF models carry `quantization_config.quant_method = "pearl"`,
  so `vllm serve` picks the plugin's PearlConfig without a `--quantization` flag. Miner settings
  are left at tested defaults (no pinned-pool override). `pearld --version` prints
  `pearld version X.Y.Z`, which bootstrap greps. install.sh runs fine as root (uses $HOME).
- Still unverified without a GPU: bootstrap build time, vLLM 0.20.2 flag names, loadgen keeping
  avg m >= 1024. Chrome extension was not connected, so the page was only curl-tested.

## Multi-rig log streaming to the site (2026-10-29)

- The monorepo now lives at `kombinacje-alpejskie/pearlstrategy/pearl`, next to the website repo
  `pearlstrategy/pearl-strategy` (Express). The website is the stable origin; rigs push to it.
- `ops/h200-solo/logstream/logstream.py` gained `--push-url/--push-token/--push-interval`
  (`push_loop`): every 3 s POST new redacted lines + status JSON to
  `https://<site>/api/miners/<id>/ingest`; cursor advances only on 2xx; 401/404 logged and retried
  every 60 s. `run.sh` passes the flags when `LOGS_PUSH_URL` and `LOGS_PUSH_TOKEN` are set in `env`.
- Register rigs on the site with `scripts/miners.js register` (needs the site's MINERS_ADMIN_KEY).

## Rig 1 (first live run, started 2026-10-29)

- Vast.ai instance 58269632 on machine 153553 (Colorado, 1x H200 NVL, 300 GB disk, ~$3.68/h,
  15-day max). Account is a teammate's; API key stored at `~/.config/vastai/vast_api_key` on
  this Mac (rotate it after the run). SSH alias `pearl-rig1` in `~/.ssh/config`
  (74.113.137.180:21626, key id_ed25519; proxy alternative ssh2.vast.ai:19904).
- Kit lives at `/workspace/ops` on the box; `env` has the mining address
  prl1pnntjpv02ywdg4h3c8yv7eh7ls6m5jysxlyw3xta80qzgprc2kcvqt68tea (from the local oyster
  wallet, SPV mode, daemon started with nohup; `prlctl --wallet getnewaddress` works while it runs).
- Bootstrap log: `/workspace/bootstrap.log`; stack logs `/workspace/logs/*.log`.
- Gotcha fixed: values with spaces/parentheses in `env` must be quoted (the file is sourced by bash).
- First-run lessons (2026-10-29): (1) `getblocktemplate` SUCCEEDS on a fresh unsynced pearld
  (height 0/1, before or just after peers connect), so it is not a sync signal on its own;
  `run.sh` now waits for >=3 peers and local height >= network tip - 2. The earlier version let
  the gateway+miner start at height 0, the GPU "found" a minimum-difficulty height-1 block and
  pearld accepted it onto a local fork; fix was to wipe `/workspace/pearld` and restart.
  (2) Headers messages carry the ZK certificates (`MsgHeaders` max = header + CertificateMaxSize),
  so header sync is bandwidth-heavy: ~4 MB/s from one sync peer. Block processing ran ~90
  blocks/s on 192 cores -> ~25-40 min to sync 135k blocks; no bootstrap.dat needed.
  (3) `pkill -f "bash /workspace/ops/run.sh"` over ssh kills the ssh shell itself (pattern
  matches its own command line); anchor it: `pkill -f "^bash /workspace/ops/run.sh"`.
  (4) The compiled pearl_gemm extension needs torch/lib on LD_LIBRARY_PATH when imported
  before torch; bootstrap/run.sh export it. Build took ~9 min, model pull ~12 min.
