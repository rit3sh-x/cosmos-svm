#!/usr/bin/env python3
"""Read the runs bench/bench.sh writes: report one, or compare two.

    bench/report.py RUN
    bench/report.py compare RUN-A RUN-B [--keys REGEX]

A run is a directory under bench/results (bench/README.md). Its result lines
are the lines of its *.txt files that start with `exp=`. Each one is a list of
key=value fields. Every other line is a log. A report prints the cells'
status, their headline numbers, and then the analysis of the run's suite, if
it has one. A comparison pairs the result lines of two runs cell by cell and
prints the ratio of every number whose key matches REGEX.

The script only reads files. It never runs during a measurement.
"""
import argparse
import collections
import glob
import math
import os
import re
import subprocess
import sys
from pathlib import Path

HEADLINE = r"_p50_|_per_s$|_per_tx$"

# A key that names a measurement, as opposed to a setting of the cell.
MEASURE = re.compile(r"_(p\d+|max|min)(_|$)|_(us|ms|ns|s|mb|b|frac)$|_per_(s|tx|write|block)$"
                     r"|^(unique|speedup|nodes|exports_completed)$|^retained_")

RUN_NAME = re.compile(r"^(.*)-\d{8}T\d{4,6}Z$")


def fields(line):
    return dict(p.split("=", 1) for p in line.split() if "=" in p)


def number(v):
    try:
        f = float(v)
    except ValueError:
        return None
    return f if math.isfinite(f) else None


def results(run):
    """Each cell's result lines, by cell name, for the cells that have any."""
    out = {}
    for f in sorted(run.glob("*.txt")):
        lines = [fields(l) for l in f.read_text(errors="replace").splitlines() if l.startswith("exp=")]
        if lines:
            out[f.stem] = lines
    return out


def suite(run):
    """The run's suite: its summary.txt header, or its directory's name."""
    summary = run / "summary.txt"
    if summary.exists():
        first = summary.read_text().splitlines()[:1]
        if first and first[0].startswith("# bench.sh run "):
            return first[0].split()[3]
    for name in (run.name, run.parent.name):
        m = RUN_NAME.match(name)
        if m:
            return m.group(1)
    return ""


def status(run):
    """The last summary.txt line of each cell, by cell: ok or FAILED, seconds."""
    out = {}
    summary = run / "summary.txt"
    if summary.exists():
        for line in summary.read_text().splitlines():
            f = line.split()
            if len(f) >= 3 and f[0] in ("ok", "FAILED"):
                out[f[1]] = (f[0], f[2])
    return out


def report(run):
    name = suite(run)
    print(f"{run} ({name or 'no suite'})")
    st = status(run)
    failed = [c for c, (s, _) in st.items() if s == "FAILED"]
    if st:
        print(f"{len(st) - len(failed)} cells ok, {len(failed)} failed{': ' + ', '.join(failed) if failed else ''}")
    headline(results(run))
    analysis = ANALYSES.get(name)
    if analysis:
        print()
        analysis(run)


def headline(res, keys=HEADLINE):
    """Print each result line's numbers whose keys match keys. Before them,
    print the settings that differ between the cell's lines of the same exp."""
    want = re.compile(keys)
    for cell, lines in res.items():
        groups = collections.defaultdict(list)
        for l in lines:
            groups[l.get("exp", "")].append(l)
        for exp, group in groups.items():
            varying = [k for k in group[0] if k != "exp" and not MEASURE.search(k) and len({l.get(k) for l in group}) > 1]
            for l in group:
                numbers = [f"{k}={v}" for k, v in l.items() if want.search(k) and number(v) is not None]
                if numbers:
                    print(f"  {cell:<24} {exp:<18} {' '.join([f'{k}={l[k]}' for k in varying if k in l] + numbers)}")


