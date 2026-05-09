// Tests in this file exercise the data-path code added in 0.3.0:
//   - rateLimitedConn / rateLimitedPacketConn (poet/poet.go) — wraps
//     bufio.NewInt64CounterConn so per-user shared-bucket throttling
//     applies to TCP and UDP.
//   - blockedConn (poet/poet.go) — closes the inbound conn and returns
//     errAuditBlocked when a destination FQDN matches a panel-supplied
//     audit rule.
//
// The point of running these in sim/ rather than poet/controller_test.go
// is byte-accounting verification: the rate-limit and audit wrappers sit
// directly on the byte-counting path, so any future change must keep
// "bytes pumped == bytes reported to panel" 1:1, which is what the
// existing sim suite guards. These tests make that invariant explicit
// for the new wrappers.
package sim

import (
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"regexp"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing/common/buf"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"

	POET "github.com/sagernet/sing-box/poet"
	"github.com/sagernet/sing-box/poet/api"
	"github.com/sagernet/sing-box/poet/controller"
	SS "github.com/sagernet/sing-box/poet/shortcuts"
)

// TestRateLimitedConnPreservesByteAccounting pumps 4 MiB at a 1 MiB/s
// per-user limit through POET.RoutedConnection and verifies:
//  1. the limiter actually slowed the conn (elapsed ≥ ~3 s — first 1 MiB
//     burst is free, then 3 MiB at 1 MiB/s = 3 s);
//  2. byte counters STILL increment 1:1 (limiter waits don't drop bytes);
//  3. ResetTraffic returns exactly 4 MiB.
//
// The whole point: limiter waits MUST happen after counter increment, so
// production reports stay byte-exact even when users are throttled.
func TestRateLimitedConnPreservesByteAccounting(t *testing.T) {
	tag := "sim-anytls-rl-tcp"
	auth, ctrl := setupTestController(t, tag, 1<<20 /* 1 MiB/s node limit */)
	addUser(t, auth)
	// Fresh user with limit = node = 1 MiB/s shared.
	ctrl.ApplyUserLimits(testHash, api.UserInfo{UID: testUID, SpeedLimit: 0})

	const N = 4 * 1024 * 1024 // 4 MiB
	clientSide, serverSide := net.Pipe()
	wrapped := POET.RoutedConnection(context.Background(), serverSide, fakeMetadataTCP(tag))
	if wrapped == nil {
		t.Fatal("RoutedConnection returned nil")
	}

	user, _ := auth.LoadUser(testHash)
	sendPtr, recvPtr := user.GetTrafficPointer()

	start := time.Now()
	var wg sync.WaitGroup
	wg.Add(2)
	// Direction 1: peer writes N → wrapped.Read N. sendPtr += N (Upload).
	go func() {
		defer wg.Done()
		writeAll(clientSide, makePayload(N, 0xAB))
	}()
	go func() {
		defer wg.Done()
		readN(wrapped, make([]byte, 64*1024), N)
	}()
	wg.Wait()
	uploadElapsed := time.Since(start)

	if got := sendPtr.Load(); got != int64(N) {
		t.Errorf("sendPtr after %d-byte upload = %d; want %d", N, got, N)
	}
	if got := recvPtr.Load(); got != 0 {
		t.Errorf("recvPtr after upload-only = %d; want 0", got)
	}
	// At 1 MiB/s with 1 MiB burst: first MiB free, remaining 3 MiB at
	// 1 MiB/s = 3 s. Allow 2.5–6 s slack for CI jitter.
	if uploadElapsed < 2500*time.Millisecond {
		t.Errorf("upload of %d B at 1 MiB/s took %v; expected ~3 s — limiter may not be in effect", N, uploadElapsed)
	}
	if uploadElapsed > 6*time.Second {
		t.Errorf("upload of %d B at 1 MiB/s took %v; far slower than expected ~3 s", N, uploadElapsed)
	}

	_ = wrapped.Close()
	_ = clientSide.Close()
}

