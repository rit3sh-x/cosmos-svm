#!/usr/bin/env bash
# Benchmark runner. See bench/README.md.
# Usage:
#   bench/bench.sh list [SUITE]
#   bench/bench.sh run [--data DIR] [--out DIR] SUITE [CELL-REGEX] [KNOB=VALUE...]
#   bench/bench.sh smoke [--data DIR]
#   bench/bench.sh report RUN
#   bench/bench.sh compare RUN-A RUN-B [--keys REGEX]
#
# Each suite in bench/suites/ names its builds and its benchmark cells. A run
# does the builds, runs the matching cells, and saves logs, results and
# hardware data under bench/results/SUITE-UTC. A failed cell does not stop the
# other cells.
#
# --data sets the scratch directory (default: BENCH_DATA, else a new temporary
# directory). --out adds to an existing run that is not committed.
set -euo pipefail
ROOT=$(cd "$(dirname "$0")/.." && pwd)
# shellcheck source=bench/lib.sh
. "$ROOT/bench/lib.sh"

usage() {
    sed -n '4,8p' "$0" >&2
    exit 2
}

# Load a suite, set its defaults, and check its knobs.
load() {
    [ -f "$ROOT/bench/suites/$1.sh" ] || die "no suite $1; bench/bench.sh list names them"
    about='' disk='' needs=() knobs=() knob_lines=()
    unset -f cells
    machine() { :; }
    # shellcheck source=/dev/null
    . "$ROOT/bench/suites/$1.sh"
    local k
    for k in "${!given[@]}"; do
        [[ " ${knobs[*]} " == *" $k "* ]] || die "suite $1 has no knob $k"
    done
}

# Declare a configurable suite parameter.
knob() {
    knobs+=("$1")
    knob_lines+=("$(printf '%-8s %-8s %s' "$1" "$2" "$3")")
    printf -v "$1" '%s' "${given[$1]-$2}"
}

# Build the binaries that a suite needs.
build() {
    local name
    for name in "$@"; do
        echo "== build $name"
        case $name in
        storage) (cd bench/storage && go build -o "$BIN/storagebench" .) ;;
        blockcost) make engine && go build -o "$BIN/blockcost" ./bench/blockcost ;;
        engine-tests) make fetch-programs && cargo test --release --locked --no-run ;;
        ffi) make engine && go test -count=1 -run '^$' ./ffi ;;
        svm) make fetch-programs && cargo build --release --locked --manifest-path bench/svm/Cargo.toml ;;
        criterion) make fetch-programs && cargo bench --locked --manifest-path bench/svm/Cargo.toml --no-run ;;
        node) make build && go build -o "$BIN/fill" ./rpc/accept/fill && go build -o "$BIN/nodetool" ./tools/nodetool ;;
        *) echo "no build named $name" && false ;;
        esac || return
    done
}

# List, count, or run a matching benchmark cell.
cell() {
    local name=$1 cap=$2
    shift 2
    [[ $name =~ $filter ]] || return 0
    case $pass in
    list) printf '  %-22s %-4s %s\n' "$name" "$cap" "$*" ;;
    plan)
        planned=$((planned + 1))
        [ "$cap" = - ] || need_sudo=1
        ;;
    run) run_cell "$name" "$cap" "$@" ;;
    esac
}

# Run an account-store benchmark and save its per-block results.
store_cell() {
    local name=$1
    shift
    cell "$name" - "$BIN/storagebench" -mode store -dir "$DATA" -series "$OUT/series" "$@"
}

# Run a block-cost benchmark, with 3 GiB of ballast if the cell is memory-capped.
# That is what a 1.5 GiB program cache budget keeps resident after two maximum
# blocks. A later -ballast-gib in the arguments wins, as a later -label does, for
# a cell that repeats a run made with 4 GiB.
blockcost_cell() {
    local name=$1 cap=$2 ballast=()
    shift 2
    [ "$cap" = - ] || ballast=(-ballast-gib 3)
    cell "$name" "$cap" "$BIN/blockcost" -dir "$DATA" -label "$name" "${ballast[@]}" "$@"
}

