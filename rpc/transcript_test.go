package rpc

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"cosmossdk.io/log/v2"
	cmted25519 "github.com/cometbft/cometbft/crypto/ed25519"
	cmttypes "github.com/cometbft/cometbft/types"
	"github.com/gorilla/websocket"
	"github.com/mr-tron/base58"

	tc "github.com/rit3sh-x/cosmos-svm/internal/testchain"
	"github.com/rit3sh-x/cosmos-svm/x/svm"
)

// agaveReference is the Agave release the transcript is recorded from. It is
// the RPC's reference version, not the engine's pin. While the engine pins an
// older release, its version strings are ledger entries. An engine pin past
// it needs a new recording.
const agaveReference = "4.3.0"

const transcriptDir = "testdata/transcript"

var (
	record        = flag.String("record", "", "record the Agave transcript from this solana-test-validator RPC URL")
	recordCommand = flag.String("record-command", "", "the exact solana-test-validator command line, for the transcript header")
)

type transcriptCase struct {
	Name string    `json:"name"`
	RPC  *rpcCase  `json:"rpc,omitempty"`
	Body string    `json:"body,omitempty"`
	WS   *wsCase   `json:"ws,omitempty"`
	HTTP *httpCase `json:"http,omitempty"`
}

// rpcCase is one JSON-RPC call. Params stays raw, so a case can send an
// object, nothing, or a wrong shape on purpose.
type rpcCase struct {
	Method string         `json:"method"`
	Params jsontext.Value `json:"params,omitempty"`
}

// wsCase is one subscription: the subscribe, the transaction that triggers
// it, the notifications it must send, and the unsubscribe.
type wsCase struct {
	Method        string         `json:"method"`
	Params        jsontext.Value `json:"params,omitempty"`
	Trigger       string         `json:"trigger,omitempty"`
	Notifications int            `json:"notifications,omitzero"`
}

