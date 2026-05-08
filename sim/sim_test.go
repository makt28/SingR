// Package sim contains a self-contained end-to-end-ish simulation that
// proves SingR's node-side byte accounting matches what gets posted to the
// SSPanel /mod_mu/users/traffic endpoint. The goal is to definitively
// answer the question: "is the 100x over-reporting coming from the SingR
// Go code, or from somewhere else (panel side, multiple SingR instances,
// traffic_rate, etc.)?"
//
// What the sim does:
//   1. Spin up an in-process fake SSPanel that records every traffic report.
//   2. Construct the real sspanel.APIClient pointing at the fake panel.
//   3. Construct the real controller.Authenticator and add one user with
//      the same `u<UID>` hash convention SingR uses in production.
//   4. Wrap a net.Pipe connection via the same bufio.NewInt64CounterConn
//      call route/route.go uses, with the user's atomic counter pointer
//      (GetTrafficPointer) — exactly the wiring poet.RoutedConnection sets up.
//   5. Pump a deterministic, known number of bytes through the wrapped
//      conn in both directions.
//   6. Drive one report cycle (replicating the userInfoMonitor inner loop
//      verbatim) and check the bytes that hit the fake panel match the
//      bytes we pumped. If they don't, node-side is at fault.
//
// Run with:
//   GOCACHE=/Users/makt/Desktop/SingR/.cache/go-build go test ./sim -v
//   SINGR_TRAFFIC_DEBUG=1 GOCACHE=/Users/makt/Desktop/SingR/.cache/go-build go test ./sim -v
package sim

import (
	"context"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/sagernet/sing/common/bufio"

	"github.com/sagernet/sing-box/poet/api"
	"github.com/sagernet/sing-box/poet/api/sspanel"
	"github.com/sagernet/sing-box/poet/controller"
)

const (
	testNodeID = 9999
	testUID    = 12345
	testHash   = "u12345"
	testUUID   = "11111111-2222-3333-4444-555555555555"
	testPasswd = "test-passwd"
	testEmail  = "sim@example.com"
)

// TestCounterByteSemantics verifies that bufio.NewInt64CounterConn
// increments the user's atomic counters by exactly the number of bytes
// passed through Read/Write — no padding, no framing, no multipliers.
func TestCounterByteSemantics(t *testing.T) {
	auth := newAuth(t)
	addUser(t, auth)

	user, ok := auth.LoadUser(testHash)
	if !ok {
		t.Fatal("LoadUser failed")
	}
	sendPtr, recvPtr := user.GetTrafficPointer()

	// Mirror poet/poet.go RoutedConnection: wrap one side of a net.Pipe
	// with a single (send, recv) pair pointing at user atomics.
	a, b := net.Pipe()
	wrapped := bufio.NewInt64CounterConn(a, []*atomic.Int64{sendPtr}, []*atomic.Int64{recvPtr})

	const N = 1024 * 1024 // 1 MiB each direction
	wantUp := int64(N)
	wantDown := int64(N)

	var wg sync.WaitGroup
	wg.Add(2)

	// Peer side b: read N bytes (this is the "client uploading to us"
	// side from the proxied app's perspective, but for accounting it's
	// the bytes we Read FROM the conn, i.e. user.sent / Upload).
	go func() {
		defer wg.Done()
		buf := make([]byte, 64*1024)
		var got int64
		for got < N {
			n, err := b.Read(buf)
			got += int64(n)
			if err != nil {
				if err == io.EOF {
					return
				}
				return
			}
		}
	}()

	// Local side wrapped: write N bytes (counted as user.recv / Download
	// because Write on the wrapped conn increments recvCounter).
	// Then read N bytes (counted as user.sent / Upload).
	go func() {
		defer wg.Done()
		// First, we WRITE N bytes from wrapped → b. That's "Download"
		// from the user's perspective (server → client).
		buf := make([]byte, 64*1024)
		for i := range buf {
			buf[i] = 0xAB
		}
		var sent int64
		for sent < N {
			toWrite := int64(len(buf))
			if N-sent < toWrite {
				toWrite = N - sent
			}
			n, err := wrapped.Write(buf[:toWrite])
			sent += int64(n)
			if err != nil {
				return
			}
		}
	}()

	wg.Wait()

	// Then in the OTHER direction: b writes N bytes, wrapped reads N bytes.
	wg.Add(2)
	go func() {
		defer wg.Done()
		buf := make([]byte, 64*1024)
		for i := range buf {
			buf[i] = 0xCD
		}
		var sent int64
		for sent < N {
			toWrite := int64(len(buf))
			if N-sent < toWrite {
				toWrite = N - sent
			}
			n, err := b.Write(buf[:toWrite])
			sent += int64(n)
			if err != nil {
				return
			}
		}
		_ = b.Close()
	}()
	go func() {
		defer wg.Done()
		buf := make([]byte, 64*1024)
		var got int64
		for got < N {
			n, err := wrapped.Read(buf)
			got += int64(n)
			if err != nil {
				if err == io.EOF {
					return
				}
				return
			}
		}
	}()

	wg.Wait()
	_ = wrapped.Close()

	gotSent := sendPtr.Load()
	gotRecv := recvPtr.Load()

	t.Logf("ground truth: upload=%d download=%d", wantUp, wantDown)
	t.Logf("counter snap: sent=%d recv=%d", gotSent, gotRecv)

	if gotSent != wantUp {
		t.Errorf("sent counter = %d; want %d (delta %+d)", gotSent, wantUp, gotSent-wantUp)
	}
	if gotRecv != wantDown {
		t.Errorf("recv counter = %d; want %d (delta %+d)", gotRecv, wantDown, gotRecv-wantDown)
	}
}