def compare(a, b, keys):
    want = re.compile(keys)
    ra, rb = results(a), results(b)
    print(f"A {a}\nB {b}\nkeys matching {keys}\n")
    print(f"{'cell':<24} {'line':>4}  {'key':<24} {'A':>12} {'B':>12} {'B/A':>7}")
    notes = []
    for cell in sorted(ra.keys() & rb.keys()):
        la, lb = ra[cell], rb[cell]
        if len(la) != len(lb):
            notes.append(f"{cell}: {len(la)} result lines in A, {len(lb)} in B; the first {min(len(la), len(lb))} are paired")
        for i, (x, y) in enumerate(zip(la, lb), 1):
            for k in x.keys() & y.keys():
                if not MEASURE.search(k) and x[k] != y[k]:
                    notes.append(f"{cell} line {i}: {k} is {x[k]} in A and {y[k]} in B")
            for k, v in x.items():
                va, vb = number(v), number(y.get(k, ""))
                if want.search(k) and va is not None and vb is not None:
                    ratio = f"{vb / va:7.2f}" if va else "      -"
                    print(f"{cell:<24} {i:>4}  {k:<24} {v:>12} {y[k]:>12} {ratio}")
    for cell in sorted(ra.keys() - rb.keys()):
        notes.append(f"{cell}: only in A")
    for cell in sorted(rb.keys() - ra.keys()):
        notes.append(f"{cell}: only in B")
    if notes:
        print()
        for n in notes:
            print("note:", n)


# Every block limit on this chain is `ceil(rate * us)` against one bar. A block
# that spends all 60,000,000 units takes 7.45 s. So a unit is 7.45 s /
# 60,000,000, and a microsecond is 8.0537 units. block_limits applies this rule
# to each term that a run measured, so the arithmetic from a measurement to a
# constant is in code.
#
# A cell's phases are load, write, index and commit. The block total is their
# sum plus the untimed remainder. A term's time comes from the phase that holds
# only that term:
#
#     write       block total minus the reads, over the writable accounts
#     name        the reads, over the names and the tables
#     allocation  block total minus the reads, over the allocated bytes
#     loaded byte the load and the copy of a cell that loads -big accounts
#                 and nothing else, over their bytes
#     index       one cell's block total minus the same cell's without -index
#
# The reads are the resolve phase, the load and the copy. Each is zero in a
# cell without -tables or -big.
#
# The harness times Go's side of a block, not the engine's. NAME_UNITS 245 is
# `ceil(8.0537 * (30.25 + 0.135))`. The 0.135 us is the engine's own arena
# decode of one key, measured separately. The name cell's result here leaves it
# out, so add 0.135 us before you compare it with NAME_UNITS. No cell measures
# execution either. Execution takes about 400 ms of a 7.45 s block, and every
# term priced here is a store term.

BAR_S = 7.45
MAX_BLOCK_UNITS = 60_000_000
RATE = MAX_BLOCK_UNITS / (BAR_S * 1e6)  # units per microsecond


def phase2_cells(d):
    """Each cell's last exp=phase2 line, by label."""
    out = {}
    for f in sorted(d.glob("*.txt")):
        line = ""
        for l in f.read_text().splitlines():
            if l.startswith("exp=phase2"):
                line = l
        if not line:
            continue
        kv = fields(line)
        out[kv["label"]] = kv
    return out


def price(us):
    return math.ceil(RATE * us)


