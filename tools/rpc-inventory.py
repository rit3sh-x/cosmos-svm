#!/usr/bin/env python3
"""Write rpc/testdata/inventory.json.

The file lists every JSON-RPC method and websocket subscription that Agave
serves at AGAVE. Each method carries its registration set. Each method and
websocket call carries its required and total positional parameter counts.

The names come from Agave's rpc crate, published as solana-rpc. The script
downloads that crate from crates.io and checks it against CRATE_SHA256:
  - src/rpc.rs declares each method with #[rpc(meta, name = "...")] inside
    one of five modules, rpc_minimal, rpc_bank, rpc_accounts,
    rpc_accounts_scan and rpc_full.
  - src/rpc_pubsub.rs declares each subscription with name = "...Subscribe".

On an Agave bump, set AGAVE and CRATE_SHA256 (crates.io lists the checksum),
then run `make inventory`.
"""
import hashlib
import io
import json
import pathlib
import re
import sys
import tarfile
import urllib.request

AGAVE = "4.3.0"
CRATE_SHA256 = "69fda2e711ff62027438877f34177983f5429d2d4d7433cac035d6f07801c45e"
OUT = pathlib.Path(__file__).resolve().parent.parent / "rpc/testdata/inventory.json"

url = f"https://static.crates.io/crates/solana-rpc/solana-rpc-{AGAVE}.crate"
crate = urllib.request.urlopen(urllib.request.Request(url, headers={"User-Agent": "rpc-inventory"})).read()
if hashlib.sha256(crate).hexdigest() != CRATE_SHA256:
    sys.exit(f"{url} does not match CRATE_SHA256")
archive = tarfile.open(fileobj=io.BytesIO(crate))


def source(path):
    return archive.extractfile(f"solana-rpc-{AGAVE}/{path}").read().decode()


sets = {
    "rpc_minimal": "minimal",
    "rpc_bank": "bank",
    "rpc_accounts": "accounts",
    "rpc_accounts_scan": "accounts-scan",
    "rpc_full": "full"
}


def arity(source, at):
    """Return the required and total positional parameter counts of the fn
    declared after offset at. The counts leave out &self and meta."""
    sig = re.compile(r"fn \w+(?:<[^>]*>)?\s*\((.*?)\)\s*->", re.S).search(source, at)
    args, depth, cur = [], 0, ""
    for ch in sig.group(1):
        depth += ch in "<([" and 1 or ch in ">)]" and -1 or 0
        if ch == "," and depth == 0:
            args.append(cur.strip())
            cur = ""
        else:
            cur += ch
    args.append(cur.strip())
    types = [a.split(":", 1)[1].strip() for a in args if ":" in a and not a.startswith("meta")]
    trailing = 0
    for t in reversed(types):
        if not t.startswith("Option<"):
            break
        trailing += 1
    return len(types) - trailing, len(types)


rpc = source("src/rpc.rs")
methods, current = [], None
for m in re.finditer(r'^pub mod (rpc_\w+) \{|#\[rpc\(meta, name = "(\w+)"\)\]', rpc, re.M):
    if m.group(1):
        current = sets.get(m.group(1))
    elif current:
        required, total = arity(rpc, m.end())
        methods.append({
            "name": m.group(2),
            "set": current,
            "required": required,
            "total": total
        })

pubsub = source("src/rpc_pubsub.rs")
subscriptions = sorted(set(re.findall(r'name = "(\w+Subscribe)"', pubsub)))
trait = pubsub[pubsub.index("pub trait RpcSolPubSubInternal"):]
websocket = []
for m in re.finditer(r'#\[rpc\(name = "(\w+)"\)\]', trait):
    required, total = arity(trait, m.end())
    websocket.append({
        "name": m.group(1),
        "required": required,
        "total": total
    })

if len({m["name"] for m in methods}) != len(methods):
    sys.exit("a method is declared twice")
OUT.write_text(json.dumps({
    "agave": AGAVE,
    "methods": methods,
    "subscriptions": subscriptions,
    "websocket": websocket
}, indent=1) + "\n")
