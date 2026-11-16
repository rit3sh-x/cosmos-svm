package rpc

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"cosmossdk.io/log/v2"
)

func encodeFrame(kind byte, payload []byte) []byte {
	return append(append([]byte{kind}, binary.LittleEndian.AppendUint16(nil, uint16(len(payload)))...), payload...)
}

// TestTheFrameReader proves readFrame reads each frame type and refuses an
// unknown type, a transaction over 4,096 bytes and a truncated frame.
func TestTheFrameReader(t *testing.T) {
	stream := slices.Concat(encodeFrame(frameReady, []byte("127.0.0.1:9000")),
		encodeFrame(frameTransaction, bytes.Repeat([]byte{7}, maxQuicTransaction)),
		encodeFrame(frameStats, []byte(`{"new_connections":2}`)))
	r := bufio.NewReader(bytes.NewReader(stream))
	for _, want := range []frame{{frameReady, []byte("127.0.0.1:9000")}, {frameTransaction, bytes.Repeat([]byte{7}, 4096)},
		{frameStats, []byte(`{"new_connections":2}`)}} {
		got, err := readFrame(r)
		if err != nil || got.kind != want.kind || !bytes.Equal(got.payload, want.payload) {
			t.Fatalf("read %d %d bytes, %v; want %d %d bytes", got.kind, len(got.payload), err, want.kind, len(want.payload))
		}
	}
	if _, err := readFrame(r); !errors.Is(err, io.EOF) {
		t.Fatalf("the end of the stream is %v, want EOF", err)
	}
	for name, raw := range map[string][]byte{
		"unknown type": encodeFrame(3, nil),
		"oversized":    encodeFrame(frameTransaction, make([]byte, maxQuicTransaction+1)),
		"truncated":    encodeFrame(frameTransaction, make([]byte, 10))[:8],
	} {
		if _, err := readFrame(bufio.NewReader(bytes.NewReader(raw))); err == nil || errors.Is(err, io.EOF) {
			t.Errorf("a frame that is %s read as %v", name, err)
		}
	}
}

// stubChild is a child whose standard output the test writes.
type stubChild struct {
	out   *io.PipeReader
	in    *io.PipeWriter
	once  sync.Once
	ended chan struct{}
}

func newStubChild() *stubChild {
	r, w := io.Pipe()
	return &stubChild{out: r, in: w, ended: make(chan struct{})}
}

func (c *stubChild) Stdout() io.Reader { return c.out }
func (c *stubChild) Stop()             { c.exit() }
func (c *stubChild) Wait() error       { <-c.ended; return nil }
func (c *stubChild) exit() {
	c.once.Do(func() { _ = c.in.Close(); close(c.ended) })
}

// stubClock is a clock the test moves, and whose waits it releases.
type stubClock struct {
	mu    sync.Mutex
	now   time.Time
	waits chan time.Duration
	fire  chan time.Time
}