def block_limits(d):
    c = phase2_cells(d)
    if not c:
        print(f"no phase2 cells in {d}")
        return
    num = lambda k, key: float(k[key])
    print(f"rate {RATE:.4f} units per microsecond, bar {BAR_S} s, "
          f"block {MAX_BLOCK_UNITS:,} units\n")
    print(f"{'cell':<18} {'block p50':>10} {'p95':>8} {'load':>8} {'write':>8} "
          f"{'index':>8} {'commit':>8} {'of bar':>7}")
    for label, k in c.items():
        b = num(k, "block_p50_s")
        print(f"{label:<18} {b:>9.2f}s {num(k,'block_p95_s'):>7.2f}s "
              f"{num(k,'load_p50_s'):>7.2f}s {num(k,'write_p50_s'):>7.2f}s "
              f"{num(k,'index_p50_s'):>7.2f}s {num(k,'commit_p50_s'):>7.2f}s "
              f"{b / BAR_S:>6.2f}x")

    print("\nterms")
    for label, k in c.items():
        names, writes, new = int(k["names"]), int(k["writes"]), int(k["new"])
        tables, big = int(k.get("tables", 0)), int(k.get("big", 0))
        load = sum(float(k.get(p, 0)) for p in ("resolve_p50_s", "load_p50_s", "copy_p50_s"))
        block = num(k, "block_p50_s")
        if big and names <= big and not writes and not new:
            total = big * int(k["big_bytes"])
            print(f"  {label:<16} loaded byte {load * 1e9 / total:8.4f} ns/byte over {total:,} bytes")
            continue
        if (names or tables) and not writes and not new:
            us = load * 1e6 / (names + tables)
            print(f"  {label:<16} name        {us:8.2f} us  ->  {price(us):>7,} units"
                  f"   (NAME_UNITS)")
        if writes and not new:
            us = (block - load) * 1e6 / writes
            print(f"  {label:<16} write       {us:8.2f} us  ->  {price(us):>7,} units"
                  f"   (WRITE_LOCK_UNITS)")
        if new:
            total = new * int(k["new_bytes"])
            us = (block - load) * 1e6 / total
            per_unit = 1 / (RATE * us) if us else float("inf")
            print(f"  {label:<16} allocation  {us*1000:8.4f} ns/byte  ->  "
                  f"bytes / {per_unit:.2f}   (ALLOCATED_BYTES_PER_UNIT)")

    for a, b in [("blk-16g", "blk-16g-noindex"), ("blk-20g", "blk-20g-noindex")]:
        if a in c and b in c:
            d_s = num(c[a], "block_p50_s") - num(c[b], "block_p50_s")
            txs = int(c[a]["txs"])
            print(f"  {a:<16} index       {d_s:8.3f} s a block, {d_s*1e6/txs:.2f} us "
                  f"a transaction over {txs:,}")

    for lo, hi in [("w-16g", "w-20g"), ("n-16g", "n-20g"), ("blk-16g", "blk-20g")]:
        if lo in c and hi in c:
            a, b = num(c[lo], "block_p50_s"), num(c[hi], "block_p50_s")
            print(f"  {lo} -> {hi}: {a:.2f}s -> {b:.2f}s, {a/b:.2f}x" if b else "")


# validator_memory prints the peak of every sampled series by phase. Then it
# splits each Go heap profile by the package that allocated. CometBFT,
# goleveldb, IAVL, the SVM module, the RPC server and the Go runtime each get a
# row. It needs `go` on PATH for `go tool pprof`.

OWNERS = [
    ("cometbft", "github.com/cometbft/"),
    ("goleveldb", "github.com/syndtr/goleveldb/"),
    ("iavl", "github.com/cosmos/iavl"),
    ("store", "github.com/cosmos/cosmos-sdk/store"),
    ("svm rpc", "github.com/rit3sh-x/cosmos-svm/rpc"),
    ("svm module", "github.com/rit3sh-x/cosmos-svm/x/"),
    ("svm ffi", "github.com/rit3sh-x/cosmos-svm/ffi"),
    ("svm other", "github.com/rit3sh-x/cosmos-svm/"),
    ("cosmos-sdk", "github.com/cosmos/cosmos-sdk/"),
    ("gogoproto", "github.com/cosmos/gogoproto"),
    ("protobuf", "google.golang.org/protobuf"),
    ("go runtime and stdlib", ""),
]

PPROF_ROW = re.compile(r"\s*([\d.]+)([kMG]?B)\s+[\d.]+%\s+[\d.]+%\s+[\d.]+[kMG]?B\s+[\d.]+%\s+(.+)$")


def owner(fn):
    for name, prefix in OWNERS:
        if prefix and fn.startswith(prefix):
            return name
    return "other modules" if "." in fn.split("/")[0] and "/" in fn else "go runtime and stdlib"


