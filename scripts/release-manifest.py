#!/usr/bin/env python3
"""Prints an svmd release manifest from the binary's own report. Every field
but the target comes from the binary, and an empty one fails the release.

    svmd version --long --output json | python3 scripts/release-manifest.py TARGET
"""
import json
import sys

info = json.load(sys.stdin)
extra = info.get("extra_info") or {}
manifest = {
    "version": info.get("version"),
    "commit": info.get("commit"),
    "target": sys.argv[1],
    "network": extra.get("network"),
    "engine_digest": extra.get("engine_digest"),
}
for key, value in manifest.items():
    if not value:
        sys.exit("release: the binary reports no %s" % key)
for key, value in manifest.items():
    print("%s: %s" % (key, value))
