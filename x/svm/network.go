//go:build !devnet

package svm

// Network is what this binary is. It is a compile-time constant, declared
// twice under opposite build tags, not a variable, a flag or a config key.
// There is nothing to set and nothing to get wrong. A production build cannot
// be told it is a devnet.
//
// The engine digest cannot carry this. digest.rs hashes file bytes, and a
// build tag changes no file, so both builds report the same ENGINE_DIGEST.
// That is correct, because the engine is identical. The guard is genesis state
// instead.
const Network = "production"
