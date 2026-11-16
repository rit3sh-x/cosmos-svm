package main

import (
	"context"
	"encoding/hex"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/cometbft/cometbft/privval"
	cmtproto "github.com/cometbft/cometbft/proto/tendermint/types"
	cmthttp "github.com/cometbft/cometbft/rpc/client/http"
	cmttypes "github.com/cometbft/cometbft/types"
	dbm "github.com/cosmos/cosmos-db"
	gogoproto "github.com/cosmos/gogoproto/proto"

	"cosmossdk.io/log/v2"
	"cosmossdk.io/math"

	sdked25519 "github.com/cosmos/cosmos-sdk/crypto/keys/ed25519"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/types/tx/signing"
	authsigning "github.com/cosmos/cosmos-sdk/x/auth/signing"
	authtypes "github.com/cosmos/cosmos-sdk/x/auth/types"
	banktypes "github.com/cosmos/cosmos-sdk/x/bank/types"
	slashingtypes "github.com/cosmos/cosmos-sdk/x/slashing/types"
	stakingtypes "github.com/cosmos/cosmos-sdk/x/staking/types"

	"github.com/rit3sh-x/cosmos-svm/app"
)

// operator is one validator's home as the harness built it. With no operator
// flags, `init` derives the account from the consensus key, so that key signs
// for the account.
type operator struct {
	key     *sdked25519.PrivKey
	account sdk.AccAddress
}

func loadOperator(home string) operator {
	pv := privval.LoadFilePVEmptyState(filepath.Join(home, "config", "priv_validator_key.json"), "")
	key := &sdked25519.PrivKey{Key: pv.Key.PrivKey.Bytes()}
	return operator{key: key, account: sdk.AccAddress(key.PubKey().Address())}
}

// abciQuery runs one gRPC query through CometBFT's abci_query. This is the only
// query path that a machine here serves, because svmd has no query subcommands.
func abciQuery(node, path string, req, resp gogoproto.Message) error {
	data, err := gogoproto.Marshal(req)
	if err != nil {
		return err
	}
	var out struct {
		Result struct {
			Response struct {
				Code  uint32 `json:"code"`
				Log   string `json:"log"`
				Value []byte `json:"value"`
			} `json:"response"`
		} `json:"result"`
	}
	if err := get(fmt.Sprintf("%s/abci_query?path=%s&data=0x%s", node, url.QueryEscape(`"`+path+`"`), hex.EncodeToString(data)), &out); err != nil {
		return err
	}
	if r := out.Result.Response; r.Code != 0 {
		return fmt.Errorf("%s: code %d, %s", path, r.Code, r.Log)
	}
	return gogoproto.Unmarshal(out.Result.Response.Value, resp)
}

func get(u string, into any) error {
	resp, err := http.Get(u)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return json.UnmarshalRead(resp.Body, into)
}

// validatorState returns what the slashing assertions read: whether the
// validator's consensus key is tombstoned, whether it is jailed, its bond, and
// the whole supply of the bond denom, which a burn lowers.
func validatorState(node string, op operator) (tombstoned, jailed bool, tokens, supply math.Int, err error) {
	var info slashingtypes.QuerySigningInfoResponse
	if err = abciQuery(node, "/cosmos.slashing.v1beta1.Query/SigningInfo",
		&slashingtypes.QuerySigningInfoRequest{ConsAddress: sdk.ConsAddress(op.account).String()}, &info); err != nil {
		return
	}
	var v stakingtypes.QueryValidatorResponse
	if err = abciQuery(node, "/cosmos.staking.v1beta1.Query/Validator",
		&stakingtypes.QueryValidatorRequest{ValidatorAddr: sdk.ValAddress(op.account).String()}, &v); err != nil {
		return
	}
	var s banktypes.QuerySupplyOfResponse
	if err = abciQuery(node, "/cosmos.bank.v1beta1.Query/SupplyOf", &banktypes.QuerySupplyOfRequest{Denom: app.BondDenom}, &s); err != nil {
		return
	}
	return info.ValSigningInfo.Tombstoned, v.Validator.Jailed, v.Validator.Tokens, s.Amount.Amount, nil
}

// printValidator is -mode validator. It prints one line that the harness reads.
func printValidator(home, node string) error {
	tombstoned, jailed, tokens, supply, err := validatorState(node, loadOperator(home))
	if err != nil {
		return err
	}
	fmt.Println(tombstoned, jailed, tokens, supply)
	return nil
}

// unjail is -mode unjail. It sends the operator's MsgUnjail, signed with the
// key in home and paying the fee floor, and waits until the validator is no
// longer jailed.
func unjail(home, node, chain string) error {
	op := loadOperator(home)
	if err := broadcast(node, chain, op, &slashingtypes.MsgUnjail{ValidatorAddr: sdk.ValAddress(op.account).String()}); err != nil {
		return err
	}
	for deadline := time.Now().Add(2 * time.Minute); time.Now().Before(deadline); time.Sleep(time.Second) {
		if _, jailed, _, _, err := validatorState(node, op); err == nil && !jailed {
			fmt.Printf("unjail: %s is no longer jailed\n", sdk.ValAddress(op.account))
			return nil
		}
	}
	return fmt.Errorf("%s was still jailed two minutes after MsgUnjail", sdk.ValAddress(op.account))
}

