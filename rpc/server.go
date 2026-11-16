// Package rpc is this chain's machine-local Solana JSON-RPC server.
// It reads committed state, never writes it, and answers every
// request at finalized commitment.
//
// A method is one row in methods and one handler. A subscription is
// one row in subscriptions. Nothing else routes.
package rpc

import (
	"context"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"runtime"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/gorilla/websocket"
	"github.com/prometheus/client_golang/prometheus"

	"cosmossdk.io/log/v2"

	"github.com/cosmos/cosmos-sdk/client"
	storetypes "github.com/cosmos/cosmos-sdk/store/v2/types"

	"github.com/rit3sh-x/cosmos-svm/app"
)

// apiVersion is the Agave release this RPC follows.
const apiVersion = "4.3.0"

// JSON-RPC 2.0 and Agave error codes.
const (
	codeParse          = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternal       = -32603
	// Agave's JSON_RPC_SERVER_ERROR_SEND_TRANSACTION_PREFLIGHT_FAILURE.
	codeSendTransactionFailure = -32002
	// Agave's JSON_RPC_SERVER_ERROR_BLOCK_NOT_AVAILABLE,
	// MIN_CONTEXT_SLOT_NOT_REACHED and FILTER_TRANSACTION_NOT_FOUND.
	codeBlockNotAvailable             = -32004
	codeKeyExcludedFromSecondaryIndex = -32010
	codeScanError                     = -32012
	codeMinContextSlot                = -32016
	codeFilterTransactionNotFound     = -32020
)

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	// Data is Agave's error payload, present only where Agave has one, such
	// as getHealth's numSlotsBehind.
	Data any `json:"data,omitzero"`
}

func (e *rpcError) Error() string { return e.Message }

func invalidParams(format string, args ...any) error {
	return &rpcError{Code: codeInvalidParams, Message: fmt.Sprintf(format, args...)}
}

type handler func(ctx context.Context, s *Server, params []jsontext.Value) (any, error)

var methods = map[string]handler{
	"getAccountInfo":                    getAccountInfo,
	"getMultipleAccounts":               getMultipleAccounts,
	"getProgramAccounts":                getProgramAccounts,
	"getBalance":                        getBalance,
	"getLatestBlockhash":                getLatestBlockhash,
	"isBlockhashValid":                  isBlockhashValid,
	"getFeeForMessage":                  getFeeForMessage,
	"getMinimumBalanceForRentExemption": getMinimumBalanceForRentExemption,
	"sendTransaction":                   sendTransaction,
	"simulateTransaction":               simulateTransaction,
	"getSignatureStatuses":              getSignatureStatuses,
	"getBlockHeight":                    getBlockHeight,
	"getSlot":                           getBlockHeight,
	"getTransaction":                    getTransaction,
	"getSignaturesForAddress":           getSignaturesForAddress,
	"getTokenAccountsByOwner":           getTokenAccountsByOwner,
	"getTokenAccountBalance":            getTokenAccountBalance,
	"getBlock":                          getBlock,
	"getFirstAvailableBlock":            getFirstAvailableBlock,
	"getRecentPrioritizationFees":       getRecentPrioritizationFees,
	"getHealth":                         getHealth,
	"getVersion":                        getVersion,
	"requestAirdrop":                    noFaucet, // a devnet build serves it
	"getGenesisHash":                    getGenesisHash,
	"getSlotLeaders":                    getSlotLeaders,
	"getLeaderSchedule":                 getLeaderSchedule,
	"getSlotLeader":                     getSlotLeader,
	"getBlockProduction":                getBlockProduction,
	"getEpochSchedule":                  getEpochSchedule,
	"getEpochInfo":                      getEpochInfo,
	"getIdentity":                       getIdentity,
	"getClusterNodes":                   getClusterNodes,
	"getVoteAccounts":                   getVoteAccounts,
	"getInflationGovernor":              getInflationGovernor,
	"getInflationRate":                  getInflationRate,
	"getInflationReward":                getInflationReward,
	"getBlocks":                         getBlocks,
	"getBlocksWithLimit":                getBlocksWithLimit,
	"getBlockTime":                      getBlockTime,
	"minimumLedgerSlot":                 minimumLedgerSlot,
	"getHighestSnapshotSlot":            getHighestSnapshotSlot,
	"getMaxShredInsertSlot":             getMaxShredInsertSlot,
	"getMaxRetransmitSlot":              getMaxRetransmitSlot,
	"getBlockCommitment":                getBlockCommitment,
	"getAgGenesisCert":                  getAgGenesisCert,
	"getTokenSupply":                    getTokenSupply,
	"getTokenLargestAccounts":           getTokenLargestAccounts,
	"getTokenAccountsByDelegate":        getTokenAccountsByDelegate,
	"getSupply":                         getSupply,
	"getLargestAccounts":                getLargestAccounts,
	"getTransactionCount":               getTransactionCount,
	"getRecentPerformanceSamples":       getRecentPerformanceSamples,
}