// httpCase is one raw request, for the answers outside JSON-RPC.
type httpCase struct {
	Method  string            `json:"method"`
	Path    string            `json:"path"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    string            `json:"body,omitempty"`
	Pad     int               `json:"pad,omitzero"`
}

// method is what normalizations and ledger entries name a case by.
func (c transcriptCase) method() string {
	switch {
	case c.RPC != nil:
		return c.RPC.Method
	case c.WS != nil:
		return c.WS.Method
	case c.HTTP != nil:
		// The path is part of the name, so a normalization can name one
		// path's body without covering every other's.
		return "http " + c.HTTP.Path
	}
	var b struct {
		Method string `json:"method"`
	}
	if json.Unmarshal([]byte(c.Body), &b) == nil && b.Method != "" {
		return b.Method
	}
	return "framing"
}

type transcriptHeader struct {
	Agave    string `json:"agave"`
	Command  string `json:"command"`
	Corpus   string `json:"corpus"`
	Recorded string `json:"recorded"`
}

type transcript struct {
	Header   transcriptHeader          `json:"header"`
	Bindings map[string]jsontext.Value `json:"bindings"`
	Answers  map[string]jsontext.Value `json:"answers"`
}

// loadCorpus runs gen_corpus.py, the corpus's one source, and returns its
// cases and the sha256 of what it printed.
func loadCorpus(t *testing.T) ([]transcriptCase, string) {
	t.Helper()
	b, err := exec.Command("python3", transcriptDir+"/gen_corpus.py").Output()
	if err != nil {
		t.Fatalf("%s/gen_corpus.py: %v", transcriptDir, err)
	}
	var corpus struct {
		Cases []transcriptCase `json:"cases"`
	}
	if err := json.Unmarshal(b, &corpus); err != nil {
		t.Fatalf("gen_corpus.py: %v", err)
	}
	seen := map[string]bool{}
	for _, c := range corpus.Cases {
		if seen[c.Name] {
			t.Fatalf("gen_corpus.py names %s twice", c.Name)
		}
		seen[c.Name] = true
	}
	sum := sha256.Sum256(b)
	return corpus.Cases, hex.EncodeToString(sum[:])
}

// side is where a corpus is answered: the in-process chain, or a live
// solana-test-validator while recording.
type side interface {
	post(body []byte) []byte
	raw(h httpCase) jsontext.Value
	wsURL() string
	fund(to [32]byte, lamports uint64)
	send(tx []byte, wantErr bool) uint64
	latest() [32]byte
	// drains reports whether every notification a block causes is queued by
	// the time send returns. If so, a request sent after it is answered after
	// them. The chain side's notify is synchronous and each connection has
	// one writer. A live validator notifies later.
	drains() bool
}

type chainSide struct {
	t        *testing.T
	c        *tc.Chain
	s        *Server
	httpSrv  *httptest.Server
	wsSrv    *httptest.Server
	funderPK ed25519.PrivateKey
}

func newChainSide(t *testing.T) *chainSide {
	t.Helper()
	return newChainSideWith(t, tc.Options{Programs: tc.ManifestPrograms(t)})
}

func newChainSideWith(t *testing.T, opts tc.Options) *chainSide {
	t.Helper()
	funder := tc.Key(1)
	c := tc.Start(t, tc.Pub(funder), opts)
	// The chain's one operator is the validator this machine runs, as on a
	// single-validator devnet.
	self := cmted25519.PubKey(tc.OperatorKey(0).Public().(ed25519.PublicKey))
	s := New(c.App, newComet(c, self, cmttypes.NewValidator(self, 10)), log.NewNopLogger())
	c.Block()
	cs := &chainSide{t: t, c: c, s: s, funderPK: funder,
		httpSrv: httptest.NewServer(http.HandlerFunc(s.serveHTTP)),
		wsSrv:   httptest.NewServer(http.HandlerFunc(s.serveWS))}
	t.Cleanup(cs.httpSrv.Close)
	t.Cleanup(cs.wsSrv.Close)
	rpcAddr, wsAddr := strings.TrimPrefix(cs.httpSrv.URL, "http://"), strings.TrimPrefix(cs.wsSrv.URL, "http://")
	s.addrs.rpc, s.addrs.ws = &rpcAddr, &wsAddr
	// Advertise the QUIC port as a ready svm-tpu does. rpc/quic_test.go and
	// the integration tests run the child itself.
	quicAddr := "127.0.0.1:8009"
	s.quic.advertised.Store(&quicAddr)
	return cs
}

func (cs *chainSide) post(body []byte) []byte { return postTo(cs.t, cs.httpSrv.URL, body) }
func (cs *chainSide) raw(h httpCase) jsontext.Value {
	return rawTo(cs.t, cs.httpSrv.URL, h)
}
func (cs *chainSide) wsURL() string    { return "ws" + strings.TrimPrefix(cs.wsSrv.URL, "http") }
func (cs *chainSide) latest() [32]byte { return cs.c.Latest() }
func (cs *chainSide) drains() bool     { return true }

func (cs *chainSide) fund(to [32]byte, lamports uint64) {
	cs.send(tc.Tx(cs.c.Latest(), cs.funderPK, nil, tc.Transfer(cs.funderPK, to, lamports)), false)
}

// send commits the transaction in a block of its own and tells subscribers,
// as the Follow loop does once the height commits.
func (cs *chainSide) send(tx []byte, wantErr bool) uint64 {
	cs.t.Helper()
	res, _ := cs.c.Block(tx)
	if failed := res[0].Code == 1; res[0].Code > 1 || failed != wantErr { // 1 is the engine's failed status, x/svm runner.go
		logs := cs.post(encode(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "getTransaction",
			"params": []any{sigOf(tx), map[string]string{"encoding": "base64"}}}))
		cs.t.Fatalf("setup transaction %s: status %d, want failed %v: %s", sigOf(tx), res[0].Code, wantErr, logs)
	}
	if err := cs.s.notify(uint64(cs.c.Height)); err != nil {
		cs.t.Fatal(err)
	}
	return uint64(cs.c.Height)
}

type liveSide struct {
	t   *testing.T
	url string
}

func (l *liveSide) post(body []byte) []byte       { return postTo(l.t, l.url, body) }
func (l *liveSide) raw(h httpCase) jsontext.Value { return rawTo(l.t, l.url, h) }
func (l *liveSide) drains() bool                  { return false }
func (l *liveSide) wsURL() string {
	// Agave's websocket is the RPC port plus one.
	m := regexp.MustCompile(`^http://([^:]+):(\d+)$`).FindStringSubmatch(l.url)
	if m == nil {
		l.t.Fatalf("-record wants http://host:port, not %s", l.url)
	}
	var port int
	fmt.Sscan(m[2], &port)
	return fmt.Sprintf("ws://%s:%d", m[1], port+1)
}

func (l *liveSide) call(method string, params ...any) jsontext.Value {
	l.t.Helper()
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params}, json.Deterministic(true))
	var resp struct {
		Result jsontext.Value `json:"result"`
		Error  jsontext.Value `json:"error"`
	}
	out := l.post(body)
	if err := json.Unmarshal(out, &resp); err != nil || resp.Error != nil {
		l.t.Fatalf("%s: %s", method, out)
	}
	return resp.Result
}

func (l *liveSide) latest() [32]byte {
	var r struct {
		Value struct {
			Blockhash string `json:"blockhash"`
		} `json:"value"`
	}
	json.Unmarshal(l.call("getLatestBlockhash", map[string]string{"commitment": "finalized"}), &r)
	b, err := base58.Decode(r.Value.Blockhash)
	if err != nil {
		l.t.Fatal(err)
	}
	return [32]byte(b)
}

