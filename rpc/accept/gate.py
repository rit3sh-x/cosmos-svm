"""The gate of the acceptance runs. It judges this chain against Agave.

    gate.py KNOWN AGAVE SVMD [--require SUITE]...

AGAVE and SVMD are the result directories of one run on each base chain. Each
holds one file for each suite, and the gate reads a file by its suffix:

    NAME.mocha.json   mocha's json reporter
    NAME.cargo.log    the output of cargo test, as MagicBlock's runner prints it
    NAME.jsonl        one {"test", "ok", "error", "value"} object a line

A test is "NAME: TITLE". The rules:

- A test that passes on Agave must pass on this chain, unless KNOWN lists it.
- A listed test must fail on this chain with its assertion in the error. A test
  that did not run has the error "did not run", and an entry may list it,
  because a runner that stops a group at its first failure skips the rest.
- A listed test fails the run if it passes, fails another way, or does not run
  when it should, so the list cannot hide a new failure. Delete the entry
  when the difference closes.
- A test that fails on Agave is reported and does not gate.
- A test that carries a value must carry the same value on both chains.
- Every test of a --require suite must pass on Agave. If one does not, the run
  is broken, not this chain.

KNOWN is a JSON list of entries: {"difference", "compatibility", "accepted",
"tests": {TEST: ASSERTION}}. An accepted entry is a difference that stays.
The report goes to stdout. The exit status is 1 when the run fails.
"""
import argparse
import json
import pathlib
import re
import sys

ANSI = re.compile(r"\x1b\[[0-9;]*m")


def mocha(text):
    doc = json.loads(text)
    out = {}
    for t in doc.get("tests", []) + doc.get("failures", []):
        if t["fullTitle"] in out and not out[t["fullTitle"]]["ok"]:
            continue
        err = t.get("err") or {}
        out[t["fullTitle"]] = {"ok": not err, "error": err.get("message", "")}
    for t in doc.get("pending", []):
        out.pop(t["fullTitle"], None)
    return out


def cargo(text):
    """Reads the output of MagicBlock's runner, which runs cargo test with
    --nocapture. A section is one test binary, from cargo's "Running
    tests/FILE.rs" (or src/FILE.rs) line to its "test result:" line. A test is
    FILE/NAME.

    A test fails if libtest lists it under "failures:". Its error is its panic
    message, or else the last lines it printed. A section that never reaches
    its result line did not finish, so the tests it started fail. A test that
    the runner runs more than once fails if any run fails."""
    out, file, section = {}, "", None
    failed, panics, tails, current, panic, listing = set(), {}, {}, None, None, False

    def close(finished):
        for key in section or []:
            ok = finished and key not in failed
            error = panics.get(key) or "\n".join(tails.get(key, [])[-40:])
            if key in out and not out[key]["ok"]:
                continue
            out[key] = {"ok": ok, "error": "" if ok else error or "the test binary did not finish"}

    for line in ANSI.sub("", text).split("\n"):
        m = re.match(r"\s+Running (?:tests|src)/(\S+?)\.rs ", line)
        if m:
            close(False)
            file = m.group(1)
            section, failed, panics, tails, current, panic, listing = [], set(), {}, {}, None, None, False
            continue
        if section is None:
            continue
        if line.startswith("test result:"):
            close(True)
            section = None
            continue
        if line == "failures:":
            listing, panic = True, None
            continue
        m = re.match(r"    (\S+)$", line)
        if listing and m:
            failed.add(f"{file}/{m.group(1)}")
            continue
        started = [n for n, rest in re.findall(r"test (\S+) \.\.\. ?(\S*)", line) if rest != "ignored"]
        for name in started:
            current = f"{file}/{name}"
            section.append(current)
        if started:
            panic = None
        m = re.match(r"thread '([^']+)'.* panicked at", line)
        if m:
            panic = f"{file}/{m.group(1)}"
            panics[panic] = panics.get(panic, "") + line + "\n"
        elif panic and line.strip():
            panics[panic] += line + "\n"
        elif panic:
            panic = None
        if current:
            tails.setdefault(current, []).append(line)
    close(False)
    return out


