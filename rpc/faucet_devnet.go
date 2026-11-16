//go:build devnet

package rpc

import (
	"context"
	"crypto/ed25519"
	"encoding/binary"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/mr-tron/base58"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/rit3sh-x/cosmos-svm/ffi"
	"github.com/rit3sh-x/cosmos-svm/x/svm"
)

// faucet is the devnet faucet. A devnet build holds a private key and signs
// with it, and that is its only difference from a production build. This file
// holds the key, the signature and the method they serve. A production binary
// does not compile it.
//
// The faucet does not mint. It signs an ordinary System transfer with its key
// and submits it through the send path that every client uses. The chain state
// machine does not change.
type faucet struct {
	key    ed25519.PrivateKey
	pubkey [32]byte
	limits *limiter
}

// loaded is this machine's faucet, or nil when no key is configured. The key is
// the account, so one process holds at most one key. That makes loaded a
// package value, not a field on every Server.
var loaded *faucet

// loadFaucet reads the key file and makes this machine a faucet. An empty path
// leaves requestAirdrop unserved. That is the normal state of every devnet
// machine except the faucet machine. A devnet is a chain of many machines, and
// a key on all of them means a copy of one private key for every operator.
func loadFaucet(path string, rows []window, perRequest uint64) error {
	if path == "" {
		loaded = nil
		return nil
	}
	key, err := readKeypair(path)
	if err != nil {
		return err
	}
	loaded = &faucet{key: key, pubkey: [32]byte(key.Public().(ed25519.PublicKey)), limits: newLimiter(rows, perRequest)}
	return nil
}

// readKeypair reads the solana-keygen key file. `read_keypair` requires a JSON
// array of exactly KEYPAIR_LENGTH (64) elements from ed25519_dalek. The array
// is the seed, then the public key. That layout is `ed25519.PrivateKey` byte
// for byte, so the loader is an unmarshal and a length check. `solana-keygen`
// is the only tool an operator needs.
//
// Every failure names the path and the reason, and svmd does not start.
// CometBFT does the same for its own key file. `loadFilePV` exits on a read
// error and on a parse error. A configured key that does not load is an
// operator error, and startup is when the operator watches.
func readKeypair(path string) (ed25519.PrivateKey, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("faucet-key %s: %w", path, err)
	}
	// CometBFT and Agave do not make this check. CometBFT writes its privval
	// key at 0600 and does not check the mode on read. Agave's
	// read_keypair_file does not check it either. svmd adds the check because
	// it costs four lines.
	if mode := info.Mode().Perm(); mode&0o077 != 0 {
		return nil, fmt.Errorf("faucet-key %s is mode %#o: a key readable by group or other is refused, run chmod 600", path, mode)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("faucet-key %s: %w", path, err)
	}
	// encoding/json decodes a []byte from base64, but solana-keygen writes
	// numbers.
	var bytes []int
	if err := json.Unmarshal(raw, &bytes, agaveInput); err != nil {
		return nil, fmt.Errorf("faucet-key %s is not solana-keygen's format, a JSON array of 64 bytes: %w", path, err)
	}
	if len(bytes) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("faucet-key %s holds %d bytes, solana-keygen writes %d", path, len(bytes), ed25519.PrivateKeySize)
	}
	key := make(ed25519.PrivateKey, ed25519.PrivateKeySize)
	for i, b := range bytes {
		if b < 0 || b > 255 {
			return nil, fmt.Errorf("faucet-key %s holds %d at offset %d, which is not a byte", path, b, i)
		}
		key[i] = byte(b)
	}
	return key, nil
}

// devnetMethod serves requestAirdrop when this machine holds a key. Without a
// key, a devnet machine answers -32600 Invalid request (noFaucet), as a
// production binary does.
//
// requestAirdrop uses this hook, not the method table, for two reasons. It is
// the only method that reads the caller's address. The table is one map that
// every Server in the process shares, but the peer changes per request.
func devnetMethod(method, peer string) (handler, bool) {
	if method != "requestAirdrop" || loaded == nil {
		return nil, false
	}
	return func(_ context.Context, s *Server, params []jsontext.Value) (any, error) {
		return loaded.airdrop(s, peer, params)
	}, true
}

// airdrop serves Agave's requestAirdrop. It signs a System transfer from the
// faucet account to the recipient and submits it through the ordinary send
// path. The faucet account is the account of the loaded key, as in Agave, so
// no second setting can name the wrong account.
//
// The checks and refusals follow Agave's order:
//   - the recipient key
//   - the recentBlockhash the client sent, or else the latest blockhash
//   - the faucet, whose refusal reaches the client as Internal error alone
func (f *faucet) airdrop(s *Server, peer string, params []jsontext.Value) (any, error) {
	to, err := pubkeyParam(params, 0)
	if err != nil {
		return nil, err
	}
	var lamports uint64
	if err := param(params, 1, &lamports); err != nil {
		return nil, err
	}
	var cfg struct {
		RecentBlockhash *string `json:"recentBlockhash"`
	}
	if err := param(params, 2, &cfg); err != nil {
		return nil, err
	}
	var blockhash []byte
	if cfg.RecentBlockhash != nil {
		var fault string
		if blockhash, fault = parseBase58(*cfg.RecentBlockhash, 32); fault != "" {
			return nil, invalidParams("Invalid param: %s", fault)
		}
	} else {
		ms, _, err := s.snapshot()
		if err != nil {
			return nil, err
		}
		blockhash = ms.GetKVStore(s.app.Stores.Meta).Get(svm.MetaBlockhashKey)
	}
	refused := &rpcError{Code: codeInternal, Message: "Internal error"}
	if lamports == 0 || len(blockhash) != 32 {
		return nil, refused
	}
	if err := f.limits.admit(peer, to, lamports); err != nil {
		s.logger.Info("the faucet refused an airdrop", "to", base58.Encode(to[:]), "err", err)
		return nil, refused
	}
	return s.broadcast(f.transfer([32]byte(blockhash), to, lamports), "rpc")
}

