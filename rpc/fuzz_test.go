package rpc

import (
	json "encoding/json/v2"
	"fmt"
	"math"
	"testing"

	"github.com/rit3sh-x/cosmos-svm/ffi"
	"github.com/rit3sh-x/cosmos-svm/x/svm"
)

// FuzzAccountRendering fuzzes the account rendering paths that a client's
// configuration reaches:
//   - the jsonParsed parsers, on any bytes, under each owner they parse
//   - every encoding
//   - a data slice of any offset and length
//   - a memcmp filter at any offset
//
// For a subscription these run on the block notifier, where a panic stops the
// node.
func FuzzAccountRendering(f *testing.F) {
	f.Add(byte(0), make([]byte, 165), make([]byte, 82), int64(0), uint64(0), uint64(0))
	token := make([]byte, 165)
	token[108] = 1
	f.Add(byte(1), append(token, 2, 0, 0, 0), make([]byte, 165), int64(1_700_000_000), uint64(164), uint64(math.MaxUint64))
	owners := [][32]byte{svm.SplToken, svm.SplToken2022, svm.SystemProgram, addressLookupTable, svm.LoaderV3, svm.SysvarOwner}
	f.Fuzz(func(t *testing.T, which byte, data, mint []byte, now int64, offset, length uint64) {
		owner := owners[int(which)%len(owners)]
		var m *mintData
		if d, ok := mintDataFromAccount(mint, now); ok {
			m = d
		}
		var pubkey [32]byte
		pubkey[0] = which
		if parsed, ok := parseAccountData(pubkey, owner, data, m); ok {
			_, _ = json.Marshal(parsed, json.Deterministic(true))
		}
		for _, encoding := range []string{"base64", "base58", "base64+zstd", "jsonParsed"} {
			var cfg accountConfig
			raw := fmt.Sprintf(`{"encoding":%q,"dataSlice":{"offset":%d,"length":%d}}`, encoding, offset, length)
			if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
				t.Fatal(err)
			}
			if ui, err := renderAccount(pubkey, ffi.Account{Owner: owner, Data: data, Lamports: 1}, cfg, nil); err == nil {
				_, _ = json.Marshal(ui, json.Deterministic(true))
			}
		}
		matcher{offset: uint(offset), bytes: mint}.match(data)
	})
}
