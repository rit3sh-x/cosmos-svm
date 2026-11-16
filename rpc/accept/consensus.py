"""The CometBFT queries of the four-validator acceptance run and the retention run.

Every query reads the RPC of a running machine. No query restates a chain
constant. The block limit, the timeouts and the heights come from the chain, so
the assertions stay true when a parameter changes.
"""

import base64
import datetime
import http.client
import json
import sys
import time
import urllib.request


def rpc(url, path):
    # Five tries, because a machine on a box deep in swap can drop one request
    # and still commit. The harness's own height() tolerates the same.
    for attempt in range(5):
        try:
            with urllib.request.urlopen("%s/%s" % (url, path), timeout=30) as r:
                out = json.load(r)
            break
        except (OSError, http.client.HTTPException):
            if attempt == 4:
                raise
            time.sleep(2)
    if "error" in out:
        raise SystemExit("%s/%s: %s" % (url, path, out["error"]))
    return out["result"]


def status(url):
    """Returns the height window that one machine can answer for."""
    info = rpc(url, "status")["sync_info"]
    return int(info["earliest_block_height"]), int(info["latest_block_height"])


def metas(url, lo, hi):
    """Returns block metadata by height, read in CometBFT's 20-height pages."""
    out = {}
    while hi >= lo:
        page = rpc(url, "blockchain?minHeight=%d&maxHeight=%d" % (max(lo, hi - 19), hi))
        for meta in page["block_metas"]:
            out[int(meta["header"]["height"])] = meta
        hi -= 20
    return out


def cmd_window(argv):
    """Prints the lowest height that one machine holds, and its tip."""
    print(*status(argv[0]))


def cmd_trust(argv):
    """Prints a trust height and hash for a joiner. The height is `back`
    blocks below the tip, so the block is settled."""
    url, back = argv[0], int(argv[1])
    _, latest = status(url)
    height = max(1, latest - back)
    block = rpc(url, "block?height=%d" % height)
    print(height, block["block_id"]["hash"])


def cmd_agree(argv):
    """Checks that every machine reports the same app hash at every height it
    reports. This is the baseline under every assertion. It runs after each
    assertion, and over the whole run at the end."""
    lo, hi, urls = int(argv[0]), int(argv[1]), argv[2:]
    seen = {}
    windows = []
    for url in urls:
        first, last = status(url)
        windows.append((url, first, last))
        for height, meta in metas(url, max(lo, first), min(hi, last)).items():
            seen.setdefault(height, {})[url] = meta["header"]["app_hash"]
    checked = 0
    for height in sorted(seen):
        hashes = seen[height]
        if len(set(hashes.values())) != 1:
            raise SystemExit("height %d: %s" % (height, json.dumps(hashes, indent=1)))
        checked += 1
    if not checked:
        raise SystemExit("no common height between %d and %d: %s" % (lo, hi, windows))
    covered = sorted(h for h in seen if len(seen[h]) == len(urls))
    if not covered:
        raise SystemExit("no height was reported by all %d machines" % len(urls))
    print("agree: %d machines, %d heights, all %d of them from %d to %d" %
          (len(urls), checked, len(covered), covered[0], covered[-1]))


def cmd_biggest(argv):
    """Prints the largest block committed in a window, with its cost in wall
    time. The interval is the whole round: propose, propagate, prevote,
    precommit and the commit timeout. The fourth field subtracts the commit
    timeout, which leaves the part that moves the block."""
    url, lo, hi = argv[0], int(argv[1]), int(argv[2])
    commit = float(argv[3]) if len(argv) > 3 else 0.0
    got = metas(url, lo, hi)
    if not got:
        raise SystemExit("no block between %d and %d" % (lo, hi))
    top = max(got, key=lambda h: int(got[h]["block_size"]))
    size = int(got[top]["block_size"])
    times = {h: timestamp(got[h]["header"]["time"]) for h in got}
    interval = times[top] - times[top - 1] if top - 1 in times else float("nan")
    print("%d %d %.3f %.3f %d" % (top, size, interval, interval - commit,
                                  int(got[top]["num_txs"])))


def timestamp(text):
    """Parses CometBFT's RFC 3339 time with nanoseconds. datetime cannot parse
    it before Python 3.11 and truncates it after."""
    date, _, clock = text.partition("T")
    clock = clock.rstrip("Z")
    hh, mm, ss = clock.split(":")
    whole, _, frac = ss.partition(".")
    day = datetime.date.fromisoformat(date).toordinal() * 86400
    return day + int(hh) * 3600 + int(mm) * 60 + int(whole) + float("0." + (frac or "0"))


def cmd_round(argv):
    """Prints the consensus round that committed a height, from the
    LastCommit of the next block. Round zero is what "inside the configured
    timeout" means. A block that does not propagate and get votes in time is
    not committed late. CometBFT abandons it, and the next round proposes
    another block."""
    url, height = argv[0], int(argv[1])
    print(rpc(url, "block?height=%d" % (height + 1))["block"]["last_commit"]["round"])


def cmd_params(argv):
    """Prints the configured block limit, read from the chain, not restated."""
    params = rpc(argv[0], "consensus_params")["consensus_params"]["block"]
    print(params["max_bytes"], params["max_gas"])


def cmd_address(argv):
    """Prints a machine's validator address, as CometBFT's commits name it."""
    print(rpc(argv[0], "status")["validator_info"]["address"])


def in_set(url, address, height):
    vals = rpc(url, "validators?height=%d&per_page=100" % height)["validators"]
    return any(v["address"] == address for v in vals)


