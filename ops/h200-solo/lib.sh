#!/usr/bin/env bash
# Shared helpers for run.sh / seed-chain.sh on the box. Source after loading env.

# The exact pearld command line, so a restart from any script is identical to run.sh's.
pearld_cmd() {
  echo pearld \
    --datadir="$WORKSPACE/pearld" --logdir="$WORKSPACE/pearld/logs" \
    --notls --rpclisten=127.0.0.1:44107 \
    --rpcuser="$PEARLD_RPC_USER" --rpcpass="$PEARLD_RPC_PASSWORD" \
    --miningaddr="$PEARLD_MINING_ADDRESS" --debuglevel=info
}
