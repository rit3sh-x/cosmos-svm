#!/usr/bin/env bash
# Regenerate x/svm/tx.pb.go from proto/svm/v1/tx.proto.
#
# protoc is the only additional code-generation tool (provided by the
# development flake or protobuf-compiler). The Go generator comes from
# the gogoproto version pinned in go.mod. Imported Cosmos SDK protos
# come from the Cosmos SDK module pinned in go.mod.
#
# This keeps generated code consistent with the libraries svmd links.

set -euo pipefail

cd "$(dirname "$0")/.."

fail() {
    printf 'gen-proto: %s\n' "$*" >&2
    exit 1
}

# Check external tools before doing any work.
for tool in go protoc; do
    command -v "$tool" >/dev/null 2>&1 ||
        fail "required command not found: $tool"
done

readonly proto_file=proto/svm/v1/tx.proto
readonly output_file=x/svm/tx.pb.go

[[ -f "$proto_file" ]] || fail "missing input: $proto_file"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

# Build the generator from the version pinned in go.mod.
go build -o "$tmp/protoc-gen-gocosmos" github.com/cosmos/gogoproto/protoc-gen-gocosmos

# Resolve imported Cosmos SDK protos from the pinned Go module.
sdk_dir=$(go list -m -f '{{.Dir}}' github.com/cosmos/cosmos-sdk) ||
    fail "could not resolve the Cosmos SDK module"

[[ -d "$sdk_dir/proto" ]] ||
    fail "Cosmos SDK proto directory not found: $sdk_dir/proto"

# Generate into a temporary directory to avoid modifying the output
# if protoc fails.
protoc \
    -I proto \
    -I "$sdk_dir/proto" \
    --plugin=protoc-gen-gocosmos="$tmp/protoc-gen-gocosmos" \
    --gocosmos_out=plugins=grpc,paths=source_relative:"$tmp" \
    "$proto_file"

generated="$tmp/svm/v1/tx.pb.go"

[[ -s "$generated" ]] ||
    fail "protoc did not produce a valid output: $generated"

# Replace the checked-in generated file only after successful generation.
cp "$generated" "$output_file"

printf 'gen-proto: updated %s\n' "$output_file"
