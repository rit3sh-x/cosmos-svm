package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"fmt"
	"os"
	"strings"

	"cosmossdk.io/math"

	"github.com/cosmos/cosmos-sdk/codec"
	"github.com/cosmos/cosmos-sdk/crypto/keys/ed25519"
	cryptotypes "github.com/cosmos/cosmos-sdk/crypto/types"
	sdk "github.com/cosmos/cosmos-sdk/types"

	"github.com/rit3sh-x/cosmos-svm/app"
)

// The flags of `init`. flagValidator and flagValidatorsFile are the two
// operator input forms, after `solana-genesis`. That tool has a repeated
// `--bootstrap-validator` and a `--validator-accounts-file`, and sends both
// into one builder. Here both go to `operators`, and from there to
// `app.Permission`, which takes the whole set.
//
// Neither form carries a signature, as neither of Agave's forms does. Only
// the operators hold the bond denom, so whoever writes the genesis chooses
// the set by choosing whom to mint it to. A signature would prove only that
// the operator was reachable while the genesis was built.
const (
	flagValidator          = "validator"
	flagValidatorsFile     = "validators-file"
	flagEphemeralValidator = "ephemeral-validator"
	flagVRFOracle          = "vrf-oracle"
)

// validatorsFile is the `--validators-file` document. It is snake_case JSON
// in the style of manifest.json, because the repository already has a
// machine-checked JSON genesis input, and a second format adds nothing.
// Agave's own file is YAML, and this is the one place the format differs
// from Agave.
type validatorsFile struct {
	Validators []validatorEntry `json:"validators"`
}

// validatorEntry is app.Operator with its two key fields in their text forms.
// The file format is this struct.
type validatorEntry struct {
	// ConsensusPubkey is the output of `svmd comet show-validator` verbatim,
	// the Any JSON object. A bare base64 string is also accepted, because the
	// flag form cannot hold that object.
	ConsensusPubkey jsontext.Value `json:"consensus_pubkey"`
	Account         string         `json:"account"`
	SelfBond        math.Int       `json:"self_bond"`
	Free            math.Int       `json:"free"`
	Moniker         string         `json:"moniker"`
}

// operators reads both input forms into the list that app.Permission takes.
// No flags and no file is not an error. It means that the one operator is
// this machine, and finishGenesis then derives it.
func operators(cdc codec.Codec, tuples []string, path string) ([]app.Operator, error) {
	var entries []validatorEntry
	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		var file validatorsFile
		if err := json.Unmarshal(raw, &file, json.RejectUnknownMembers(true)); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		entries = append(entries, file.Validators...)
	}
	for _, tuple := range tuples {
		entry, err := parseTuple(tuple)
		if err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}

	out := make([]app.Operator, 0, len(entries))
	pubkeys := map[string]int{}
	accounts := map[string]int{}
	for i, entry := range entries {
		pubkey, err := consensusPubkey(cdc, entry.ConsensusPubkey)
		if err != nil {
			return nil, fmt.Errorf("operator %d: consensus pubkey: %w", i, err)
		}
		account, err := sdk.AccAddressFromBech32(entry.Account)
		if err != nil {
			return nil, fmt.Errorf("operator %d: account: %w", i, err)
		}
		if entry.SelfBond.IsNil() || entry.Free.IsNil() {
			return nil, fmt.Errorf("operator %d: self_bond and free are both required", i)
		}
		if entry.Free.IsNegative() {
			return nil, fmt.Errorf("operator %d: free is %s", i, entry.Free)
		}
		// Agave refuses a duplicate pubkey by name and exits non-zero, and so
		// does svmd. app.Permission refuses a self-bond below one unit of
		// voting power, which is this chain's analogue of Agave's rent-exempt
		// floor.
		fingerprint := base64.StdEncoding.EncodeToString(pubkey.Bytes())
		if prior, ok := pubkeys[fingerprint]; ok {
			return nil, fmt.Errorf("operators %d and %d name the same consensus pubkey", prior, i)
		}
		if prior, ok := accounts[entry.Account]; ok {
			return nil, fmt.Errorf("operators %d and %d name the same account %s", prior, i, entry.Account)
		}
		pubkeys[fingerprint] = i
		accounts[entry.Account] = i

		out = append(out, app.Operator{
			Account:         account,
			ConsensusPubkey: pubkey,
			SelfBond:        entry.SelfBond,
			Free:            entry.Free,
			Moniker:         entry.Moniker,
		})
	}
	return out, nil
}

// parseTuple reads one `--validator` value. Cobra has no direct equivalent of
// Agave's space-separated `number_of_values(3)`. The value is instead a
// comma-separated tuple in a StringArray, which cobra does not split. The
// moniker is last and takes the rest, so it can contain commas.
func parseTuple(s string) (validatorEntry, error) {
	fields := strings.SplitN(s, ",", 5)
	if len(fields) != 5 {
		return validatorEntry{}, fmt.Errorf("--validator %q: want consensus-pubkey,account,self-bond,free,moniker", s)
	}
	selfBond, ok := math.NewIntFromString(fields[2])
	if !ok {
		return validatorEntry{}, fmt.Errorf("--validator %q: self-bond %q is not a number", s, fields[2])
	}
	free, ok := math.NewIntFromString(fields[3])
	if !ok {
		return validatorEntry{}, fmt.Errorf("--validator %q: free %q is not a number", s, fields[3])
	}
	// The flag cannot carry the Any JSON that `comet show-validator` prints,
	// because that object holds commas. The flag carries the base64 key alone,
	// and this code quotes it into the same JSON field that the file fills.
	pubkey, err := json.Marshal(fields[0], json.Deterministic(true))
	if err != nil {
		return validatorEntry{}, err
	}
	return validatorEntry{
		ConsensusPubkey: pubkey,
		Account:         fields[1],
		SelfBond:        selfBond,
		Free:            free,
		Moniker:         fields[4],
	}, nil
}

// consensusPubkey accepts two forms:
//   - the Any JSON object that `svmd comet show-validator` prints, which a
//     file should hold
//   - a bare base64 ed25519 key, the only form the flag can carry
func consensusPubkey(cdc codec.Codec, raw jsontext.Value) (cryptotypes.PubKey, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil, fmt.Errorf("missing")
	}
	if raw[0] == '{' {
		var pubkey cryptotypes.PubKey
		if err := cdc.UnmarshalInterfaceJSON(raw, &pubkey); err != nil {
			return nil, err
		}
		return pubkey, nil
	}
	var encoded string
	if err := json.Unmarshal(raw, &encoded); err != nil {
		return nil, err
	}
	key, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, err
	}
	if len(key) != ed25519.PubKeySize {
		return nil, fmt.Errorf("%d bytes, want %d", len(key), ed25519.PubKeySize)
	}
	return &ed25519.PubKey{Key: key}, nil
}