// broadcast signs msgs with the operator's key, paying the fee floor, and
// sends them to node. It returns once CheckTx admits them.
func broadcast(node, chain string, op operator, msgs ...sdk.Msg) error {
	tmpHome, err := os.MkdirTemp("", "fill")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpHome)
	a, err := app.New(log.NewNopLogger(), dbm.NewMemDB(), app.Local{}, tmpHome)
	if err != nil {
		return err
	}
	var acct authtypes.QueryAccountResponse
	if err := abciQuery(node, "/cosmos.auth.v1beta1.Query/Account", &authtypes.QueryAccountRequest{Address: op.account.String()}, &acct); err != nil {
		return err
	}
	var account sdk.AccountI
	if err := a.Codec.UnpackAny(acct.Account, &account); err != nil {
		return err
	}

	b := a.TxConfig.NewTxBuilder()
	if err := b.SetMsgs(msgs...); err != nil {
		return err
	}
	// The floor is ceil(0.000001 x gas) usvm, one usvm up to a million gas.
	b.SetGasLimit(1_000_000)
	b.SetFeeAmount(sdk.NewCoins(sdk.NewInt64Coin(app.BondDenom, 1)))
	sig := signing.SignatureV2{
		PubKey:   op.key.PubKey(),
		Data:     &signing.SingleSignatureData{SignMode: signing.SignMode_SIGN_MODE_DIRECT},
		Sequence: account.GetSequence(),
	}
	if err := b.SetSignatures(sig); err != nil {
		return err
	}
	bytes, err := authsigning.GetSignBytesAdapter(context.Background(), a.TxConfig.SignModeHandler(), signing.SignMode_SIGN_MODE_DIRECT,
		authsigning.SignerData{ChainID: chain, AccountNumber: account.GetAccountNumber(), Sequence: account.GetSequence(),
			PubKey: op.key.PubKey(), Address: op.account.String()}, b.GetTx())
	if err != nil {
		return err
	}
	if sig.Data.(*signing.SingleSignatureData).Signature, err = op.key.Sign(bytes); err != nil {
		return err
	}
	if err := b.SetSignatures(sig); err != nil {
		return err
	}
	raw, err := a.TxConfig.TxEncoder()(b.GetTx())
	if err != nil {
		return err
	}
	var out struct {
		Result struct {
			Code uint32 `json:"code"`
			Log  string `json:"log"`
		} `json:"result"`
		Error jsontext.Value `json:"error"`
	}
	if err := get(fmt.Sprintf("%s/broadcast_tx_sync?tx=0x%s", node, hex.EncodeToString(raw)), &out); err != nil {
		return err
	}
	if out.Error != nil || out.Result.Code != 0 {
		return fmt.Errorf("%T refused at CheckTx: code %d, %s %s", msgs[0], out.Result.Code, out.Result.Log, out.Error)
	}
	return nil
}

// equivocate is -mode evidence. It signs two prevotes for different blocks at
// height with the key in home, and broadcasts them to node as
// DuplicateVoteEvidence. That lets the retention run put evidence from 1,500
// heights back into a block on demand, which a live double-sign cannot do.
func equivocate(home, node, chain string, height int64) error {
	ctx := context.Background()
	pv := privval.LoadFilePVEmptyState(filepath.Join(home, "config", "priv_validator_key.json"), "")
	c, err := cmthttp.New(node, "/websocket")
	if err != nil {
		return err
	}
	block, err := c.Block(ctx, &height)
	if err != nil {
		return err
	}
	perPage := 100
	vals, err := c.Validators(ctx, &height, nil, &perPage)
	if err != nil {
		return err
	}
	set := cmttypes.NewValidatorSet(vals.Validators)
	index, _ := set.GetByAddress(pv.Key.Address)
	vote := func(b byte) (*cmttypes.Vote, error) {
		hash := make([]byte, 32)
		hash[0] = b
		v := &cmttypes.Vote{
			Type:             cmtproto.PrevoteType,
			Height:           height,
			BlockID:          cmttypes.BlockID{Hash: hash, PartSetHeader: cmttypes.PartSetHeader{Total: 1, Hash: hash}},
			Timestamp:        block.Block.Time,
			ValidatorAddress: pv.Key.Address,
			ValidatorIndex:   index,
		}
		var err error
		v.Signature, err = pv.Key.PrivKey.Sign(cmttypes.VoteSignBytes(chain, v.ToProto()))
		return v, err
	}
	a, err := vote(1)
	if err != nil {
		return err
	}
	b, err := vote(2)
	if err != nil {
		return err
	}
	ev, err := cmttypes.NewDuplicateVoteEvidence(a, b, block.Block.Time, set)
	if err != nil {
		return err
	}
	if _, err := c.BroadcastEvidence(ctx, ev); err != nil {
		return err
	}
	fmt.Printf("evidence: %s signed two prevotes at height %d\n", pv.Key.Address, height)
	return nil
}