# Run a cell, collect system metrics, and record its result.
run_cell() {
    local name=$1 cap=$2 log=$OUT/$1.txt t0=$SECONDS mon=() line
    shift 2
    echo "== $name"
    { printf '# cell %s, cap %s, command:' "$name" "$cap" && printf ' %q' "$@" && echo; } >"$log"
    if command -v iostat >/dev/null; then
        iostat -dxmt 10 >"$OUT/$name.iostat" 2>&1 &
        mon+=($!)
        vmstat -t 10 >"$OUT/$name.vmstat" 2>&1 &
        mon+=($!)
    fi
    [ "$cap" = - ] || set -- capped "bench-$name" "$cap" "$@"
    if "$@" >>"$log" 2>&1; then
        line="ok     $name $((SECONDS - t0))s $(grep '^exp=' "$log" | tail -1 || true)"
    else
        line="FAILED $name $((SECONDS - t0))s $(tail -1 "$log")"
        failed=1
    fi
    if [ ${#mon[@]} -gt 0 ]; then kill "${mon[@]}" 2>/dev/null || true; fi
    echo "$line" | tee -a "$OUT/summary.txt"
}

# Record hardware, tool versions, revision, and program hashes.
record_machine() {
    uname -a
    lscpu
    free -g
    df -h "$DATA"
    lsblk -o NAME,SIZE,TYPE,MOUNTPOINT,MODEL
    go version
    rustc --version
    echo "revision $(git describe --always --dirty 2>/dev/null || echo unknown)"
    sha256sum programs/*.so
    machine
}

# List available suites or cells.
list() {
    local f
    if [ $# -eq 0 ]; then
        for f in "$ROOT"/bench/suites/*.sh; do
            f=$(basename "$f" .sh)
            load "$f"
            printf '%-18s %s\n' "$f" "$about"
        done
        return
    fi
    load "$1"
    echo "$about"
    [ -z "$disk" ] || echo "measures the disk under --data"
    [ ${#knob_lines[@]} -eq 0 ] || printf 'knob %s\n' "${knob_lines[@]}"
    DATA='<data>' BIN='<data>/bin' OUT='<run>' pass=list
    cells
}

# Check, build, and run a benchmark suite.
run() {
    local out='' first='' arg k values=''
    DATA=${BENCH_DATA:-}
    while [[ ${1:-} == --* ]]; do
        case $1 in
        --data) DATA=$2 ;;
        --out) out=$2 ;;
        *) usage ;;
        esac
        shift 2
    done
    # Resolve paths from the caller's directory, because every cell runs from
    # the repository root.
    [ -z "$DATA" ] || DATA=$(realpath -m "$DATA")
    [ -z "$out" ] || out=$(realpath -m "$out")
    cd "$ROOT"
    [ $# -gt 0 ] || usage
    local suite=$1
    shift
    for arg in "$@"; do
        if [[ $arg == *=* ]]; then given[${arg%%=*}]=${arg#*=}; else filter=$arg; fi
    done
    load "$suite"

    # Refuse a bad run here, before the build and the first cell. A later
    # refusal would come hours into the run, at the first capped cell.
    if [ -z "$DATA" ]; then
        [ -z "$disk" ] || die "$suite measures the disk under --data DIR; name a directory on that disk"
        DATA=$(mktemp -d "${TMPDIR:-/tmp}/bench-$suite.XXXX")
    fi
    if [ -n "$out" ]; then
        OUT=$out
        read -r first 2>/dev/null <"$OUT/summary.txt" || true
        [[ $first == '# bench.sh run'* ]] || die "$out is not a run of bench/bench.sh, and a baseline is never rewritten"
        [ -z "$(git ls-files "$OUT" 2>/dev/null)" ] || die "$out is committed, and a baseline is never rewritten"
    else
        OUT=$ROOT/bench/results/$suite-$(date -u +%Y%m%dT%H%M%SZ)
    fi
    BIN=$DATA/bin
    pass=plan planned=0 need_sudo=''
    cells
    [ "$planned" -gt 0 ] || die "no cell of $suite matches $filter"
    if [ -n "$need_sudo" ]; then
        command -v systemd-run >/dev/null || die "$suite has capped cells, which need systemd-run"
        [ -n "${BENCH_USER_SCOPE:-}" ] || sudo -n true 2>/dev/null ||
            die "$suite has capped cells, which need sudo without a password, or BENCH_USER_SCOPE=1"
    fi

    mkdir -p "$OUT" "$BIN"
    # Put the cells' files on the disk under test, os.TempDir() included.
    export ROOT OUT DATA BIN TMPDIR=$DATA
    trap 'jobs -p | xargs -r kill 2>/dev/null || true' EXIT
    for k in "${knobs[@]}"; do values+=" $k=${!k}"; done
    echo "# bench.sh run $suite cells=$filter knobs=${values# }" >>"$OUT/summary.txt"
    # The build downloads the programs that record_machine hashes.
    build "${needs[@]}" >>"$OUT/build.txt" 2>&1 || die "build failed; see $OUT/build.txt"
    { date -u && record_machine; } >>"$OUT/hardware.txt" 2>&1 || true
    pass=run failed=''
    cells
    echo "results: $OUT"
    python3 bench/report.py "$OUT" || echo "the report failed; the run is in $OUT" >&2
    [ -z "$failed" ] || die "a cell failed; see $OUT/summary.txt"
}

declare -A given=()
filter=.
case ${1:-} in
list) shift && list "$@" ;;
run) shift && run "$@" ;;
smoke) shift && run "$@" smoke ;;
report) shift && exec python3 "$ROOT/bench/report.py" "$@" ;;
compare) exec python3 "$ROOT/bench/report.py" "$@" ;;
*) usage ;;
esac
