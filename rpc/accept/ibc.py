"""Reads values for ibc.sh out of the JSON output of stock simd. Each command
reads that output on stdin.

    python3 ibc.py balance DENOM   < simd query bank balances --output json
    python3 ibc.py txhash          < simd tx ... --output json
    python3 ibc.py landed          < simd query tx HASH --output json
    python3 ibc.py proof-height SEQ < simd query txs --query ... --output json

A command exits nonzero with the log of a failed transaction.
"""

import json
import sys


def cmd_balance(denom):
    """Prints the amount of one denom, or 0 if the account does not hold it."""
    coins = json.load(sys.stdin).get("balances", [])
    print(next((c["amount"] for c in coins if c["denom"] == denom), "0"))


def cmd_txhash():
    """Prints the hash of a transaction that simd broadcast. The answer is the
    last line, because gas estimation prints before it."""
    r = json.loads(sys.stdin.read().splitlines()[-1])
    assert r["code"] == 0, r["raw_log"]
    print(r["txhash"])


def cmd_landed():
    """Prints the block height of a transaction and the sequence of the packet
    it sent."""
    r = json.load(sys.stdin)
    assert r["code"] == 0, r["raw_log"]
    seq = [a["value"] for e in r["events"] if e["type"] == "send_packet"
           for a in e["attributes"] if a["key"] == "packet_sequence"]
    print(r["height"], seq[0])


def cmd_proof_height(seq):
    """Prints the proof height that the MsgRecvPacket for packet seq carried."""
    for tx in json.load(sys.stdin)["txs"]:
        for m in tx["tx"]["body"]["messages"]:
            if m["@type"].endswith("MsgRecvPacket") and m["packet"]["sequence"] == seq:
                print(m["proof_height"]["revision_height"])
                return


if __name__ == "__main__":
    commands = {name[4:].replace("_", "-"): fn for name, fn in list(globals().items()) if name.startswith("cmd_")}
    if len(sys.argv) < 2 or sys.argv[1] not in commands:
        raise SystemExit("usage: ibc.py {%s} ..." % "|".join(sorted(commands)))
    commands[sys.argv[1]](*sys.argv[2:])