func (l *liveSide) fund(to [32]byte, lamports uint64) {
	var sig string
	json.Unmarshal(l.call("requestAirdrop", base58.Encode(to[:]), lamports), &sig)
	l.finalized(sig, false)
}

func (l *liveSide) send(tx []byte, wantErr bool) uint64 {
	var sig string
	json.Unmarshal(l.call("sendTransaction", base64.StdEncoding.EncodeToString(tx),
		map[string]any{"encoding": "base64", "skipPreflight": wantErr}), &sig)
	return l.finalized(sig, wantErr)
}

// finalized waits until the signature is final, so every later case reads
// it at Agave's default commitment. The deadline only stops a recording that
// would hang. It decides nothing.
func (l *liveSide) finalized(sig string, wantErr bool) uint64 {
	l.t.Helper()
	for deadline := time.Now().Add(2 * time.Minute); time.Now().Before(deadline); time.Sleep(time.Second) {
		var r struct {
			Value []*struct {
				Slot               uint64         `json:"slot"`
				Err                jsontext.Value `json:"err"`
				ConfirmationStatus string         `json:"confirmationStatus"`
			} `json:"value"`
		}
		json.Unmarshal(l.call("getSignatureStatuses", []string{sig}, map[string]bool{"searchTransactionHistory": true}), &r)
		if len(r.Value) == 1 && r.Value[0] != nil && r.Value[0].ConfirmationStatus == "finalized" {
			if failed := string(r.Value[0].Err) != "null" && len(r.Value[0].Err) > 0; failed != wantErr {
				l.t.Fatalf("%s finalized with err %s", sig, r.Value[0].Err)
			}
			return r.Value[0].Slot
		}
	}
	l.t.Fatalf("%s did not finalize", sig)
	return 0
}

func postTo(t *testing.T, url string, body []byte) []byte {
	t.Helper()
	resp, err := http.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// answeredHeaders are the headers Agave's HTTP layer decides, which an HTTP
// case compares. Date and length are the transport's.
var answeredHeaders = []string{"content-type", "allow", "accept", "vary",
	"access-control-allow-origin", "access-control-allow-methods",
	"access-control-allow-headers", "access-control-max-age"}

func rawTo(t *testing.T, base string, h httpCase) jsontext.Value {
	t.Helper()
	body := h.Body + strings.Repeat(" ", h.Pad)
	req, err := http.NewRequest(h.Method, base+h.Path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range h.Headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	// An archive or any large or binary body is kept as its length and hash,
	// which still pins both sides exactly.
	var kept any = string(out)
	if len(out) > 4096 || !utf8.Valid(out) {
		sum := sha256.Sum256(out)
		kept = map[string]any{"length": len(out), "sha256": hex.EncodeToString(sum[:])}
	}
	headers := map[string]string{}
	for _, k := range answeredHeaders {
		if v := resp.Header.Values(k); len(v) > 0 {
			headers[k] = strings.Join(v, ", ")
		}
	}
	return encode(map[string]any{"status": resp.StatusCode, "headers": headers, "body": kept})
}

var (
	memoV3 = mustBase58("MemoSq4gqABAXKb96qnH8TysNcWxMyWCqXgDLGmfcHr")
)

func mustBase58(s string) [32]byte {
	b, err := base58.Decode(s)
	if err != nil || len(b) != 32 {
		panic(s)
	}
	return [32]byte(b)
}

// The fixed keys of the setup. Seed 1 funds the chain side. The live side
// funds from its faucet, so no answer names seed 1.
var (
	payerKey     = tc.Key(2)
	recipientKey = tc.Key(3)
	mintKey      = tc.Key(4)
	tokenAKey    = tc.Key(5)
	tokenBKey    = tc.Key(6)
	delegateKey  = tc.Key(7)
	unfundedKey  = tc.Key(9)
)

const (
	payerLamports = 100_000_000_000
	mintDecimals  = 6
)

func createAccountIx(payer, account ed25519.PrivateKey, lamports, space uint64, owner [32]byte) tc.Instruction {
	data := binary.LittleEndian.AppendUint32(nil, 0)
	data = binary.LittleEndian.AppendUint64(data, lamports)
	data = binary.LittleEndian.AppendUint64(data, space)
	data = append(data, owner[:]...)
	return tc.Instruction{Program: svm.SystemProgram, Accounts: []tc.Meta{tc.Signer(tc.Pub(payer), true), tc.Signer(tc.Pub(account), true)}, Data: data}
}

func tokenIx(data []byte, metas ...tc.Meta) tc.Instruction {
	return tc.Instruction{Program: svm.SplToken, Accounts: metas, Data: data}
}

// rentFor asks the side, because rent differs between the two and
// each side's accounts must be rent-exempt on that side.
func rentFor(t *testing.T, s side, space uint64) uint64 {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "getMinimumBalanceForRentExemption", "params": []uint64{space}}, json.Deterministic(true))
	var resp struct {
		Result uint64 `json:"result"`
	}
	if err := json.Unmarshal(s.post(body), &resp); err != nil || resp.Result == 0 {
		t.Fatalf("rent for %d bytes", space)
	}
	return resp.Result
}

