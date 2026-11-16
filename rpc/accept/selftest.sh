#!/usr/bin/env bash
# Tests the app-hash check of the four-validator harness against machines that
# disagree. It points agree from cluster.sh, which consensus.sh runs, at three
# stub CometBFT RPCs. Two machines that match must agree. A third machine with
# a different app hash at height 2 must stop the harness, with the height and
# the assertion's name. It needs no chain, so the lint job runs it.
set -euo pipefail
cd "$(dirname "$0")"
ROOT=$(cd ../.. && pwd)
WORK=$(mktemp -d)
# shellcheck source=rpc/accept/lib.sh
. ./lib.sh
# shellcheck source=rpc/accept/cluster.sh
. ./cluster.sh
trap 'stop_pidfile "$WORK/stubs.pid"; rm -rf "$WORK"' EXIT

spawn stubs python3 "$PWD/selftest_stub.py" "$WORK/ports"
wait_for 30 "the stub RPCs to start" test -s "$WORK/ports"
read -r ONE TWO DIVERGED <"$WORK/ports"

# Here a machine index is a stub's port. agree stops the harness with exit, so
# each run is a subshell. A line after the run shows if the harness continued.
cmt() { echo "http://127.0.0.1:$1"; }

out=$( (agree 1 3 "two that match" "$ONE" "$TWO") 2>&1) || {
    echo "two machines that match did not agree:" >&2
    echo "$out" >&2
    exit 1
}
grep -q 'two that match: agree: 2 machines, 3 heights' <<<"$out" || { echo "unexpected: $out" >&2; exit 1; }

status=0
out=$( (agree 1 3 "a divergence" "$ONE" "$TWO" "$DIVERGED"; echo "the harness carried on") 2>&1) || status=$?
if [ "$status" -eq 0 ] || grep -q 'the harness carried on' <<<"$out" ||
    ! grep -q 'height 2' <<<"$out" || ! grep -q 'a divergence: the machines do not agree' <<<"$out"; then
    echo "a divergent app hash did not stop the harness by name (status $status):" >&2
    echo "$out" >&2
    exit 1
fi
echo "agree: a divergent app hash stops the harness at height 2, by name"
echo PASS
