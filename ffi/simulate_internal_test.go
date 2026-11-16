package ffi

import (
	"encoding/binary"
	"testing"
)

// TestASimulatedLogKeepsItsTrailingBytes proves that a simulated log line that
// ends in NUL keeps it. Arena records are padded with zero bytes, so the
// decoder reads each log line by its length and trims nothing.
func TestASimulatedLogKeepsItsTrailingBytes(t *testing.T) {
	head := make([]byte, 0, 40)
	head = binary.LittleEndian.AppendUint32(head, 0) // status
	head = binary.LittleEndian.AppendUint32(head, 0) // loaded accounts data size
	head = binary.LittleEndian.AppendUint64(head, 0) // units
	head = binary.LittleEndian.AppendUint64(head, 0) // fee
	head = binary.LittleEndian.AppendUint32(head, 0) // rendered error
	head = binary.LittleEndian.AppendUint32(head, 0) // display text
	head = binary.LittleEndian.AppendUint32(head, 0) // writable
	head = binary.LittleEndian.AppendUint32(head, 0) // readonly
	head = append(head, 0)                           // no balances
	head = binary.LittleEndian.AppendUint32(head, 0) // pre token balances
	head = binary.LittleEndian.AppendUint32(head, 0) // post token balances
	head = append(head, 0)                           // no inner instructions
	head = binary.LittleEndian.AppendUint32(head, 0) // accounts
	w := NewArenaWriter(KindSimulateResp, 0)
	w.Push(head)
	w.Push(nil)
	for _, line := range []string{"Program log: ends in a nul\x00", "Program log: three bytes"} {
		w.Push(append(binary.LittleEndian.AppendUint32(nil, uint32(len(line))), line...))
	}
	sim, err := decodeSimulation(w.Finish())
	if err != nil {
		t.Fatal(err)
	}
	if len(sim.Logs) != 2 || sim.Logs[0] != "Program log: ends in a nul\x00" || sim.Logs[1] != "Program log: three bytes" {
		t.Fatalf("the logs decoded as %q", sim.Logs)
	}
}
