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
- RIG 1 FIRST HASHES (2026-10-29 18:15 UTC): node synced ~2h after start (one 9-min peer stall,
  one 2-min self-inflicted pearld stop). vLLM 70B loaded in ~3 min. Work rate steady at
  ~270 TH/s (= 270 T int8-MAC/s; 1 "inner hash" == 1 MAC in our accounting), avg m ≈ 15k tokens
  per noisy GEMM (max_num_batched_tokens 16384 is the cap), GPU 99 % @ 583 W, loadgen 2.1 req/s,
  ~6.1k prompt tok/s. That is ~40 % of the ~700 TH/s pools report for an H200 NVL: pool miners run
  bare GEMMs; the vLLM path also pays attention, norms, activation quantisation, per-GEMM tensor
  hashing of A and B, and noising kernels. Expected solo yield at this rate: ~0.0017 blocks/day.
  Idea to test on rig 2: smaller model (Llama-3.1-8B-pearl) may give more MAC/s (less non-GEMM overhead).
- RIG 2 (Japan, instance 58284662, alias pearl-rig2 144.79.51.1:50088): bootstrap 6 min, chain
  seeded from rig 1 (9.3 GB tar, ~5 min total), hashing 272 TH/s on 70B = same as rig 1.
  Model comparison on rig 2: Llama-3.1-8B-pearl = 208 TH/s (2300 GEMMs/10 s, 44.7k prompt tok/s)
  vs 70B = 272 TH/s. Bigger GEMMs win; keep 70B. Next lever tested: MAX_NUM_BATCHED_TOKENS 32768.
- Vast SSH gotcha: keys attached via API after creation may never land in the container (rig 2's
  first instance). Reliable fix: on-start script that appends the pubkey to /root/.ssh/authorized_keys
  (`vastai create instance <offer> --template_hash ... --onstart-cmd ...`). `vastai destroy instance`
  prompts and aborts non-interactively; use the API DELETE instead.
- Batch cap test on rig 2: MAX_NUM_BATCHED_TOKENS 32768 + LOADGEN_CONCURRENCY 24 -> 274-282 TH/s (avg m ~22k),
  ~+2 % over 16k. Adopted as default on both rigs and in env.example.

## Pool mode switch (2026-10-29 19:30 UTC)

- Both rigs now run `ops/h200-solo/run-pool.sh`: WildRig Multi 0.51.8, `--algo pearlhash`, Pearlhash
  pool, worker = rig id, payouts to the treasury address. Rig 1 ~681 TH/s, rig 2 ~703 TH/s, shares
  accepted within a minute, est ~9.3-9.6 PRL/day each (1 % fee). Solo stack stopped (logs kept as
  `*.solo.old`); switch back with `MODE=solo` + `run.sh` (chain data still on disk).
- Oyster Pool is NOT usable: its installer downloads from github.com/oysterpoolcom/pool which 404s.
  pearlhash/pearlhash-miner is "legacy" with no assets. SRBMiner/BzMiner never name sm90.
  WildRig's notes name H20/H100/H200/H800 explicitly and it ran first try on sm90.
