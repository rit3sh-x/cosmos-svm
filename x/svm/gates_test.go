package svm

import (
	"strings"
	"testing"
)

// TestOnlyGovernanceMovesAGate proves that the only authority is the
// governance module account. There is no ante decorator and no allowlist in
// state. The check is one comparison in the message server.
func TestOnlyGovernanceMovesAGate(t *testing.T) {
	m := MsgService{Authority: "cosmos1gov"}
	for _, who := range []string{"", "cosmos1someone", "Cosmos1Gov"} {
		err := m.check(who)
		if err == nil {
			t.Fatalf("%q was accepted as the gate authority", who)
		}
		if !strings.Contains(err.Error(), "cosmos1gov") {
			t.Fatalf("the refusal does not name the authority: %v", err)
		}
	}
	if err := m.check("cosmos1gov"); err != nil {
		t.Fatalf("the authority itself was refused: %v", err)
	}
}

func TestValidTargets(t *testing.T) {
	valid := []string{
		"6TkHkRmP7JZy1fdM6fg5uXn76wChQBWGokHBJzrLB3mj", // raise_cpi_nesting_limit_to_8, svm
		EnableTxV1,
		"4a6f7o7iTcA8hRDCrPLkSatnt5Ykxiu36wo5p1Tt12wC", // set_lamports_per_byte_to_6333
		"rnt8ZQpz2HYhX3DkYBDGjJS1a36mYq69oXka7JrhEdi",  // set_lamports_per_byte_to_6960, classed cleaned
		"rntTjNZ9boq8owDxjGVFHPfWNQPDaKiM5JcjxmDGg47",  // set_lamports_per_byte_to_696, classed cleaned
	}
	for _, id := range valid {
		if !ValidTarget(id) {
			t.Fatalf("%s should be a valid target", id)
		}
	}
	invalid := []string{
		"P1BCUMpAC7V2GRBRiJCNUgpMyWZhoqt3LKo712ePqsz",  // raise_block_limits_to_100m, bank, no port
		"iBRL5RuWhw4yqaAZu96RUULHckHTZAoe2b77qaV38JZ",  // reduce_slot_time_to_350ms, no port
		"5AqsUgSb6cgLizSaNiFn3o9XB7VUtKDtDZfcKEjEDmni", // set_lamports_per_byte_to_6960 before v4.3.0 rekeyed it
		"not a pubkey",
	}
	for _, id := range invalid {
		if ValidTarget(id) {
			t.Fatalf("%s should not be a valid target", id)
		}
	}
}

// TestTheRentGateOrderIsAgaves proves that the six rent gates are in Agave's
// own order, with the safeguard last so it overrides a cut that activates in
// the same block.
func TestTheRentGateOrderIsAgaves(t *testing.T) {
	want := []uint64{6_333, 5_080, 2_575, 1_322, 696, 6_960}
	if len(rentGates) != len(want) {
		t.Fatalf("%d rent gates, want %d", len(rentGates), len(want))
	}
	for i, g := range rentGates {
		if g.perByte != want[i] {
			t.Fatalf("rent gate %d is %d, want %d", i, g.perByte, want[i])
		}
	}
}

// Rent::minimum_balance at Rent::default's 6,960 lamports a byte: 890,880
// lamports for an empty account and 2,039,280 for an SPL token account.
func TestTheRentExemptMinimumIsMainnets(t *testing.T) {
	if got := RentExemptMinimum(6_960, 0); got != 890_880 {
		t.Fatalf("an empty account needs %d, want 890,880", got)
	}
	if got := RentExemptMinimum(6_960, 165); got != 2_039_280 {
		t.Fatalf("a token account needs %d, want 2,039,280", got)
	}
}

// The svmmeta keys of a gate: active/<gate>, and pending/<height,
// big-endian><gate>, which splitPendingKey reads back.
func TestTheGateKeyLayout(t *testing.T) {
	id := [32]byte{7}
	if got := string(activeKey(id)); got != "active/"+string(id[:]) {
		t.Fatalf("active key %x", got)
	}
	k := pendingKey(258, id)
	if string(k) != "pending/\x00\x00\x00\x00\x00\x00\x01\x02"+string(id[:]) {
		t.Fatalf("pending key %x", k)
	}
	if height, got, ok := splitPendingKey(k); !ok || height != 258 || got != id {
		t.Fatalf("split %d, %x, %v", height, got, ok)
	}
	if _, _, ok := splitPendingKey(k[:len(k)-1]); ok {
		t.Fatal("a short key split")
	}
}
