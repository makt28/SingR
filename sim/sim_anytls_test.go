// AnyTLS-layer simulation. Brings up a real anytls.Service + anytls.Client
// in-process (over loopback TCP, no TLS — Service expects plaintext after
// the inbound's TLS layer would have stripped it, so this exercises every
// AnyTLS protocol surface that could plausibly inflate counted bytes:
// session multiplex framing, padding scheme, control frames, keepalives,
// substream open/close).
//
// What we assert: total bytes pushed by the client through CreateProxy
// equals the byte count accumulated on the user's atomic counters wrapped
// at the Service-side substream Stream API — exactly the layer
// poet.RoutedConnection wraps in production. If this passes, AnyTLS layer
// adds zero invisible bytes to the report.
//
// Run with:
//   GOCACHE=$(pwd)/.cache/go-build go test ./sim -v -run AnyTLS
package sim

import (
	"context"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	anytls "github.com/anytls/sing-anytls"
	"github.com/anytls/sing-anytls/padding"
	"github.com/sagernet/sing/common/bufio"
	"github.com/sagernet/sing/common/logger"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"

	"github.com/sagernet/sing-box/poet/api"
	"github.com/sagernet/sing-box/poet/api/sspanel"
)

const anytlsPassword = "anytls-sim-password"

// echoCountingHandler is the Service-side handler. For each substream it
// receives, it wraps the stream with bufio.NewInt64CounterConn pointing
// at the SingR Authenticator user's send/recv atomics — the SAME wrap
// poet.RoutedConnection performs in production — then echoes bytes back.
// After the test completes, the user's atomics tell us exactly what bytes
// the AnyTLS layer surfaced as "application payload".
// echoCountingHandler is the Service-side handler. AnyTLS Stream does not
// propagate CloseWrite/EOF reliably, so we read a fixed length n agreed
// with the test client out-of-band, echo it back, and close. n is set per
// test via SetN before each stream.
type echoCountingHandler struct {
	sendPtr *atomic.Int64
	recvPtr *atomic.Int64
	n       int
}

func (h *echoCountingHandler) NewConnectionEx(ctx context.Context, conn net.Conn, source M.Socksaddr, destination M.Socksaddr, onClose N.CloseHandlerFunc) {
	wrapped := bufio.NewInt64CounterConn(conn, []*atomic.Int64{h.sendPtr}, []*atomic.Int64{h.recvPtr})
	defer wrapped.Close()
	if onClose != nil {
		defer onClose(nil)
	}
	buf := make([]byte, h.n)
	if _, err := io.ReadFull(wrapped, buf); err != nil {
		return
	}
	// Single >64 KiB Write: exercises sing-anytls writeDataFrame frame
	// splitting (uint16 overflow fixed in the fork's v0.0.12 sync).
	_, _ = wrapped.Write(buf)
}