type subscriptionHandler func(s *Server, c *wsConn, params []jsontext.Value) (any, error)

var subscriptions = map[string]subscriptionHandler{
	"signatureSubscribe":      signatureSubscribe,
	"signatureUnsubscribe":    signatureUnsubscribe,
	"accountSubscribe":        accountSubscribe,
	"accountUnsubscribe":      unsubscribe,
	"programSubscribe":        programSubscribe,
	"programUnsubscribe":      unsubscribe,
	"logsSubscribe":           logsSubscribe,
	"logsUnsubscribe":         unsubscribe,
	"slotSubscribe":           slotSubscribe,
	"slotUnsubscribe":         unsubscribe,
	"slotsUpdatesSubscribe":   slotsUpdatesSubscribe,
	"slotsUpdatesUnsubscribe": unsubscribe,
	"rootSubscribe":           rootSubscribe,
	"rootUnsubscribe":         unsubscribe,
	// Agave serves getVersion on the websocket too.
	"getVersion": func(s *Server, _ *wsConn, params []jsontext.Value) (any, error) {
		return getVersion(context.Background(), s, params)
	},
}

// Server serves JSON-RPC over HTTP and subscriptions over a websocket.
type Server struct {
	app    *app.App
	node   client.CometRPC
	logger log.Logger
	// genesis is the sha256 of the genesis file this machine started from.
	// getGenesisHash serves it. It is zero until SetGenesisHash sets it.
	genesis [32]byte
	// genesisTime is the CometBFT genesis document's time in Unix seconds.
	// getBlockTime answers it at slot 0, as Agave answers its genesis
	// creation time.
	genesisTime int64
	// addrs are the HTTP and websocket addresses Serve listens on.
	// getClusterNodes gives them as this machine's rpc and pubsub.
	addrs struct{ rpc, ws *string }
	// quic is the QUIC port's child process and the address it advertises.
	quic quicState

	// The three websocket bounds. Their products are the machine's ceilings.
	maxConns      int
	maxSubs       int
	maxQueueBytes int
	deadlines     deadlines

	// threads holds one token for each HTTP request, from the time its body
	// is read until its answer is written. Its capacity is [svm] rpc-threads.
	// A request takes its simulation or scan slot while it holds this one.
	threads chan struct{}
	// writeTimeout is [svm] rpc-write-timeout: the total time one answer may
	// spend on writes before its client is cut. writeClock measures it.
	writeTimeout time.Duration
	writeClock   quicClock
	// simulations holds one token for each simulation in flight
	// (simulateTransaction or a sendTransaction preflight). Its capacity is
	// [svm] rpc-max-simulations.
	simulations chan struct{}
	// scans holds one token for each full scan of committed state in
	// flight. Its capacity is [svm] rpc-max-scans.
	scans chan struct{}
	// scanLimit is [svm] rpc-scan-results-limit-mb in bytes. Zero is no limit.
	scanLimit int
	// origins is [svm] rpc-cors-origins, for both ports.
	origins Origins
	// receipts carries each block's signatures from the runner's
	// receivedSignature hook to Follow, so the hook never waits on mu.
	receipts chan receipt

	// peers is getHealth's view of the highest peer height. It is read at
	// most once an interval.
	peers peerHeights

	mu sync.Mutex
	// live is the number of open websocket connections. serveWS reserves a
	// slot before the upgrade, so a connection refused for the cap never opens.
	live int
	// nextSub is the next distinct subscription's id. shared is every
	// distinct subscription by its params.
	nextSub uint64
	shared  map[string]*shared
	// watchers is the signature index. Each subscription lives in its
	// connection's map, so this holds the same pointers.
	watchers map[[64]byte][]*sub
	// receivers counts the signature subscriptions that asked for
	// receivedSignature. The runner's hook is set while it is above zero.
	receivers int
	// The four fan-out indexes. A written account costs one map lookup on
	// its pubkey and one on its owner.
	accounts map[[32]byte][]*sub
	programs map[[32]byte][]*sub
	logs     []*sub
	slots    []*sub
	// updates are the slotsUpdatesSubscribe subscribers.
	updates []*sub
	// roots are the rootSubscribe subscribers.
	roots []*sub
	// largest is getLargestAccounts' cache and its one scan.
	largest largest
	// perfClock paces the performance sampler.
	perfClock quicClock
	// fees is the minimum compute unit price of the last 150 blocks, oldest
	// first.
	fees []prioritizationFee
	// notified is the highest height whose signature subscribers have been
	// served. Statuses and subscriptions read history up to it. So history
	// answers a subscription, or Follow notifies it, never both.
	notified uint64
}

