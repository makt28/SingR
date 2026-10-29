package naive_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	stdtls "crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/sagernet/quic-go"
	"github.com/sagernet/quic-go/http3"
	"github.com/sagernet/sing-box/adapter"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing-box/poet/api"
	"github.com/sagernet/sing-box/protocol/naive"
	_ "github.com/sagernet/sing-box/protocol/naive/quic"
	"github.com/sagernet/sing/common/json/badoption"
	N "github.com/sagernet/sing/common/network"

	"golang.org/x/net/http2"
)

func TestBuildNaiveUsersUsesCanonicalNameAndUUIDFallback(t *testing.T) {
	names, passwords := naive.BuildNaiveUsers([]api.UserInfo{
		{UID: 1001, UUID: "uuid-1001", Passwd: "passwd-1001"},
		{UID: 1002, Passwd: "passwd-1002"},
	})
	if len(names) != 2 || names[0] != "u1001" || passwords[0] != "uuid-1001" || names[1] != "u1002" || passwords[1] != "passwd-1002" {
		t.Fatalf("users = %v / %v, want canonical names with UUID-first passwords", names, passwords)
	}
}

// Authentication is by password only and the identity comes from the user
// table: the client-supplied username is ignored, including one that names
// a different user.
func TestNaivePanelUserUpdatesAffectAuthentication(t *testing.T) {
	h := newTestNaiveInbound(t, testFreePort(t), "initial.example.com")
	defer h.Close()
	router := &captureRouter{}
	h.SetRouterForTest(router)

	users := []api.UserInfo{
		{UID: 1001, UUID: "uuid-1001", Passwd: "unused-passwd"},
		{UID: 1002, Passwd: "passwd-1002"},
	}
	if err := h.RefreshUsers(&users, &api.NodeInfo{NodeType: C.TypeNaive}); err != nil {
		t.Fatal(err)
	}
	assertServeHTTPUser(t, h, router, "anything", "uuid-1001", "u1001")
	assertServeHTTPUser(t, h, router, "u1002", "uuid-1001", "u1001")
	assertServeHTTPUser(t, h, router, "", "unused-passwd", "")
	assertServeHTTPUser(t, h, router, "x", "passwd-1002", "u1002")

	rotated := []api.UserInfo{{UID: 1001, UUID: "uuid-1001-rotated"}}
	if err := h.AddUsers(&rotated, &api.NodeInfo{NodeType: C.TypeNaive}); err != nil {
		t.Fatal(err)
	}
	assertServeHTTPUser(t, h, router, "x", "uuid-1001", "")
	assertServeHTTPUser(t, h, router, "x", "uuid-1001-rotated", "u1001")

	if err := h.RemoveUsers([]string{"u1002"}); err != nil {
		t.Fatal(err)
	}
	assertServeHTTPUser(t, h, router, "x", "passwd-1002", "")
}

func TestNaiveConfigureFromPanelNodePreStart(t *testing.T) {
	initialPort := testFreePort(t)
	panelPort := testFreePort(t)
	h := newTestNaiveInbound(t, initialPort, "initial.example.com")
	defer h.Close()

	if err := h.ConfigureFromPanelNode(&api.NodeInfo{NodeType: C.TypeNaive, Port: uint32(panelPort), Host: "panel.example.com"}); err != nil {
		t.Fatal(err)
	}
	options := h.OptionsForTest()
	if options.ListenPort != uint16(panelPort) || options.TLS == nil || options.TLS.ServerName != "panel.example.com" {
		t.Fatalf("options after pre-Start configure = %#v", options)
	}
	if err := h.Start(adapter.StartStateStart, newTestScope()); err != nil {
		t.Fatal(err)
	}
	assertPortBound(t, panelPort, true)
	assertPortBound(t, initialPort, false)
}