// transfer builds and signs a legacy message with one System transfer. The
// faucet is the payer and the only signer.
//
// The general builder is in internal/testchain, which imports testing, so this
// function encodes the message itself. Three accounts in a fixed order reduce
// the compiler to a header and a list. The payer is the writable signer, the
// recipient is the writable non-signer, and the program is the readonly
// non-signer. Every count is below 128, so each ShortU16 is one byte.
func (f *faucet) transfer(blockhash, to [32]byte, lamports uint64) []byte {
	data := binary.LittleEndian.AppendUint32(nil, 2) // SystemInstruction::Transfer
	data = binary.LittleEndian.AppendUint64(data, lamports)

	m := []byte{1, 0, 1} // signers, readonly signers, readonly non-signers
	m = append(m, 3)     // account keys
	m = append(m, f.pubkey[:]...)
	m = append(m, to[:]...)
	m = append(m, svm.SystemProgram[:]...)
	m = append(m, blockhash[:]...)
	m = append(m, 1)       // instructions
	m = append(m, 2)       // program id index
	m = append(m, 2, 0, 1) // account indexes: the faucet, then the recipient
	m = append(m, byte(len(data)))
	m = append(m, data...)

	tx := append([]byte{1}, ed25519.Sign(f.key, m)...)
	return append(tx, m...)
}

var metricsOnce sync.Once

// devnetReady registers the faucet balance gauge, once per process.
//
// The gauge comes with the rent-exempt floor, because the account cannot be
// spent below that floor. A System transfer that leaves a non-zero balance
// below the minimum fails, because `transition_allowed` returns false from
// RentExempt to RentPaying. SIMD-0392 does not relax this for a debit. An
// empty faucet shows as InsufficientFundsForRent, not as a zero balance. An
// alert on zero reports funds while every request fails.
func devnetReady(s *Server) {
	if loaded == nil {
		return
	}
	metricsOnce.Do(func() {
		prometheus.MustRegister(&faucetCollector{s: s, f: loaded})
	})
}

type faucetCollector struct {
	s *Server
	f *faucet
}

var (
	faucetBalance = prometheus.NewDesc("svm_faucet_lamports",
		"the devnet faucet account's balance", nil, nil)
	faucetFloor = prometheus.NewDesc("svm_faucet_floor_lamports",
		"the rent-exempt minimum the faucet account cannot be spent below", nil, nil)
)

func (c *faucetCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- faucetBalance
	ch <- faucetFloor
}

func (c *faucetCollector) Collect(ch chan<- prometheus.Metric) {
	ms, _, err := c.s.snapshot()
	if err != nil {
		c.s.logger.Error("faucet balance", "err", err)
		return
	}
	var lamports uint64
	if v := c.s.app.Stores.GetAccount(ms, c.f.pubkey); v != nil {
		lamports = ffi.Lamports(v)
	}
	ch <- prometheus.MustNewConstMetric(faucetBalance, prometheus.GaugeValue, float64(lamports))
	// The floor uses the current Rent sysvar price (Rent::minimum_balance(0)),
	// so a rent cut by governance moves it in the same block.
	perByte, err := c.s.app.Stores.LamportsPerByte(ms)
	if err != nil {
		c.s.logger.Error("faucet floor", "err", err)
		return
	}
	ch <- prometheus.MustNewConstMetric(faucetFloor, prometheus.GaugeValue, float64(svm.RentExemptMinimum(perByte, 0)))
}

// FaucetPubkey returns the account that the transfers come from, for tests and
// for the runbook's rotation step. It returns "" when no key is loaded.
func FaucetPubkey() string {
	if loaded == nil {
		return ""
	}
	return base58.Encode(loaded.pubkey[:])
}

// FaucetLimit is one row of the window table as app.toml writes it. It is the
// only faucet type that a caller outside this package names. The table and the
// per-request cap are machine-local settings with protective defaults. An
// operator can lower or raise them, or add rows. Unlike index-max-bytes, an
// unset value does not stop svmd from starting. A devnet with a default is
// better than a devnet that does not start until its faucet is tuned.
type FaucetLimit struct {
	Window   time.Duration
	Requests uint64
	Lamports uint64
}

// LoadFaucet configures this machine's faucet, or leaves it without one when
// path is empty. An empty table and a zero cap take the shipped defaults.
func LoadFaucet(path string, limits []FaucetLimit, perRequest uint64) error {
	rows := defaultWindows()
	if len(limits) > 0 {
		rows = nil
		for _, l := range limits {
			if l.Window <= 0 || l.Requests == 0 || l.Lamports == 0 {
				return fmt.Errorf("faucet-limit row %+v: window, requests and lamports must all be set", l)
			}
			rows = append(rows, window{l.Window, l.Requests, l.Lamports})
		}
	}
	if perRequest == 0 {
		perRequest = defaultPerRequest
	}
	return loadFaucet(path, rows, perRequest)
}