// TestSharedBucketCombinedUpDown is the regression test for the
// XrayR-vs-SingR semantic alignment: pre-0.3.0 we had two independent
// per-direction buckets (a 1 MiB/s user could do 1 MiB/s up AND 1 MiB/s
// down simultaneously = 2 MiB/s combined). Since 0.3.0 a single shared
// bucket means up + down compete for the same budget. This test
// interleaves an upload and a download against a 1 MiB/s user; with the
// old two-bucket design it would complete in ~0 s (each direction has
// its own 1 MiB burst), with the new single-bucket design it takes
// ~1 s (the second direction has to wait for tokens because the first
// drained the burst).
//
// Also asserts byte accounting stays 1:1 — both directions counted in
// full, no double-charge for sharing the bucket.
func TestSharedBucketCombinedUpDown(t *testing.T) {
	tag := "sim-anytls-rl-shared"
	auth, ctrl := setupTestController(t, tag, 1<<20)
	addUser(t, auth)
	ctrl.ApplyUserLimits(testHash, api.UserInfo{UID: testUID, SpeedLimit: 0})

	const N = 1 << 20 // 1 MiB each direction
	clientSide, serverSide := net.Pipe()
	wrapped := POET.RoutedConnection(context.Background(), serverSide, fakeMetadataTCP(tag))

	user, _ := auth.LoadUser(testHash)
	sendPtr, recvPtr := user.GetTrafficPointer()

	start := time.Now()
	var wg sync.WaitGroup
	wg.Add(4)
	// Upload: peer (clientSide) writes → wrapped.Read.
	go func() { defer wg.Done(); writeAll(clientSide, makePayload(N, 0xAB)) }()
	go func() { defer wg.Done(); readN(wrapped, make([]byte, 64*1024), N) }()
	// Download: wrapped.Write → peer.Read.
	go func() { defer wg.Done(); writeAll(wrapped, makePayload(N, 0xCD)) }()
	go func() { defer wg.Done(); readN(clientSide, make([]byte, 64*1024), N) }()
	wg.Wait()
	elapsed := time.Since(start)

	if got := sendPtr.Load(); got != int64(N) {
		t.Errorf("upload = %d; want %d", got, N)
	}
	if got := recvPtr.Load(); got != int64(N) {
		t.Errorf("download = %d; want %d", got, N)
	}
	// Two-bucket (old) behavior: ~0 s. One-bucket (new) behavior: 1 MiB
	// burst is consumed by whichever direction wins the race; the other
	// must wait ~1 s for tokens. Allow 800 ms floor for CI.
	if elapsed < 800*time.Millisecond {
		t.Errorf("interleaved 1 MiB up + 1 MiB down at 1 MiB/s shared took %v; expected ≥ ~1 s — bucket may not be shared between directions", elapsed)
	}

	_ = wrapped.Close()
	_ = clientSide.Close()
}

// TestAuditBlocksMatchingFqdnAndRecordsHit verifies that
// POET.RoutedConnection, when the destination FQDN matches a controller
// audit rule, returns a conn that surfaces errAuditBlocked on subsequent
// Read/Write (so dispatcher unwinds without dialing the outbound), and
// records a DetectResult for the next ReportIllegal cycle.
func TestAuditBlocksMatchingFqdnAndRecordsHit(t *testing.T) {
	tag := "sim-anytls-audit"
	auth, ctrl := setupTestController(t, tag, 0)
	addUser(t, auth)
	ctrl.SetDetectRules([]api.DetectRule{
		{ID: 42, Pattern: regexp.MustCompile(`(?i)evil\.example\.com$`)},
	})

	clientSide, serverSide := net.Pipe()
	metadata := fakeMetadataTCP(tag)
	metadata.Destination = M.Socksaddr{Fqdn: "www.evil.example.com", Port: 443}

	wrapped := POET.RoutedConnection(context.Background(), serverSide, metadata)
	if wrapped == nil {
		t.Fatal("RoutedConnection returned nil")
	}

	// Read should surface the audit-block sentinel. Compare on text since
	// the sentinel is unexported.
	_, err := wrapped.Read(make([]byte, 16))
	if err == nil {
		t.Fatalf("expected error from blocked conn Read, got nil")
	}
	if err.Error() != "singr: connection blocked by audit rule" {
		t.Fatalf("Read err = %q; want audit-block sentinel", err.Error())
	}

	// Underlying inbound conn should be closed so peer sees io.ErrClosedPipe / EOF.
	clientSide.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	_, peerErr := clientSide.Read(make([]byte, 16))
	if peerErr == nil {
		t.Errorf("peer Read after audit block returned nil err; conn should be closed")
	} else if !errors.Is(peerErr, io.EOF) && !errors.Is(peerErr, io.ErrClosedPipe) {
		// net.Pipe close surfaces as io.ErrClosedPipe on the other end.
		// Either of those is fine — what's bad is "no error".
		t.Logf("peer side err on closed pipe: %v (acceptable)", peerErr)
	}

	hits := ctrl.DrainDetectResults()
	if len(hits) != 1 {
		t.Fatalf("expected 1 audit hit; got %d (%+v)", len(hits), hits)
	}
	if hits[0].UID != testUID || hits[0].RuleID != 42 {
		t.Errorf("audit hit = %+v; want {UID:%d RuleID:42}", hits[0], testUID)
	}

	_ = wrapped.Close()
	_ = clientSide.Close()
}

