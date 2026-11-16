"""The JSON-RPC assertions of rpc.sh and devnet.sh. Each assertion is a command.

    python3 checks.py fees URL
    python3 checks.py tokens URL OWNER MINT ATA
    python3 checks.py program-accounts URL MINT
    python3 checks.py held MINT                 < spl-token accounts --output json
    python3 checks.py token-amount              < solana account --output json
    python3 checks.py signature                 < solana airdrop --output json
    python3 checks.py airdrop-caps URL DEST LOG
    python3 checks.py faucet-metrics URL

A command prints one line on success. A failed assertion exits nonzero and
prints what it saw.
"""

import base64
import json
import sys
import urllib.request

TOKEN = "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"


def post(url, method, params):
    body = json.dumps({"jsonrpc": "2.0", "id": 1, "method": method, "params": params}).encode()
    req = urllib.request.Request(url, body, {"Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=60) as r:
        return json.load(r)


def call(url, method, params):
    out = post(url, method, params)
    assert "error" not in out, out
    return out["result"]


def cmd_fees(url):
    """Checks that getRecentPrioritizationFees has mainnet's shape and window."""
    rows = call(url, "getRecentPrioritizationFees", [[]])
    assert rows, "no block reported a prioritization fee"
    assert len(rows) <= 150, len(rows)
    slots = [r["slot"] for r in rows]
    assert slots == sorted(slots), slots
    assert all(set(r) == {"slot", "prioritizationFee"} for r in rows), rows[0]
    print("getRecentPrioritizationFees: %d blocks, slots %d to %d" % (len(rows), slots[0], slots[-1]))


def cmd_tokens(url, owner, mint, ata):
    """Checks the owner index and getTokenAccountBalance."""
    by_program = call(url, "getTokenAccountsByOwner", [owner, {"programId": TOKEN}, {"encoding": "base64"}])
    by_mint = call(url, "getTokenAccountsByOwner", [owner, {"mint": mint}, {"encoding": "base64"}])
    assert any(a["pubkey"] == ata for a in by_program["value"]), by_program
    assert [a["pubkey"] for a in by_mint["value"]] == [ata], by_mint
    balance = call(url, "getTokenAccountBalance", [ata])["value"]
    want = {"amount": "750000000000", "decimals": 9, "uiAmount": 750.0, "uiAmountString": "750"}
    assert balance == want, balance
    print("getTokenAccountsByOwner: %d owned, 1 of mint; getTokenAccountBalance: 750" % len(by_program["value"]))


def cmd_program_accounts(url, mint):
    """Checks getProgramAccounts over the real SPL Token accounts, in the
    request shapes that Hyperlane's Sealevel adapter sends."""
    owned = call(url, "getProgramAccounts", [TOKEN, {"encoding": "base64"}])
    assert len(owned) == 3, f"the Token program owns {len(owned)} accounts, want the mint and two token accounts"

    by_mint = call(url, "getProgramAccounts", [TOKEN, {"encoding": "base64", "filters": [
        {"dataSize": 165}, {"memcmp": {"offset": 0, "bytes": mint}}]}])
    assert len(by_mint) == 2, f"the mint filter returned {len(by_mint)}, want 2"
    for a in by_mint:
        data = base64.b64decode(a["account"]["data"][0])
        assert len(data) == 165 and data[:32] == base64.b64decode(by_mint[0]["account"]["data"][0])[:32]

    sliced = call(url, "getProgramAccounts", [TOKEN, {"encoding": "base64",
        "dataSlice": {"offset": 64, "length": 8},
        "filters": [{"memcmp": {"offset": 0, "bytes": mint}}]}])
    total = sum(int.from_bytes(base64.b64decode(a["account"]["data"][0]), "little") for a in sliced)
    assert total == 1000 * 10**9, f"the sliced amounts total {total}"

    slot = call(url, "getSlot", [])
    assert call(url, "isBlockhashValid", [call(url, "getLatestBlockhash", [])["value"]["blockhash"]])["value"] is True
    print(f"getProgramAccounts: {len(owned)} owned, {len(by_mint)} of mint {mint}, sliced total {total} at slot {slot}")


def cmd_held(mint):
    """Checks that `spl-token accounts`, which reads jsonParsed over the owner
    index, shows one account of mint with 750 in it."""
    rows = json.load(sys.stdin)["accounts"]
    held = [r for r in rows if r["mint"] == mint]
    assert len(held) == 1, rows
    assert held[0]["tokenAmount"]["uiAmountString"] == "750", held
    print("spl-token accounts: jsonParsed over the owner index")


def cmd_token_amount():
    """Prints the amount of a token account: bytes 64 to 72 of its data."""
    data = base64.b64decode(json.load(sys.stdin)["account"]["data"][0])
    print(int.from_bytes(data[64:72], "little"))


def cmd_signature():
    """Prints the signature from `solana airdrop --output json`, which prints
    the JSON object between two lines of prose."""
    text = sys.stdin.read()
    print(json.loads(text[text.index("{"):text.rindex("}") + 1])["signature"])


def cmd_airdrop_caps(url, dest, log):
    """Checks the faucet caps directly. For a refusal the client sees only
    Agave's -32603 Internal error. The check reads the cap that refused
    from the svmd log."""

    def refused_by(got, cause):
        assert got.get("error") == {"code": -32603, "message": "Internal error"}, got
        # svmd writes the log line before the answer, so the line is there to read.
        assert any("the faucet refused an airdrop" in line and cause in line for line in open(log)), cause

    def airdrop(lamports):
        return post(url, "requestAirdrop", [dest, lamports])

    refused_by(airdrop(1000 * 10**9), "per-request cap")
    print("requestAirdrop: the per-request cap refuses as Agave's Internal error, named in the log, and sends nothing")

    # The recipient row still applies to a loopback caller, which is exempt
    # only from the caller row. The 10 SOL airdrop in devnet.sh was the first
    # request of the minute for this recipient, so four more reach the default
    # count of five.
    #
    # The amounts differ because two identical airdrops inside one blockhash
    # are one transaction. The same payer, recipient and lamports give the same
    # signature, and the dedup set refuses it. Agave has the same property. It
    # is a problem for the recipient, not for the faucet.
    for extra in (0, 100, 200, 300):
        got = airdrop(10**9 + extra * 10**6)
        assert "result" in got, got
    refused_by(airdrop(10**9 + 400 * 10**6), "requests")
    print("requestAirdrop: the minute row refuses the sixth request to one recipient")


def cmd_faucet_metrics(url):
    """Checks the faucet gauges on the registry that the CometBFT metrics
    server serves."""
    lines = urllib.request.urlopen(url).read().decode().splitlines()
    series = {line.split()[0]: float(line.split()[1]) for line in lines if line.startswith("svm_faucet")}
    assert set(series) == {"svm_faucet_lamports", "svm_faucet_floor_lamports"}, series
    assert series["svm_faucet_lamports"] > series["svm_faucet_floor_lamports"], series
    print("metrics: svm_faucet_lamports %d beside svm_faucet_floor_lamports %d"
          % (series["svm_faucet_lamports"], series["svm_faucet_floor_lamports"]))


if __name__ == "__main__":
    commands = {name[4:].replace("_", "-"): fn for name, fn in list(globals().items()) if name.startswith("cmd_")}
    if len(sys.argv) < 2 or sys.argv[1] not in commands:
        raise SystemExit("usage: checks.py {%s} ..." % "|".join(sorted(commands)))
    commands[sys.argv[1]](*sys.argv[2:])
