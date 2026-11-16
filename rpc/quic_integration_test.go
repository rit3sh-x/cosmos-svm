package rpc

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	cmted25519 "github.com/cometbft/cometbft/crypto/ed25519"
	cmttypes "github.com/cometbft/cometbft/types"
	"github.com/mr-tron/base58"
	"github.com/prometheus/client_golang/prometheus"

	"cosmossdk.io/log/v2"

	tc "github.com/rit3sh-x/cosmos-svm/internal/testchain"
	"github.com/rit3sh-x/cosmos-svm/x/svm"
)

// quicBinaries returns the built svm-tpu and tools/quic-client, which is
// Agave's streamer test client. With SVM_REQUIRE_QUIC set, as `make test` sets
// it in CI, a missing binary fails the test. Without it, the test skips.
func quicBinaries(t *testing.T) (tpu, client string) {
	t.Helper()
	root, _ := filepath.Abs("..")
	tpu = filepath.Join(root, "tpu", "target", "release", "svm-tpu")
	client = filepath.Join(root, "tools", "quic-client", "target", "release", "quic-client")
	for _, p := range []string{tpu, client} {
		if _, err := os.Stat(p); err != nil {
			if os.Getenv("SVM_REQUIRE_QUIC") != "" {
				t.Fatalf("%s is not built", p)
			}
			t.Skipf("%s is not built: cargo build --release --locked --manifest-path in tpu/ and tools/quic-client/", p)
		}
	}
	return tpu, client
}

// freeUDP is a loopback UDP port nothing holds.
func freeUDP(t *testing.T) string {
	t.Helper()
	c, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	return c.LocalAddr().String()
}

// within polls a condition the test is waiting on. The deadline only stops a
// test that would hang, and decides nothing.
func within(t *testing.T, d time.Duration, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(d); !ok(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("waited %v for %s", d, what)
		}
	}
}

// quicChain is a chain whose machine runs the port with cfg.
type quicChain struct {
	c      *tc.Chain
	m      *comet
	s      *Server
	faucet ed25519.PrivateKey
	addr   string
	child  chan *os.Process
}

