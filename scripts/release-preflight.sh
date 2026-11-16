#!/usr/bin/env bash
# Check that a release builds from a clean, tagged commit.
set -euo pipefail
cd "$(dirname "$0")/.."

if [ -n "$(git status --porcelain)" ]; then
    echo "release: the tree is dirty, so the artifact would not match any commit" >&2
    git status --short >&2
    exit 1
fi
if ! git describe --tags --exact-match >/dev/null 2>&1; then
    echo "release: HEAD is not at a tag, so the artifact would have no version" >&2
    exit 1
fi
echo "release: $(git describe --tags --exact-match) at $(git rev-parse HEAD), clean"
