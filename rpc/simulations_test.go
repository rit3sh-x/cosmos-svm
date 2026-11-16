package rpc

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/binary"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"

	"github.com/rit3sh-x/cosmos-svm/ffi"
	tc "github.com/rit3sh-x/cosmos-svm/internal/testchain"
	"github.com/rit3sh-x/cosmos-svm/x/svm"
)

// bigAccounts creates n System accounts of 10 MiB from the faucet. It puts two
// in a transaction, the most one may allocate, and four in a block.
func bigAccounts(t testing.TB, c *tc.Chain, n int) [][32]byte {
	t.Helper()
	const size = 10 << 20
	faucet := tc.Key(1)
	data := binary.LittleEndian.AppendUint32(nil, 0)
	data = binary.LittleEndian.AppendUint64(data, svm.MinimumBalance(size))
	data = binary.LittleEndian.AppendUint64(data, size)
	data = append(data, svm.SystemProgram[:]...)
	create := func(k ed25519.PrivateKey) tc.Instruction {
		return tc.Instruction{Program: svm.SystemProgram, Data: data,
			Accounts: []tc.Meta{tc.Signer(tc.Pub(faucet), true), tc.Signer(tc.Pub(k), true)}}
	}
	// The faucet pays for each account, and a third transaction in a block
	// would take it past its share.
	var keys [][32]byte
	for i := 0; i < n; i += 4 {
		var txs [][]byte
		for j := i; j < i+4 && j < n; j += 2 {
			a, b := tc.Key(byte(100+j)), tc.Key(byte(101+j))
			txs = append(txs, tc.Tx(c.Latest(), faucet, []ed25519.PrivateKey{a, b}, create(a), create(b)))
			keys = append(keys, tc.Pub(a), tc.Pub(b))
		}
		res, _ := c.Block(txs...)
		for k, r := range res {
			if r.Code != 0 {
				t.Fatalf("creating the 10 MiB accounts: transaction %d failed with %d", k, r.Code)
			}
		}
	}
	return keys
}

// simulation is a simulateTransaction request for tx, in base64.
func simulation(tx []byte) call {
	return call{v2: true, id: []byte("1"), method: "simulateTransaction", params: encode(simulationParams(tx))}
}

func simulationParams(tx []byte) []jsontext.Value {
	encoded, _ := json.Marshal(base64.StdEncoding.EncodeToString(tx), json.Deterministic(true))
	cfg, _ := json.Marshal(map[string]any{"encoding": "base64"}, json.Deterministic(true))
	return []jsontext.Value{encoded, cfg}
}