// New returns a Server over a and node, with every bound at its default.
func New(a *app.App, node client.CometRPC, logger log.Logger) *Server {
	s := &Server{
		app: a, node: node, logger: logger,
		maxConns:      DefaultWSMaxConnections,
		maxSubs:       DefaultWSMaxSubscriptions,
		maxQueueBytes: DefaultWSMaxQueueBytes,
		deadlines:     deadlines{idle: idleTimeout, ping: pingEvery, header: headerTimeout},
		peers:         peerHeights{every: time.Second},
		threads:       make(chan struct{}, runtime.GOMAXPROCS(0)),
		writeTimeout:  DefaultWriteTimeout,
		writeClock:    wallClock{},
		simulations:   make(chan struct{}, 1),
		scans:         make(chan struct{}, scanSlots(runtime.GOMAXPROCS(0))),
		origins:       AnyOrigin,
		receipts:      make(chan receipt, maxReceipts),
		largest:       largest{clock: wallClock{}, entries: map[string]largestAccounts{}},
		perfClock:     wallClock{},
		watchers:      map[[64]byte][]*sub{},
		shared:        map[string]*shared{},
		accounts:      map[[32]byte][]*sub{},
		programs:      map[[32]byte][]*sub{},
		notified:      a.CommittedHeight(),
	}
	registerOnce.Do(func() {
		prometheus.MustRegister(requestsWaiting, simulationsWaiting, scansWaiting, ingress, quicEvents, quicLevels)
	})
	devnetReady(s)
	return s
}

var registerOnce sync.Once

// requestsWaiting counts HTTP requests queued for an rpc-threads slot. A high
// value tells an operator to raise rpc-threads.
var requestsWaiting = prometheus.NewGauge(prometheus.GaugeOpts{
	Name: "svm_rpc_requests_waiting",
	Help: "HTTP requests queued for one of the rpc-threads slots.",
})

// simulationsWaiting counts simulations queued for a slot. A high value tells
// an operator to raise rpc-max-simulations.
var simulationsWaiting = prometheus.NewGauge(prometheus.GaugeOpts{
	Name: "svm_rpc_simulations_waiting",
	Help: "simulateTransaction calls queued for one of the rpc-max-simulations slots.",
})

// scansWaiting counts full scans queued for a slot. A high value tells an
// operator to raise rpc-max-scans.
var scansWaiting = prometheus.NewGauge(prometheus.GaugeOpts{
	Name: "svm_rpc_scans_waiting",
	Help: "Full scans of committed state queued for one of the rpc-max-scans slots.",
})

// scanSlots is Agave's default for --rpc-blocking-threads: 1.max(num_cpus / 4).
// GOMAXPROCS and num_cpus both honour a cgroup CPU limit.
func scanSlots(procs int) int { return max(1, procs/4) }

// scanning reports whether a call reads all of committed state or walks the
// proposer schedule. Such a call takes a scan slot. Point reads never wait
// for a scan slot. They wait only for an rpc-threads slot.
func scanning(method string, params []jsontext.Value) bool {
	switch method {
	case "getProgramAccounts":
		return true
	case "getLeaderSchedule", "getSlotLeaders", "getBlockProduction":
		// Each one walks up to two epochs of proposer priorities, about
		// 864,000 steps. Agave reads a schedule that it computes once an
		// epoch. One request can batch hundreds of these calls.
		return true
	case "getTokenAccountsByDelegate":
		// Only the {programId} path scans. {mint} reads the mint keyspace.
		f, err := tokenAccountsFilter(params, 1)
		return err == nil && f.program != nil
	}
	return false
}

// The websocket's three bounds. They bound memory in a process that listens
// on localhost. If they run out, a subscribe is refused and the machine runs on.
//
// The products match Agave's numbers. 1,024 connections by 1,024
// subscriptions is 1,048,576, against its DEFAULT_MAX_ACTIVE_SUBSCRIPTIONS of
// 1,000,000. 1,024 connections by 256 KB is 256 MB, against its
// DEFAULT_QUEUE_CAPACITY_BYTES.
const (
	DefaultWSMaxConnections   = 1024
	DefaultWSMaxSubscriptions = 1024
	DefaultWSMaxQueueBytes    = 256 << 10
)

