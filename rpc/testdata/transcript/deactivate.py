#!/usr/bin/env python3
"""Prints the feature ids that solana-test-validator must deactivate to run the
transcript under this chain's gate set.

The set is every id that agave-feature-set 4.3.0 declares, less the ids active
at this chain's mainnet pin and the ids that genesis/exceptions.json turns on at
genesis. The six ids that v4.3.0 rekeyed and its two new gates are inactive
here, so the same rule deactivates them.

An exception that stays on is also active from the test validator's genesis, so
the gate sets match. A rent cut active from genesis does not change the test
validator's price, because solana-test-validator always starts at
Rent::default. The divergence ledger records that difference.

Usage: deactivate.py <agave-feature-set 4.3.0 src/lib.rs> <genesis/features.json>
"""
import json
import os
import re
import sys

declared = re.findall(r'declare_id!\("([1-9A-HJ-NP-Za-km-z]{32,44})"\)', open(sys.argv[1]).read())
active = {g["id"] for g in json.load(open(sys.argv[2]))["gates"] if g.get("active_at_pin")}
exceptions = os.path.join(os.path.dirname(sys.argv[2]), "exceptions.json")
active |= {g["id"] for g in json.load(open(exceptions))["gates"]}
for k in sorted(set(declared) - active):
    print(k)
