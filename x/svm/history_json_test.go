package svm

import (
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"reflect"
	"testing"

	"github.com/rit3sh-x/cosmos-svm/ffi"
)

// TestARecordReadsBackAsWritten is what a purge and getTransaction rely on.
func TestARecordReadsBackAsWritten(t *testing.T) {
	units := uint64(7)
	program := [32]byte{9}
	r := Record{Height: 5, Index: 1, Tx: []byte{1, 2, 3}, Signature: [64]byte{4}, Meta: ffi.TxMeta{
		PreBalances: []uint64{1}, PostBalances: []uint64{2}, Inner: [][]ffi.InnerInstruction{},
		ReturnProgram: &program, ReturnData: []byte{8}, Logs: []string{"Program log: a"},
		LoadedWritable: [][32]byte{{1}}, LoadedReadonly: [][32]byte{{3}},
		Err: jsontext.Value(`{"InstructionError":[0,"Custom"]}`), CostUnits: &units,
		StaticKeys: ffi.Keys32{{2}},
	}}
	v, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var back Record
	if err := json.Unmarshal(v, &back); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back, r) {
		t.Fatalf("read back %+v\nwrote %+v", back, r)
	}
}
