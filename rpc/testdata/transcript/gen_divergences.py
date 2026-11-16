#!/usr/bin/env python3
"""Writes divergences.json, next to this script, from the replay differences.
Each difference gets the reason that this chain keeps it and the phrase that
docs/compatibility.md must say. The script prints a difference that no rule
matches, writes nothing, and fails. To close or add a difference, edit the
rules here and run `make divergences`.

Usage: gen_divergences.py <diffs.json from TRANSCRIPT_DIFFS>
"""
import json
import pathlib
import re
import sys

HERE = pathlib.Path(__file__).resolve().parent
diffs = json.load(open(sys.argv[1]))
compat = (HERE.parent.parent.parent / "docs/compatibility.md").read_text()
rows = {m.group(1): m.group(0) for m in re.finditer(r"^\| `(\w+)` \|.*\|$", compat, re.M)}

LABEL = {
    "subs": "**Subscriptions.**",
    "cost": "**`costUnits`.**",
    "http": "**The HTTP layer.**",
    "kept": "**Kept differences.**",
    "leaders": "**Leaders, epochs and the cluster.**",
    "excluded": "**Vote, stake and inflation.**",
}


def entry(d, reason, page):
    e = {"method": d["method"], "case": d["case"], "path": d["path"]}
    if "agave" in d:
        e["agave"] = d["agave"]
    if "chain" in d:
        e["chain"] = d["chain"]
    e.update({"reason": reason, "page": page})
    return e


# Genesis rent here is mainnet's 5,080 lamports a byte. solana-test-validator
# always starts at the 6,960 of Rent::default. An account funded at its rent
# minimum differs by (128 + len) * 1,880, and its funder by the sum. A
# lamports difference that is a whole multiple of 1,880 comes from rent only.
RENT_STEP = 6960 - 5080


def rent_shaped(d):
    m, path, a, c = d["method"], d["path"], d.get("agave"), d.get("chain")
    lamports = path.endswith("lamports") or re.search(r"Balances\[\d+\]$", path) or \
        m in ("getBalance", "getMinimumBalanceForRentExemption")
    ints = all(isinstance(v, int) and not isinstance(v, bool) for v in (a, c))
    return bool(lamports) and ints and a != c and (a - c) % RENT_STEP == 0


def classify(d):
    m, case, path = d["method"], d["case"], d["path"]
    if m == "getSupply" and path.startswith("result.value.nonCirculatingAccounts"):
        return entry(d, "kept: Agave lists mainnet's 214 non-circulating addresses on every cluster; they mean nothing here, and nothing on this chain is locked, so its list is empty",
                     "`nonCirculatingAccounts` is empty and `nonCirculating` is 0")
    if rent_shaped(d):
        return entry(d, "kept: genesis rent here is mainnet's 5,080 lamports a byte, with SIMD-0437-1 and -2 active from block 0, and solana-test-validator always starts at Rent::default's 6,960, so an account funded at its rent minimum, and its funder, differ by a multiple of 1,880",
                     "`Rent.lamports_per_byte` is 5,080 here")
    if m == "getHighestSnapshotSlot":
        return entry(d, "kept: the test validator holds a snapshot archive, and this machine holds no Agave snapshot, because CometBFT state sync does that job", rows[m])
    if m == "getBlockCommitment":
        return entry(d, "kept: the test validator has a vote account and stake, and this chain has neither", rows[m])
    if m == "getStakeMinimumDelegation":
        return entry(d, "kept: the Stake program is not on this chain", rows[m])
    if m == "requestAirdrop":
        return entry(d, "kept: the test validator runs a faucet, and a machine without one answers -32600, as an Agave RPC without one does", rows[m])
    if m.startswith("http "):
        if case in ("http/genesis", "http/snapshot"):
            return entry(d, "kept: this chain has no Agave genesis archive and no Agave snapshot, because CometBFT state sync does both jobs, so it answers 404 as Agave does for a file it does not hold", LABEL["http"])
        return None
    if m in ("getVoteAccounts", "getInflationGovernor", "getInflationRate"):
        return entry(d, "kept: there are no vote accounts, no stake and no inflation here, and the test validator has all three", LABEL["excluded"])
    if m == "getClusterNodes":
        return entry(d, "kept: there is no gossip, shred, vote or repair port here, and this software is none of Agave's client ids", LABEL["leaders"])
    if m == "getBlockProduction" and ("byIdentity" in path or path.endswith("range.firstSlot")):
        return entry(d, "kept: slot 0 is genesis and holds no block here, and a leader's identity is a key name that no rule normalizes", LABEL["leaders"])
    if m == "slotsUpdatesSubscribe" and path == "notifications":
        return entry(d, "kept: a commit is completed, createdBank, optimisticConfirmation and root at once, and frozen, whose stats count PoH entries, is never sent", LABEL["leaders"])
    if m == "signatureSubscribe" and case == "signatureSubscribe/received" and path == "notifications":
        return entry(d, "kept: a single test validator leads every slot and receives no shreds, so it never sends receivedSignature; this machine sends it when the block reaches it", LABEL["subs"])
    if m in ("getTransaction", "getBlock"):
        if any(k in path for k in ("result.rewards", "result.signatures")) or \
                (m == "getBlock" and path == "result.transactions"):
            return entry(d, "kept: Agave's blocks carry vote transactions and fee rewards; this chain has no vote accounts", LABEL["kept"])
        if path == "result.transactionIndex":
            return entry(d, "kept: an Agave block holds a vote transaction before this one; this chain has no vote accounts", LABEL["kept"])
        if path.endswith(".costUnits"):
            return entry(d, "kept: costUnits is this chain's cost against the block limit, on its own scale", LABEL["cost"])
        return None
    if "zstd" in case:
        return entry(d, "kept: the two compressors write different bytes for the same data, and both decompress to it", LABEL["kept"])
    return None


out, unknown = [], []
for d in diffs:
    e = classify(d)
    (out if e else unknown).append(e or d)
for d in unknown:
    print("UNCLASSIFIED", d["case"], d["path"], json.dumps(d.get("agave"))[:80], json.dumps(d.get("chain"))[:80], file=sys.stderr)
if unknown:
    sys.exit(f"{len(unknown)} differences match no rule; divergences.json is unchanged")
(HERE / "divergences.json").write_text(json.dumps(out, indent=1) + "\n")
