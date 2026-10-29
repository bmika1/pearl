# H200 solo mining kit with a public, redacted live log

Self-hosted ("non-pooled") Pearl mining on one rented Hopper GPU, with a web page that
streams the stack's logs to anyone you give the link to. Credentials, the mining address,
peer IPs and request payloads are masked on the box before a byte leaves it. Page lag is
about 3 seconds for logs and 15 seconds for the status cards.

What runs on the box, in start order:

| process   | what                                                                     | log            |
|-----------|--------------------------------------------------------------------------|----------------|
| logstream | tails `logs/*.log`, redacts, serves the page on 127.0.0.1:8080           | logstream.log  |
| tunnel    | `cloudflared` quick tunnel: public `https://<random>.trycloudflare.com`  | tunnel.log     |
| status    | every 15 s: node height, GPU, work rate, blocks, process liveness        | status.log     |
| pearld    | full node, loopback RPC, `--miningaddr` set to your address              | pearld.log     |
| gateway   | `pearl-gateway`: block templates to the miner, ZK proof + submit on a hit | gateway.log    |
| vllm      | `vllm serve` with the Pearl plugin: the GEMMs inside it are the mining    | vllm.log       |
| loadgen   | keeps long prompts in flight so every forward pass is >= 1024 tokens     | loadgen.log    |
| watchdog  | restarts light processes; reports pearld/vLLM deaths                     | status.log     |

Expectation management: one H200 is about 0.001% of the network. Expected time to a solo
block is roughly 215 days. The page will show the node syncing, the GPU hashing at
~700 TH/s, and, almost certainly, no block. That is the honest demo.

## 0. What you prepare locally (before renting)

1. **Wallet, on your own machine, never on the box.**
   ```bash
   curl -fsSL https://raw.githubusercontent.com/pearl-research-labs/pearl/master/install.sh | sh
   oystercli            # create wallet -> write down the seed -> start daemon -> Receive -> new address
   ```
   You need one mainnet Taproot address (`prl1p...`). Rewards go there; the seed stays with you.
2. Copy `env.example` to `env`, set `PEARLD_MINING_ADDRESS`. Leave `PEARLD_RPC_PASSWORD` empty; the
   bootstrap generates one. Pick `MODEL` (70B is the real thing; 8B gets hashing in minutes for a test).
3. Optional: run the local checks once (no GPU needed):
   ```bash
   ops/h200-solo/test_local.sh
   ```

## 1. Rent

Vast.ai filters: 1x H200 (SXM preferred over NVL), **disk >= 300 GB**, max duration >= 1 month,
reliability >= 99%, download >= 1 Gbps. Template settings:

- Image: `nvidia/cuda:13.0.3-devel-ubuntu24.04` (must be a *devel* image: the kernels are compiled on the box)
- Launch mode: SSH. No port mappings needed; the page is published through the tunnel.
- Disk: 300 GB or more (chain + 73 GB model + build artifacts).

If the chosen image lacks `/workspace`, set `WORKSPACE` in `env` to the persistent path Vast shows.

## 2. Install on the box

```bash
scp -r ops/h200-solo root@<box>:/workspace/ops          # includes your filled-in env
ssh root@<box>
bash /workspace/ops/bootstrap.sh                          # 20-45 min, mostly CUDA/Rust compile + model pull
```

The bootstrap: apt deps, uv, Rust, `pearld`/`prlctl` release binaries, cloudflared, a clone of the
repo at `PEARL_VERSION`, applies `patches/`, builds the miner (`uv sync --package vllm-miner`), and
pre-downloads the model. It refuses to run on a non-sm90 GPU or with < 200 GB free.

## 3. Run

```bash
bash /workspace/ops/run.sh
```

It prints the public URL once the tunnel is up (also: `grep -o 'https://.*trycloudflare.com' /workspace/logs/tunnel.log`).
Share that link. Then it waits for pearld to finish initial block download (this is the long part on
a fresh box: budget 30-90 min; the page shows height climbing), starts the gateway, vLLM, and the
load generator. Within a minute of vLLM being ready you should see in `vllm.log`:

```
miner work rate: 7xx.xx TH/s (inner hashes) | ... | NNN noisy GEMMs in 10.0s | avg m=...
```

If `avg m` is below 1024 the miner is not hashing: raise `LOADGEN_PROMPT_TOKENS` or `LOADGEN_CONCURRENCY`.

Useful:

```bash
python3 /workspace/ops/status.py --env /workspace/ops/env        # one-line status
tail -f /workspace/logs/vllm.log | grep -E 'work rate|Block found'
tail -f /workspace/logs/gateway.log                              # "Block accepted by node!" is the jackpot
bash /workspace/ops/stop.sh                                      # stop everything, keep data
```

## 4. Teardown

`stop.sh`, then destroy the instance in Vast. Nothing on the box can spend funds: it only ever had
the public address. Rotate nothing. If you re-rent, the bootstrap is idempotent.

## Adding a rig without the hour of chain sync

After `bootstrap.sh` on the new rig and before its `run.sh`, seed its chain from a rig that is
already synced (run from your laptop; both are SSH aliases):

```bash
ops/h200-solo/seed-chain.sh pearl-rig1 pearl-rig2
```

The source's pearld stops for about a minute while its data directory is tarred, then restarts;
the target pulls the tarball directly from the source and extracts it. `run.sh` on the target then
only has to catch up the few blocks mined in between.

## Streaming to the Pearl Strategy site (multiple rigs)

Instead of (or as well as) the quick tunnel, every rig can push its redacted lines to the
website, which shows all rigs on `/miners` with a switcher. On the site:

```bash
MINERS_ADMIN_KEY=... SITE_URL=https://<site> node scripts/miners.js register --name "H200 Georgia" --gpu "1x H200 SXM" --provider vast.ai
```

Copy the printed `LOGS_PUSH_URL` and `LOGS_PUSH_TOKEN` into this rig's `env`, set
`PUBLISH_TUNNEL=0` if you do not want the tunnel too, and start `run.sh`. The push uses the same
redacted buffer the local page serves; nothing leaves the box that the local page would not show.
Each rig has its own token, so a leaked token only lets someone write to that rig's log.

## Operational notes

- **Quick tunnels have no SLA.** The URL changes every restart and Cloudflare can drop it. For a
  stable link, use a named tunnel (`cloudflared tunnel login` on the box, then route a hostname) or
  map port 8080 in the Vast template and set `--bind 0.0.0.0` in `run.sh`. The page is read-only
  either way.
- **The prebuilt GHCR miner image is stale** (tagged v1.1.0, before the salted-seed fork at block
  99,000). Miners built from it produce invalid shares, hence the source build.
- **Mining only happens in forward passes with m >= 1024** (`vllm_miner/config.yaml`). An idle vLLM
  mines nothing. The load generator sends ~3000-token prompts with `max_tokens=1` so the GPU does
  prefill almost exclusively; prefix caching is disabled so no work is skipped.
- **The gateway errors until the node is synced**, so `run.sh` waits on `getblocktemplate`
  succeeding before starting it.
- **Work-rate unit.** `inner_hash_per_matmul` counts the hash-tile int8 operations the kernel
  commits to; pools call the same thing "hashes". Treat the TH/s figure as comparable to pool
  dashboards within ~10%.
- **Redaction** rules live in `logstream/redact.py` with tests. Lines that match `DROP` never enter
  the buffer (request payloads, health checks). Add a rule before adding a new log source.

## Files

```
env.example        settings template (copy to env)
bootstrap.sh       one-time install + build on the box
run.sh / stop.sh   start/stop the stack
status.py          status snapshot (JSON for the page, one-liner for the log)
loadgen.py         synthetic prefill load (stdlib only)
logstream/         tailer + redactor + HTTP server + page
patches/           miner patch: periodic "miner work rate" log line
test_local.sh      local tests: redaction unit tests + page smoke test
```