// See TestAnyTLSConfigureFromPanelNodeRejectsOtherNodeType. The started
// inbound must stay on its port; the mismatched node's port stays free.
func TestNaiveConfigureFromPanelNodeRejectsOtherNodeType(t *testing.T) {
	port := testFreePort(t)
	h := newStartedNaiveInbound(t, port)
	defer h.Close()

	for _, nodeType := range []string{C.TypeAnyTLS, C.TypeHysteria2, "V2ray", ""} {
		otherPort := testFreePort(t)
		err := h.ConfigureFromPanelNode(&api.NodeInfo{NodeType: nodeType, Port: uint32(otherPort), Host: "other.example.com"})
		if err == nil {
			t.Fatalf("node type %q: expected mismatch error", nodeType)
		}
		assertPortBound(t, otherPort, false)
		assertPortBound(t, port, true)
	}
	if err := h.ConfigureFromPanelNode(nil); err != nil {
		t.Fatalf("nil node info: %v", err)
	}
}

// Both transports serve, and a port change moves both of them.
func TestNaiveServesHTTP2AndHTTP3AcrossPortReload(t *testing.T) {
	initialPort := testFreePort(t)
	reloadPort := testFreePort(t)
	h := newStartedNaiveInbound(t, initialPort)
	defer h.Close()
	router := &captureRouter{}
	h.SetRouterForTest(router)

	assertConnect(t, router, connectHTTP2, initialPort, "initial.example.com", "uuid-1001", "u1001")
	assertConnect(t, router, connectHTTP3, initialPort, "initial.example.com", "uuid-1001", "u1001")

	if err := h.ConfigureFromPanelNode(&api.NodeInfo{NodeType: C.TypeNaive, Port: uint32(reloadPort), Host: "initial.example.com"}); err != nil {
		t.Fatal(err)
	}
	assertPortBound(t, reloadPort, true)
	assertPortBound(t, initialPort, false)
	assertConnect(t, router, connectHTTP2, reloadPort, "initial.example.com", "uuid-1001", "u1001")
	assertConnect(t, router, connectHTTP3, reloadPort, "initial.example.com", "uuid-1001", "u1001")
}

// An SNI-only change swaps the TLS config without rebuilding the listener or
// the HTTP/3 server, and QUIC handshakes resolve to the new config.
func TestNaiveSNIOnlyReloadKeepsSockets(t *testing.T) {
	port := testFreePort(t)
	h := newStartedNaiveInbound(t, port)
	defer h.Close()
	router := &captureRouter{}
	h.SetRouterForTest(router)

	oldListener, oldH3, oldTLS := h.ComponentsForTest()
	if err := h.ConfigureFromPanelNode(&api.NodeInfo{NodeType: C.TypeNaive, Port: uint32(port), Host: "reloaded.example.com"}); err != nil {
		t.Fatal(err)
	}
	newListener, newH3, newTLS := h.ComponentsForTest()
	if newListener != oldListener || newH3 != oldH3 {
		t.Fatal("SNI-only reload rebuilt the listener or the HTTP/3 server")
	}
	if newTLS == oldTLS {
		t.Fatal("SNI-only reload did not replace the TLS config")
	}
	if options := h.OptionsForTest(); options.TLS.ServerName != "reloaded.example.com" {
		t.Fatalf("SNI after reload = %q", options.TLS.ServerName)
	}
	quicConfig, err := h.QUICConfigForTest()
	if err != nil {
		t.Fatal(err)
	}
	currentConfig, err := h.CurrentSTDConfigForTest()
	if err != nil {
		t.Fatal(err)
	}
	if quicConfig != currentConfig || quicConfig.ServerName != "reloaded.example.com" {
		t.Fatalf("QUIC handshake config = %p (server name %q), want current %p", quicConfig, quicConfig.ServerName, currentConfig)
	}
	for _, protocol := range []string{"h2", "http/1.1", "h3"} {
		if !containsString(quicConfig.NextProtos, protocol) {
			t.Fatalf("reloaded TLS ALPN = %v, missing %s", quicConfig.NextProtos, protocol)
		}
	}
	assertConnect(t, router, connectHTTP2, port, "reloaded.example.com", "uuid-1001", "u1001")
	assertConnect(t, router, connectHTTP3, port, "reloaded.example.com", "uuid-1001", "u1001")
}