def jsonl(text):
    out = {}
    for line in text.splitlines():
        if line.strip():
            r = json.loads(line)
            out[r["test"]] = {"ok": r["ok"], "error": r.get("error", ""), "value": r.get("value")}
    return out


READERS = {".mocha.json": mocha, ".cargo.log": cargo, ".jsonl": jsonl}


def results(directory):
    out = {}
    for path in sorted(pathlib.Path(directory).iterdir()):
        for suffix, read in READERS.items():
            if path.name.endswith(suffix):
                suite = path.name[: -len(suffix)]
                for title, r in read(path.read_text(errors="replace")).items():
                    out[f"{suite}: {title}"] = dict(r, suite=suite)
    return out


def first_line(error):
    return next((line.strip() for line in error.splitlines() if line.strip()), "no message")


def gate(known, agave, svmd, required):
    """Returns a (verdict, test, reason) row for each test and whether the
    run passes. FAIL and BROKEN fail the run."""
    listed = {test: (entry, assertion) for entry in known for test, assertion in entry["tests"].items()}
    rows = []

    def say(verdict, test, why=""):
        rows.append((verdict, test, why))

    for suite in required:
        if not any(r["suite"] == suite for r in agave.values()):
            say("BROKEN", suite, "no test ran on Agave")
    # The gate does not judge a listed test of a suite that this run did not run.
    ran = {r["suite"] for r in list(agave.values()) + list(svmd.values())}
    listed = {t: v for t, v in listed.items() if t.split(": ", 1)[0] in ran}
    for test in sorted(set(agave) | set(svmd) | set(listed)):
        a, b = agave.get(test), svmd.get(test)
        if test in listed:
            entry, assertion = listed[test]
            if a is None and b is None:
                say("FAIL", test, f"listed for \"{entry['difference']}\", but did not run")
            elif a is None or not a["ok"]:
                say("BOTH", test, f"listed, and fails on Agave too: {first_line(a['error']) if a else 'did not run'}")
            elif b is not None and b["ok"]:
                say("FAIL", test, f"listed for \"{entry['difference']}\", but passes on this chain: delete the entry")
            elif assertion not in (b["error"] if b else "did not run"):
                got = first_line(b["error"]) if b else "did not run"
                say("FAIL", test, f"listed for \"{entry['difference']}\" with \"{assertion}\", but fails with: {got}")
            else:
                say("KNOWN", test, entry["difference"])
            continue
        if a is not None and not a["ok"]:
            if a["suite"] in required:
                say("BROKEN", test, f"a required test fails on Agave: {first_line(a['error'])}")
            else:
                say("BOTH" if b is None or not b["ok"] else "AGAVE", test,
                    f"fails on Agave: {first_line(a['error'])}")
        elif a is None:
            say("REPORT", test, "ran only on this chain" if b else "ran on neither")
        elif b is None:
            say("FAIL", test, "passes on Agave, did not run on this chain")
        elif not b["ok"]:
            say("FAIL", test, f"passes on Agave, fails on this chain: {first_line(b['error'])}")
        elif a.get("value") != b.get("value"):
            say("FAIL", test, f"Agave says {a.get('value')}, this chain says {b.get('value')}")
        else:
            say("PASS", test)
    for entry in known:
        if entry["accepted"]:
            say("ACCEPTED", entry["difference"], entry["compatibility"])
    return rows, all(verdict not in ("FAIL", "BROKEN") for verdict, _, _ in rows)


def main():
    p = argparse.ArgumentParser(description="The acceptance runs' gate.")
    p.add_argument("known")
    p.add_argument("agave")
    p.add_argument("svmd")
    p.add_argument("--require", action="append", default=[], metavar="SUITE")
    args = p.parse_args()
    known = json.loads(pathlib.Path(args.known).read_text())
    rows, ok = gate(known, results(args.agave), results(args.svmd), args.require)
    counts = {}
    for verdict, test, why in rows:
        print(f"{verdict:8} {test}" + (f": {why}" if why else ""))
        counts[verdict] = counts.get(verdict, 0) + 1
    print(", ".join(f"{v} {k}" for k, v in sorted(counts.items())))
    print("PASS" if ok else "FAIL")
    return 0 if ok else 1


if __name__ == "__main__":
    sys.exit(main())