func TestAnyTLSLayerNoByteInflation(t *testing.T) {
	// Bring up a TCP listener that the AnyTLS Service will accept on.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	t.Logf("anytls service listening on %s", ln.Addr())

	// Use a real SingR Authenticator + add user u1, exactly mirroring
	// the production user-add path.
	auth := newAuth(t)
	addUser(t, auth)
	user, ok := auth.LoadUser(testHash)
	if !ok {
		t.Fatal("LoadUser failed")
	}
	sendPtr, recvPtr := user.GetTrafficPointer()

	handler := &echoCountingHandler{sendPtr: sendPtr, recvPtr: recvPtr}

	service, err := anytls.NewService(anytls.ServiceConfig{
		PaddingScheme: padding.DefaultPaddingScheme,
		Users:         []anytls.User{{Name: testHash, Password: anytlsPassword}},
		Handler:       handler,
		Logger:        logger.NOP(),
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	// Accept goroutine: hand every accepted conn to Service.NewConnection.
	acceptDone := make(chan struct{})
	go func() {
		defer close(acceptDone)
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				_ = service.NewConnection(ctx, c, M.SocksaddrFromNet(c.RemoteAddr()), nil)
			}(conn)
		}
	}()

	// Build the AnyTLS Client; its DialOut is a plain TCP dial to the
	// listener — no TLS in this sim because the Service operates on
	// plaintext (TLS would be the inbound's job, irrelevant to byte
	// accounting).
	clientCtx, clientCancel := context.WithCancel(context.Background())
	defer clientCancel()
	listenAddr := ln.Addr().String()
	client, err := anytls.NewClient(clientCtx, anytls.ClientConfig{
		Password: anytlsPassword,
		DialOut: func(ctx context.Context) (net.Conn, error) {
			d := net.Dialer{Timeout: 3 * time.Second}
			return d.DialContext(ctx, "tcp", listenAddr)
		},
		Logger: logger.NOP(),
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer client.Close()

	// Open multiple substreams sequentially; each pushes a known number
	// of bytes and waits for echo. Total payload across all conns
	// should match the user's atomic counters exactly.
	const perStream = 256 * 1024
	const streams = 3
	totalEachWay := int64(perStream) * int64(streams)
	handler.n = perStream

	dst := M.ParseSocksaddrHostPort("127.0.0.1", uint16(9999)) // ignored by echo handler

	for i := 0; i < streams; i++ {
		conn, err := client.CreateProxy(context.Background(), dst)
		if err != nil {
			t.Fatalf("stream %d CreateProxy: %v", i, err)
		}
		payload := makePayload(perStream, byte(0xA0+i))
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, _ = conn.Write(payload)
		}()
		got := make([]byte, perStream)
		go func() {
			defer wg.Done()
			readN(conn, got, perStream)
		}()
		wg.Wait()
		_ = conn.Close()
	}

	// Give the service side a brief moment to finish accounting on the
	// last stream's close path.
	time.Sleep(50 * time.Millisecond)

	gotSent := sendPtr.Load()
	gotRecv := recvPtr.Load()

	t.Logf("ground truth: per-stream=%d streams=%d total each way=%d", perStream, streams, totalEachWay)
	t.Logf("AnyTLS counter snap: sendPtr=%d recvPtr=%d", gotSent, gotRecv)

	if gotSent != totalEachWay {
		t.Errorf("sendPtr = %d; want %d (ratio %.4fx)", gotSent, totalEachWay, float64(gotSent)/float64(totalEachWay))
	}
	if gotRecv != totalEachWay {
		t.Errorf("recvPtr = %d; want %d (ratio %.4fx)", gotRecv, totalEachWay, float64(gotRecv)/float64(totalEachWay))
	}
}

// TestAnyTLSReportE2E pushes bytes through the AnyTLS layer and then
// drives one full SingR report cycle (ResetTraffic → ReportUserTraffic
// → fake SSPanel) and asserts the panel records the correct bytes.
func TestAnyTLSReportE2E(t *testing.T) {
	fp := NewFakePanel(testNodeID, 14555, testUID, testUUID, testPasswd)
	defer fp.Close()

	apiClient := sspanel.New(&api.Config{
		APIHost:             fp.URL,
		NodeID:              testNodeID,
		Key:                 "test-key",
		NodeType:            "V2ray",
		Timeout:             5,
		DisableCustomConfig: true,
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	auth := newAuth(t)
	addUser(t, auth)
	user, ok := auth.LoadUser(testHash)
	if !ok {
		t.Fatal("LoadUser failed")
	}
	sendPtr, recvPtr := user.GetTrafficPointer()

	handler := &echoCountingHandler{sendPtr: sendPtr, recvPtr: recvPtr}

	service, err := anytls.NewService(anytls.ServiceConfig{
		PaddingScheme: padding.DefaultPaddingScheme,
		Users:         []anytls.User{{Name: testHash, Password: anytlsPassword}},
		Handler:       handler,
		Logger:        logger.NOP(),
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				_ = service.NewConnection(ctx, c, M.SocksaddrFromNet(c.RemoteAddr()), nil)
			}(conn)
		}
	}()

	clientCtx, clientCancel := context.WithCancel(context.Background())
	defer clientCancel()
	listenAddr := ln.Addr().String()
	client, err := anytls.NewClient(clientCtx, anytls.ClientConfig{
		Password: anytlsPassword,
		DialOut: func(ctx context.Context) (net.Conn, error) {
			d := net.Dialer{Timeout: 3 * time.Second}
			return d.DialContext(ctx, "tcp", listenAddr)
		},
		Logger: logger.NOP(),
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer client.Close()

	const perStream = 100 * 1024
	const streams = 5
	totalEachWay := int64(perStream) * int64(streams)
	handler.n = perStream

	dst := M.ParseSocksaddrHostPort("127.0.0.1", uint16(9999))
	for i := 0; i < streams; i++ {
		conn, err := client.CreateProxy(context.Background(), dst)
		if err != nil {
			t.Fatalf("stream %d: %v", i, err)
		}
		payload := makePayload(perStream, 0xC0)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, _ = conn.Write(payload)
		}()
		got := make([]byte, perStream)
		go func() {
			defer wg.Done()
			readN(conn, got, perStream)
		}()
		wg.Wait()
		_ = conn.Close()
	}
	time.Sleep(50 * time.Millisecond)

	// Drive one report cycle (the same loop userInfoMonitor runs).
	var userTraffic []api.UserTraffic
	for _, u := range auth.ListUsers() {
		sent, recv := u.ResetTraffic()
		t.Logf("ResetTraffic UID=%d sent=%d recv=%d", u.UID, sent, recv)
		if sent == 0 && recv == 0 {
			continue
		}
		userTraffic = append(userTraffic, api.UserTraffic{
			UID:      u.UID,
			Email:    u.Email,
			Upload:   sent,
			Download: recv,
		})
	}
	if err := apiClient.ReportUserTraffic(&userTraffic); err != nil {
		t.Fatalf("ReportUserTraffic: %v", err)
	}

	upload, download := fp.SumReported(testUID)
	t.Logf("ground truth (each direction): %d", totalEachWay)
	t.Logf("panel received: upload=%d download=%d", upload, download)

	if upload != totalEachWay {
		t.Errorf("panel upload = %d; want %d (ratio %.4fx)", upload, totalEachWay, float64(upload)/float64(totalEachWay))
	}
	if download != totalEachWay {
		t.Errorf("panel download = %d; want %d (ratio %.4fx)", download, totalEachWay, float64(download)/float64(totalEachWay))
	}
}