// Both accept paths read the TLS config per connection: once the inbound's
// config is replaced, HTTP/2 and HTTP/3 handshakes on the very same sockets
// present the new certificate. This is what lets an SNI change skip the
// UDP rebuild; passing a fixed config to the HTTP/3 listener (upstream's
// shape) fails here.
func TestNaiveTransportsFollowCurrentTLSConfig(t *testing.T) {
	port := testFreePort(t)
	h := newStartedNaiveInbound(t, port)
	defer h.Close()
	router := &captureRouter{}
	h.SetRouterForTest(router)

	for _, connect := range []connectFunc{connectHTTP2, connectHTTP3} {
		if status, serial := connect(t, port, "initial.example.com", "uuid-1001"); status != http.StatusOK || serial.Int64() != 1 {
			t.Fatalf("before swap: status %d certificate serial %v, want 200 and 1", status, serial)
		}
	}
	certPEM, keyPEM := testSelfSignedCertificate(t, 2)
	if err := h.SwapTLSForTest(option.InboundTLSOptions{
		Enabled:     true,
		ServerName:  "initial.example.com",
		Certificate: badoption.Listable[string]{certPEM},
		Key:         badoption.Listable[string]{keyPEM},
	}); err != nil {
		t.Fatal(err)
	}
	for _, connect := range []connectFunc{connectHTTP2, connectHTTP3} {
		if status, serial := connect(t, port, "initial.example.com", "uuid-1001"); status != http.StatusOK || serial.Int64() != 2 {
			t.Fatalf("after swap: status %d certificate serial %v, want 200 and 2", status, serial)
		}
	}
}

// A port reload is all-or-nothing across TCP and UDP: if either socket of
// the new port cannot bind, the old port keeps serving both and nothing is
// left bound on the new one.
func TestNaivePortReloadRollsBackBothTransports(t *testing.T) {
	port := testFreePort(t)
	h := newStartedNaiveInbound(t, port)
	defer h.Close()
	oldListener, oldH3, oldTLS := h.ComponentsForTest()

	for _, network := range []string{"tcp", "udp"} {
		t.Run(network, func(t *testing.T) {
			targetPort := testFreePort(t)
			occupied := occupyPort(t, network, targetPort)
			defer occupied.Close()
			if err := h.ConfigureFromPanelNode(&api.NodeInfo{NodeType: C.TypeNaive, Port: uint32(targetPort), Host: "must-not-commit.example.com"}); err == nil {
				t.Fatal("expected reload onto an occupied port to fail")
			}
			listener, h3Server, tlsConfig := h.ComponentsForTest()
			if listener != oldListener || h3Server != oldH3 || tlsConfig != oldTLS {
				t.Fatal("failed reload replaced live components")
			}
			if options := h.OptionsForTest(); options.ListenPort != uint16(port) || options.TLS.ServerName != "initial.example.com" {
				t.Fatalf("failed reload committed options: %#v", options)
			}
			_ = occupied.Close()
			assertPortBound(t, targetPort, false)
			assertPortBound(t, port, true)
		})
	}
}

// The 1.14.3 lifecycle closes inbounds through the Scope passed to Start.
// The scope must release the port that is live at shutdown — the
// hot-reloaded one — on both TCP and UDP.
func TestNaiveScopeCloseReleasesHotReloadedListener(t *testing.T) {
	initialPort := testFreePort(t)
	reloadPort := testFreePort(t)
	h := newTestNaiveInbound(t, initialPort, "initial.example.com")
	scope := newTestScope()
	if err := h.Start(adapter.StartStateStart, scope); err != nil {
		t.Fatal(err)
	}
	if err := h.ConfigureFromPanelNode(&api.NodeInfo{NodeType: C.TypeNaive, Port: uint32(reloadPort), Host: "initial.example.com"}); err != nil {
		t.Fatal(err)
	}
	assertPortBound(t, reloadPort, true)
	assertPortBound(t, initialPort, false)
	if err := scope.Close(); err != nil {
		t.Fatal(err)
	}
	assertPortBound(t, reloadPort, false)
}