def signed(url, address, height):
    """Returns whether the commit for height carries the precommit of address
    for the block (BLOCK_ID_FLAG_COMMIT)."""
    sigs = rpc(url, "commit?height=%d" % height)["signed_header"]["commit"]["signatures"]
    return any(s["validator_address"] == address and s["block_id_flag"] == 2 for s in sigs)


def walk(url, start, timeout, done):
    """Returns the first height from start where done holds, as the chain reaches it."""
    deadline = time.time() + timeout
    height = start
    while time.time() < deadline:
        _, latest = status(url)
        while height < latest:
            if done(height):
                return height
            height += 1
        time.sleep(1)
    raise SystemExit("nothing by height %d after %d s" % (height, timeout))


def cmd_jailed(argv):
    """Prints the last height that address signed, and the first height from
    start whose validator set no longer holds address."""
    url, address, start, timeout = argv[0], argv[1], int(argv[2]), int(argv[3])
    gone = walk(url, start, timeout, lambda h: not in_set(url, address, h))
    last = next(h for h in range(gone, 0, -1) if signed(url, address, h))
    print(last, gone)


def cmd_gone(argv):
    """Prints the first height from start whose validator set no longer holds
    address."""
    url, address, start, timeout = argv[0], argv[1], int(argv[2]), int(argv[3])
    print(walk(url, start, timeout, lambda h: not in_set(url, address, h)))


def cmd_evidence(argv):
    """Prints the first height from start whose block carries evidence, and
    how many blocks back from it the evidence height `at` is."""
    url, start, at, timeout = argv[0], int(argv[1]), int(argv[2]), int(argv[3])
    carried = walk(url, start, timeout, lambda h: rpc(url, "block?height=%d" % h)["block"]["evidence"]["evidence"])
    print(carried, carried - at)


def cmd_returns(argv):
    """Prints the first height whose validator set holds address again, and
    the first height from there whose commit carries its signature."""
    url, address, start, timeout = argv[0], argv[1], int(argv[2]), int(argv[3])
    back = walk(url, start, timeout, lambda h: in_set(url, address, h))
    print(back, walk(url, back, timeout, lambda h: signed(url, address, h)))


B58 = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"


def base58(raw):
    n = int.from_bytes(raw, "big")
    out = ""
    while n:
        n, r = divmod(n, 58)
        out = B58[r] + out
    return "1" * (len(raw) - len(raw.lstrip(b"\0"))) + out


def solana(url, method, params):
    body = json.dumps({"jsonrpc": "2.0", "id": 1, "method": method, "params": params}).encode()
    req = urllib.request.Request(url, body, {"Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=30) as r:
        out = json.load(r)
    if "error" in out:
        raise SystemExit("%s %s: %s" % (method, params, out["error"]))
    return out["result"]


def identities(url, height):
    """Returns each validator address with its consensus key in base58, which
    is the leader's name."""
    vals = rpc(url, "validators?height=%d&per_page=100" % height)["validators"]
    return {v["address"]: base58(base64.b64decode(v["pub_key"]["value"])) for v in vals}


def cmd_leaders(argv):
    """Asks the Solana RPC of every machine for the schedule over the last
    `back` heights and the next `ahead`. That range reaches past h+2 into the
    projection. Every machine must name the same leaders. When the heights
    commit, the named leader must have proposed every block decided in round 0.
    In round 0 there is no earlier valid block, so the proposer of the block is
    the round-0 proposer."""
    back, ahead = int(argv[0]), int(argv[1])
    split = argv.index("--")
    cmts, svms = argv[2:split], argv[split + 1:]
    first, latest = status(cmts[0])
    lo = max(first + 1, latest - back)
    count = latest + ahead - lo + 1
    schedules = [solana(url, "getSlotLeaders", [lo, count]) for url in svms]
    if any(s != schedules[0] for s in schedules):
        raise SystemExit("the machines name different leaders from %d" % lo)
    deadline = time.time() + 600
    while status(cmts[0])[1] < lo + count:
        if time.time() > deadline:
            raise SystemExit("the chain did not reach %d" % (lo + count))
        time.sleep(1)
    checked = later = by_leader = 0
    for i, leader in enumerate(schedules[0]):
        height = lo + i
        rnd = int(rpc(cmts[0], "block?height=%d" % (height + 1))["block"]["last_commit"]["round"])
        proposer = identities(cmts[0], height)[
            rpc(cmts[0], "block?height=%d" % height)["block"]["header"]["proposer_address"]]
        by_leader += proposer == leader
        if rnd != 0:
            later += 1
        elif proposer != leader:
            raise SystemExit("height %d: the schedule named %s, the block's proposer is %s" % (height, leader, proposer))
        else:
            checked += 1
    named = len(set(schedules[0]))
    # A block that a later round decided can still be the leader's own block, proposed again.
    production = solana(svms[0], "getBlockProduction", [{"range": {"firstSlot": lo, "lastSlot": lo + count - 1}}])
    slots = sum(e[0] for e in production["value"]["byIdentity"].values())
    made = sum(e[1] for e in production["value"]["byIdentity"].values())
    if slots != count or made != by_leader:
        raise SystemExit("getBlockProduction counts %d slots and %d blocks, want %d and %d" % (slots, made, count, by_leader))
    print("leaders: %d machines agree on %d slots from %d (h was %d), %d leaders named; "
          "%d round-0 blocks by their leader, %d decided in a later round" %
          (len(svms), count, lo, latest, named, checked, later))


if __name__ == "__main__":
    commands = {name[4:]: fn for name, fn in list(globals().items()) if name.startswith("cmd_")}
    if len(sys.argv) < 2 or sys.argv[1] not in commands:
        raise SystemExit("usage: consensus.py {%s} ..." % "|".join(sorted(commands)))
    commands[sys.argv[1]](sys.argv[2:])