func startQuic(t *testing.T, cfg QuicConfig) *quicChain {
	t.Helper()
	tpu, _ := quicBinaries(t)
	q := &quicChain{faucet: tc.Key(1), child: make(chan *os.Process, 16)}
	q.c = tc.Start(t, tc.Pub(q.faucet), tc.Options{})
	self := cmted25519.PubKey(tc.OperatorKey(0).Public().(ed25519.PublicKey))
	q.m = newComet(q.c, self, cmttypes.NewValidator(self, 10))
	q.s = New(q.c.App, q.m, log.NewNopLogger())
	q.c.Block()
	cfg.Binary, cfg.Bind = tpu, freeUDP(t)
	q.addr = cfg.Bind
	q.s.quic.start = func(cfg QuicConfig) (child, error) {
		c, err := startTPU(cfg)
		if err == nil {
			q.child <- c.(*process).cmd.Process
		}
		return c, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = q.s.ServeQUIC(ctx, cfg); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	within(t, 30*time.Second, "svm-tpu to be ready", func() bool { return q.s.quicAddress() != nil })
	return q
}

// client runs tools/quic-client and returns its lines.
func (q *quicChain) client(t *testing.T, args ...string) []string {
	t.Helper()
	_, client := quicBinaries(t)
	out, err := exec.Command(client, append([]string{args[0], q.addr}, args[1:]...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("quic-client %v: %v\n%s", args, err, out)
	}
	return strings.Split(strings.TrimSpace(string(out)), "\n")
}

// send sends each transaction over QUIC. It waits until the ingress counter
// shows that svmd handed every one to the pool.
func (q *quicChain) send(t *testing.T, txs ...[]byte) {
	t.Helper()
	before := ingressTotal("quic")
	var files []string
	for i, tx := range txs {
		f := filepath.Join(t.TempDir(), "tx"+string(rune('a'+i)))
		if err := os.WriteFile(f, tx, 0o644); err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
	}
	q.client(t, append([]string{"send"}, files...)...)
	within(t, 30*time.Second, "the QUIC transactions to reach the pool", func() bool {
		return ingressTotal("quic") >= before+float64(len(txs))
	})
}

func ingressTotal(path string) float64 {
	var n float64
	for _, outcome := range []string{"admitted", "rejected", "duplicate"} {
		n += metricValue("svm_ingress_total", map[string]string{"path": path, "outcome": outcome})
	}
	return n
}

// metricValue reads one series from the process registry.
func metricValue(name string, labels map[string]string) float64 {
	families, _ := prometheus.DefaultGatherer.Gather()
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.GetMetric() {
			match := true
			for _, l := range m.GetLabel() {
				if want, ok := labels[l.GetName()]; ok && want != l.GetValue() {
					match = false
				}
			}
			if match {
				if c := m.GetCounter(); c != nil {
					return c.GetValue()
				}
				return m.GetGauge().GetValue()
			}
		}
	}
	return 0
}

// TestTheQuicPortLandsATransaction proves a transaction sent over QUIC lands,
// and a stream over 4,096 bytes closes the connection with code 5. While the
// child is down, tpuQuic is null.
func TestTheQuicPortLandsATransaction(t *testing.T) {
	q := startQuic(t, QuicConfig{PerPeer: 8, PerIPPerMinute: 100, MaxUnstakedConns: 2000})
	var nodes []map[string]any
	mustAnswer(t, q.s, &nodes, "getClusterNodes")
	if nodes[0]["tpuQuic"] != q.addr || nodes[0]["tpuForwardsQuic"] != q.addr {
		t.Fatalf("a ready child advertises %v", nodes[0])
	}

	tx := tc.Tx(q.c.Latest(), q.faucet, nil, tc.Transfer(q.faucet, tc.Pub(tc.Key(40)), svm.MinimumBalance(0)))
	q.send(t, tx)
	pool := q.m.pool()
	if len(pool) != 1 || !bytes.Equal(pool[0], tx) {
		t.Fatalf("the pool holds %d transactions", len(pool))
	}
	if res, _ := q.c.Block(pool...); res[0].Code != 0 {
		t.Fatalf("the QUIC transaction committed as %d", res[0].Code)
	}
	if err := q.s.notify(uint64(q.c.Height)); err != nil {
		t.Fatal(err)
	}
	var statuses struct {
		Value []*struct {
			ConfirmationStatus string `json:"confirmationStatus"`
		} `json:"value"`
	}
	mustAnswer(t, q.s, &statuses, "getSignatureStatuses", []string{base58.Encode(tx[1:65])})
	if statuses.Value[0] == nil || statuses.Value[0].ConfirmationStatus != "finalized" {
		t.Fatal("the QUIC transaction did not land")
	}

	if got := q.client(t, "oversized"); got[0] != "closed 5" {
		t.Fatalf("a 4,097-byte stream got %q, want the connection closed with code 5", got)
	}

	process := <-q.child
	if err := process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	within(t, 30*time.Second, "the killed child to be withdrawn", func() bool { return q.s.quicAddress() == nil })
	mustAnswer(t, q.s, &nodes, "getClusterNodes")
	if nodes[0]["tpuQuic"] != nil {
		t.Fatalf("a killed child is still advertised: %v", nodes[0])
	}
	within(t, 30*time.Second, "the child to restart", func() bool { return q.s.quicAddress() != nil })
}

// TestTheQuicPerPeerCapClosesWithTooMany proves that, with the rate raised
// and two per IP, a third connection closes with code 4.
func TestTheQuicPerPeerCapClosesWithTooMany(t *testing.T) {
	q := startQuic(t, QuicConfig{PerPeer: 2, PerIPPerMinute: 100, MaxUnstakedConns: 2000})
	got := q.client(t, "connections", "3")
	if len(got) != 3 || got[0] == "closed 4" || got[1] == "closed 4" || got[2] != "closed 4" {
		t.Fatalf("three connections from one IP gave %q, want the third closed with code 4", got)
	}
}

// TestTheQuicConnectionRateIsCounted proves that, with Agave's defaults
// (eight a minute, eight open), a ninth new connection in a burst is refused.
// The rate is checked before the per-peer cap. The streamer's per-IP rate
// counter reaches svm_quic_total.
func TestTheQuicConnectionRateIsCounted(t *testing.T) {
	q := startQuic(t, QuicConfig{PerPeer: 8, PerIPPerMinute: 8, MaxUnstakedConns: 2000})
	limited := map[string]string{"event": "connection_rate_limited_per_ipaddr"}
	before := metricValue("svm_quic_total", limited)
	got := q.client(t, "connections", "9")
	// The streamer checks the rate twice: before the handshake, where it
	// ignores the connection, and after it, where it closes it with code 2.
	// Which one the ninth meets depends on whether the eighth has finished
	// its handshake. Both raise the same counter.
	if got[8] != "no handshake" && got[8] != "closed 2" {
		t.Fatalf("the ninth connection in a minute was not refused for the rate: %q", got)
	}
	// The streamer reports on solana-metrics' cadence, ten seconds.
	within(t, 60*time.Second, "the rate-limited counter to rise", func() bool {
		return metricValue("svm_quic_total", limited) > before
	})
}

// TestQuicAndSendTransactionGiveTheSameOutcome proves each entry sent through
// sendTransaction with skipPreflight and through QUIC gets the same refusal
// reason, or both reach the pool. QUIC has no reply, so the test reads its
// outcome from the counters.
func TestQuicAndSendTransactionGiveTheSameOutcome(t *testing.T) {
	// The admission reasons are x/svm's counters, which svmd registers.
	svm.RegisterMetrics(nil)
	q := startQuic(t, QuicConfig{PerPeer: 8, PerIPPerMinute: 100, MaxUnstakedConns: 2000})
	poor := tc.Key(41)
	dest := func(i byte) [32]byte { return tc.Pub(tc.Key(50 + i)) }
	transfer := func(i byte) []byte {
		return tc.Tx(q.c.Latest(), q.faucet, nil, tc.Transfer(q.faucet, dest(i), svm.MinimumBalance(0)+uint64(i)))
	}
	processed := transfer(0)
	if res, _ := q.c.Block(processed); res[0].Code != 0 {
		t.Fatalf("the processed transfer committed as %d", res[0].Code)
	}
	forged := func(tx []byte) []byte { f := append([]byte(nil), tx...); f[1] ^= 1; return f }
	expired := func(i byte) []byte {
		return tc.Tx(tc.HashAt(999_999), q.faucet, nil, tc.Transfer(q.faucet, dest(i), 1))
	}
	unpayable := func(i byte) []byte {
		return tc.Tx(q.c.Latest(), poor, nil, tc.Transfer(poor, dest(i), uint64(i)+1))
	}
	oversized := func(i byte) []byte {
		ix := tc.Instruction{Program: svm.SystemProgram, Data: bytes.Repeat([]byte{i}, 1_300)}
		return tc.Tx(q.c.Latest(), q.faucet, nil, ix)
	}
	garbage := func(i byte) []byte { return bytes.Repeat([]byte{i, 0xee}, 40) }
	// A transaction already in the pool, so both sends are its duplicate.
	duplicate := transfer(9)
	if _, err := submit(q.s, duplicate, map[string]any{"skipPreflight": true}); err != nil {
		t.Fatal(err)
	}

	for _, entry := range []struct {
		name     string
		rpc, qic []byte
		want     string
	}{
		{"valid transfer", transfer(1), transfer(2), "admitted"},
		{"bad signature", forged(transfer(3)), forged(transfer(4)), "rejected"},
		{"expired blockhash", expired(5), expired(6), "blockhash_not_found"},
		{"already processed", processed, processed, "already_processed"},
		{"payer that cannot pay", unpayable(7), unpayable(8), "unpayable"},
		{"legacy over 1,232 bytes", oversized(12), oversized(13), "rejected"},
		{"bytes that are not a transaction", garbage(14), garbage(15), "rejected"},
		{"duplicate", duplicate, duplicate, "duplicate"},
	} {
		if got := q.outcomeOverRPC(t, entry.rpc); got != entry.want {
			t.Errorf("%s over sendTransaction: %s, want %s", entry.name, got, entry.want)
		}
		if got := q.outcomeOverQuic(t, entry.qic); got != entry.want {
			t.Errorf("%s over QUIC: %s, want %s", entry.name, got, entry.want)
		}
	}
}

var admissionReasons = []string{"rejected", "blockhash_not_found", "already_processed", "unpayable"}

// outcome reads which counter one hand-off moved: the pool's admission or
// duplicate, or the admission reason of a refusal.
func outcome(before map[string]float64) string {
	for _, reason := range admissionReasons {
		if metricValue("svm_admission_rejected_total", map[string]string{"reason": reason}) > before[reason] {
			return reason
		}
	}
	for _, o := range []string{"admitted", "duplicate", "rejected"} {
		if metricValue("svm_ingress_total", map[string]string{"outcome": o, "path": "quic"})+
			metricValue("svm_ingress_total", map[string]string{"outcome": o, "path": "rpc"}) > before["ingress:"+o] {
			return o
		}
	}
	return "nothing"
}

func snapshotCounters() map[string]float64 {
	out := map[string]float64{}
	for _, reason := range admissionReasons {
		out[reason] = metricValue("svm_admission_rejected_total", map[string]string{"reason": reason})
	}
	for _, o := range []string{"admitted", "duplicate", "rejected"} {
		out["ingress:"+o] = metricValue("svm_ingress_total", map[string]string{"outcome": o, "path": "quic"}) +
			metricValue("svm_ingress_total", map[string]string{"outcome": o, "path": "rpc"})
	}
	return out
}

// outcomeOverRPC sends with skipPreflight. A refusal that the RPC gives
// before the pool (Agave's sanitize gives it too) counts as "rejected", the
// class of the engine's admission refusal.
func (q *quicChain) outcomeOverRPC(t *testing.T, tx []byte) string {
	t.Helper()
	before := snapshotCounters()
	if _, err := submit(q.s, tx, map[string]any{"skipPreflight": true}); err != nil {
		if re := asRPCError(err); re.Code == codeInvalidParams {
			return "rejected"
		}
		t.Fatalf("sendTransaction: %v", err)
	}
	return outcome(before)
}

func (q *quicChain) outcomeOverQuic(t *testing.T, tx []byte) string {
	t.Helper()
	before := snapshotCounters()
	q.send(t, tx)
	return outcome(before)
}