func sigOf(tx []byte) string { return base58.Encode(tx[1:65]) }

func bindString(b map[string]jsontext.Value, name, v string) { b[name] = encode(v) }
func bindNumber(b map[string]jsontext.Value, name string, v uint64) {
	b[name] = jsontext.Value(fmt.Sprint(v))
}

// setup builds the same state on either side and names what the corpus
// refers to. Every step is its own block, so each named slot holds one.
func setup(t *testing.T, s side) map[string]jsontext.Value {
	t.Helper()
	b := map[string]jsontext.Value{}
	payer, recipient := payerKey, recipientKey
	for name, k := range map[string]ed25519.PrivateKey{"payer": payerKey, "recipient": recipientKey,
		"mint": mintKey, "tokenA": tokenAKey, "tokenB": tokenBKey, "delegate": delegateKey, "unfunded": unfundedKey} {
		pk := tc.Pub(k)
		bindString(b, name, base58.Encode(pk[:]))
	}
	bindString(b, "tokenProgram", base58.Encode(svm.SplToken[:]))
	bindString(b, "systemProgram", base58.Encode(svm.SystemProgram[:]))

	s.fund(tc.Pub(payer), payerLamports)

	tx := tc.Tx(s.latest(), payer, nil, tc.Transfer(payer, tc.Pub(recipient), 1_000_000_000))
	bindNumber(b, "slot.transfer", s.send(tx, false))
	bindString(b, "sig.transfer", sigOf(tx))

	mintRent, accountRent := rentFor(t, s, 82), rentFor(t, s, 165)
	mint, a, bb := tc.Pub(mintKey), tc.Pub(tokenAKey), tc.Pub(tokenBKey)
	payerPub, recipientPub := tc.Pub(payer), tc.Pub(recipient)
	initMint := append([]byte{20, mintDecimals}, payerPub[:]...)
	initMint = append(initMint, 0)
	tx = tc.Tx(s.latest(), payer, []ed25519.PrivateKey{mintKey},
		createAccountIx(payer, mintKey, mintRent, 82, svm.SplToken),
		tokenIx(initMint, tc.Writable(mint)))
	bindNumber(b, "slot.mint", s.send(tx, false))
	bindString(b, "sig.mint", sigOf(tx))

	tx = tc.Tx(s.latest(), payer, []ed25519.PrivateKey{tokenAKey, tokenBKey},
		createAccountIx(payer, tokenAKey, accountRent, 165, svm.SplToken),
		tokenIx(append([]byte{18}, payerPub[:]...), tc.Writable(a), tc.Readonly(mint)),
		createAccountIx(payer, tokenBKey, accountRent, 165, svm.SplToken),
		tokenIx(append([]byte{18}, recipientPub[:]...), tc.Writable(bb), tc.Readonly(mint)),
		tokenIx(binary.LittleEndian.AppendUint64([]byte{7}, 1_000_000), tc.Writable(mint), tc.Writable(a), tc.Signer(payerPub, false)))
	bindNumber(b, "slot.accounts", s.send(tx, false))
	bindString(b, "sig.accounts", sigOf(tx))

	transfer := binary.LittleEndian.AppendUint64([]byte{12}, 250_000)
	transfer = append(transfer, mintDecimals)
	tx = tc.Tx(s.latest(), payer, nil,
		tokenIx(transfer, tc.Writable(a), tc.Readonly(mint), tc.Writable(bb), tc.Signer(payerPub, false)))
	bindNumber(b, "slot.tokenTransfer", s.send(tx, false))
	bindString(b, "sig.tokenTransfer", sigOf(tx))

	tx = tc.Tx(s.latest(), payer, nil,
		tokenIx(binary.LittleEndian.AppendUint64([]byte{4}, 100), tc.Writable(a), tc.Readonly(tc.Pub(delegateKey)), tc.Signer(payerPub, false)))
	bindNumber(b, "slot.approve", s.send(tx, false))
	bindString(b, "sig.approve", sigOf(tx))

	tx = tc.Tx(s.latest(), payer, nil, tc.Instruction{Program: memoV3,
		Accounts: []tc.Meta{tc.Signer(payerPub, false)}, Data: []byte("agave transcript")})
	bindNumber(b, "slot.memo", s.send(tx, false))
	bindString(b, "sig.memo", sigOf(tx))

	// More than the recipient holds, so it fails in execution and pays its fee.
	tx = tc.Tx(s.latest(), recipient, nil, tc.Transfer(recipient, payerPub, 5_000_000_000))
	bindNumber(b, "slot.failed", s.send(tx, true))
	bindString(b, "sig.failed", sigOf(tx))
	return b
}

