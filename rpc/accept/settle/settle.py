"""The Python steps of rpc/accept/settle.sh.

  settle.py squads-config SRC DST NOBODY   Squads v4's global config for Agave's side
  settle.py e2e-result LOG STATUS          the e2e's one result line, for gate.py
"""
import base64
import json
import re
import sys

B58 = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"


def squads_config(src, dst, nobody):
    """Writes DST as a copy of the account file SRC, which `solana account
    --output json` wrote. The copy has a fee of 0, and its authority and
    treasury are NOBODY, an address that nobody can sign for. ProgramConfig is
    an 8-byte discriminator, the authority, multisig_creation_fee (u64), the
    treasury, then reserved bytes."""
    n = 0
    for c in nobody:
        n = n * 58 + B58.index(c)
    key = n.to_bytes(32, "big")
    d = json.load(open(src))
    data = bytearray(base64.b64decode(d["account"]["data"][0]))
    data[8:40] = key
    data[40:48] = bytes(8)
    data[48:80] = key
    d["account"]["data"][0] = base64.b64encode(bytes(data)).decode()
    json.dump(d, open(dst, "w"))


def e2e_result(log, status):
    """Prints the e2e result: whether it passed, and the lines of LOG that
    tell why it failed. It skips the stack offset warnings of cargo-build-sbf,
    which say "Error" but fail nothing. The client message and the panics go
    first, because the gate report quotes them."""
    text, status = open(log, errors="replace").read(), int(status)
    said = [l for l in re.sub(r"\x1b\[[0-9;]*m", "", text).splitlines()
            if re.search(r"panicked|Error|error:|not a recent slot|failed", l) and "Stack offset" not in l]
    said.sort(key=lambda l: not re.search(r"message: |panicked at", l))
    print(json.dumps({"test": "the sealevel e2e passes", "ok": status == 0,
                      "error": "" if status == 0 else "\n".join(said[:40]) or f"exit status {status}"}))


if __name__ == "__main__":
    mode, args = sys.argv[1], sys.argv[2:]
    {"squads-config": squads_config, "e2e-result": e2e_result}[mode](*args)
