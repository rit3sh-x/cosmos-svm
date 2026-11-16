package rpc

import (
	"context"
	json "encoding/json/v2"
	"testing"
	"time"

	"cosmossdk.io/log/v2"

	"github.com/mr-tron/base58"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/rit3sh-x/cosmos-svm/ffi"
	tc "github.com/rit3sh-x/cosmos-svm/internal/testchain"
)

func TestTheOperatorMethodsAreServed(t *testing.T) {
	faucet := tc.Key(1)
	c := tc.Start(t, tc.Pub(faucet), tc.Options{})
	s := New(c.App, nil, log.NewNopLogger())
	c.Block()

	for _, name := range []string{"getHealth", "getVersion", "getGenesisHash"} {
		if methods[name] == nil {
			t.Fatalf("%s is not in the method table", name)
		}
	}

	// A machine that is committing blocks and knows of no peer answers ok,
	// as a solo Agave validator does.
	got, err := getHealth(context.Background(), s, nil)
	if err != nil || got != "ok" {
		t.Fatalf("getHealth returned %v, %v", got, err)
	}

	version, err := getVersion(context.Background(), s, nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(version, json.Deterministic(true))
	var v struct {
		Core       string  `json:"solana-core"`
		FeatureSet *uint32 `json:"feature-set"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	// The first four bytes of agave_feature_set::ID, Agave v4.3.0's own number.
	if v.Core != apiVersion || v.FeatureSet == nil || *v.FeatureSet != ffi.FeatureSetID() || *v.FeatureSet == 0 {
		t.Fatalf("getVersion returned %s", raw)
	}

	hash := [32]byte{1, 2, 3}
	s.SetGenesisHash(hash)
	if got, err := getGenesisHash(context.Background(), s, nil); err != nil || got != base58.Encode(hash[:]) {
		t.Fatalf("getGenesisHash returned %v, %v", got, err)
	}
}

// TestGetHealthReportsHowFarBehind checks Agave's wire shape for a machine
// behind its peers: -32005, the count in the message, and the count again in
// data.numSlotsBehind.
func TestGetHealthReportsHowFarBehind(t *testing.T) {
	faucet := tc.Key(1)
	c := tc.Start(t, tc.Pub(faucet), tc.Options{})
	s := New(c.App, nil, log.NewNopLogger())
	s.peers.every = 0 // every check reads the registry
	c.Block()
	height := uint64(c.App.LastBlockHeight())

	// The peers' heights are the consensus reactor's, published on the
	// process-global registry. A test stands one in for it.
	peers := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "cometbft_consensus_peer_height",
		Help: "stand-in for the consensus reactor's per-peer heights",
	}, []string{"peer_id"})
	prometheus.MustRegister(peers)
	defer prometheus.Unregister(peers)

	// A machine inside the distance is still ok.
	peers.WithLabelValues("a").Set(float64(height + healthCheckBlockDistance))
	if got, err := getHealth(context.Background(), s, nil); err != nil || got != "ok" {
		t.Fatalf("a machine inside the distance returned %v, %v", got, err)
	}

	behind := uint64(500)
	peers.WithLabelValues("b").Set(float64(height + behind))
	_, err := getHealth(context.Background(), s, nil)
	e, ok := err.(*rpcError)
	if !ok || e.Code != codeNodeUnhealthy {
		t.Fatalf("a machine behind returned %v", err)
	}
	raw, _ := json.Marshal(e, json.Deterministic(true))
	var wire struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    struct {
			NumSlotsBehind *uint64 `json:"numSlotsBehind"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	if wire.Data.NumSlotsBehind == nil || *wire.Data.NumSlotsBehind != behind {
		t.Fatalf("the error carries %s", raw)
	}
	if wire.Message != "Node is behind by 500 slots" {
		t.Fatalf("the message is %q", wire.Message)
	}
}

// TestAHealthCheckReadsTheRegistryOncePerInterval checks the peer height
// cache. A check inside the interval answers from its last read, and a check
// after it reads the registry again. An uncached read before every send costs
// several times the simulation it guards.
func TestAHealthCheckReadsTheRegistryOncePerInterval(t *testing.T) {
	faucet := tc.Key(1)
	c := tc.Start(t, tc.Pub(faucet), tc.Options{})
	s := New(c.App, nil, log.NewNopLogger())
	if s.peers.every != time.Second {
		t.Fatalf("a served machine reads its peers every %s, want a second", s.peers.every)
	}
	s.peers.every = time.Hour
	c.Block()
	height := uint64(c.App.LastBlockHeight())

	peers := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "cometbft_consensus_peer_height",
		Help: "stand-in for the consensus reactor's per-peer heights",
	}, []string{"peer_id"})
	prometheus.MustRegister(peers)
	defer prometheus.Unregister(peers)

	if got, err := getHealth(context.Background(), s, nil); err != nil || got != "ok" {
		t.Fatalf("a machine with no peer ahead returned %v, %v", got, err)
	}
	peers.WithLabelValues("a").Set(float64(height + 500))
	if got, err := getHealth(context.Background(), s, nil); err != nil || got != "ok" {
		t.Fatalf("a check inside the interval read the registry again: %v, %v", got, err)
	}
	s.peers.every = 0
	if _, err := getHealth(context.Background(), s, nil); err == nil {
		t.Fatal("a check after the interval did not read the registry again")
	}
}