// SetLimits sets the websocket bounds an operator configured. Zero keeps the
// default, as it does for the engine's two machine-local settings.
func (s *Server) SetLimits(connections, subscriptions, queueBytes int) {
	if connections > 0 {
		s.maxConns = connections
	}
	if subscriptions > 0 {
		s.maxSubs = subscriptions
	}
	if queueBytes > 0 {
		s.maxQueueBytes = queueBytes
	}
}

// DefaultWriteTimeout is the default of [svm] rpc-write-timeout. At this
// value, a client slower than about 35 MB/s on a 1 GB answer is cut, in a
// batch or not.
const DefaultWriteTimeout = 30 * time.Second

// SetThreads sets how many HTTP requests are answered at once. Agave's
// --rpc-threads does the same, with the same default of one per CPU. Zero
// keeps the default. SetThreads returns the value in use for the start log.
func (s *Server) SetThreads(n int) int {
	if n > 0 {
		s.threads = make(chan struct{}, n)
	}
	return cap(s.threads)
}

// SetWriteTimeout sets how long one answer may spend writing. Zero keeps the
// default.
func (s *Server) SetWriteTimeout(d time.Duration) {
	if d > 0 {
		s.writeTimeout = d
	}
}

// SetMaxSimulations sets how many simulations load accounts at once. Each
// holds up to 128 MiB. Zero keeps the default of one.
func (s *Server) SetMaxSimulations(n int) {
	if n > 0 {
		s.simulations = make(chan struct{}, n)
	}
}

// SetMaxScans sets how many full scans of committed state run at once. Zero
// keeps the default. It returns the value in use, for the start log.
func (s *Server) SetMaxScans(n int) int {
	if n > 0 {
		s.scans = make(chan struct{}, n)
	}
	return cap(s.scans)
}

// SetScanLimit sets how many MiB of accounts one scan may keep before it
// stops. It is Agave's --accounts-index-scan-results-limit-mb. Zero is no
// limit, which is the Agave default.
func (s *Server) SetScanLimit(mb int) { s.scanLimit = max(mb, 0) << 20 }

// scanned is what a scan has kept. It counts the same way as Agave's
// calc_scan_result_size: the data of each account, plus 64 bytes for an
// AccountSharedData and 32 bytes for an address.
type scanned struct{ sum, limit int }

// keep counts one account of n data bytes. When the sum passes the limit, it
// returns the Agave abort: ScanError::Aborted as RpcCustomError::ScanError.
func (b *scanned) keep(n int) error {
	if b.limit == 0 {
		return nil
	}
	if b.sum += n + 64 + 32; b.sum > b.limit {
		return &rpcError{Code: codeScanError, Message: "scan aborted: The accumulated scan results exceeded the limit"}
	}
	return nil
}

// SetOrigins sets the origins a browser may call both ports from. The
// default is AnyOrigin, as on Agave.
func (s *Server) SetOrigins(o Origins) { s.origins = o }

// SetGenesisTime sets getBlockTime's answer at slot 0.
func (s *Server) SetGenesisTime(unix int64) { s.genesisTime = unix }

// SetGenesisHash sets the hash of the genesis file this machine started from.
// The binary reads the file. A test that does not need it leaves it zero.
func (s *Server) SetGenesisHash(hash [32]byte) { s.genesis = hash }

// Serve runs the HTTP and websocket listeners until ctx ends.
func (s *Server) Serve(ctx context.Context, httpAddr, wsAddr string) error {
	s.addrs.rpc, s.addrs.ws = &httpAddr, &wsAddr
	servers := []*http.Server{
		s.bounded(httpAddr, http.HandlerFunc(s.serveHTTP)),
		s.bounded(wsAddr, http.HandlerFunc(s.serveWS)),
	}
	errs := make(chan error, len(servers))
	for _, srv := range servers {
		ln, err := net.Listen("tcp", srv.Addr)
		if err != nil {
			return err
		}
		go func() { errs <- srv.Serve(ln) }()
	}
	s.logger.Info("solana json-rpc listening", "http", httpAddr, "ws", wsAddr)
	select {
	case <-ctx.Done():
	case err := <-errs:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}
	for _, srv := range servers {
		_ = srv.Close()
	}
	return nil
}

// bounded returns the http.Server for both ports. It sets no WriteTimeout,
// because that timeout counts the whole life of a request. Instead, answer
// gives rpc-write-timeout to its writes only. The websocket keeps its own
// deadlines, because gorilla clears the connection's deadline when it hijacks.
func (s *Server) bounded(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: s.deadlines.header,
		IdleTimeout:       s.deadlines.idle,
	}
}