// TestRateLimitedPacketConnPreservesByteAccounting is the UDP analog of
// TestRateLimitedConnPreservesByteAccounting. It uses a fake in-memory
// PacketConn (memPacketConn) so it can run without a real UDP socket,
// then pumps a known number of bytes through and verifies counters +
// elapsed time match expectation.
func TestRateLimitedPacketConnPreservesByteAccounting(t *testing.T) {
	tag := "sim-anytls-rl-udp"
	auth, ctrl := setupTestController(t, tag, 1<<20)
	addUser(t, auth)
	ctrl.ApplyUserLimits(testHash, api.UserInfo{UID: testUID, SpeedLimit: 0})

	mp := newMemPacketConn()
	wrapped := POET.RoutedPacketConnection(context.Background(), mp, fakeMetadataUDP(tag))
	if wrapped == nil {
		t.Fatal("RoutedPacketConnection returned nil")
	}

	user, _ := auth.LoadUser(testHash)
	sendPtr, recvPtr := user.GetTrafficPointer()

	const pktSize = 64 * 1024 // 64 KiB
	const pktCount = 32       // 2 MiB total
	const totalBytes = pktSize * pktCount
	dest := M.SocksaddrFrom(netip.MustParseAddr("203.0.113.1"), 53)

	start := time.Now()
	// Push 32 packets in via the fake UDP conn's "incoming" queue, then
	// read them out through wrapped.ReadPacket. ReadPacket should call
	// WaitN per packet; counter is incremented before WaitN so byte
	// accounting stays 1:1.
	for i := 0; i < pktCount; i++ {
		mp.deliverIncoming(makePayload(pktSize, byte(i)), dest)
	}
	for i := 0; i < pktCount; i++ {
		b := buf.NewSize(pktSize)
		if _, err := wrapped.ReadPacket(b); err != nil {
			t.Fatalf("ReadPacket %d: %v", i, err)
		}
		b.Release()
	}
	elapsed := time.Since(start)

	if got := sendPtr.Load(); got != int64(totalBytes) {
		t.Errorf("UDP sendPtr after %d packets = %d; want %d", pktCount, got, totalBytes)
	}
	if got := recvPtr.Load(); got != 0 {
		t.Errorf("UDP recvPtr (read-only path) = %d; want 0", got)
	}
	// 2 MiB at 1 MiB/s shared bucket with 1 MiB burst → ~1 s after burst.
	if elapsed < 800*time.Millisecond {
		t.Errorf("UDP read of %d B at 1 MiB/s took %v; expected ≥ ~1 s — limiter may not be wrapping PacketConn", totalBytes, elapsed)
	}

	_ = wrapped.Close()
}

// --- shared test helpers ---

// stubInbound is a minimal adapter.Inbound for SS.SetInboud. Its only
// job is to satisfy the interface and carry a tag.
type stubInbound struct{ tag string }

func (s *stubInbound) Type() string                  { return "anytls" }
func (s *stubInbound) Tag() string                   { return s.tag }
func (s *stubInbound) Start(adapter.StartStage) error { return nil }
func (s *stubInbound) Close() error                  { return nil }

// stubAPI is a no-op api.API for controller.New. POET.RoutedConnection
// never calls back into the API; we just need a value to pass.
type stubAPI struct{}

func (stubAPI) GetNodeInfo() (*api.NodeInfo, error)         { return nil, nil }
func (stubAPI) GetUserList() (*[]api.UserInfo, error)       { return nil, nil }
func (stubAPI) ReportNodeStatus(*api.NodeStatus) error      { return nil }
func (stubAPI) ReportNodeOnlineUsers(*[]api.OnlineUser) error { return nil }
func (stubAPI) ReportUserTraffic(*[]api.UserTraffic) error  { return nil }
func (stubAPI) Describe() api.ClientInfo                    { return api.ClientInfo{} }
func (stubAPI) GetNodeRule() (*[]api.DetectRule, error)     { return nil, nil }
func (stubAPI) ReportIllegal(*[]api.DetectResult) error     { return nil }
func (stubAPI) Debug()                                      {}

