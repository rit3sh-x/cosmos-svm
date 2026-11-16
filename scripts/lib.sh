# shellcheck shell=bash
# Shared helpers for shell harnesses and benchmarks.
# Source it after `set -euo pipefail`. It sets no options and runs nothing.
#
#   . "$ROOT/scripts/lib.sh"

# die prints its arguments on stderr and exits 1.
die() {
    printf '%s\n' "$*" >&2
    exit 1
}

# Run a condition until it succeeds or the deadline expires.
wait_for() {
    local seconds=$1 what=$2 deadline
    shift 2
    deadline=$((SECONDS + seconds))
    until "$@"; do
        if [ "$SECONDS" -ge "$deadline" ]; then
            printf 'timed out after %ss waiting for %s\n' "$seconds" "$what" >&2
            return 1
        fi
        sleep "${WAIT_INTERVAL:-0.5}"
    done
}

# running PID succeeds while the process exists.
running() {
    kill -0 "$1" 2>/dev/null
}

# Stop a process, escalating from TERM to KILL on timeout.
stop_pid() {
    local pid=$1
    [ -n "$pid" ] || return 0
    kill "$pid" 2>/dev/null || return 0
    if ! wait_for "${2:-60}" "process $pid to exit" not running "$pid" 2>/dev/null; then
        kill -9 "$pid" 2>/dev/null || true
        wait_for 10 "process $pid to exit after KILL" not running "$pid"
    fi
}

# Stop the process named by a PID file, if present.
stop_pidfile() {
    [ -f "$1" ] || return 0
    stop_pid "$(cat "$1")" "${2:-60}"
}

# Stop a process group and its children, escalating to KILL on timeout.
stop_group() {
    local pid
    [ -f "$1" ] || return 0
    pid=$(cat "$1")
    kill -- "-$pid" 2>/dev/null || return 0
    if ! wait_for "${2:-60}" "process group $pid to exit" not group_alive "$pid" 2>/dev/null; then
        kill -9 -- "-$pid" 2>/dev/null || true
    fi
}

# group_alive PGID succeeds while any process of the group exists.
group_alive() {
    kill -0 -- "-$1" 2>/dev/null
}

# Invert a command's exit status.
not() {
    ! "$@"
}