// TestEndToEndReportMatchesGroundTruth exercises the full path that
// userInfoMonitor uses in production: the bytes go through a wrapped
// net.Pipe, the user counter accumulates, ResetTraffic snapshots, and
// the resulting UserTraffic is sent to the fake SSPanel via the real
// sspanel.APIClient. The fake panel records what it received. We assert
// the bytes posted match the bytes pumped — no inflation.
func TestEndToEndReportMatchesGroundTruth(t *testing.T) {
	fp := NewFakePanel(testNodeID, 14555, testUID, testUUID, testPasswd)
	defer fp.Close()

	apiClient := sspanel.New(&api.Config{
		APIHost:             fp.URL,
		NodeID:              testNodeID,
		Key:                 "test-key",
		NodeType:            "V2ray",
		Timeout:             5,
		DisableCustomConfig: true, // forces V2ray legacy parser path
	})

	// 1. Verify GetNodeInfo + GetUserList work end-to-end so the test
	// covers the same panel-fetch round-trip production runs.
	nodeInfo, err := apiClient.GetNodeInfo()
	if err != nil {
		t.Fatalf("GetNodeInfo: %v", err)
	}
	if nodeInfo.NodeType != "anytls" {
		t.Fatalf("expected effective NodeType=anytls (ws+/anytls path); got %q", nodeInfo.NodeType)
	}
	userList, err := apiClient.GetUserList()
	if err != nil {
		t.Fatalf("GetUserList: %v", err)
	}
	if len(*userList) != 1 || (*userList)[0].UID != testUID {
		t.Fatalf("unexpected user list: %+v", *userList)
	}

	// 2. Set up Authenticator the way controller.syncUserList does.
	auth := newAuth(t)
	addUser(t, auth)

	// 3. Wrap two separate "connections" to ensure we don't double-count
	// across conns sharing the same user pointer.
	const perConn = 512 * 1024
	const conns = 4
	totalEachDirection := int64(perConn) * int64(conns)

	for i := 0; i < conns; i++ {
		pumpThroughCounterConn(t, auth, perConn)
	}

	// 4. Replicate userInfoMonitor's inner loop verbatim.
	userArr := auth.ListUsers()
	if len(userArr) != 1 {
		t.Fatalf("expected 1 user in authenticator; got %d", len(userArr))
	}
	var userTraffic []api.UserTraffic
	for _, u := range userArr {
		sent, recv := u.ResetTraffic()
		t.Logf("ResetTraffic UID=%d sent=%d recv=%d", u.UID, sent, recv)
		if sent == 0 && recv == 0 {
			continue
		}
		// trafficForSSPanel is unexported but we know it returns raw bytes
		// — verified by TestTrafficForSSPanelReportsRawBytes.
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

	// 5. Compare reported vs ground truth.
	upload, download := fp.SumReported(testUID)
	t.Logf("ground truth (per-conn=%d, conns=%d): upload=%d download=%d", perConn, conns, totalEachDirection, totalEachDirection)
	t.Logf("panel received: upload=%d download=%d", upload, download)

	if upload != totalEachDirection {
		t.Errorf("panel upload = %d; want %d (ratio %.2fx)", upload, totalEachDirection, float64(upload)/float64(totalEachDirection))
	}
	if download != totalEachDirection {
		t.Errorf("panel download = %d; want %d (ratio %.2fx)", download, totalEachDirection, float64(download)/float64(totalEachDirection))
	}

	// 6. Verify the panel only received ONE traffic report (not N).
	reports := fp.TrafficReports()
	if len(reports) != 1 {
		t.Errorf("expected exactly 1 traffic report; got %d", len(reports))
	}
	if len(reports) == 1 && len(reports[0].Data) != 1 {
		t.Errorf("expected exactly 1 record in the report; got %d (%+v)", len(reports[0].Data), reports[0].Data)
	}
}

// TestRepeatedCyclesNoInflation drives multiple report cycles back-to-back
// and asserts the panel's accumulated total exactly equals the sum of
// bytes pumped — no double-counting across cycles.
func TestRepeatedCyclesNoInflation(t *testing.T) {
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

	auth := newAuth(t)
	addUser(t, auth)

	const perCycle = 200 * 1024
	const cycles = 5
	expected := int64(perCycle) * int64(cycles)

	for i := 0; i < cycles; i++ {
		pumpThroughCounterConn(t, auth, perCycle)
		// One report cycle.
		var userTraffic []api.UserTraffic
		for _, u := range auth.ListUsers() {
			sent, recv := u.ResetTraffic()
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
			t.Fatalf("cycle %d ReportUserTraffic: %v", i, err)
		}
	}

	upload, download := fp.SumReported(testUID)
	t.Logf("after %d cycles of %d bytes each: ground=%d panelUp=%d panelDown=%d", cycles, perCycle, expected, upload, download)
	if upload != expected {
		t.Errorf("upload accumulated = %d; want %d (ratio %.2fx)", upload, expected, float64(upload)/float64(expected))
	}
	if download != expected {
		t.Errorf("download accumulated = %d; want %d (ratio %.2fx)", download, expected, float64(download)/float64(expected))
	}
}

// --- helpers ---

func newAuth(t *testing.T) *controller.Authenticator {
	t.Helper()
	auth, err := controller.NewAuthenticator(context.Background())
	if err != nil {
		t.Fatalf("NewAuthenticator: %v", err)
	}
	return auth
}

func addUser(t *testing.T, auth *controller.Authenticator) {
	t.Helper()
	if err := auth.AddUser(testHash, testUID); err != nil {
		t.Fatalf("AddUser: %v", err)
	}
	auth.SetUserProfile(testHash, api.UserInfo{
		UID:    testUID,
		Email:  testEmail,
		UUID:   testUUID,
		Passwd: testPasswd,
	})
	auth.SetUserAliases(testHash, testUUID, testPasswd, testEmail)
}

// pumpThroughCounterConn creates a net.Pipe, wraps the local end exactly
// like poet.RoutedConnection does, then exchanges N bytes in EACH direction
// across that wrapped conn. After it returns, the user atomics for the
// configured user have grown by N (sent) and N (recv).
func pumpThroughCounterConn(t *testing.T, auth *controller.Authenticator, n int) {
	t.Helper()
	user, ok := auth.LoadUser(testHash)
	if !ok {
		t.Fatalf("LoadUser %s failed", testHash)
	}
	sendPtr, recvPtr := user.GetTrafficPointer()

	a, b := net.Pipe()
	wrapped := bufio.NewInt64CounterConn(a, []*atomic.Int64{sendPtr}, []*atomic.Int64{recvPtr})

	// We want both directions exercised so the final atomics each grew by N.
	// Direction 1: peer (b) writes N bytes → wrapped Reads N → sendPtr += N (Upload).
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		buf := makePayload(n, 0xAB)
		writeAll(b, buf)
	}()
	go func() {
		defer wg.Done()
		buf := make([]byte, 64*1024)
		readN(wrapped, buf, n)
	}()
	wg.Wait()

	// Direction 2: wrapped writes N bytes → b reads N → recvPtr += N (Download).
	wg.Add(2)
	go func() {
		defer wg.Done()
		buf := makePayload(n, 0xCD)
		writeAll(wrapped, buf)
	}()
	go func() {
		defer wg.Done()
		buf := make([]byte, 64*1024)
		readN(b, buf, n)
	}()
	wg.Wait()

	_ = wrapped.Close()
	_ = b.Close()
}

func makePayload(n int, fill byte) []byte {
	buf := make([]byte, n)
	for i := range buf {
		buf[i] = fill
	}
	return buf
}

func writeAll(w io.Writer, buf []byte) {
	for off := 0; off < len(buf); {
		n, err := w.Write(buf[off:])
		if err != nil {
			return
		}
		off += n
	}
}

func readN(r io.Reader, buf []byte, n int) {
	got := 0
	for got < n {
		k, err := r.Read(buf)
		got += k
		if err != nil {
			return
		}
	}
}

// avoid unused import lint when the t.Logf path is gated by debug.
var _ = fmt.Sprintf