// response is one answer. JSONRPC is empty for a method call that did not
// send "jsonrpc": "2.0". jsonrpc-core answers such a call without the field.
type response struct {
	JSONRPC string         `json:"jsonrpc"`
	ID      jsontext.Value `json:"id"`
	Result  any            `json:"result,omitzero"`
	Error   *rpcError      `json:"error,omitzero"`
}

// wire is the value that a response is encoded as. It keeps "result": null
// for a null result, which clients read as "not found". It leaves result out
// of an error. It is a plain value, not a MarshalJSON method, because that
// method made each answer be encoded twice.
func (r response) wire() any {
	if r.Error != nil {
		return struct {
			JSONRPC string         `json:"jsonrpc,omitempty"`
			Error   *rpcError      `json:"error"`
			ID      jsontext.Value `json:"id"`
		}{r.JSONRPC, r.Error, r.ID}
	}
	return struct {
		JSONRPC string         `json:"jsonrpc,omitempty"`
		Result  any            `json:"result"`
		ID      jsontext.Value `json:"id"`
	}{r.JSONRPC, r.Result, r.ID}
}

// answer runs one body, a call or a batch, and writes its answer to w. It
// writes nothing if there is nothing to answer. It encodes each entry once,
// directly to w. It writes a batch one entry at a time, so
// it holds the answer of only one entry at once. A failed write ends the
// batch, because the client reads no more of it.
//
// The answer may spend rpc-write-timeout on writes, in total. The deadline of
// each write is the remaining budget. The time that a call runs or waits for
// a slot does not count. A budget at or below zero ends the answer. So a slow
// reader holds its slot for one timeout, however many entries it asks for.
func (s *Server) answer(ctx context.Context, body []byte, peer string, w http.ResponseWriter) {
	calls, batch, ok := parseBody(body)
	deadline := http.NewResponseController(w)
	left := s.writeTimeout
	var err error
	write := func(f func() error) {
		if err != nil {
			return
		}
		start := s.writeClock.Now()
		_ = deadline.SetWriteDeadline(start.Add(left))
		err = f()
		if left -= s.writeClock.Now().Sub(start); err == nil && left <= 0 {
			err = os.ErrDeadlineExceeded
		}
	}
	written := 0
	emit := func(r response) {
		write(func() error {
			if batch {
				sep := ","
				if written == 0 {
					sep = "["
				}
				if _, err := io.WriteString(w, sep); err != nil {
					return err
				}
			}
			return writeEntry(w, r)
		})
		written++
	}
	if !ok {
		emit(response{JSONRPC: "2.0", ID: jsontext.Value("null"), Error: &rpcError{Code: codeParse, Message: "Parse error"}})
	} else {
		for _, c := range calls {
			if err != nil {
				return
			}
			s.call(ctx, c, peer, emit)
		}
	}
	if written > 0 {
		end := "\n"
		if batch {
			end = "]\n"
		}
		write(func() error {
			_, err := io.WriteString(w, end)
			return err
		})
	}
}

// writeEntry encodes one response to w, but without a
// buffer that holds the whole answer.
func writeEntry(w io.Writer, r response) error {
	return json.MarshalWrite(w, r.wire(), json.Deterministic(true))
}

// maxBodyBytes is Agave's MAX_REQUEST_BODY_SIZE, `50 * (1 << 10)` (50 KiB, not
// MiB). No request this RPC serves comes close. getMultipleAccounts with
// its full 100 keys is about 5 kB.
const maxBodyBytes = 50 << 10

// call runs one request. If the request is answered, call gives the answer to
// emit. A request with no id is a JSON-RPC notification. svmd executes it and
// writes nothing. The test is `req.id == nil`. jsontext.Value is nil only if
// the key is absent, so an explicit `"id": null` is still answered.
//
// peer is the caller's address, as caller reads it. Only the devnet faucet
// reads it, to rate-limit. Agave's faucet is a separate process that sees its
// own RPC's address. That address is loopback in the standard deployment,
// which the limit exempts.
//
// A simulation or a full scan takes a slot first. If no slot is free, it
// waits until one frees or ctx, the HTTP request's context, ends. So a client
// that hangs up gives its place back. A batch takes a slot once per entry.
// call gives the slot back after emit writes the answer, so the slot bounds
// the answer until it is out.
func (s *Server) call(ctx context.Context, req call, peer string, emit func(response)) {
	var held chan struct{}
	defer func() {
		if held != nil {
			<-held
		}
	}()
	if resp, answered := s.run(ctx, req, peer, &held); answered {
		emit(resp)
	}
}

