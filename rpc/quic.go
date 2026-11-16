package rpc

import (
	"bufio"
	"context"
	"encoding/binary"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// The QUIC port. svm-tpu runs as a child of svmd. It is Agave's
// solana-streamer 4.3.0 with an empty stake map. It writes each transaction it
// receives to its standard output as a frame. svmd hands each one to
// broadcast with path quic, the hand-off sendTransaction makes with
// skipPreflight. svmd restarts the child when it exits. This machine
// advertises the child's address as every leader's tpuQuic only between the
// ready frame and the child's exit.

// QuicConfig holds the QUIC port's settings. An empty Bind turns the port off.
type QuicConfig struct {
	Binary string
	Bind   string
	// Public is the advertised address, or empty to advertise Bind.
	Public string
	// Agave's three unstaked bounds, with its names and defaults.
	PerPeer          int
	PerIPPerMinute   int
	MaxUnstakedConns int
}

// Agave's unstaked defaults.
const (
	DefaultTPUConnectionsPerPeer             = 8
	DefaultTPUConnectionsPerIPMinute         = 8
	DefaultTPUMaxUnstakedConnections         = 2000
	maxQuicTransaction                       = 4096
	maxQuicFrame                             = 1 << 16
	frameTransaction, frameReady, frameStats = 0, 1, 2
)

// Advertised is the address getClusterNodes gives while the child is ready.
func (c QuicConfig) Advertised() string {
	if c.Public != "" {
		return c.Public
	}
	return c.Bind
}

// Check returns the start refusals that need no running chain: no binary, an
// unspecified bind with no public address, and an IPv6 advertised address.
// The v4.3.0 client binds IPv4, so it cannot reach an IPv6 address.
func (c QuicConfig) Check() error {
	if c.Binary == "" {
		return errors.New("svm.tpu-quic-address is set, and no svm-tpu binary was found: put it beside svmd or set svm.tpu-quic-binary")
	}
	if info, err := os.Stat(c.Binary); err != nil || info.IsDir() {
		return fmt.Errorf("svm.tpu-quic-address is set, and the svm-tpu binary %s is not there: put it beside svmd or set svm.tpu-quic-binary", c.Binary)
	}
	bindHost, _, err := net.SplitHostPort(c.Bind)
	if err != nil {
		return fmt.Errorf("svm.tpu-quic-address %q is not HOST:PORT: %w", c.Bind, err)
	}
	if ip := net.ParseIP(bindHost); c.Public == "" && (bindHost == "" || ip != nil && ip.IsUnspecified()) {
		return fmt.Errorf("svm.tpu-quic-address %s names no host a client can reach: set svm.public-tpu-address", c.Bind)
	}
	host, _, err := net.SplitHostPort(c.Advertised())
	if err != nil {
		return fmt.Errorf("svm.public-tpu-address %q is not HOST:PORT: %w", c.Public, err)
	}
	if ip := net.ParseIP(host); ip != nil && ip.To4() == nil {
		return fmt.Errorf("the advertised QUIC address %s is IPv6, and the Solana 4.3.0 client binds IPv4 only", c.Advertised())
	}
	return nil
}

// Args is the child's command line: the bind and the three bounds.
func (c QuicConfig) Args() []string {
	return []string{
		"--bind", c.Bind,
		"--max-connections-per-unstaked-peer", strconv.Itoa(c.PerPeer),
		"--max-connections-per-ipaddr-per-minute", strconv.Itoa(c.PerIPPerMinute),
		"--max-unstaked-connections", strconv.Itoa(c.MaxUnstakedConns),
	}
}

// frame is one frame on the child's standard output.
type frame struct {
	kind    byte
	payload []byte
}

// readFrame reads one frame: type u8, length u16 LE, payload. An unknown
// type, a transaction over 4,096 bytes and a truncated frame are all the
// child's fault.
func readFrame(r *bufio.Reader) (frame, error) {
	var head [3]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return frame{}, err
	}
	f := frame{kind: head[0]}
	n := int(binary.LittleEndian.Uint16(head[1:3]))
	switch {
	case f.kind > frameStats:
		return frame{}, fmt.Errorf("frame type %d is not a frame type", f.kind)
	case f.kind == frameTransaction && n > maxQuicTransaction:
		return frame{}, fmt.Errorf("a transaction frame of %d bytes is over 4,096", n)
	}
	f.payload = make([]byte, n)
	if _, err := io.ReadFull(r, f.payload); err != nil {
		return frame{}, fmt.Errorf("a truncated frame: %w", io.ErrUnexpectedEOF)
	}
	return f, nil
}

// child is a running svm-tpu, or a test's stand-in for one.
type child interface {
	Stdout() io.Reader
	// Stop sends SIGTERM, then SIGKILL after 5 s.
	Stop()
	Wait() error
}

type spawner func(QuicConfig) (child, error)

// quicClock is the time source, which a test drives.
type quicClock interface {
	Now() time.Time
	After(time.Duration) <-chan time.Time
}

type wallClock struct{}

