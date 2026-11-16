package rpc

import (
	"context"
	"encoding/json/jsontext"
	"fmt"
	"sync"
	"time"

	"github.com/mr-tron/base58"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/rit3sh-x/cosmos-svm/ffi"
)

// codeNodeUnhealthy is Agave's JSON_RPC_SERVER_ERROR_NODE_UNHEALTHY.
const codeNodeUnhealthy = -32005

// healthCheckBlockDistance is how many blocks behind the highest peer this
// machine may be and still answer ok. It is a block count, not a duration, and
// it is machine-local. Agave ships 128 slots, about 51 s at 400 ms. Eight
// blocks is about 3 s at the shipped timeout_commit of 400 ms, and longer when
// blocks are full. One slow commit then is not an outage, but a stalled
// machine is.
const healthCheckBlockDistance = 8

// nodeUnhealthy returns Agave's error. data.numSlotsBehind carries the count
// when there is one, and is present and null when there is not.
func nodeUnhealthy(behind *uint64) error {
	e := &rpcError{Code: codeNodeUnhealthy, Message: "Node is unhealthy"}
	if behind != nil {
		e.Message = fmt.Sprintf("Node is behind by %d slots", *behind)
	}
	e.Data = struct {
		NumSlotsBehind *uint64 `json:"numSlotsBehind"`
	}{behind}
	return e
}

// getHealth compares this machine's committed height with its peers' heights,
// as Agave's health check does. The cluster tip must come from outside this
// machine's own replay. Otherwise a machine checks its health against itself.
//
// This machine's height is the app's committed height. The peers' heights are
// the CometBFT consensus reactor's own view, exported per peer as
// cometbft_consensus_peer_height. The reactor learns them from consensus
// messages, not from this machine's commits, as Agave learns its cluster tip
// from gossip.
func getHealth(_ context.Context, s *Server, _ []jsontext.Value) (any, error) {
	if behind, ok := s.health(); !ok {
		return nil, nodeUnhealthy(behind)
	}
	return "ok", nil
}

// health is Agave's RpcHealthStatus: ok, behind by a count of blocks, or
// unknown, which is unhealthy with no count.
func (s *Server) health() (behind *uint64, ok bool) {
	if s.node != nil {
		status, err := s.node.Status(context.Background())
		if err != nil || status.SyncInfo.CatchingUp {
			return nil, false
		}
	}
	height := s.app.CommittedHeight()
	peak, known := s.peers.highest()
	if !known || height+healthCheckBlockDistance >= peak {
		return nil, true
	}
	n := peak - height
	return &n, false
}

// healthText answers GET /health with the same check as getHealth.
func (s *Server) healthText() string {
	switch behind, ok := s.health(); {
	case ok:
		return "ok"
	case behind != nil:
		return "behind"
	}
	return "unknown"
}

// highestPeerHeight reads the consensus reactor's per-peer heights from the
// process-global registry. CometBFT publishes them there, and nowhere else in
// this process are they visible. An operator who turns off instrumentation
// gives up the comparison, and getHealth then answers on catching_up alone.
func highestPeerHeight() (uint64, bool) {
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		return 0, false
	}
	var peak float64
	var known bool
	for _, f := range families {
		if f.GetName() != "cometbft_consensus_peer_height" {
			continue
		}
		for _, m := range f.GetMetric() {
			v := gaugeValue(m)
			if v <= 0 {
				continue
			}
			known = true
			peak = max(peak, v)
		}
	}
	return uint64(peak), known
}

// peerHeights caches highestPeerHeight's answer for an interval, one second on
// a served machine. A read gathers the whole process-global registry, about
// 0.45 ms on the reference VM. Preflight runs it before every send's
// simulation, so an uncached read costs several times the simulation. The
// answer is compared with a distance of eight blocks, so a one-second-old
// answer changes nothing that an operator or a wallet can see.
type peerHeights struct {
	mu    sync.Mutex
	every time.Duration
	at    time.Time
	peak  uint64
	known bool
}

func (p *peerHeights) highest() (uint64, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if now := time.Now(); p.at.IsZero() || now.Sub(p.at) >= p.every {
		p.peak, p.known = highestPeerHeight()
		p.at = now
	}
	return p.peak, p.known
}

func gaugeValue(m *dto.Metric) float64 {
	if g := m.GetGauge(); g != nil {
		return g.GetValue()
	}
	return 0
}

// getVersion returns Agave's RpcVersionInfo. feature-set is the first four
// bytes of agave_feature_set::ID, which names the gate list that the linked
// crate knows. So it is Agave v4.3.0's own number. svmmeta records which of
// those gates a chain has active, as the bank does on Agave.
func getVersion(_ context.Context, _ *Server, _ []jsontext.Value) (any, error) {
	return map[string]any{"solana-core": apiVersion, "feature-set": ffi.FeatureSetID()}, nil
}

// getGenesisHash returns the sha256 of the genesis file that this machine
// started from, in base58 like every hash this RPC returns. Two machines that
// disagree about it cannot produce the same app hash at block 1. CometBFT's
// handshake compares neither hash.
func getGenesisHash(_ context.Context, s *Server, _ []jsontext.Value) (any, error) {
	return base58.Encode(s.genesis[:]), nil
}