- Pearlhash pool API: only `https://pearlhash.xyz/api/stats` (pool hashrate ~15.6 EH/s); no per-miner
  endpoint found. Network hashrate for the pool-mode estimate = Blockbook difficulty x 1.44e12
  (empirical ratio from our node's getmininginfo), matches trackers (7.13e19).
- status.py pool mode parses WildRig's ANSI-coloured output (logstream strips ANSI now).

## 2026-10-29 21:15 UTC — rigs stopped (not destroyed)
- Ran `stop.sh` + killed leftover wildrig/status loops on both rigs, then `vastai stop instance 58269632` (Colorado) and `58284662` (Japan). `cur_state=stopped`, `intended_status=stopped`; `actual_status` lags a minute.
- Stopped instances still bill storage: 300 GB × $0.20/GB/month ≈ $2.00/day each ($4/day for both). GPU rent ($3.68 + $4.08/h) stops.
- Restart: `vastai start instance <id>` then `bash /workspace/ops/run-pool.sh` on each. Restart can fail if the host has rented the GPU to someone else meanwhile; then destroy + recreate (pool mode needs no model pull, ~15 min).
- At shutdown: 102 and 117 accepted shares on Pearlhash, treasury still 0 PRL (no payout reached the 1 PRL minimum). Pool `/api/stats` was returning empty from the Mac, so pending balance could not be read; it is credited to the address and pays out once ≥1 PRL.
- Site: both rig tabs will show stale status ("last seen" grows); nothing to change.
- 21:20 UTC: both instances destroyed via API DELETE (`{"success": true}` each); `vastai show instances` is empty. No Vast billing remains. Ops kit, env files and site registrations are untouched; new rigs need `bootstrap.sh` (pool mode: skip model pull) and a fresh push token only if the rig id changes.

## 2026-10-30 13:30 UTC — rig 1 re-created in France (pool only)
- Offer 57954849 → instance 58407406, machine 152607, H200 NVL, France, $4.00/h, 100 GB disk
  (~$0.67/day storage if stopped), reliability 0.97, 6.9 Gbps. Cheapest H200 that morning was
  Washington at $3.98/h with reliability 0.80 — skipped. ssh: `pearl-rig1` alias = ssh2.vast.ai:31932.
- Pool-only bring-up takes ~3 min: no bootstrap.sh. `apt-get install python3 curl tar procps`,
  scp ops kit + `env` (MODE=pool, PUBLISH_TUNNEL=0, POOL_WORKER=rig-1), `nohup bash run-pool.sh`.
  Local copy of the env is `ops/h200-solo/env.rig3` (gitignored).
- Site registry: removed old rig-1/rig-2, re-registered `rig-1` as "Rig 1 · H200 France" with a new
  token. Push URL now uses the custom domain: https://www.pearlstrategy.org/api/miners/rig-1/ingest
  (pearlstrategy.org 308-redirects to www; the apex would break POSTs, always use www).
- First lines reached the site within 10 s of start; status shows mode=pool, GPU, network height.
- 14:50 UTC: France rig stopped cleanly (87 accepted shares, 0 rejected over ~80 min) and instance
  58407406 destroyed via API (`{"success": true}`). No Vast instances remain. Site registry still
  has `rig-1` (token valid, now offline); reuse it for the next rig by putting the same
  LOGS_PUSH_URL/TOKEN from `ops/h200-solo/env.rig3` into the new box's env.

## 2026-11-05 12:35 UTC — Vast.ai Hopper price check (no instances running)

- Chain: height 138747, difficulty 53.1M -> network ~7.65e19 H/s (diff x 1.44e12), block reward 2193.6 PRL,
  Pearlhash pool 14.2 EH/s. Pool-mode yield per card at 1 % fee: H200 SXM (742 TH/s) 9.4 PRL/day,
  H200 NVL (690 measured on our rigs) 8.7, H100 SXM (610) 7.7, H100 NVL (~560, unmeasured) 7.1,
  H100 PCIe (~500, unmeasured) 6.3. PRL price not fetched (CoinGecko/hashrate.no block curl).
- `vastai search offers 'num_gpus=1 gpu_name in [H200,H200_NVL,H100_SXM,H100_NVL,H100_PCIE,...]
  rentable=true verified=any disk_space>=60 reliability>=0.90 duration>=7' -o dph+ --raw` -> 23 offers.
  Cheapest by $/PRL: 56833416 H100 NVL Japan $2.745/h (unverified host, rel 0.986, 60 d) ≈ $9.3/PRL;
  58013808 H100 NVL Bulgaria $2.80 (deverified) ≈ $9.5; 28418866 H200 NVL Texas $3.535 (verified,
  rel 0.975, 60 d) ≈ $9.7; 59249684 H200 SXM New Jersey $3.818 (verified, rel 0.951, 34 d, 7.7 Gbps)
  ≈ $9.8; 57219147 H200 NVL Colorado $3.60 = our original rig-1 machine 153553, but only 9 d left.
  Verified H100 SXM start at $3.30 (Australia) ≈ $10.3/PRL; H100 PCIe Czechia $2.85 ≈ $10.8/PRL.
- Picks for the next two rigs: 28418866 (Texas H200 NVL) and 59249684 (New Jersey H200 SXM): verified
  hosts, known card class, ≥34-day windows. H100 NVL offers are cheaper per hour but the host is
  unverified and WildRig throughput on NVL is unmeasured. Not created yet (needs go-ahead).

## 2026-11-05 12:45 UTC — rig 1 re-created in New Jersey (pool only)
- Texas offer 28418866 was gone by the time of `create` (no_such_ask). Offer 59249684 → instance
  59264907, machine 156047, 1x H200 SXM (not NVL), New Jersey, $3.98/h incl. 100 GB disk, rel 0.95,
  34-day window. Created with `vastai create instance <offer> --image nvidia/cuda:13.0.3-devel-ubuntu24.04
  --disk 100 --ssh --direct --label pearl-rig-1 --onstart-cmd "<append ~/.ssh/id_ed25519.pub to
  /root/.ssh/authorized_keys>" --env '-p 8080:8080' --cancel-unavail`. SSH alias `pearl-rig1` =
  ssh2.vast.ai:18456 (direct port was not assigned; proxy works).
- Deploy helper: scratchpad `deploy-rig.sh <ssh-port> <env-file>` = tar the kit to /workspace/ops,
  scp env, apt-get python3/curl/tar/procps, `nohup bash run-pool.sh`. Up and hashing 60 s after start.
  Env is `ops/h200-solo/env.rig4` (gitignored) = env.rig3 with the New Jersey title; still POOL_WORKER
  and push URL `rig-1`, so the site tab is the old "Rig 1 · H200 France" registration (rename needs
  the production MINERS_ADMIN_KEY, which is not on this Mac; the previous session had the user run it).
- 12:51 UTC: WildRig 735 TH/s on the H200 SXM (vs 681-703 on NVL), 688 W, shares accepted from the
  first minute, status loop estimates 9.3 PRL/day. status.py already reports the treasury address
  balance via Blockbook: 1.054 PRL received in 1 tx = the first Pearlhash payout from the October runs.
- Second rig NOT created: `vastai create instance 58281236` (H200 NVL Ukraine $3.336/h, rel 0.972,
  88 d) was blocked by the Claude Code auto-mode permission classifier. Fallback offers at that
  moment: 56980961 H200 NVL Washington $3.474 rel 0.994 167 d; 57391418 H200 NVL Colorado $3.60.
  Registering it on the site as `rig-2` needs the production MINERS_ADMIN_KEY.
- 12:53 UTC: second create approved and run: offer 58281236 → instance 59265450, machine 155567,
  1x H200 NVL, Ukraine, $3.39/h incl. disk, rel 0.972, 88-day window. SSH alias `pearl-rig2` =
  ssh8.vast.ai:34900 (note: ssh8, not ssh2; deploy-rig.sh takes the host as 3rd arg). Env
  `ops/h200-solo/env.rig5` = POOL_WORKER=rig-2, LOGS_PUSH_URL/TOKEN empty (not registered on the
  site yet; run-pool.sh skips the push when they are empty, logstream stays loopback-only).
- 12:57 UTC: rig-2 at 668 TH/s / 575 W, 3 shares ok / 0 rej; rig-1 (NJ H200 SXM) 733 TH/s / 689 W,
  4 ok / 0 rej. Combined est ~17.7 PRL/day for $7.37/h ≈ $177/day ≈ $10/PRL.
- Labels fixed in Vast: 59264907 = pearl-rig-1, 59265450 = pearl-rig-2.
- 13:08 UTC: rig-2 registered on the site (`scripts/miners.js register --id rig-2 --name "Rig 2 · H200 NVL
  Ukraine" --gpu "1x H200 NVL"`), token in env.rig5, logstream restarted with push args (kill the
  logstream pid, re-run run-pool.sh; it only starts what is not alive). rig-1 renamed via
  `PATCH /api/miners/rig-1` to "Rig 1 · H200 New Jersey" / "1x H200 SXM". Both show online on
  https://www.pearlstrategy.org/miners (rig-1 ~728 TH/s, rig-2 ~608-668 TH/s).
- The production `MINERS_ADMIN_KEY` is now saved on this Mac as `MINERS_ADMIN_KEY_PROD` in the
  gitignored `pearl-strategy/.env` (the same value is in the Vercel project's Production env vars on the
  queady account). Use it with `MINERS_ADMIN_KEY="$(sed -n 's/^MINERS_ADMIN_KEY_PROD=//p' .env)"
  SITE_URL=https://www.pearlstrategy.org node scripts/miners.js ...`. The local `MINERS_ADMIN_KEY=`
  line in that file is still the dev key for localhost. Don't confuse the two.

## 2026-11-05 15:40 UTC — both rigs stopped and destroyed
- Ran ~2h50m (12:50 → 15:40 UTC). Final: rig-1 (NJ H200 SXM) 727 TH/s, 203 shares ok / 0 rej;
  rig-2 (Ukraine H200 NVL) had drifted down to 590 TH/s at 514 W (host seems power-limited; it was
  668 TH/s / 575 W at start), 177 ok / 3 rej. Treasury still 1.054 PRL: today's shares are credited at
  Pearlhash and pay out once the pending balance reaches 1 PRL.
- `stop.sh` on both, then API DELETE on instances 59264907 and 59265450 (`{"success": true}` each);
  `vastai show instances` is empty, no Vast billing remains. Spend ≈ $21 (2.83 h × $7.37/h).
- Site registrations rig-1 / rig-2 and their tokens stay valid (env.rig4 / env.rig5 locally); SSH
  aliases pearl-rig1/pearl-rig2 now point at dead ports. Next rigs: `vastai search offers` as above,
  create with the on-start pubkey command, `deploy-rig.sh <port> env.rigN <ssh-host>`, done in ~3 min.