func (wallClock) Now() time.Time                         { return time.Now() }
func (wallClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// quicState is the port's state on the Server.
type quicState struct {
	advertised atomic.Pointer[string]
	start      spawner
	clock      quicClock
	// ingest hands one transaction to the pool (broadcast with path quic)
	// and returns only after the pool answers.
	ingest func([]byte)
}

// quicAddress is the tpuQuic to advertise. It is nil while no child is ready.
func (s *Server) quicAddress() *string { return s.quic.advertised.Load() }

// Restart backoff: 1 s, doubling to 30 s, back to 1 s after a run of 60 s.
const (
	quicFirstBackoff = time.Second
	quicMaxBackoff   = 30 * time.Second
	quicHealthyRun   = 60 * time.Second
	quicStopGrace    = 5 * time.Second
)

// ServeQUIC runs svm-tpu until ctx ends, restarting it when it exits.
func (s *Server) ServeQUIC(ctx context.Context, cfg QuicConfig) error {
	q := &s.quic
	if q.start == nil {
		q.start = startTPU
	}
	if q.clock == nil {
		q.clock = wallClock{}
	}
	if q.ingest == nil {
		q.ingest = func(raw []byte) { _, _ = s.broadcast(raw, "quic") }
	}
	backoff := quicFirstBackoff
	for {
		began := q.clock.Now()
		err := s.runTPU(ctx, cfg)
		q.advertised.Store(nil)
		if ctx.Err() != nil {
			return nil
		}
		if q.clock.Now().Sub(began) >= quicHealthyRun {
			backoff = quicFirstBackoff
		}
		s.logger.Error("svm-tpu exited; restarting", "after", backoff, "err", err)
		select {
		case <-ctx.Done():
			return nil
		case <-q.clock.After(backoff):
		}
		backoff = min(2*backoff, quicMaxBackoff)
	}
}

// runTPU runs one child to its end. It reads one frame at a time, and reads
// the next only after broadcast returns, because every CheckTx runs under one
// lock.
func (s *Server) runTPU(ctx context.Context, cfg QuicConfig) error {
	c, err := s.quic.start(cfg)
	if err != nil {
		return err
	}
	stopped := make(chan struct{})
	defer close(stopped)
	go func() {
		select {
		case <-ctx.Done():
			c.Stop()
		case <-stopped:
		}
	}()
	r := bufio.NewReaderSize(c.Stdout(), maxQuicFrame)
	for {
		f, err := readFrame(r)
		if err != nil {
			c.Stop()
			waitErr := c.Wait()
			if errors.Is(err, io.EOF) {
				return waitErr
			}
			return fmt.Errorf("malformed frame from svm-tpu: %w", err)
		}
		switch f.kind {
		case frameReady:
			advertised := cfg.Advertised()
			s.quic.advertised.Store(&advertised)
			s.logger.Info("svm-tpu ready", "bound", string(f.payload), "advertised", advertised)
		case frameTransaction:
			s.quic.ingest(f.payload)
		case frameStats:
			observeQuic(f.payload)
		}
	}
}

// startTPU starts the real binary. Its standard error goes to svmd's log.
// SOLANA_METRICS_CONFIG is removed, so nothing reports off the machine. The
// child dies with svmd.
func startTPU(cfg QuicConfig) (child, error) {
	cmd := exec.Command(cfg.Binary, cfg.Args()...)
	cmd.Stderr = os.Stderr
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "SOLANA_METRICS_CONFIG=") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &process{cmd: cmd, out: out, done: make(chan struct{})}, nil
}

type process struct {
	cmd  *exec.Cmd
	out  io.Reader
	done chan struct{}
	err  error
	once atomic.Bool
}

func (p *process) Stdout() io.Reader { return p.out }

func (p *process) Stop() {
	_ = p.cmd.Process.Signal(syscall.SIGTERM)
	go func() {
		select {
		case <-p.done:
		case <-time.After(quicStopGrace):
			_ = p.cmd.Process.Kill()
		}
	}()
}

func (p *process) Wait() error {
	if p.once.CompareAndSwap(false, true) {
		p.err = p.cmd.Wait()
		close(p.done)
	}
	<-p.done
	return p.err
}

// The streamer's counters. Its report swaps most of them to zero each time,
// so they arrive as increments. It loads the rest, which are levels. Only
// these names become series, so a child cannot grow the label sets.
var (
	quicEvents = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "svm_quic_total",
		Help: "The QUIC port's streamer counters, by event: refusals, oversized streams, full-queue drops.",
	}, []string{"event"})
	quicLevels = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "svm_quic_current",
		Help: "The QUIC port's streamer levels, by name: open connections, active streams.",
	}, []string{"name"})
	quicLevelNames = map[string]bool{
		"active_connections": true, "active_streams": true, "open_connections": true,
		"outstanding_incoming_connection_attempts": true, "quic_endpoints_count": true,
		"stream_load_ema": true, "stream_load_ema_overflow": true, "stream_load_capacity_overflow": true,
	}
	quicEventNames = map[string]bool{
		"new_connections": true, "new_streams": true, "evictions_unstaked": true,
		"connection_added_from_unstaked_peer": true, "connection_add_failed": true,
		"connection_add_failed_unstaked_node": true, "connection_add_failed_on_pruning": true,
		"connection_removed": true, "connection_setup_timeout": true, "connection_setup_error": true,
		"connection_rate_limited_across_all": true, "connection_rate_limited_per_ipaddr": true,
		"invalid_stream_size": true, "packets_sent_to_consumer": true, "bytes_sent_to_consumer": true,
		"total_handle_chunk_to_packet_send_full_err": true, "stream_read_errors": true,
		"stream_read_timeouts": true, "throttled_streams": true, "throttled_unstaked_streams": true,
		"refused_connections_too_many_open_connections": true,
	}
)

// observeQuic records one stats frame.
func observeQuic(payload []byte) {
	var fields map[string]int64
	if json.Unmarshal(payload, &fields, agaveInput) != nil {
		return
	}
	for name, v := range fields {
		switch {
		case quicLevelNames[name]:
			quicLevels.WithLabelValues(name).Set(float64(v))
		case quicEventNames[name] && v > 0:
			quicEvents.WithLabelValues(name).Add(float64(v))
		}
	}
}