func TestNaiveConfigureFromPanelNodeRejectsInvalidPort(t *testing.T) {
	h := newTestNaiveInbound(t, testFreePort(t), "initial.example.com")
	defer h.Close()
	oldOptions := h.OptionsForTest()
	for _, port := range []uint32{0, 65536} {
		if err := h.ConfigureFromPanelNode(&api.NodeInfo{NodeType: C.TypeNaive, Port: port}); err == nil {
			t.Fatalf("port %d: expected validation error", port)
		}
		if options := h.OptionsForTest(); options.ListenPort != oldOptions.ListenPort || options.TLS.ServerName != oldOptions.TLS.ServerName {
			t.Fatalf("port %d: invalid update changed options", port)
		}
	}
}

func TestNaiveConfigureFromPanelNodeNoOpPreservesComponents(t *testing.T) {
	port := testFreePort(t)
	h := newStartedNaiveInbound(t, port)
	defer h.Close()
	oldListener, oldH3, oldTLS := h.ComponentsForTest()
	if err := h.ConfigureFromPanelNode(&api.NodeInfo{NodeType: C.TypeNaive, Port: uint32(port), Host: "initial.example.com"}); err != nil {
		t.Fatal(err)
	}
	if listener, h3Server, tlsConfig := h.ComponentsForTest(); listener != oldListener || h3Server != oldH3 || tlsConfig != oldTLS {
		t.Fatal("identical panel configuration rebuilt live components")
	}
}

// A panel tick that lands after Close must not bring a listener back up.
func TestNaiveConfigureFromPanelNodeAfterCloseIsNoOp(t *testing.T) {
	port := testFreePort(t)
	reloadPort := testFreePort(t)
	h := newStartedNaiveInbound(t, port)
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	if err := h.ConfigureFromPanelNode(&api.NodeInfo{NodeType: C.TypeNaive, Port: uint32(reloadPort), Host: "initial.example.com"}); err != nil {
		t.Fatal(err)
	}
	assertPortBound(t, reloadPort, false)
	assertPortBound(t, port, false)
}

func TestNaiveConfigureFromPanelNodeConcurrentReloadAndUsers(t *testing.T) {
	portA, portB := testFreePort(t), testFreePort(t)
	h := newStartedNaiveInbound(t, portA)
	defer h.Close()
	router := &captureRouter{}
	h.SetRouterForTest(router)

	start := make(chan struct{})
	errs := make(chan error, 3)
	var waitGroup sync.WaitGroup
	waitGroup.Add(3)
	go func() {
		defer waitGroup.Done()
		<-start
		ports := []int{portB, portA}
		hosts := []string{"reloaded.example.com", "initial.example.com"}
		for index := 0; index < 20; index++ {
			if err := h.ConfigureFromPanelNode(&api.NodeInfo{NodeType: C.TypeNaive, Port: uint32(ports[index%2]), Host: hosts[index%2]}); err != nil {
				errs <- err
				return
			}
		}
	}()
	go func() {
		defer waitGroup.Done()
		<-start
		for index := 0; index < 60; index++ {
			users := []api.UserInfo{{UID: 1001, UUID: "uuid-1001"}, {UID: 1002, UUID: fmt.Sprintf("uuid-1002-%d", index)}}
			var err error
			switch index % 3 {
			case 0:
				err = h.AddUsers(&users, &api.NodeInfo{NodeType: C.TypeNaive})
			case 1:
				err = h.RemoveUsers([]string{"u1002"})
			default:
				err = h.RefreshUsers(&users, &api.NodeInfo{NodeType: C.TypeNaive})
			}
			if err != nil {
				errs <- err
				return
			}
		}
	}()
	go func() {
		defer waitGroup.Done()
		<-start
		for index := 0; index < 60; index++ {
			if _, err := h.QUICConfigForTest(); err != nil {
				errs <- err
				return
			}
			recorder := httptest.NewRecorder()
			h.ServeHTTP(recorder, newConnectRequest("x", "uuid-1001"))
		}
	}()
	close(start)
	waitGroup.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}

type captureRouter struct {
	adapter.ConnectionRouterEx
	access sync.Mutex
	users  []string
}

func (r *captureRouter) RouteConnectionEx(_ context.Context, conn net.Conn, metadata adapter.InboundContext, onClose N.CloseHandlerFunc) {
	r.access.Lock()
	r.users = append(r.users, metadata.User)
	r.access.Unlock()
	_ = conn.Close()
	if onClose != nil {
		onClose(nil)
	}
}

