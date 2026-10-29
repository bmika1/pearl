#!/usr/bin/env bash
# One-time setup on the rented GPU box. Run as root inside an Ubuntu 24.04 + CUDA 13 container
# (recommended Vast.ai image: nvidia/cuda:13.0.3-devel-ubuntu24.04). Idempotent: re-run safely.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ENV_FILE="${ENV_FILE:-$HERE/env}"
[ -f "$ENV_FILE" ] || { echo "missing $ENV_FILE (copy env.example and fill in)"; exit 1; }
# shellcheck disable=SC1090
set -a; . "$ENV_FILE"; set +a

WORKSPACE="${WORKSPACE:-/workspace}"
PEARL_VERSION="${PEARL_VERSION:-v1.5.5}"
PEARL_REPO="${PEARL_REPO:-https://github.com/pearl-research-labs/pearl.git}"
SRC="$WORKSPACE/pearl"
VENV="$WORKSPACE/venv"
export HF_HOME="${HF_HOME:-$WORKSPACE/hf}"
export PATH="/usr/local/cargo/bin:/usr/local/bin:$PATH"
export RUSTUP_HOME=/usr/local/rustup CARGO_HOME=/usr/local/cargo

log() { printf '\n[bootstrap %s] %s\n' "$(date -u +%H:%M:%S)" "$*"; }

log "preflight"
[ -n "${PEARLD_MINING_ADDRESS:-}" ] || { echo "PEARLD_MINING_ADDRESS is empty"; exit 1; }
case "$PEARLD_MINING_ADDRESS" in prl1p*) ;; *) echo "mining address must be a mainnet Taproot address (prl1p...)"; exit 1;; esac
command -v nvidia-smi >/dev/null || { echo "nvidia-smi not found: not a GPU container"; exit 1; }
CC="$(nvidia-smi --query-gpu=compute_cap --format=csv,noheader | head -1)"
[ "$CC" = "9.0" ] || { echo "GPU compute capability is $CC; pearl-gemm kernels are sm90 (H100/H200) only"; exit 1; }
command -v nvcc >/dev/null || { echo "nvcc not found: use a CUDA *devel* image"; exit 1; }
FREE_GB=$(df -BG --output=avail "$WORKSPACE" | tail -1 | tr -dc '0-9')
[ "$FREE_GB" -ge 200 ] || { echo "only ${FREE_GB} GB free in $WORKSPACE; need >= 200 GB"; exit 1; }
mkdir -p "$WORKSPACE/logs" "$WORKSPACE/pearld" "$HF_HOME"

if [ -z "${PEARLD_RPC_PASSWORD:-}" ]; then
  PW="$(head -c 24 /dev/urandom | base64 | tr -dc 'A-Za-z0-9' | head -c 32)"
  if grep -q '^PEARLD_RPC_PASSWORD=' "$ENV_FILE"; then
    sed -i "s|^PEARLD_RPC_PASSWORD=.*|PEARLD_RPC_PASSWORD=$PW|" "$ENV_FILE"
  else
    echo "PEARLD_RPC_PASSWORD=$PW" >> "$ENV_FILE"
  fi
  log "generated pearld RPC password into $ENV_FILE"
fi
chmod 600 "$ENV_FILE"

log "apt packages"
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y -qq --no-install-recommends \
  git curl ca-certificates build-essential pkg-config libssl-dev clang cmake python3 jq >/dev/null

log "uv"
if ! command -v uv >/dev/null; then
  curl -LsSf https://astral.sh/uv/install.sh | env UV_INSTALL_DIR=/usr/local/bin INSTALLER_NO_MODIFY_PATH=1 sh
fi
uv --version

log "rust toolchain (zk-pow, py-pearl-mining)"
if ! command -v cargo >/dev/null; then
  curl -sSf https://sh.rustup.rs | sh -s -- -y --no-modify-path --profile minimal
fi
cargo --version

log "pearld / prlctl $PEARL_VERSION"
if ! command -v pearld >/dev/null || ! pearld --version 2>/dev/null | grep -q "${PEARL_VERSION#v}"; then
  curl -fsSL "https://raw.githubusercontent.com/pearl-research-labs/pearl/$PEARL_VERSION/install.sh" -o /tmp/pearl-install.sh
  sh /tmp/pearl-install.sh --version "$PEARL_VERSION" --bin-dir /usr/local/bin
fi
pearld --version

log "cloudflared"
if ! command -v cloudflared >/dev/null; then
  curl -fsSL https://github.com/cloudflare/cloudflared/releases/latest/download/cloudflared-linux-amd64 -o /usr/local/bin/cloudflared
  chmod +x /usr/local/bin/cloudflared
fi
cloudflared --version

log "source checkout $PEARL_VERSION -> $SRC"
if [ ! -d "$SRC/.git" ]; then
  git clone --depth 1 --branch "$PEARL_VERSION" --recurse-submodules --shallow-submodules "$PEARL_REPO" "$SRC"
fi
cd "$SRC"
for p in "$HERE"/patches/*.patch; do
  [ -f "$p" ] || continue
  if git apply --check "$p" 2>/dev/null; then git apply "$p"; log "applied $(basename "$p")";
  elif git apply --reverse --check "$p" 2>/dev/null; then log "$(basename "$p") already applied";
  else echo "patch $(basename "$p") does not apply to $PEARL_VERSION; fix or delete it"; exit 1; fi
done

log "python env + CUDA kernel build (this is the slow step: CUTLASS kernels + plonky2 prover)"
export UV_PROJECT_ENVIRONMENT="$VENV" UV_TORCH_BACKEND=cu130 UV_LINK_MODE=copy UV_PYTHON_INSTALL_DIR="$WORKSPACE/uv-python"
uv python install 3.12
MAX_JOBS="$(nproc)" uv sync --package vllm-miner --no-dev
"$VENV/bin/python" -c "import pearl_gemm, vllm_miner, pearl_gateway, pearl_mining; print('miner packages ok, pearl_mining', pearl_mining.__version__)"

log "model download: $MODEL"
"$VENV/bin/python" - <<'PY'
import os
from huggingface_hub import snapshot_download
p = snapshot_download(os.environ["MODEL"], token=os.environ.get("HF_TOKEN") or None)
print("model at", p)
PY

log "bootstrap complete. next: $HERE/run.sh"