// refreshes counts the calls to fresh. Each call uses new amounts, so no two
// transactions share a signature on either side. Both sides build the same
// sequence.
var refreshes uint64

// fresh builds the transactions that the send and simulate cases name against
// the side's latest blockhash. It runs before each such case, because a
// recording waits on finalized subscriptions longer than a blockhash lives.
func fresh(t *testing.T, s side, b map[string]jsontext.Value) {
	t.Helper()
	refreshes++
	h := s.latest()
	bindString(b, "blockhash", base58.Encode(h[:]))
	payer, recipientPub := payerKey, tc.Pub(recipientKey)
	base := 2000 + 10*refreshes
	tx := func(n uint64) []byte { return tc.Tx(h, payer, nil, tc.Transfer(payer, recipientPub, base+n)) }
	bindString(b, "tx.send", base64.StdEncoding.EncodeToString(tx(1)))
	bindString(b, "tx.send.base58", base58.Encode(tx(2)))
	bindString(b, "tx.simulate", base64.StdEncoding.EncodeToString(tx(3)))
	bad := tx(4)
	bad[10] ^= 0xff
	bindString(b, "tx.badSignature", base64.StdEncoding.EncodeToString(bad))
	unfunded := tc.Tx(h, unfundedKey, nil, tc.Transfer(unfundedKey, recipientPub, base+5))
	bindString(b, "tx.unfunded", base64.StdEncoding.EncodeToString(unfunded))
	m := tx(6)
	bindString(b, "message.transfer", base64.StdEncoding.EncodeToString(m[65:]))
}

// needsFresh reports whether a case names a transaction or blockhash that
// must be current when it is sent.
func needsFresh(c transcriptCase) bool {
	return bytes.Contains(encode(c), []byte(`$tx.`)) || bytes.Contains(encode(c), []byte(`$message.`)) ||
		bytes.Contains(encode(c), []byte(`$blockhash`))
}

// substitute replaces every string that is exactly "$name" with the binding,
// so a slot binding becomes a number where the corpus wrote "$slot.x".
func substitute(t *testing.T, raw jsontext.Value, b map[string]jsontext.Value) jsontext.Value {
	t.Helper()
	if len(raw) == 0 {
		return raw
	}
	v, err := decode(raw)
	if err != nil {
		t.Fatalf("%s: %v", raw, err)
	}
	var walk func(any) any
	walk = func(v any) any {
		switch x := v.(type) {
		case string:
			if strings.HasPrefix(x, "$") {
				bound, ok := b[x[1:]]
				if !ok {
					t.Fatalf("the corpus names %s and the setup binds no such value", x)
				}
				d, _ := decode(bound)
				return d
			}
		case []any:
			for i := range x {
				x[i] = walk(x[i])
			}
		case map[string]any:
			for k := range x {
				x[k] = walk(x[k])
			}
		}
		return v
	}
	return encode(walk(v))
}

// substituteText is substitute for a raw body, where a placeholder sits
// inside JSON text the case wrote by hand.
func substituteText(body string, b map[string]jsontext.Value) string {
	names := make([]string, 0, len(b))
	for k := range b {
		names = append(names, k)
	}
	sort.Slice(names, func(i, j int) bool { return len(names[i]) > len(names[j]) })
	for _, k := range names {
		v := string(b[k])
		var s string
		if json.Unmarshal(b[k], &s) == nil {
			v = s
		}
		body = strings.ReplaceAll(body, "$"+k, v)
	}
	return body
}

func asJSON(out []byte) jsontext.Value {
	if jsontext.Value(out).IsValid() {
		return append(jsontext.Value(nil), out...)
	}
	return encode(string(out))
}

func runCase(t *testing.T, s side, c transcriptCase, b map[string]jsontext.Value) jsontext.Value {
	t.Helper()
	switch {
	case c.RPC != nil:
		req := map[string]any{"jsonrpc": "2.0", "id": 1, "method": c.RPC.Method}
		if len(c.RPC.Params) > 0 {
			req["params"] = substitute(t, c.RPC.Params, b)
		}
		return asJSON(s.post(encode(req)))
	case c.Body != "":
		return asJSON(s.post([]byte(substituteText(c.Body, b))))
	case c.HTTP != nil:
		h := *c.HTTP
		h.Body = substituteText(h.Body, b)
		return s.raw(h)
	case c.WS != nil:
		return runSubscription(t, s, c, b)
	}
	t.Fatalf("%s has no request", c.Name)
	return nil
}

// triggers counts the calls to triggerTx. Each call sends a new amount, so
// repeated triggers never share a signature.
var triggers uint64