func (r *captureRouter) RoutePacketConnectionEx(context.Context, N.PacketConn, adapter.InboundContext, N.CloseHandlerFunc) {
}

func (r *captureRouter) take() []string {
	r.access.Lock()
	defer r.access.Unlock()
	users := r.users
	r.users = nil
	return users
}

func newConnectRequest(username, password string) *http.Request {
	request := httptest.NewRequest(http.MethodConnect, "https://example.com:443", nil)
	request.Host = "example.com:443"
	request.Header.Set("Padding", "~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~")
	request.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(username+":"+password)))
	return request
}

// assertServeHTTPUser drives ServeHTTP directly; wantUser "" means the
// request must be rejected with 407.
func assertServeHTTPUser(t *testing.T, h *naive.Inbound, router *captureRouter, username, password, wantUser string) {
	t.Helper()
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, newConnectRequest(username, password))
	users := router.take()
	if wantUser == "" {
		if recorder.Code != http.StatusProxyAuthRequired || len(users) != 0 {
			t.Fatalf("%s:%s: status %d routed %v, want 407 and no route", username, password, recorder.Code, users)
		}
		return
	}
	if recorder.Code != http.StatusOK || len(users) != 1 || users[0] != wantUser {
		t.Fatalf("%s:%s: status %d routed %v, want 200 as %s", username, password, recorder.Code, users, wantUser)
	}
}

// connectFunc sends one CONNECT and returns the status and the serial of
// the certificate the server presented.
type connectFunc func(t *testing.T, port int, serverName, password string) (int, *big.Int)

func connectHTTP2(t *testing.T, port int, serverName, password string) (int, *big.Int) {
	t.Helper()
	transport := &http2.Transport{
		DialTLSContext: func(ctx context.Context, _, _ string, _ *stdtls.Config) (net.Conn, error) {
			dialer := stdtls.Dialer{Config: &stdtls.Config{InsecureSkipVerify: true, ServerName: serverName, NextProtos: []string{"h2"}}}
			return dialer.DialContext(ctx, "tcp", net.JoinHostPort("127.0.0.1", fmt.Sprint(port)))
		},
	}
	defer transport.CloseIdleConnections()
	return doConnect(t, transport, password)
}

func connectHTTP3(t *testing.T, port int, serverName, password string) (int, *big.Int) {
	t.Helper()
	transport := &http3.Transport{
		TLSClientConfig: &stdtls.Config{InsecureSkipVerify: true, ServerName: serverName},
		Dial: func(ctx context.Context, _ string, tlsConfig *stdtls.Config, quicConfig *quic.Config) (*quic.Conn, error) {
			return quic.DialAddrEarly(ctx, net.JoinHostPort("127.0.0.1", fmt.Sprint(port)), tlsConfig, quicConfig)
		},
	}
	defer transport.Close()
	return doConnect(t, transport, password)
}

func doConnect(t *testing.T, transport http.RoundTripper, password string) (int, *big.Int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	body, bodyWriter := io.Pipe()
	defer bodyWriter.Close()
	request, err := http.NewRequestWithContext(ctx, http.MethodConnect, "https://example.com:443", body)
	if err != nil {
		t.Fatal(err)
	}
	request.Host = "example.com:443"
	request.Header.Set("Padding", "~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~")
	request.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte("anything:"+password)))
	response, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.TLS == nil || len(response.TLS.PeerCertificates) == 0 {
		t.Fatal("response carries no TLS state")
	}
	return response.StatusCode, response.TLS.PeerCertificates[0].SerialNumber
}

