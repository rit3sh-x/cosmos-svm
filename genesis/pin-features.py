#!/usr/bin/env python3
"""Pin the genesis feature gate set to mainnet at the manifest's slot.

    python3 genesis/pin-features.py

The script classifies every id in the pinned agave-feature-set with Anza's own
projection. It reads each feature account's activated_at from mainnet and
writes genesis/features.json. A gate is active at the pin if and only if its
account holds Some(slot) with slot <= the manifest's mainnet_slot. So the pin
is reproducible on any later day.

Genesis creates an account for each active svm or cleaned gate, and never for a
bank gate. svmmeta holds the active svm gates. This script records the facts,
and the chain applies the rule.
"""

import base64
import json
import os
import subprocess
import sys
import urllib.request

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.dirname(HERE)
FEATURE_PROGRAM = "Feature111111111111111111111111111111111111"
ENDPOINT = os.environ.get("SOLANA_RPC", "https://api.mainnet-beta.solana.com")


def rpc(method, params):
    body = json.dumps({"jsonrpc": "2.0", "id": 1, "method": method, "params": params}).encode()
    req = urllib.request.Request(ENDPOINT, body, {"content-type": "application/json"})
    return json.load(urllib.request.urlopen(req, timeout=60))["result"]


def crate_version():
    """The agave-feature-set version the engine's lockfile resolved."""
    lock = open(os.path.join(ROOT, "Cargo.lock")).read()
    head = '[[package]]\nname = "agave-feature-set"\nversion = "'
    at = lock.index(head) + len(head)
    return lock[at:lock.index('"', at)]


def main():
    slot = json.load(open(os.path.join(HERE, "manifest.json")))["mainnet_slot"]
    out = subprocess.run(
        ["cargo", "test", "--release", "--locked", "print_feature_classes", "--", "--ignored", "--nocapture"],
        cwd=ROOT, capture_output=True, text=True, check=True).stdout
    rows = [line.split("\t")[1:] for line in out.splitlines() if line.startswith("FEATURE\t")]
    gates = []
    for i in range(0, len(rows), 100):
        chunk = rows[i:i + 100]
        result = rpc("getMultipleAccounts", [[r[0] for r in chunk], {"encoding": "base64"}])
        for (id_, cls, description), account in zip(chunk, result["value"]):
            activated = None
            # The feature program owns a feature account, and the account holds
            # bincode Feature { activated_at: Option<u64> }. Anything else at the
            # id, an empty account included, is not an activation.
            if account is not None and account["owner"] == FEATURE_PROGRAM:
                data = base64.b64decode(account["data"][0])
                if len(data) >= 9 and data[0] == 1:
                    activated = int.from_bytes(data[1:9], "little")
            gates.append({"id": id_, "class": cls, "activated_at": activated,
                          "active_at_pin": activated is not None and activated <= slot,
                          "description": description})
    counts = {}
    for g in gates:
        key = (g["class"], g["active_at_pin"])
        counts[key] = counts.get(key, 0) + 1
    for key in sorted(counts):
        print("%-8s active=%-5s %d" % (key[0], key[1], counts[key]))
    json.dump({"mainnet_slot": slot, "read_at_slot": result["context"]["slot"],
               "crate": "agave-feature-set " + crate_version(), "gates": gates},
              open(os.path.join(HERE, "features.json"), "w"), indent=1)
    open(os.path.join(HERE, "features.json"), "a").write("\n")
    return 0


if __name__ == "__main__":
    sys.exit(main())