// simulatedErr is the err field of a simulateTransaction answer.
func simulatedErr(t *testing.T, result any) string {
	t.Helper()
	raw, _ := json.Marshal(result, json.Deterministic(true))
	var got struct {
		Value struct {
			Err jsontext.Value `json:"err"`
		} `json:"value"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	return string(got.Value.Err)
}

// rssAnon is the process's resident anonymous memory: the Go heap and the
// engine's allocations together.
func rssAnon(t testing.TB) int64 {
	t.Helper()
	status, err := os.ReadFile("/proc/self/status")
	if err != nil {
		t.Skip("no /proc/self/status")
	}
	for _, line := range strings.Split(string(status), "\n") {
		if v, ok := strings.CutPrefix(line, "RssAnon:"); ok {
			kb, err := strconv.ParseInt(strings.TrimSuffix(strings.TrimSpace(v), " kB"), 10, 64)
			if err != nil {
				t.Fatal(err)
			}
			return kb << 10
		}
	}
	t.Fatal("no RssAnon in /proc/self/status")
	return 0
}

// TestASimulationLoadsAtMost64MiB checks that a simulation of 32 accounts of
// 10 MiB loads at most 64 MiB. The answer equals that of a full load, which is
// what Anza does before it applies its own 64 MiB limit. 64 concurrent
// simulations hold one load's memory, not 64.
func TestASimulationLoadsAtMost64MiB(t *testing.T) {
	if testing.Short() {
		t.Skip("creates 320 MiB of accounts")
	}
	c, s := server(t)
	faucet := tc.Key(1)
	ix := tc.Instruction{Program: svm.SystemProgram}
	for _, k := range bigAccounts(t, c, 32) {
		ix.Accounts = append(ix.Accounts, tc.Readonly(k))
	}
	tx := tc.Tx(c.Latest(), faucet, nil, ix)
	if len(tx) > packetDataSize {
		t.Fatalf("the transaction is %d bytes", len(tx))
	}

	keys, _, err := ffi.TransactionKeys(tx)
	if err != nil {
		t.Fatal(err)
	}
	simulate := func(budget int64) (ffi.Simulation, int64, int) {
		ms, next, err := s.app.Next()
		if err != nil {
			t.Fatal(err)
		}
		w := ffi.NewArenaWriter(ffi.KindSimulateReq, 0)
		w.Push(tx)
		dropped, used := s.app.Stores.PushAccounts(w, ms, keys, budget, nil)
		cut := make([]byte, 0, 32*len(dropped))
		for _, k := range dropped {
			cut = append(cut, k[:]...)
		}
		w.Push(cut)
		sim, err := ffi.Simulate(next.Height, next.Hash, svm.NextOwnHash, false, false, false, w)
		if err != nil {
			t.Fatal(err)
		}
		return sim, used, len(dropped)
	}
	whole, _, _ := simulate(math.MaxInt64)
	sim, used, dropped := simulate(svm.SimulationBytes)
	if used > svm.SimulationBytes || dropped == 0 {
		t.Fatalf("the cut pushed %d bytes and dropped %d keys", used, dropped)
	}
	if string(sim.Err) != `"MaxLoadedAccountsDataSizeExceeded"` || sim.Status != 1 || sim.Fee != 5_000 || sim.LoadedAccountsDataSize != 64<<20 {
		t.Fatalf("simulated: status %d, err %s, fee %d, %d bytes loaded", sim.Status, sim.Err, sim.Fee, sim.LoadedAccountsDataSize)
	}
	// A cut account is not in the arena, so the processor collects 0 for it
	// where loading everything collects its lamports. The rest of the answer
	// is the same.
	for _, s := range []*ffi.Simulation{&sim, &whole} {
		s.PreBalances, s.PostBalances, s.PreTokenBalances, s.PostTokenBalances = nil, nil, nil, nil
	}
	if !reflect.DeepEqual(sim, whole) {
		t.Fatalf("the cut changed the answer: %+v, loading everything gives %+v", sim, whole)
	}
	if got := simulatedErr(t, mustCall(t, s, context.Background(), simulation(tx))); got != `"MaxLoadedAccountsDataSizeExceeded"` {
		t.Fatalf("simulateTransaction answered err %s", got)
	}

	runtime.GC()
	debug.FreeOSMemory()
	idle := rssAnon(t)
	var peak atomic.Int64
	stop := make(chan struct{})
	sampled := make(chan struct{})
	go func() {
		defer close(sampled)
		for {
			if r := rssAnon(t); r > peak.Load() {
				peak.Store(r)
			}
			select {
			case <-stop:
				return
			case <-time.After(time.Millisecond):
			}
		}
	}()
	var wg sync.WaitGroup
	errs := make(chan string, 64)
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp := callOne(s, context.Background(), simulation(tx), "")
			if resp.Error != nil {
				errs <- resp.Error.Message
				return
			}
			raw, _ := json.Marshal(resp.Result, json.Deterministic(true))
			if !bytes.Contains(raw, []byte(`"err":"MaxLoadedAccountsDataSizeExceeded"`)) {
				errs <- string(raw)
			}
		}()
	}
	wg.Wait()
	close(stop)
	<-sampled
	close(errs)
	for e := range errs {
		t.Fatalf("a concurrent simulation answered %s", e)
	}
	grew := peak.Load() - idle
	t.Logf("64 concurrent simulations: anonymous memory peaked %d MiB above an idle %d MiB", grew>>20, idle>>20)
	// One load is the engine's copy of the 64 MiB the cut lets through, plus
	// the 64 MiB Go reads if the store's cache does not hold them. Two loads at
	// once, or one the cut missed (which copies all 320 MiB), are well over
	// the bound. The bound is one 128 MiB load plus 32 MiB for the allocator.
	if grew > 160<<20 {
		t.Fatalf("64 concurrent simulations took anonymous memory %d MiB above idle, want at most 160 (one 128 MiB load and 32 MiB of slack)", grew>>20)
	}
}

// callOne runs one call and returns its answer, as the HTTP path writes it.
func callOne(s *Server, ctx context.Context, req call, peer string) (resp response) {
	s.call(ctx, req, peer, func(r response) { resp = r })
	return resp
}

func mustCall(t *testing.T, s *Server, ctx context.Context, req call) any {
	t.Helper()
	resp := callOne(s, ctx, req, "")
	if resp.Error != nil {
		t.Fatalf("%s: %s", req.method, resp.Error.Message)
	}
	return resp.Result
}

// TestAQueuedSimulationLeavesWithItsClient proves a simulation waits while the
// one slot is taken. A client that hangs up gives its place back, and the
// gauge counts the waiters. A batch takes the slot once per entry, so a batch
// of two finishes with one slot.
func TestAQueuedSimulationLeavesWithItsClient(t *testing.T) {
	c, s := server(t)
	// Both waiting simulations hold an HTTP slot.
	s.SetThreads(2)
	srv := httptest.NewServer(http.HandlerFunc(s.serveHTTP))
	t.Cleanup(srv.Close)
	faucet := tc.Key(1)
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "simulateTransaction",
		"params": simulationParams(tc.Tx(c.Latest(), faucet, nil, tc.Transfer(faucet, tc.Pub(tc.Key(2)), 1_000_000_000)))}, json.Deterministic(true))
	post := func(ctx context.Context, body []byte) ([]byte, error) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		var out bytes.Buffer
		_, err = out.ReadFrom(resp.Body)
		return out.Bytes(), err
	}
	gauge := func() float64 {
		var m dto.Metric
		_ = simulationsWaiting.Write(&m)
		return m.GetGauge().GetValue()
	}
	waiting := func(want float64) {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); gauge() != want; {
			if time.Now().After(deadline) {
				t.Fatalf("%v simulations waiting, want %v", gauge(), want)
			}
			time.Sleep(time.Millisecond)
		}
	}

	// The cleanup gives the slot back before the server closes. Close waits
	// for every queued handler, so a failure here fails and does not hang.
	s.simulations <- struct{}{}
	release := sync.OnceFunc(func() { <-s.simulations })
	t.Cleanup(release)
	ctx, hangUp := context.WithCancel(context.Background())
	left := make(chan error, 1)
	go func() { _, err := post(ctx, body); left <- err }()
	waiting(1)
	stayed := make(chan []byte, 1)
	go func() { out, _ := post(context.Background(), body); stayed <- out }()
	waiting(2)
	hangUp()
	if err := <-left; err == nil {
		t.Fatal("the client that hung up was answered")
	}
	waiting(1)
	release()
	if out := <-stayed; !bytes.Contains(out, []byte(`"err":null`)) {
		t.Fatalf("the client that stayed was answered %s", out)
	}
	waiting(0)

	batch := []byte("[" + string(body) + "," + string(body) + "]")
	out, err := post(context.Background(), batch)
	if err != nil || bytes.Count(out, []byte(`"err":null`)) != 2 {
		t.Fatalf("a batch of two simulations: %s, %v", out, err)
	}
}