func assertConnect(t *testing.T, router *captureRouter, connect connectFunc, port int, serverName, password, wantUser string) {
	t.Helper()
	if status, _ := connect(t, port, serverName, password); status != http.StatusOK {
		t.Fatalf("CONNECT on port %d status = %d, want 200", port, status)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if users := router.take(); len(users) > 0 {
			if users[0] != wantUser {
				t.Fatalf("routed as %v, want %s", users, wantUser)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("CONNECT was never routed")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func newTestScope() *adapter.Scope {
	return adapter.NewScope(context.Background(), log.NewNOPFactory().Logger())
}

func newStartedNaiveInbound(t *testing.T, port int) *naive.Inbound {
	t.Helper()
	h := newTestNaiveInbound(t, port, "initial.example.com")
	users := []api.UserInfo{{UID: 1001, UUID: "uuid-1001"}}
	if err := h.AddUsers(&users, &api.NodeInfo{NodeType: C.TypeNaive}); err != nil {
		t.Fatal(err)
	}
	if err := h.Start(adapter.StartStateStart, newTestScope()); err != nil {
		t.Fatal(err)
	}
	return h
}

// newTestNaiveInbound builds the inbound the way the default server.json
// does: no users, network left empty (HTTP/2 + HTTP/3).
func newTestNaiveInbound(t *testing.T, port int, serverName string) *naive.Inbound {
	t.Helper()
	certPEM, keyPEM := testSelfSignedCertificate(t, 1)
	listenAddr := badoption.Addr(netip.MustParseAddr("127.0.0.1"))
	inbound, err := naive.NewInbound(context.Background(), nil, log.NewNOPFactory().Logger(), "naive-in", option.NaiveInboundOptions{
		ListenOptions: option.ListenOptions{
			Listen:     &listenAddr,
			ListenPort: uint16(port),
		},
		InboundTLSOptionsContainer: option.InboundTLSOptionsContainer{
			TLS: &option.InboundTLSOptions{
				Enabled:     true,
				ServerName:  serverName,
				Certificate: badoption.Listable[string]{certPEM},
				Key:         badoption.Listable[string]{keyPEM},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return inbound.(*naive.Inbound)
}

// assertPortBound checks TCP and UDP together: naive binds both.
func assertPortBound(t *testing.T, port int, want bool) {
	t.Helper()
	address := net.JoinHostPort("127.0.0.1", fmt.Sprint(port))
	tcpProbe, tcpErr := net.Listen("tcp", address)
	if tcpProbe != nil {
		_ = tcpProbe.Close()
	}
	udpProbe, udpErr := net.ListenPacket("udp", address)
	if udpProbe != nil {
		_ = udpProbe.Close()
	}
	if tcpBound, udpBound := tcpErr != nil, udpErr != nil; tcpBound != want || udpBound != want {
		t.Fatalf("port %d bound tcp=%v udp=%v, want %v (errors: %v / %v)", port, tcpBound, udpBound, want, tcpErr, udpErr)
	}
}

func occupyPort(t *testing.T, network string, port int) io.Closer {
	t.Helper()
	address := net.JoinHostPort("127.0.0.1", fmt.Sprint(port))
	if network == "tcp" {
		listener, err := net.Listen("tcp", address)
		if err != nil {
			t.Fatal(err)
		}
		return listener
	}
	conn, err := net.ListenPacket("udp", address)
	if err != nil {
		t.Fatal(err)
	}
	return conn
}

// testFreePort returns a port that is free on both TCP and UDP.
func testFreePort(t *testing.T) int {
	t.Helper()
	for attempt := 0; attempt < 20; attempt++ {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		port := listener.Addr().(*net.TCPAddr).Port
		conn, err := net.ListenPacket("udp", net.JoinHostPort("127.0.0.1", fmt.Sprint(port)))
		_ = listener.Close()
		if err == nil {
			_ = conn.Close()
			return port
		}
	}
	t.Fatal("no port free on both TCP and UDP")
	return 0
}

func containsString(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}

func testSelfSignedCertificate(t *testing.T, serial int64) (string, string) {
	t.Helper()
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	template := x509.Certificate{
		SerialNumber: big.NewInt(serial),
		Subject:      pkix.Name{CommonName: "test.example.com"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		DNSNames:     []string{"initial.example.com", "panel.example.com", "reloaded.example.com"},
		KeyUsage:     x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	certDER, err := x509.CreateCertificate(rand.Reader, &template, &template, &privateKey.PublicKey, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER := x509.MarshalPKCS1PrivateKey(privateKey)
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: keyDER})
	return string(certPEM), string(keyPEM)
}