// run does the work of call up to the answer. It sets held to the pool whose
// slot it took.
func (s *Server) run(ctx context.Context, req call, peer string, held *chan struct{}) (response, bool) {
	resp := response{JSONRPC: "2.0", ID: req.id}
	answered := req.id != nil
	if resp.ID == nil {
		resp.ID = jsontext.Value("null")
	}
	if req.invalid {
		resp.Error = &rpcError{Code: codeInvalidRequest, Message: "Invalid request"}
		return resp, true
	}
	if !req.v2 {
		// jsonrpc-core answers with its own version, which is none.
		resp.JSONRPC = ""
		resp.Error = &rpcError{Code: codeInvalidRequest, Message: "Unsupported JSON-RPC protocol version"}
		return resp, answered
	}
	// A devnet build with a key serves requestAirdrop in place of the table's
	// refusal. It needs the peer address, which the table's handlers never
	// see.
	h, ok := devnetMethod(req.method, peer)
	if !ok {
		h, ok = methods[req.method]
	}
	if !ok {
		s.logger.Debug("method not served", "method", req.method)
		resp.Error = &rpcError{Code: codeMethodNotFound, Message: "Method not found"}
		return resp, answered
	}
	params, err := positional(req.params, arities[req.method])
	if err != nil {
		resp.Error = asRPCError(err)
		return resp, answered
	}
	var slots chan struct{}
	var waiting prometheus.Gauge
	switch {
	case req.method == "simulateTransaction" || req.method == "sendTransaction" && preflights(params):
		slots, waiting = s.simulations, simulationsWaiting
	case scanning(req.method, params):
		slots, waiting = s.scans, scansWaiting
	}
	if slots != nil {
		waiting.Inc()
		select {
		case slots <- struct{}{}:
			waiting.Dec()
			*held = slots
		case <-ctx.Done():
			waiting.Dec()
			resp.Error = asRPCError(ctx.Err())
			return resp, answered
		}
	}
	result, err := h(ctx, s, params)
	if err != nil {
		resp.Error = asRPCError(err)
		return resp, answered
	}
	resp.Result = result
	return resp, answered
}

// noFaucet is requestAirdrop on a machine with no faucet. Agave registers
// the method and refuses it before it reads a parameter.
func noFaucet(context.Context, *Server, []jsontext.Value) (any, error) {
	return nil, &rpcError{Code: codeInvalidRequest, Message: "Invalid request"}
}

// preflights reports whether a sendTransaction will simulate. It does unless
// it sets skipPreflight. A preflight loads what a simulation loads, so it
// takes a simulation slot.
func preflights(params []jsontext.Value) bool {
	var cfg sendConfig
	return param(params, 1, &cfg) != nil || !cfg.SkipPreflight
}

func asRPCError(err error) *rpcError {
	var re *rpcError
	if errors.As(err, &re) {
		return re
	}
	return &rpcError{Code: codeInternal, Message: err.Error()}
}

var upgrader = websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}

// The websocket's read bounds.
//
// maxMessageBytes is Agave's `set_max_message_size(4_096)`. Agave's second
// call, set_max_frame_size, needs no counterpart. gorilla has no frame
// setting, and a frame cannot exceed the message limit.
//
// idleTimeout and pingEvery are a named divergence from Agave, which needs
// neither. Every Agave connection reads one shared broadcast, so a dead peer
// falls behind and Agave drops it. Here a connection's queue gets only its own
// matches, so it never fills for an idle peer, and the server must reap it.
// A deadline alone would reap the blocking Rust PubsubClient, which sends
// nothing itself and stays alive only by answering. That is why the server
// pings.
const maxMessageBytes = 4096

const (
	idleTimeout = 60 * time.Second
	pingEvery   = 54 * time.Second
	writeWait   = 10 * time.Second
	// headerTimeout bounds the time before a connection has a request to
	// answer or a websocket to reap. net/http has no default for it. Without
	// it, a peer that opens a socket and sends nothing holds a goroutine forever.
	headerTimeout = 10 * time.Second
)

// deadlines holds, per server, the three timeouts a test shortens. New
// copies the constants in, and a test changes its own server instead of
// waiting a minute. They are not package variables, so a test cannot change
// them under connections that still read them.
type deadlines struct {
	idle, ping, header time.Duration
}

