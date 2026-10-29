#!/usr/bin/env bash
# Seed a new rig's chain data from an already-synced rig, run FROM YOUR LAPTOP with two SSH aliases:
#
#   ops/h200-solo/seed-chain.sh pearl-rig1 pearl-rig2
#
# Source rig: pearld is stopped for the ~1 minute it takes to tar the datadir, then restarted with
# the exact command run.sh uses (gateway/vLLM tolerate the gap). Target rig: pulls the tarball
# straight from the source over SSH (a throwaway key is installed for that), extracts it into
# $WORKSPACE/pearld, and is then ready for run.sh, which skips an hour of initial block download.
# Refuses to run if the target already has chain data or pearld running.
set -euo pipefail
SRC="${1:?source ssh alias}"; DST="${2:?target ssh alias}"
WS=/workspace

q() { grep -v 'Welcome to vast\|Have fun' || true; }

echo "== target preflight ($DST)"
ssh "$DST" "set -e; [ ! -s $WS/run/pearld.pid ] || ! kill -0 \$(cat $WS/run/pearld.pid) 2>/dev/null || { echo 'pearld is running on the target, stop it first'; exit 1; }
  [ ! -d $WS/pearld/mainnet ] || { echo 'target already has chain data in $WS/pearld; remove it first'; exit 1; }
  [ -f $WS/ops/env ] || { echo 'target has no $WS/ops/env (run bootstrap first)'; exit 1; }
  [ -f ~/.ssh/id_ed25519 ] || ssh-keygen -q -t ed25519 -N '' -f ~/.ssh/id_ed25519
  cat ~/.ssh/id_ed25519.pub" 2>&1 | q > /tmp/seed-target-key.pub
grep -q ssh-ed25519 /tmp/seed-target-key.pub || { echo "target preflight failed"; cat /tmp/seed-target-key.pub; exit 1; }

echo "== allow target -> source ssh"
ssh "$SRC" "mkdir -p ~/.ssh && chmod 700 ~/.ssh && grep -qxF '$(cat /tmp/seed-target-key.pub)' ~/.ssh/authorized_keys 2>/dev/null || cat >> ~/.ssh/authorized_keys" < /tmp/seed-target-key.pub 2>&1 | q
SRC_HOST=$(ssh -G "$SRC" | awk '/^hostname /{print $2}'); SRC_PORT=$(ssh -G "$SRC" | awk '/^port /{print $2}')

echo "== pack chain on source ($SRC): pearld stops for the duration of the tar"
ssh "$SRC" "set -e; cd $WS; set -a; . ops/env; set +a; . ops/lib.sh
  H=\$(prlctl --notls -s 127.0.0.1:44107 -u \"\$PEARLD_RPC_USER\" -P \"\$PEARLD_RPC_PASSWORD\" getblockcount 2>/dev/null || echo '?'); echo \"source height: \$H\"
  p=\$(cat run/pearld.pid 2>/dev/null || true)
  if [ -n \"\$p\" ] && kill -0 \$p 2>/dev/null; then kill \$p; for i in \$(seq 1 60); do kill -0 \$p 2>/dev/null || break; sleep 1; done; echo 'pearld stopped'; fi
  rm -f chain-seed.tar; tar -cf chain-seed.tar -C $WS pearld --exclude=pearld/logs; ls -lh chain-seed.tar | awk '{print \"tarball:\", \$5}'
  nohup \$(pearld_cmd) >> logs/pearld.log 2>&1 & echo \$! > run/pearld.pid; echo 'pearld restarted'" 2>&1 | q

echo "== pull tarball to target and extract"
ssh "$DST" "set -e; cd $WS; scp -q -o StrictHostKeyChecking=accept-new -P $SRC_PORT root@$SRC_HOST:$WS/chain-seed.tar . && tar -xf chain-seed.tar && rm chain-seed.tar && du -sh pearld && echo 'chain seeded; run.sh will start pearld on it'" 2>&1 | q
ssh "$SRC" "rm -f $WS/chain-seed.tar" 2>&1 | q
echo "== done"