def validator_memory(run):
    d = str(run)
    peaks = collections.defaultdict(dict)
    with open(os.path.join(d, "samples.txt")) as samples:
        for line in samples:
            f = dict(kv.split("=", 1) for kv in line.split())
            ph = f.pop("phase")
            for k, v in f.items():
                if k.endswith("_mb"):
                    peaks[ph][k] = max(peaks[ph].get(k, -10**9), int(v))
    print("peak by phase, MiB")
    cols = sorted({k for p in peaks.values() for k in p})
    print("  %-18s" % "series" + "".join("%10s" % p for p in ("idle", "fill", "drain")))
    for c in cols:
        print("  %-18s" % c + "".join("%10s" % peaks[p].get(c, "-") for p in ("idle", "fill", "drain")))

    for prof in sorted(glob.glob(os.path.join(d, "heap-*.pb.gz"))):
        out = subprocess.run(["go", "tool", "pprof", "-top", "-sample_index=inuse_space",
                              "-nodecount=1000000", prof], capture_output=True, text=True).stdout
        rows = collections.Counter()
        total = 0.0
        for line in out.splitlines():
            m = PPROF_ROW.match(line)
            if not m:
                continue
            mb = float(m.group(1)) * {"B": 1 / 2**20, "kB": 1 / 2**10, "MB": 1, "GB": 2**10}[m.group(2)]
            rows[owner(m.group(3).strip())] += mb
            total += mb
        print("\n%s, live heap by allocating package, MiB (total %.0f)" % (os.path.basename(prof), total))
        for k, v in rows.most_common():
            print("  %-24s %8.1f" % (k, v))

    blocks = []
    if os.path.exists(os.path.join(d, "blocks.txt")):
        with open(os.path.join(d, "blocks.txt")) as lines:
            blocks = [l.split() for l in lines]
    if blocks:
        top = max(blocks, key=lambda b: int(b[1]))
        print("\nbiggest block in the last 20: height %s, %s bytes, %s transactions" % tuple(top))


def proof_window(run):
    """The deepest depth each probe found provable, and which old heights."""
    for f in sorted(run.glob("probe-*.txt")):
        depth, old = "", []
        for line in f.read_text().splitlines():
            w = line.split()
            if w and w[-1] == "ok" and w[0] == "depth":
                depth = w[1]
            if w and w[-1] == "ok" and w[0] == "height":
                old.append(w[1])
        print(f"{f.stem}: deepest provable depth {depth}; old heights provable: {' '.join(old) or 'none'}")


def micro(run):
    """benchstat's table, and Criterion's estimate for each benchmark."""
    stat = run / "go-benchstat.txt"
    if stat.exists():
        print("\n".join(l for l in stat.read_text().splitlines() if not l.startswith("#")))
    crit = run / "svm-criterion.txt"
    if crit.exists():
        prev = ""
        for line in crit.read_text().splitlines():
            if "time:" in line or "thrpt:" in line:
                if "time:" in line and line[:1].isspace():
                    print(prev)
                print(line)
            prev = line


ANALYSES = {"block-limits": block_limits, "validator-memory": validator_memory,
            "proof-window": proof_window, "micro": micro}


def main(argv):
    if argv[:1] == ["compare"]:
        p = argparse.ArgumentParser(prog="bench/report.py compare")
        p.add_argument("a", type=Path)
        p.add_argument("b", type=Path)
        p.add_argument("--keys", default=HEADLINE, help=f"which numbers to compare, a regex (default {HEADLINE})")
        args = p.parse_args(argv[1:])
        runs = [args.a, args.b]
    else:
        p = argparse.ArgumentParser(prog="bench/report.py", description=__doc__.splitlines()[0])
        p.add_argument("run", type=Path)
        args = p.parse_args(argv)
        runs = [args.run]
    for r in runs:
        if not r.is_dir():
            p.error(f"{r} is not a run directory")
    if argv[:1] == ["compare"]:
        compare(args.a, args.b, args.keys)
    else:
        report(args.run)
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
