package rpc

import (
	"testing"

	tc "github.com/rit3sh-x/cosmos-svm/internal/testchain"
)

// TestTheExcludedConceptsAnswerAsAnEmptyAgave checks that the vote, inflation
// and reward methods answer as Agave does on a cluster without them. It also
// checks the refusals of getInflationReward, in Agave's order.
func TestTheExcludedConceptsAnswerAsAnEmptyAgave(t *testing.T) {
	_, s := server(t)
	var votes map[string][]any
	mustAnswer(t, s, &votes, "getVoteAccounts")
	if len(votes) != 2 || votes["current"] == nil || len(votes["current"])+len(votes["delinquent"]) != 0 {
		t.Fatalf("getVoteAccounts is %v, want two empty lists", votes)
	}
	if _, err := reply(t, s, "getVoteAccounts", map[string]string{"votePubkey": "notakey"}); err == nil || err.Message != "Invalid param: WrongSize" {
		t.Fatalf("a bad votePubkey: %v", err)
	}
	var governor map[string]float64
	mustAnswer(t, s, &governor, "getInflationGovernor")
	var rate map[string]float64
	mustAnswer(t, s, &rate, "getInflationRate")
	for k, v := range governor {
		if v != 0 {
			t.Errorf("governor %s is %v", k, v)
		}
	}
	if len(governor) != 5 || rate["total"] != 0 || rate["epoch"] != 0 {
		t.Fatalf("governor %v, rate %v", governor, rate)
	}

	payer := tc.Pub(tc.Key(1))
	key := b58(payer)
	over := make([]string, 33)
	for i := range over {
		over[i] = key
	}
	for _, tt := range []struct {
		params []any
		code   int
		want   string
	}{
		{[]any{over}, codeInvalidParams, "Too many inputs provided; max 32"},
		{[]any{[]string{key}}, codeBlockNotAvailable, "Block not available for slot 432000"},
		{[]any{[]string{key}, map[string]any{"epoch": 0, "minContextSlot": 999}}, codeBlockNotAvailable, "Block not available for slot 432000"},
		{[]any{[]string{key}, map[string]any{"minContextSlot": 999}}, codeMinContextSlot, "Minimum context slot has not been reached"},
	} {
		if _, err := reply(t, s, "getInflationReward", tt.params...); err == nil || err.Code != tt.code || err.Message != tt.want {
			t.Errorf("getInflationReward%v: %v, want %d %q", tt.params, err, tt.code, tt.want)
		}
	}
}
