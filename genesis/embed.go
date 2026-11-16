// Package genesis holds the pinned inputs that a genesis file is built from.
package genesis

import _ "embed"

// Features is features.json. It lists every gate in the pinned
// agave-feature-set with its class and its mainnet activation at the
// manifest's slot. pin-features.py writes it.
//
//go:embed features.json
var Features []byte

// Exceptions is exceptions.json. It lists the gates that genesis activates
// although the pin has them inactive. Unlike features.json, it is hand-written.
// The genesis builder, the differential and the drift report all read it, so
// they cannot disagree about which gates this chain runs.
//
//go:embed exceptions.json
var Exceptions []byte