func (c *stubClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *stubClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func (c *stubClock) After(d time.Duration) <-chan time.Time {
	c.waits <- d
	return c.fire
}

// TestTheSupervisorAdvertisesOnlyWhileReadyAndBacksOff proves a ready frame
// advertises the address. An exit withdraws it and restarts after 1 s,
// doubling to 30 s. A run of 60 s resets the backoff.
func TestTheSupervisorAdvertisesOnlyWhileReadyAndBacksOff(t *testing.T) {
	s := &Server{logger: log.NewNopLogger()}
	children := make(chan *stubChild)
	clock := &stubClock{waits: make(chan time.Duration), fire: make(chan time.Time)}
	s.quic.clock = clock
	s.quic.ingest = func([]byte) {}
	s.quic.start = func(QuicConfig) (child, error) {
		c := newStubChild()
		children <- c
		return c, nil
	}
	cfg := QuicConfig{Bind: "0.0.0.0:9000", Public: "203.0.113.7:9000"}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- s.ServeQUIC(ctx, cfg) }()

	c := <-children
	if s.quicAddress() != nil {
		t.Fatal("an address is advertised before the child is ready")
	}
	if _, err := c.in.Write(encodeFrame(frameReady, []byte("0.0.0.0:9000"))); err != nil {
		t.Fatal(err)
	}
	// The ready frame is handled before the next frame is read, so a
	// stats frame written after it proves the address is stored.
	if _, err := c.in.Write(encodeFrame(frameStats, []byte(`{}`))); err != nil {
		t.Fatal(err)
	}
	if got := s.quicAddress(); got == nil || *got != "203.0.113.7:9000" {
		t.Fatalf("the ready child advertises %v, want the public address", got)
	}
	for _, want := range []time.Duration{1, 2, 4, 8, 16, 30, 30} {
		c.exit()
		if got := <-clock.waits; got != want*time.Second {
			t.Fatalf("the restart waits %v, want %v", got, want*time.Second)
		}
		if s.quicAddress() != nil {
			t.Fatal("an exited child is still advertised")
		}
		clock.fire <- time.Time{}
		c = <-children
	}
	clock.advance(quicHealthyRun)
	c.exit()
	if got := <-clock.waits; got != time.Second {
		t.Fatalf("after a run of 60 s the restart waits %v, want 1 s", got)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// TestQuicFramesAreHandledOneAtATime proves the next frame is not read while
// broadcast runs, so the child's writer waits.
func TestQuicFramesAreHandledOneAtATime(t *testing.T) {
	s := &Server{logger: log.NewNopLogger()}
	c := newStubChild()
	s.quic.start = func(QuicConfig) (child, error) { return c, nil }
	s.quic.clock = &stubClock{waits: make(chan time.Duration, 1), fire: make(chan time.Time)}
	entered, release := make(chan []byte), make(chan struct{})
	s.quic.ingest = func(raw []byte) { entered <- raw; <-release }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = s.ServeQUIC(ctx, QuicConfig{Bind: "127.0.0.1:9000"}) }()

	go func() { _, _ = c.in.Write(encodeFrame(frameTransaction, []byte("first"))) }()
	if got := <-entered; string(got) != "first" {
		t.Fatalf("the first transaction is %q", got)
	}
	second := make(chan struct{})
	go func() {
		_, _ = c.in.Write(encodeFrame(frameTransaction, []byte("second")))
		close(second)
	}()
	// A pipe write completes only when the reader takes it, so the second
	// write cannot complete while the first broadcast holds.
	select {
	case <-second:
		t.Fatal("the next frame was read while broadcast ran")
	default:
	}
	release <- struct{}{}
	if got := <-entered; string(got) != "second" {
		t.Fatalf("the second transaction is %q", got)
	}
	<-second
	release <- struct{}{}
}

// TestTheQuicStartRefusals proves Check refuses, by name, no binary, an
// unspecified bind with no public address, and an IPv6 advertised address.
func TestTheQuicStartRefusals(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "svm-tpu")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		cfg  QuicConfig
		want string
	}{
		{QuicConfig{Bind: "127.0.0.1:9000"}, "no svm-tpu binary"},
		{QuicConfig{Binary: bin + ".absent", Bind: "127.0.0.1:9000"}, "is not there"},
		{QuicConfig{Binary: bin, Bind: "0.0.0.0:9000"}, "set svm.public-tpu-address"},
		{QuicConfig{Binary: bin, Bind: ":9000"}, "set svm.public-tpu-address"},
		{QuicConfig{Binary: bin, Bind: "0.0.0.0:9000", Public: "[2001:db8::1]:9000"}, "IPv6"},
		{QuicConfig{Binary: bin, Bind: "[::1]:9000"}, "IPv6"},
		{QuicConfig{Binary: bin, Bind: "0.0.0.0:9000", Public: "203.0.113.7:9000"}, ""},
		{QuicConfig{Binary: bin, Bind: "127.0.0.1:9000"}, ""},
	} {
		err := tt.cfg.Check()
		if tt.want == "" && err != nil || tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)) {
			t.Errorf("%+v: %v, want %q", tt.cfg, err, tt.want)
		}
	}
}

// TestQuicSettingsReachTheChild proves the three bounds reach the child's
// command line.
func TestQuicSettingsReachTheChild(t *testing.T) {
	got := strings.Join(QuicConfig{Bind: "127.0.0.1:9000", PerPeer: 2, PerIPPerMinute: 100, MaxUnstakedConns: 7}.Args(), " ")
	want := "--bind 127.0.0.1:9000 --max-connections-per-unstaked-peer 2 --max-connections-per-ipaddr-per-minute 100 --max-unstaked-connections 7"
	if got != want {
		t.Fatalf("the command line is %q, want %q", got, want)
	}
}
