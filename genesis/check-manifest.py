#!/usr/bin/env python3
"""Re-read mainnet and report drift against genesis/manifest.json.

    python3 genesis/check-manifest.py            # report drift, always exit 0

Genesis downloads each program from the release that its manifest row names.
Memo v1 always shows a difference, because no release carries the build that
mainnet runs. Any other difference is a program that mainnet upgraded after
the pin. Genesis reads a mainnet row from mainnet when it builds, so a
difference in that row also stops a new genesis until the pin moves.

The exit status is 0 either way. A program that mainnet upgrades is news about
mainnet, not a defect in this repository. A red build that nobody can fix
teaches people to ignore red builds. The script has no --update option. A
person moves a pin to a released build by hand, with its url and sha256
together.

The manifest is the single source for every pinned value. This script does not
restate it.
"""

import argparse
import base64
import hashlib
import json
import os
import struct
import sys
import time
import urllib.request

HERE = os.path.dirname(os.path.abspath(__file__))
MANIFEST = os.path.join(HERE, "manifest.json")
ENDPOINT = os.environ.get("SOLANA_RPC", "https://api.mainnet-beta.solana.com")

LOADER_V1 = "BPFLoader1111111111111111111111111111111111"
LOADER_V2 = "BPFLoader2111111111111111111111111111111111"
LOADER_V3 = "BPFLoaderUpgradeab1e11111111111111111111111"

# solana-loader-v3-interface-7.0.0
SIZE_OF_PROGRAM = 36
SIZE_OF_PROGRAMDATA_METADATA = 45

B58 = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"


def b58encode(raw):
    n = int.from_bytes(raw, "big")
    out = ""
    while n:
        n, r = divmod(n, 58)
        out = B58[r] + out
    return "1" * (len(raw) - len(raw.lstrip(b"\0"))) + out


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


def account(address):
    res = rpc("getAccountInfo", [address, {"encoding": "base64", "commitment": "finalized"}])
    value = (res.get("result") or {}).get("value")
    if value is None:
        raise SystemExit("account %s does not exist on %s" % (address, ENDPOINT))
    return value, base64.b64decode(value["data"][0])


def elf_end(data):
    """Return the offset where the ELF ends, from its section header table.

    A loader v3 ProgramData account has the size that the deployer reserved,
    not the size of the ELF, so the tail can be padding. observe checks that
    every byte past this offset is zero before it drops them.
    """
    shoff = struct.unpack_from("<Q", data, 0x28)[0]
    shentsize = struct.unpack_from("<H", data, 0x3A)[0]
    shnum = struct.unpack_from("<H", data, 0x3C)[0]
    end = shoff + shentsize * shnum
    for i in range(shnum):
        off = shoff + i * shentsize
        sh_type = struct.unpack_from("<I", data, off + 4)[0]
        sh_offset = struct.unpack_from("<Q", data, off + 0x18)[0]
        sh_size = struct.unpack_from("<Q", data, off + 0x20)[0]
        if sh_type != 8:  # SHT_NOBITS occupies no file space
            end = max(end, sh_offset + sh_size)
    return end


def observe(address):
    """Return the live (loader, elf_bytes, upstream_authority) for one program."""
    value, data = account(address)
    owner = value["owner"]
    if owner != LOADER_V3:
        return owner, data, None
    programdata = b58encode(data[4:SIZE_OF_PROGRAM])
    _, pd = account(programdata)
    body = pd[SIZE_OF_PROGRAMDATA_METADATA:]
    authority = b58encode(pd[13:45]) if pd[12] == 1 else None
    end = elf_end(body)
    if any(body[end:]):
        raise SystemExit("%s: %d non-zero bytes past the ELF, refusing to trim"
                         % (address, len(body) - end))
    return owner, body[:end], authority


def mainnet_program(pin):
    """Return the drift notes for a mainnet row.

    It compares the executable hash, which is the sha256 of the ELF without
    trailing zeros as solana-verify reports it, and the ProgramData's deploy
    slot.
    """
    _, pd = account(pin["programdata"])
    digest = hashlib.sha256(pd[SIZE_OF_PROGRAMDATA_METADATA:].rstrip(b"\0")).hexdigest()
    slot = int.from_bytes(pd[4:12], "little")
    notes = []
    if digest != pin["executable_sha256"]:
        notes.append("EXECUTABLE SHA256 %s -> %s" % (pin["executable_sha256"][:16], digest[:16]))
    if slot != pin["deploy_slot"]:
        notes.append("DEPLOY SLOT %d -> %d" % (pin["deploy_slot"], slot))
    return notes


def main():
    argparse.ArgumentParser(description=__doc__.splitlines()[0]).parse_args()

    manifest = json.load(open(MANIFEST))
    slot = rpc("getSlot", [{"commitment": "finalized"}])["result"]
    version = rpc("getVersion", [])["result"]
    print("pinned at slot %s (%s)" % (manifest["mainnet_slot"], manifest["pinned_date"]))
    print("mainnet now slot %s, solana-core %s, feature-set %s"
          % (slot, version.get("solana-core"), version.get("feature-set")))
    print()

    drift = 0
    for entry in manifest["programs"]:
        if "mainnet" in entry:
            notes = mainnet_program(entry["mainnet"])
            drift += len(notes)
            print("%-14s %-46s %s" % (entry["name"], entry["address"],
                                      "ok" if not notes else "; ".join(notes)))
            continue
        owner, elf, authority = observe(entry["address"])
        digest = hashlib.sha256(elf).hexdigest()
        notes = []
        if owner != entry["loader"]:
            notes.append("LOADER %s -> %s" % (entry["loader"], owner))
        if digest != entry["sha256"]:
            notes.append("SHA256 %s -> %s (%d -> %d bytes)"
                         % (entry["sha256"][:16], digest[:16], entry["bytes"], len(elf)))
        if authority != entry["upstream_upgrade_authority"]:
            notes.append("AUTHORITY %s -> %s" % (entry["upstream_upgrade_authority"], authority))
        drift += len(notes)
        print("%-14s %-46s %s" % (entry["name"], entry["address"],
                                  "ok" if not notes else "; ".join(notes)))
    for entry in manifest.get("accounts", []):
        if "mainnet" not in entry:
            continue
        value, data = account(entry["address"])
        notes = []
        if value["owner"] != entry["owner"]:
            notes.append("OWNER %s -> %s" % (entry["owner"], value["owner"]))
        digest = hashlib.sha256(data).hexdigest()
        if digest != entry["mainnet"]["data_sha256"]:
            notes.append("DATA SHA256 %s -> %s" % (entry["mainnet"]["data_sha256"][:16], digest[:16]))
        drift += len(notes)
        print("%-14s %-46s %s" % (entry["name"], entry["address"],
                                  "ok" if not notes else "; ".join(notes)))
    print()
    print("%d field(s) differ from the pin" % drift)
    return 0


if __name__ == "__main__":
    sys.exit(main())