// wsConn is one websocket with its own queue and its own writer. Only that
// writer writes to the socket. So a peer that stops reading stalls only
// itself, and Follow never blocks on a write.
//
// mu orders the queue. A subscribe acknowledgement must reach the client
// before any notification for the subscription it created. So the request
// path registers and enqueues its acknowledgement under mu, and deliver
// takes the same lock.
type wsConn struct {
	conn *websocket.Conn
	mu   sync.Mutex

	// queue holds the messages that the writer has yet to send, oldest
	// first. mu guards it. ready holds one signal that the queue is not
	// empty. The byte bound is the only bound. A channel of 4,096 slots
	// allocated 96 KiB at connect, also for a connection that never gets a
	// message.
	queue  [][]byte
	ready  chan struct{}
	done   chan struct{}
	closed sync.Once

	queued int           // bytes on queue, guarded by mu
	max    int           // the queue bound, fixed at construction
	ping   time.Duration // the server's ping interval, fixed at construction

	// subs is every subscription this connection holds, by id. An
	// unsubscribe may name only these, the cap counts them, and a disconnect
	// removes them. The server's registry lock guards it, not mu. This is
	// Agave's shape. Ids come from one server-wide counter, and the handles
	// live in a map per connection.
	subs map[uint64]*sub
}

func newWSConn(conn *websocket.Conn, maxBytes int, ping time.Duration) *wsConn {
	return &wsConn{
		conn:  conn,
		ready: make(chan struct{}, 1),
		done:  make(chan struct{}),
		max:   maxBytes,
		ping:  ping,
		subs:  map[uint64]*sub{},
	}
}

// write queues one reply. The caller holds mu.
func (c *wsConn) write(v any) {
	b, err := json.Marshal(v, json.Deterministic(true))
	if err == nil && c.room(len(b)) {
		c.push(b)
	}
}

// deliver queues one notification. The caller does not hold mu. The size of a
// notification is known before it is built, so nothing is built for a
// connection that is closed or has no room for it. One block can owe a
// connection more than its queue bound.
func (c *wsConn) deliver(n note) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.room(n.size()) {
		c.push(n.bytes())
	}
}

// room takes n bytes of the queue and reports whether they were free. The
// caller holds mu. A message that would exceed the bound closes the
// connection. That is Agave's policy for a subscriber that falls behind: "In
// both possible error cases we disconnect the client".
func (c *wsConn) room(n int) bool {
	select {
	case <-c.done:
		return false
	default:
	}
	if c.queued+n > c.max {
		c.kill()
		return false
	}
	c.queued += n
	return true
}

// push gives the writer a message whose room is taken. The caller holds mu.
func (c *wsConn) push(b []byte) {
	c.queue = append(c.queue, b)
	c.signal()
}

// signal wakes the writer. One waiting signal is enough.
func (c *wsConn) signal() {
	select {
	case c.ready <- struct{}{}:
	default:
	}
}

// next takes the oldest message off the queue. It reports whether more
// messages wait. When the queue is empty, next releases its array.
func (c *wsConn) next() (b []byte, more bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.queue) == 0 {
		return nil, false
	}
	b, c.queue[0] = c.queue[0], nil
	if c.queue = c.queue[1:]; len(c.queue) == 0 {
		c.queue = nil
	}
	c.queued -= len(b)
	return b, c.queue != nil
}

// kill closes the connection. That unblocks the reader and ends the writer.
// serveWS cleans up the registry when the reader exits.
func (c *wsConn) kill() {
	c.closed.Do(func() {
		close(c.done)
		_ = c.conn.Close()
	})
}

func (c *wsConn) run() {
	ping := time.NewTicker(c.ping)
	defer ping.Stop()
	for {
		select {
		case <-c.done:
			return
		case <-ping.C:
			// WriteControl is the one write gorilla allows beside this
			// goroutine's own writes. The ping keeps alive a client that
			// only reads.
			if err := c.conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(writeWait)); err != nil {
				c.kill()
				return
			}
		case <-c.ready:
			b, more := c.next()
			if more {
				// Send one message a turn, so the select still sees the ping
				// and done cases.
				c.signal()
			}
			if b == nil {
				continue
			}
			// Without this deadline, the write blocks while the peer's
			// receive window stays shut. A peer that keeps sending but reads
			// nothing refreshes its read deadline, so the reader never reaps
			// it. The queue bound closes it only if new messages arrive.
			_ = c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.conn.WriteMessage(websocket.TextMessage, b); err != nil {
				c.kill()
				return
			}
		}
	}
}

// openConn reserves one of the machine's connection slots and reports
// whether one was free. A refused connection has no JSON-RPC channel yet, so
// it cannot get the subscribe refusal. It gets HTTP 503 before the upgrade.
// Agave has no such case.
func (s *Server) openConn() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.live >= s.maxConns {
		return false
	}
	s.live++
	return true
}

func (s *Server) closeConn() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.live--
}