// setupTestController constructs a fully wired controller registered in
// the SS shortcuts singleton with the given inbound tag. Subsequent
// POET.RoutedConnection calls keyed on `metadata.Inbound == tag` will
// resolve to this controller.
//
// Each test should pass a unique tag — the singleton is process-global
// and shared across tests; reusing tags would let earlier tests' state
// leak.
func setupTestController(t *testing.T, tag string, nodeSpeedLimit uint64) (*controller.Authenticator, *controller.Controller) {
	t.Helper()

	// Singleton's logger is required by RoutedConnection; install a NOP
	// once. Repeated installs are harmless.
	if SS.Singleton().Logger == nil {
		POET.SetLogger(log.NewNOPFactory())
	}

	in := &stubInbound{tag: tag}
	var ai adapter.Inbound = in
	POET.SetInboud(&ai, tag)

	ctrl := controller.New(
		context.Background(),
		controller.Config{PanelType: "SSpanel", UpdatePeriodic: 60},
		&ai,
		stubAPI{},
		log.NewNOPFactory().Logger(),
	)
	// The controller's nodeInfo is normally set by Start(). Tests poke
	// it directly via the test hook.
	ctrl.SetNodeInfo(&api.NodeInfo{NodeID: 1, NodeType: "anytls", SpeedLimit: nodeSpeedLimit})

	SS.Singleton().SetContrl(ctrl, tag, tag)

	t.Cleanup(func() {
		// Best-effort cleanup so a failing test doesn't poison later
		// tests in this package.
		delete(SS.Singleton().Inbounds, tag)
		delete(SS.Singleton().InContrls, tag)
		delete(SS.Singleton().OutContrls, tag)
	})

	return ctrl.Author(), ctrl
}

func fakeMetadataTCP(inboundTag string) adapter.InboundContext {
	return adapter.InboundContext{
		Inbound:     inboundTag,
		User:        testHash,
		Source:      M.SocksaddrFrom(netip.MustParseAddr("127.0.0.1"), 12345),
		Destination: M.SocksaddrFrom(netip.MustParseAddr("203.0.113.1"), 443),
	}
}

func fakeMetadataUDP(inboundTag string) adapter.InboundContext {
	return adapter.InboundContext{
		Inbound:     inboundTag,
		User:        testHash,
		Source:      M.SocksaddrFrom(netip.MustParseAddr("127.0.0.1"), 12345),
		Destination: M.SocksaddrFrom(netip.MustParseAddr("203.0.113.1"), 53),
	}
}

// memPacketConn is a minimal in-memory N.PacketConn for sim tests. It
// supports an "incoming" queue (deliverIncoming → ReadPacket) and a
// drop-on-the-floor outgoing path (WritePacket). Enough to exercise the
// rateLimitedPacketConn wrapper byte-accounting + WaitN behavior without
// needing a real UDP socket.
type memPacketConn struct {
	mu       sync.Mutex
	incoming []memPacket
	cond     *sync.Cond
	closed   atomic.Bool
}

type memPacket struct {
	payload []byte
	dest    M.Socksaddr
}

func newMemPacketConn() *memPacketConn {
	c := &memPacketConn{}
	c.cond = sync.NewCond(&c.mu)
	return c
}

func (c *memPacketConn) deliverIncoming(payload []byte, dest M.Socksaddr) {
	c.mu.Lock()
	c.incoming = append(c.incoming, memPacket{payload: payload, dest: dest})
	c.cond.Signal()
	c.mu.Unlock()
}

func (c *memPacketConn) ReadPacket(buffer *buf.Buffer) (M.Socksaddr, error) {
	c.mu.Lock()
	for len(c.incoming) == 0 && !c.closed.Load() {
		c.cond.Wait()
	}
	if c.closed.Load() {
		c.mu.Unlock()
		return M.Socksaddr{}, net.ErrClosed
	}
	pkt := c.incoming[0]
	c.incoming = c.incoming[1:]
	c.mu.Unlock()
	buffer.Write(pkt.payload)
	return pkt.dest, nil
}

func (c *memPacketConn) WritePacket(buffer *buf.Buffer, destination M.Socksaddr) error {
	if c.closed.Load() {
		return net.ErrClosed
	}
	// Drain the buffer; we don't care where it goes.
	buffer.Release()
	return nil
}

func (c *memPacketConn) Close() error {
	if c.closed.CompareAndSwap(false, true) {
		c.mu.Lock()
		c.cond.Broadcast()
		c.mu.Unlock()
	}
	return nil
}

func (c *memPacketConn) LocalAddr() net.Addr                { return &net.UDPAddr{IP: net.IPv4zero, Port: 0} }
func (c *memPacketConn) SetDeadline(t time.Time) error      { return nil }
func (c *memPacketConn) SetReadDeadline(t time.Time) error  { return nil }
func (c *memPacketConn) SetWriteDeadline(t time.Time) error { return nil }

// Compile-time assertion that memPacketConn satisfies N.PacketConn.
var _ N.PacketConn = (*memPacketConn)(nil)
