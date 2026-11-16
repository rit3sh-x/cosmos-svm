#!/usr/bin/env python3
"""Re-read mainnet and report feature gate drift against genesis/features.json.

    python3 genesis/check-features.py            # report drift, always exit 0

The exit status is 0 whether or not mainnet has moved, for the reason
genesis/check-manifest.py gives. A gate that mainnet activates is news about
mainnet, not a defect in this repository. A red build that nobody can fix
teaches people to ignore red builds.

The script has no --update option, because the pin does not move.
features.json, manifest.json and the sysvar contents all come from the same
mainnet slot. Re-pinning one of them alone would leave three files that
describe three different mainnets. A gate moves here through governance, at a
height, as a proposal that people can read.

The script reads the live activated_at of every pinned id and reports three
kinds of drift:

- a gate that activated after the pin
- a gate that the pin records as activated and that mainnet no longer shows
- a gate whose activation slot changed
"""

import base64
import json
import os
import sys
import time
import urllib.request

HERE = os.path.dirname(os.path.abspath(__file__))
FEATURES = os.path.join(HERE, "features.json")
FEATURE_PROGRAM = "Feature111111111111111111111111111111111111"
EXCEPTIONS = os.path.join(HERE, "exceptions.json")
ENDPOINT = os.environ.get("SOLANA_RPC", "https://api.mainnet-beta.solana.com")


def genesis_exceptions():
    """The ids that genesis activates although the pin has them off.

    x/svm and the engine's instruction-level differential read the same file,
    so the report cannot claim mainnet is ahead on a gate this chain runs.
    """
    return {g["id"] for g in json.load(open(EXCEPTIONS))["gates"]}


def rpc(method, params, attempts=6):
    body = json.dumps({"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
    req = urllib.request.Request(
        ENDPOINT, data=body.encode(), headers={"Content-Type": "application/json"}
    )
    last = None
    for _ in range(attempts):
        try:
            return json.loads(urllib.request.urlopen(req, timeout=90).read())
        except Exception as exc:  # network flake, rate limit
            last = exc
            time.sleep(4)
    raise SystemExit("RPC %s failed against %s: %s" % (method, ENDPOINT, last))


def activated_at(account):
    """The activation slot in a feature account, or None.

    The rule is the same as in genesis/pin-features.py. The feature program
    must own the account, and the account must hold bincode
    Feature { activated_at: Some(slot) }.
    """
    if account is None or account["owner"] != FEATURE_PROGRAM:
        return None
    data = base64.b64decode(account["data"][0])
    if len(data) >= 9 and data[0] == 1:
        return int.from_bytes(data[1:9], "little")
    return None


def main():
    pinned = json.load(open(FEATURES))
    gates = pinned["gates"]
    slot = rpc("getSlot", [{"commitment": "finalized"}])["result"]
    version = rpc("getVersion", [])["result"]
    print("pinned at slot %s, read at slot %s, %s"
          % (pinned["mainnet_slot"], pinned["read_at_slot"], pinned["crate"]))
    print("mainnet now slot %s, solana-core %s, feature-set %s"
          % (slot, version.get("solana-core"), version.get("feature-set")))
    print()

    live = {}
    for i in range(0, len(gates), 100):
        chunk = gates[i:i + 100]
        result = rpc("getMultipleAccounts",
                     [[g["id"] for g in chunk], {"encoding": "base64", "commitment": "finalized"}])
        for gate, account in zip(chunk, result["result"]["value"]):
            live[gate["id"]] = activated_at(account)

    ahead, since = [], []
    for gate in gates:
        now = live[gate["id"]]
        if now != gate["activated_at"]:
            if gate["activated_at"] is None:
                since.append((gate, "ACTIVATED at %d" % now))
            elif now is None:
                since.append((gate, "NO LONGER ACTIVE (pinned %d)" % gate["activated_at"]))
            else:
                since.append((gate, "SLOT %d -> %d" % (gate["activated_at"], now)))
        if now is not None and now > pinned["mainnet_slot"] and not gate["active_at_pin"]:
            ahead.append((gate, now))

    exceptions = genesis_exceptions()
    print("Mainnet is ahead of the pin on %d gate(s). Genesis runs without them unless the row"
          % len(ahead))
    print("says otherwise; each is a governance decision at a height, not a re-pin.")
    for gate, at in sorted(ahead, key=lambda row: row[1]):
        here = "on here by exception" if gate["id"] in exceptions else "off here"
        print("  %-7s %-46s activated %d  %-20s %s"
              % (gate["class"], gate["id"], at, here, gate["description"]))
    print()

    print("Changed since features.json was read at slot %s: %d gate(s)."
          % (pinned["read_at_slot"], len(since)))
    for gate, what in since:
        print("  %-7s %-46s %s  %s" % (gate["class"], gate["id"], what, gate["description"]))
    print()
    print("%d pinned gates checked against %s" % (len(gates), ENDPOINT))
    return 0


if __name__ == "__main__":
    sys.exit(main())
