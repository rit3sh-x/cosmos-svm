#!/usr/bin/env bash
# Measure how many blocks a committed height stays provable. This is the cell
# of the proof-window suite, which sets COMMIT.
#
# One validator runs on the shipped pruning and snapshot settings, past the
# first snapshot interval. The script asks for a bank-store proof at every
# depth from the tip down. It probes before the first snapshot, after it, and
# after a restart, because the pruning manager treats a restarted store
# differently. Each probe-*.txt line ends in ok or refused. bench/bench.sh
# report summarizes them.
set -euo pipefail
# shellcheck source=bench/lib.sh
. "$ROOT/bench/lib.sh"
WORK=$(mktemp -d "$DATA/proof-window.XXXX")
H=$WORK/h0
trap 'stop_pidfile "$WORK/svmd.pid"' EXIT

one_validator "$WORK" svm-proof-window
"$BIN/nodetool" set "$H/config/config.toml" timeout_commit="\"$COMMIT\""
"$BIN/nodetool" set "$H/config/app.toml" min-retain-blocks=2000 index-max-bytes=1073741824
grep -E '^(pruning|pruning-keep-recent|pruning-interval|snapshot-interval|snapshot-keep-recent|min-retain-blocks) =' \
    "$H/config/app.toml" >"$OUT/settings.txt"

start() {
    "$ROOT/bin/svmd" start --home "$H" >>"$OUT/svmd.log" 2>&1 &
    echo $! >"$WORK/svmd.pid"
}
tip() { "$BIN/nodetool" height 2>/dev/null || echo 0; }
at_least() { [ "$(tip)" -ge "$1" ]; }
until_height() { wait_for 3600 "height $1" at_least "$1"; }
proven() {
    if "$BIN/nodetool" proven -path /store/bank/key -data 0x01 -height "$1" 2>/dev/null; then echo ok; else echo refused; fi
}
# Measure each depth from a tip read right before its query, so a block that
# lands mid-probe does not shift the answer. Then probe a few fixed old heights.
probe() {
    local depth t h
    for depth in $(seq 0 12); do
        t=$(tip)
        h=$((t - depth))
        echo "depth $depth height $h tip $t $(proven "$h")"
    done
    for h in 1 2 100 500 998 999 1000 1001; do
        if [ "$h" -lt "$(tip)" ]; then echo "height $h $(proven "$h")"; fi
    done
}

start
until_height 500
probe >"$OUT/probe-before-snapshot.txt"
# The node takes the first snapshot at height 1000. Pruning waits for that
# snapshot to complete.
until_height 1100
probe >"$OUT/probe-after-snapshot.txt"
last=$(tip)
stop_pidfile "$WORK/svmd.pid"
start
until_height $((last + 20))
probe >"$OUT/probe-after-restart.txt"
