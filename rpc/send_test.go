package rpc

import (
	"bytes"
	"testing"

	tc "github.com/rit3sh-x/cosmos-svm/internal/testchain"
)

// TestFirstSignatureFramesBothLayouts proves firstSignature finds the
// signature in both wire framings. A V1 transaction opens with the message,
// not with a ShortU16 signature count below 128.
func TestFirstSignatureFramesBothLayouts(t *testing.T) {
	payer := tc.Key(1)
	ix := tc.Transfer(payer, tc.Pub(tc.Key(2)), 1)
	blockhash := tc.HashAt(1)

	legacy := tc.Tx(blockhash, payer, nil, ix)
	got, err := firstSignature(legacy)
	if err != nil || !bytes.Equal(got, legacy[1:65]) {
		t.Fatalf("legacy framing: %v", err)
	}

	v1 := tc.TxV1(blockhash, tc.V1Config{ComputeUnitLimit: 1_000}, payer, nil, ix)
	if v1[0] != 0x81 {
		t.Fatalf("a V1 transaction opens with %#x, want the 0x81 message prefix", v1[0])
	}
	got, err = firstSignature(v1)
	if err != nil || !bytes.Equal(got, v1[len(v1)-64:]) {
		t.Fatalf("V1 framing: %v", err)
	}

	for _, bad := range [][]byte{nil, {0x81, 1}, {0}, bytes.Repeat([]byte{0x81}, 65)} {
		if _, err := firstSignature(bad); err == nil {
			t.Fatalf("%x framed as a transaction", bad)
		}
	}
}
