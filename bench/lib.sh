# shellcheck shell=bash
# Shared helpers for benchmark suites and scenario tests.
# Source after setting ROOT and BIN and enabling strict Bash mode.

# shellcheck source=scripts/lib.sh
. "$ROOT/scripts/lib.sh"

# Run a command in a systemd scope with a memory limit and no swap.
# Drop the page cache first, and keep the caller's environment. A scope that
# the kernel killed stays loaded as failed, and systemd-run refuses its name
# until it is reset.
#
# BENCH_CPUS, a taskset list such as 0-3, pins the command to those CPUs. A
# large machine then runs a cell on the core count of the minimum machine.
# BENCH_USER_SCOPE=1 caps the command in a scope of this user's systemd. Use
# it on a machine with no sudo without a password. That systemd must delegate
# the memory controller. The page cache is not dropped then. A blockcost cell
# copies its seed first, so the files it works on are charged to its scope.
capped() {
    local unit=$1 mem=$2 vars pin=()
    shift 2
    [ -z "${BENCH_CPUS:-}" ] || pin=(taskset -c "$BENCH_CPUS")
    if [ -n "${BENCH_USER_SCOPE:-}" ]; then
        systemctl --user reset-failed "$unit.scope" 2>/dev/null || true
        sync
        systemd-run --user --scope --quiet --unit="$unit" -p MemoryMax="$mem" -p MemorySwapMax=0 "${pin[@]}" "$@"
        return
    fi
    sudo -n systemctl reset-failed "$unit.scope" 2>/dev/null || true

    sync
    # shellcheck disable=SC2024 # the redirect is tee's stdout, not the file
    echo 3 | sudo -n tee /proc/sys/vm/drop_caches >/dev/null
    mapfile -d '' vars < <(env -0)

    sudo -n systemd-run --scope --quiet --unit="$unit" \
        -p MemoryMax="$mem" -p MemorySwapMax=0 \
        --uid="$(id -u)" --gid="$(id -g)" \
        env -i "${vars[@]}" "${pin[@]}" "$@"
}

# Initialize a single-validator chain with pinned genesis programs.
one_validator() {
    local work=$1 chain=$2 svmd=$ROOT/bin/svmd

    "$svmd" init v0 --chain-id "$chain" --home "$work/h0" \
        >"$work/init.log" 2>&1

    "$BIN/nodetool" validators \
        -genesis "$work/h0/config/genesis.json" \
        -pubkey "$("$svmd" comet show-validator --home "$work/h0")" \
        -out "$work/validators.json"

    "$svmd" init coordinator --chain-id "$chain" --home "$work/h0" \
        --overwrite --validators-file "$work/validators.json" \
        >"$work/genesis.log" 2>&1

    "$svmd" add-genesis-programs "$ROOT/genesis" --home "$work/h0"
}