// triggerTx builds the transaction that a subscription waits on.
func triggerTx(t *testing.T, s side, name string) []byte {
	t.Helper()
	triggers++
	payerPub := tc.Pub(payerKey)
	switch name {
	case "transfer":
		return tc.Tx(s.latest(), payerKey, nil, tc.Transfer(payerKey, tc.Pub(recipientKey), 3000+triggers))
	case "tokenTransfer":
		data := binary.LittleEndian.AppendUint64([]byte{12}, triggers)
		data = append(data, mintDecimals)
		return tc.Tx(s.latest(), payerKey, nil, tokenIx(data, tc.Writable(tc.Pub(tokenAKey)),
			tc.Readonly(tc.Pub(mintKey)), tc.Writable(tc.Pub(tokenBKey)), tc.Signer(payerPub, false)))
	}
	t.Fatalf("no trigger %s", name)
	return nil
}

// runSubscription records the acknowledgement, the notifications the trigger
// causes, and the unsubscribe. The read deadline only stops a run that would
// hang. Each read waits for a message, not for time.
func runSubscription(t *testing.T, s side, c transcriptCase, b map[string]jsontext.Value) jsontext.Value {
	t.Helper()
	conn, _, err := websocket.DefaultDialer.Dial(s.wsURL(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var tx []byte
	if c.WS.Trigger != "" {
		tx = triggerTx(t, s, c.WS.Trigger)
		bindString(b, "sig.trigger", sigOf(tx))
	}
	req := map[string]any{"jsonrpc": "2.0", "id": 1, "method": c.WS.Method}
	if len(c.WS.Params) > 0 {
		req["params"] = substitute(t, c.WS.Params, b)
	}
	read := func() jsontext.Value {
		conn.SetReadDeadline(time.Now().Add(90 * time.Second))
		_, msg, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("%s: %v", c.Name, err)
		}
		return msg
	}
	if err := conn.WriteJSON(req); err != nil {
		t.Fatal(err)
	}
	ack := read()
	out := map[string]any{"ack": jsontext.Value(ack)}
	var id struct {
		Result *uint64 `json:"result"`
	}
	json.Unmarshal(ack, &id)
	if id.Result == nil {
		return encode(out)
	}
	if tx != nil {
		s.send(tx, false)
	}
	// The chain side reads every notification the trigger caused, however
	// many, by reading up to the answer of a request sent after it. A live
	// validator is read for the count the case names.
	var notes []jsontext.Value
	if s.drains() {
		if err := conn.WriteJSON(map[string]any{"jsonrpc": "2.0", "id": 3, "method": "getHealth"}); err != nil {
			t.Fatal(err)
		}
		for {
			msg := read()
			if bytes.Contains(msg, []byte(`"id":3`)) {
				break
			}
			if bytes.Contains(msg, []byte(`"method"`)) {
				notes = append(notes, msg)
			}
		}
	} else {
		// A signature subscription ends with its final notification, which
		// carries "err". Agave sends receivedSignature before it only if
		// shreds reach the validator from the network. A single test
		// validator never sees them, so the test reads to the final one.
		final := []byte(`"err"`)
		for len(notes) < c.WS.Notifications {
			msg := read()
			if !bytes.Contains(msg, []byte(`"method"`)) {
				continue
			}
			notes = append(notes, msg)
			if c.WS.Method == "signatureSubscribe" && bytes.Contains(msg, final) {
				break
			}
		}
	}
	out["notifications"] = nil
	if notes != nil {
		out["notifications"] = notes
	}
	unsub := strings.Replace(c.WS.Method, "Subscribe", "Unsubscribe", 1)
	if err := conn.WriteJSON(map[string]any{"jsonrpc": "2.0", "id": 2, "method": unsub, "params": []uint64{*id.Result}}); err != nil {
		t.Fatal(err)
	}
	for {
		msg := read()
		if bytes.Contains(msg, []byte(`"id":2`)) {
			out["unsubscribe"] = jsontext.Value(msg)
			return encode(out)
		}
	}
}

// answer runs the whole corpus on one side, in corpus order.
func answer(t *testing.T, s side, cases []transcriptCase) (map[string]jsontext.Value, map[string]jsontext.Value) {
	t.Helper()
	triggers, refreshes = 0, 0
	b := setup(t, s)
	out := map[string]jsontext.Value{}
	for _, c := range cases {
		if needsFresh(c) {
			fresh(t, s, b)
		}
		out[c.Name] = runCase(t, s, c, b)
	}
	return b, out
}

// enginePin reads the solana-svm version the engine links.
func enginePin(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("../engine/Cargo.toml")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^solana-svm\s*=\s*\{[^}]*version\s*=\s*"=?([0-9.]+)"`).FindSubmatch(b)
	if m == nil {
		m = regexp.MustCompile(`(?m)^solana-svm\s*=\s*"=?([0-9.]+)"`).FindSubmatch(b)
	}
	if m == nil {
		t.Fatal("engine/Cargo.toml pins no solana-svm")
	}
	return string(m[1])
}

func newer(a, b string) bool {
	var x, y [3]int
	fmt.Sscanf(a, "%d.%d.%d", &x[0], &x[1], &x[2])
	fmt.Sscanf(b, "%d.%d.%d", &y[0], &y[1], &y[2])
	for i := range x {
		if x[i] != y[i] {
			return x[i] > y[i]
		}
	}
	return false
}

// TestTheChainAnswersTheAgaveTranscript replays the corpus on the chain. Each
// answer must equal Agave's under the normalizations, or be a divergence that
// the ledger pins with both values.
func TestTheChainAnswersTheAgaveTranscript(t *testing.T) {
	if *record != "" {
		t.Skip("recording")
	}
	cases, corpusHash := loadCorpus(t)
	// TRANSCRIPT_ACCOUNTS writes this chain's genesis program accounts as
	// solana-test-validator --account-dir files and stops, so the recording
	// runs the same program bytes under the same loaders.
	if dir := os.Getenv("TRANSCRIPT_ACCOUNTS"); dir != "" {
		dumpPrograms(t, newChainSide(t), dir)
		return
	}
	// TRANSCRIPT_CHAIN writes the chain's own answers and stops. Use it to
	// check a corpus before it is recorded.
	if path := os.Getenv("TRANSCRIPT_CHAIN"); path != "" {
		b, got := answer(t, newChainSide(t), cases)
		out, _ := json.Marshal(transcript{Bindings: b, Answers: got}, json.Deterministic(true), jsontext.WithIndent(" "))
		if err := os.WriteFile(path, out, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	var want transcript
	readJSON(t, transcriptDir+"/agave.json", &want)
	if want.Header.Agave != agaveReference {
		t.Fatalf("re-record: the transcript is Agave %s and the reference is %s", want.Header.Agave, agaveReference)
	}
	if pin := enginePin(t); newer(pin, agaveReference) {
		t.Fatalf("re-record: the engine links solana-svm %s, past the transcript's %s", pin, agaveReference)
	}
	if want.Header.Corpus != corpusHash {
		t.Fatalf("re-record: gen_corpus.py changed since the transcript was recorded")
	}
	var rules []normalization
	readJSON(t, transcriptDir+"/normalize.json", &rules)
	var ledger []divergence
	readJSON(t, transcriptDir+"/divergences.json", &ledger)

	_, got := answer(t, newChainSide(t), cases)
	var diffs []difference
	for _, c := range cases {
		a, ok := want.Answers[c.Name]
		if !ok {
			t.Fatalf("re-record: the transcript has no answer for %s", c.Name)
		}
		x, err := decode(a)
		if err != nil {
			t.Fatal(err)
		}
		y, err := decode(got[c.Name])
		if err != nil {
			t.Fatal(err)
		}
		if c.HTTP != nil {
			x, y = jsonBody(x), jsonBody(y)
		}
		if c.RPC != nil && c.RPC.Method == "getSupply" {
			// Agave collects the 214 addresses through a HashSet, so their
			// order carries no meaning. They compare as a set.
			x, y = sortedAt(x, "result", "value", "nonCirculatingAccounts"), sortedAt(y, "result", "value", "nonCirculatingAccounts")
		}
		diffs = append(diffs, compare(c.Name, c.method(), x, y, rules)...)
	}
	// TRANSCRIPT_DIFFS writes the differences and stops, for
	// gen_divergences.py to classify (make divergences).
	if path := os.Getenv("TRANSCRIPT_DIFFS"); path != "" {
		if err := os.WriteFile(path, encode(diffs), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	unlisted, stale := reconcile(diffs, ledger)
	for _, d := range unlisted {
		t.Errorf("not in the ledger: %s", d)
	}
	for _, e := range stale {
		t.Errorf("stale ledger entry, its case no longer differs: %s at %s", e.Case, e.Path)
	}
}

// jsonBody reads an HTTP answer's body as JSON if it is a JSON-RPC answer.
// Its values are then compared and normalized as an RPC case's are.
// TestTheHTTPLayerIsAgaves checks the bytes around it: the trailing newline
// and the key order.
func jsonBody(v any) any {
	m, ok := v.(map[string]any)
	if !ok {
		return v
	}
	if b, ok := m["body"].(string); ok {
		if parsed, err := decode(jsontext.Value(b)); err == nil {
			if _, object := parsed.(map[string]any); object {
				m["body"] = parsed
			}
		}
	}
	return v
}

// sortedAt sorts the list of strings at path, in place.
func sortedAt(v any, path ...string) any {
	m, ok := v.(map[string]any)
	for _, k := range path[:len(path)-1] {
		if !ok {
			return v
		}
		m, ok = m[k].(map[string]any)
	}
	if !ok {
		return v
	}
	if list, ok := m[path[len(path)-1]].([]any); ok {
		slices.SortFunc(list, func(a, b any) int { return strings.Compare(fmt.Sprint(a), fmt.Sprint(b)) })
	}
	return v
}

var upgradeableLoader = mustBase58("BPFLoaderUpgradeab1e11111111111111111111111")

// dumpPrograms writes each genesis program, and the ProgramData account of an
// upgradeable one, in the CliAccount JSON that --account-dir reads. The test
// validator inserts its own SPL programs only where no such account was given.
func dumpPrograms(t *testing.T, s side, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	var queue [][32]byte
	for _, p := range tc.ManifestPrograms(t) {
		queue = append(queue, mustBase58(p.Address))
	}
	for len(queue) > 0 {
		k := queue[0]
		queue = queue[1:]
		addr := base58.Encode(k[:])
		var resp struct {
			Result struct {
				Value *struct {
					Lamports   uint64    `json:"lamports"`
					Data       [2]string `json:"data"`
					Owner      string    `json:"owner"`
					Executable bool      `json:"executable"`
					Space      uint64    `json:"space"`
				} `json:"value"`
			} `json:"result"`
		}
		out := s.post(encode(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "getAccountInfo",
			"params": []any{addr, map[string]string{"encoding": "base64"}}}))
		if err := json.Unmarshal(out, &resp); err != nil || resp.Result.Value == nil {
			t.Fatalf("genesis program %s: %s", addr, out)
		}
		v := resp.Result.Value
		file := encode(map[string]any{"pubkey": addr, "account": map[string]any{"lamports": v.Lamports,
			"data": v.Data, "owner": v.Owner, "executable": v.Executable, "rentEpoch": 0, "space": v.Space}})
		if err := os.WriteFile(dir+"/"+addr+".json", file, 0o644); err != nil {
			t.Fatal(err)
		}
		data, _ := base64.StdEncoding.DecodeString(v.Data[0])
		if v.Owner == base58.Encode(upgradeableLoader[:]) && v.Executable && len(data) >= 36 {
			queue = append(queue, [32]byte(data[4:36]))
		}
	}
}

// TestRecordAgaveTranscript records the transcript from a running
// solana-test-validator. It runs only with -record, which
// rpc/testdata/transcript/record.sh passes (make transcript). make test never
// records.
func TestRecordAgaveTranscript(t *testing.T) {
	if *record == "" {
		t.Skip("records only with -record=http://host:port")
	}
	cases, corpusHash := loadCorpus(t)
	l := &liveSide{t: t, url: *record}
	var version struct {
		SolanaCore string `json:"solana-core"`
	}
	json.Unmarshal(l.call("getVersion"), &version)
	if version.SolanaCore != agaveReference {
		t.Fatalf("the validator is %s and the reference is %s", version.SolanaCore, agaveReference)
	}
	bindings, answers := answer(t, l, cases)
	out := transcript{
		Header: transcriptHeader{Agave: version.SolanaCore, Command: *recordCommand, Corpus: corpusHash,
			Recorded: time.Now().UTC().Format(time.RFC3339)},
		Bindings: bindings,
		Answers:  answers,
	}
	b, _ := json.Marshal(out, json.Deterministic(true), jsontext.WithIndent(" "))
	if err := os.WriteFile(transcriptDir+"/agave.json", append(b, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

// inventory lists Agave's RPC methods and subscriptions at the reference tag.
// tools/rpc-inventory.py generates it from Agave's source.
type inventory struct {
	Agave   string `json:"agave"`
	Methods []struct {
		Name string `json:"name"`
		Set  string `json:"set"`
	} `json:"methods"`
	Subscriptions []string `json:"subscriptions"`
}

func loadInventory(t *testing.T) inventory {
	t.Helper()
	var inv inventory
	readJSON(t, "testdata/inventory.json", &inv)
	if inv.Agave != agaveReference {
		t.Fatalf("testdata/inventory.json is Agave %s and the reference is %s; run make inventory", inv.Agave, agaveReference)
	}
	return inv
}

// TestTheTranscriptCoversEveryMethod checks that each of Agave's methods and
// subscriptions has at least one case.
func TestTheTranscriptCoversEveryMethod(t *testing.T) {
	cases, _ := loadCorpus(t)
	asked := map[string]bool{}
	for _, c := range cases {
		asked[c.method()] = true
	}
	inv := loadInventory(t)
	for _, m := range inv.Methods {
		if !asked[m.Name] {
			t.Errorf("gen_corpus.py has no case for %s", m.Name)
		}
	}
	for _, s := range inv.Subscriptions {
		if !asked[s] {
			t.Errorf("gen_corpus.py has no case for %s", s)
		}
	}
	if len(inv.Methods) != 53 || len(inv.Subscriptions) != 9 {
		t.Errorf("Agave %s has %d methods and %d subscriptions; docs/compatibility.md says 53 and 9",
			inv.Agave, len(inv.Methods), len(inv.Subscriptions))
	}
}
