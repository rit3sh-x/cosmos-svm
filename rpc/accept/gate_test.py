"""Tests the gate rules and readers on fixtures. It needs no chain, so the lint
job runs it: python3 rpc/accept/gate_test.py"""
import json
import pathlib
import sys
import tempfile
import unittest

sys.path.insert(0, str(pathlib.Path(__file__).parent))
import gate  # noqa: E402

CARGO = """\
Running committor (ix_singles_core) [files] tests in /x (files: ["a"])
     Running tests/a.rs (target/debug/deps/a-1)

running 2 tests
test good ... \x1b[2m2026-10-16T07:29:43Z\x1b[0m INFO sending
more log
ok
test bad ... log line
thread 'bad' (87236) panicked at tests/a.rs:411:5:
assertion `left == right` failed
  left: 1
 right: 2

FAILED

failures:

failures:
    bad

test result: FAILED. 1 passed; 1 failed; 0 ignored; 0 measured; 0 filtered out; finished in 1.00s

Running committor (other) [files] tests in /x (files: ["b"])
     Running tests/b.rs (target/debug/deps/b-1)
test skipped ... ignored
test hangs ... waiting
"""

MOCHA = {
    "tests": [
        {"fullTitle": "counter Initialize", "err": {}},
        {"fullTitle": "counter Commit", "err": {"message": "Unable to find Commitment signature"}},
    ],
    "failures": [{"fullTitle": "counter Commit", "err": {"message": "Unable to find Commitment signature"}}],
    "pending": [],
}


class Readers(unittest.TestCase):
    def test_cargo_reads_passes_panics_and_an_unfinished_binary(self):
        r = gate.cargo(CARGO)
        self.assertEqual(set(r), {"a/good", "a/bad", "b/hangs"})
        self.assertTrue(r["a/good"]["ok"])
        self.assertFalse(r["a/bad"]["ok"])
        self.assertIn("left: 1", r["a/bad"]["error"])
        self.assertFalse(r["b/hangs"]["ok"])

    def test_cargo_fails_a_test_that_fails_in_any_run(self):
        second = CARGO.split("Running committor (other)")[0].replace("FAILED. 1 passed; 1 failed", "ok. 2 passed; 0 failed")
        second = second.replace("failures:\n    bad\n", "")
        r = gate.cargo(CARGO + second)
        self.assertFalse(r["a/bad"]["ok"])

    def test_mocha_reads_its_json_reporter(self):
        r = gate.mocha(json.dumps(MOCHA))
        self.assertTrue(r["counter Initialize"]["ok"])
        self.assertEqual(r["counter Commit"], {"ok": False, "error": "Unable to find Commitment signature"})


def res(ok, error="", value=None, suite="s"):
    return {"ok": ok, "error": error, "value": value, "suite": suite}


V1 = {"difference": "V1 off", "compatibility": "c", "accepted": False, "tests": {"s: listed": "unsupported"}}
UNITS = {"difference": "units", "compatibility": "c", "accepted": True, "tests": {}}


class Rules(unittest.TestCase):
    def verdicts(self, agave, svmd, known=(V1, UNITS), required=()):
        rows, ok = gate.gate(list(known), agave, svmd, list(required))
        return {test: verdict for verdict, test, _ in rows}, ok

    def test_a_listed_failure_with_its_assertion_passes_the_run(self):
        v, ok = self.verdicts({"s: listed": res(True)}, {"s: listed": res(False, "version unsupported")})
        self.assertEqual(v["s: listed"], "KNOWN")
        self.assertEqual(v["units"], "ACCEPTED")
        self.assertTrue(ok)

    def test_a_listed_test_that_passes_or_fails_another_way_fails_the_run(self):
        for svmd in (res(True), res(False, "something else"), None):
            b = {"s: listed": svmd} if svmd else {}
            _, ok = self.verdicts({"s: listed": res(True)}, b)
            self.assertFalse(ok, svmd)

    def test_a_listed_test_that_never_ran_in_a_suite_that_ran_fails_the_run(self):
        _, ok = self.verdicts({"s: other": res(True)}, {"s: other": res(True)})
        self.assertFalse(ok)

    def test_a_listed_test_of_a_suite_this_run_left_out_is_not_judged(self):
        v, ok = self.verdicts({"t: x": res(True, suite="t")}, {"t: x": res(True, suite="t")})
        self.assertTrue(ok)
        self.assertNotIn("s: listed", v)

    def test_an_svmd_failure_of_an_agave_pass_fails_the_run(self):
        _, ok = self.verdicts({"s: listed": res(True), "s: t": res(True)}, {"s: listed": res(False, "unsupported"), "s: t": res(False, "x")})
        self.assertFalse(ok)

    def test_a_failure_on_both_only_reports(self):
        v, ok = self.verdicts({"s: listed": res(True), "s: t": res(False, "x")},
                              {"s: listed": res(False, "unsupported"), "s: t": res(False, "x")})
        self.assertTrue(ok)
        self.assertEqual(v["s: t"], "BOTH")

    def test_values_must_agree(self):
        base = {"s: listed": res(True)}
        b = {"s: listed": res(False, "unsupported")}
        _, ok = self.verdicts(dict(base, **{"s: u": res(True, value="84442")}), dict(b, **{"s: u": res(True, value="84443")}))
        self.assertFalse(ok)

    def test_a_required_suite_failing_on_agave_breaks_the_run(self):
        a = {"s: listed": res(True), "r: t": res(False, "x", suite="r")}
        _, ok = self.verdicts(a, {"s: listed": res(False, "unsupported"), "r: t": res(False, "x", suite="r")}, required=["r"])
        self.assertFalse(ok)
        _, ok = self.verdicts({"s: listed": res(True)}, {"s: listed": res(False, "unsupported")}, required=["r"])
        self.assertFalse(ok)

    def test_results_reads_a_directory_by_suffix(self):
        with tempfile.TemporaryDirectory() as d:
            p = pathlib.Path(d)
            (p / "counter.mocha.json").write_text(json.dumps(MOCHA))
            (p / "committor.cargo.log").write_text(CARGO)
            (p / "pyth-default.jsonl").write_text(json.dumps({"test": "pull", "ok": True, "value": "1 2"}) + "\n")
            r = gate.results(d)
        self.assertEqual(r["pyth-default: pull"]["value"], "1 2")
        self.assertEqual(r["counter: counter Initialize"]["suite"], "counter")
        self.assertIn("committor: a/good", r)


if __name__ == "__main__":
    unittest.main()