func (s *Server) serveWS(w http.ResponseWriter, r *http.Request) {
	// svmd refuses an unlisted origin before it takes a connection slot. It
	// uses jsonrpc-http-server's text, because Agave never refuses a
	// handshake and has no text of its own.
	if !s.origins.upgrade(headerText(r.Header.Get("Origin")), r.Host) {
		plain(w, http.StatusForbidden, textOriginRefused)
		return
	}
	if !s.openConn() {
		http.Error(w, "websocket connection limit reached", http.StatusServiceUnavailable)
		return
	}
	defer s.closeConn()
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	conn.SetReadLimit(maxMessageBytes)
	// Any frame refreshes the deadline. So a pong answers the writer's ping,
	// and a ping from the client is enough on its own. gorilla's ping handler
	// stays, because it sends the pong back.
	alive := func() { _ = conn.SetReadDeadline(time.Now().Add(s.deadlines.idle)) }
	alive()
	conn.SetPongHandler(func(string) error { alive(); return nil })
	pong := conn.PingHandler()
	conn.SetPingHandler(func(data string) error { alive(); return pong(data) })
	c := newWSConn(conn, s.maxQueueBytes, s.deadlines.ping)
	go c.run()
	defer func() {
		s.dropConn(c)
		c.kill()
	}()
	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			return
		}
		alive()
		// Agave closes a connection whose message is not UTF-8.
		if !utf8.Valid(msg) {
			return
		}
		c.mu.Lock()
		s.wsAnswer(c, msg)
		c.mu.Unlock()
	}
}

// wsAnswer runs one websocket message under the connection's write lock. It
// uses the HTTP path's framing, because jsonrpc-core runs both. If a
// subscribe's first notification is already due, the acknowledgement goes first.
func (s *Server) wsAnswer(c *wsConn, msg []byte) {
	calls, batch, ok := parseBody(msg)
	if !ok {
		c.write(response{JSONRPC: "2.0", ID: jsontext.Value("null"), Error: &rpcError{Code: codeParse, Message: "Parse error"}}.wire())
		return
	}
	var resps []any
	var then []any
	for _, req := range calls {
		resp, answered, due := s.wsCall(c, req)
		if answered {
			resps = append(resps, resp.wire())
			if due != nil {
				then = append(then, due)
			}
		}
	}
	switch {
	case len(resps) == 0:
	case batch:
		c.write(resps)
	default:
		c.write(resps[0])
	}
	for _, n := range then {
		c.write(n)
	}
}

// wsCall runs one call of a websocket message. A request with no id is a
// notification. svmd executes it and never answers. web3.js sends one every
// five seconds.
func (s *Server) wsCall(c *wsConn, req call) (resp response, answered bool, due any) {
	resp = response{JSONRPC: "2.0", ID: req.id}
	answered = req.id != nil
	if resp.ID == nil {
		resp.ID = jsontext.Value("null")
	}
	if req.invalid {
		resp.Error = &rpcError{Code: codeInvalidRequest, Message: "Invalid request"}
		return resp, true, nil
	}
	if !req.v2 {
		resp.JSONRPC = ""
		resp.Error = &rpcError{Code: codeInvalidRequest, Message: "Unsupported JSON-RPC protocol version"}
		return resp, answered, nil
	}
	h, ok := subscriptions[req.method]
	if !ok {
		resp.Error = &rpcError{Code: codeMethodNotFound, Message: "Method not found"}
		return resp, answered, nil
	}
	params, err := positional(req.params, arities[req.method])
	if err != nil {
		resp.Error = asRPCError(err)
		return resp, answered, nil
	}
	result, err := h(s, c, params)
	if err != nil {
		resp.Error = asRPCError(err)
		return resp, answered, nil
	}
	if n, ok := result.(ackThen); ok {
		resp.Result = n.ack
		return resp, answered, n.then
	}
	resp.Result = result
	return resp, answered, nil
}

// ackThen is a subscribe result whose notification is already due. The
// acknowledgement goes first, then the notification, under one write lock.
type ackThen struct {
	ack  any
	then any
}

// snapshot returns committed state at the latest height.
func (s *Server) snapshot() (storetypes.MultiStore, uint64, error) {
	ms, h, err := s.app.Committed()
	if err != nil {
		return nil, 0, &rpcError{Code: codeInternal, Message: err.Error()}
	}
	return ms, h, nil
}

// nextHeight is the block that a transaction sent now can first execute in.
// Admission and simulation run at that height.
func (s *Server) nextHeight() (uint64, error) {
	_, next, err := s.app.Next()
	if err != nil {
		return 0, &rpcError{Code: codeInternal, Message: err.Error()}
	}
	return next.Height, nil
}

type contextSlot struct {
	Slot       uint64 `json:"slot"`
	APIVersion string `json:"apiVersion"`
}

type withContext struct {
	Context contextSlot `json:"context"`
	Value   any         `json:"value"`
}

func at(slot uint64, v any) withContext {
	return withContext{contextSlot{slot, apiVersion}, v}
}
