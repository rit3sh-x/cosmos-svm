#!/usr/bin/env bash
# Build the release artifacts: svmd, svmd-devnet and svm-tpu, each with a
# manifest beside it. The flavor is in the basename, so no two artifacts share
# a name.
#
#   scripts/release-build.sh [output directory]
#
# VERSION and COMMIT default to the tag and the commit at HEAD. `make release`
# runs release-preflight.sh first, which checks that both exist.
set -euo pipefail
cd "$(dirname "$0")/.."
OUT=${1:-dist}
VERSION=${VERSION:-$(git describe --tags --exact-match)}
COMMIT=${COMMIT:-$(git rev-parse HEAD)}
TARGET=linux/amd64
SDK=github.com/cosmos/cosmos-sdk/version

mkdir -p "$OUT"
for name in svmd svmd-devnet; do
    tags=""
    [ "$name" = svmd ] || tags=devnet
    artifact="$OUT/$name-$VERSION-${TARGET/\//-}"
    go build -tags "$tags" -o "$artifact" \
        -ldflags "-X $SDK.Name=svm -X $SDK.AppName=svmd -X $SDK.Version=$VERSION -X $SDK.Commit=$COMMIT -X $SDK.BuildTags=$tags" \
        ./cmd/svmd
    # The manifest comes from the binary that it names, so it cannot describe a
    # build that does not exist.
    "$artifact" version --long --output json | python3 scripts/release-manifest.py "$TARGET" >"$artifact.yml"
    echo "release: $(basename "$artifact") and its manifest"
done

# svm-tpu has no devnet difference, so both networks run this one build. Its
# manifest names the streamer it wraps, as its own lockfile pins it.
cargo build --release --locked --manifest-path tpu/Cargo.toml
artifact="$OUT/svm-tpu-$VERSION-${TARGET/\//-}"
cp tpu/target/release/svm-tpu "$artifact"
streamer=$(cargo pkgid --locked --manifest-path tpu/Cargo.toml solana-streamer)
{
    echo "version: $VERSION"
    echo "commit: $COMMIT"
    echo "target: $TARGET"
    echo "solana_streamer: ${streamer##*@}"
    echo "sha256: $(sha256sum "$artifact" | cut -c1-64)"
} >"$artifact.yml"
echo "release: $(basename "$artifact") and its manifest"
