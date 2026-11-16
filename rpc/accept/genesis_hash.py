"""Prints the base58 sha256 of a genesis file, the value that getGenesisHash serves."""
import hashlib
import sys

ALPHABET = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"
digest = hashlib.sha256(open(sys.argv[1], "rb").read()).digest()
n = int.from_bytes(digest, "big")
out = ""
while n:
    n, r = divmod(n, 58)
    out = ALPHABET[r] + out
print("1" * (len(digest) - len(digest.lstrip(b"\x00"))) + out)
