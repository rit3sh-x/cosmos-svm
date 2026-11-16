# shellcheck shell=bash disable=SC2034 # bench/bench.sh reads about, disk and needs
# Measure how many blocks a committed height stays provable. The scenario is
# bench/scenarios/proof-window.sh.
about="How long a committed height stays provable"
needs=(node)
knob COMMIT 100ms "timeout_commit, so a thousand blocks take minutes"

cells() {
    cell proof-window - env COMMIT="$COMMIT" bench/scenarios/proof-window.sh
}
