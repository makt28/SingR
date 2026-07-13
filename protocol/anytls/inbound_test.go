package anytls

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sagernet/sing-box/adapter"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing-box/poet/api"
	"github.com/sagernet/sing/common/auth"
	"github.com/sagernet/sing/common/json/badoption"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
)

func TestBuildAnyTLSUsersUsesCanonicalNameAndUUIDFallback(t *testing.T) {
	users := buildAnyTLSUsers([]api.UserInfo{
		{UID: 1001, UUID: "uuid-1001", Passwd: "passwd-1001"},
		{UID: 1002, Passwd: "passwd-1002"},
	})
	if len(users) != 2 {
		t.Fatalf("got %d users, want 2", len(users))
	}
	if users[0].Name != "u1001" || users[0].Password != "uuid-1001" {
		t.Fatalf("first user = %#v, want canonical name with UUID password", users[0])
	}
	if users[1].Name != "u1002" || users[1].Password != "passwd-1002" {
		t.Fatalf("second user = %#v, want canonical name with passwd fallback", users[1])
	}
}

func TestAnyTLSPanelUserUpdatesAffectAuthentication(t *testing.T) {
	h := newTestAnyTLSInbound(t, testFreeTCPPort(t), "initial.example.com")
	defer h.Close()

	users := []api.UserInfo{
		{UID: 1001, UUID: "uuid-1001", Passwd: "unused-passwd"},
		{UID: 1002, Passwd: "passwd-1002"},
	}
	if err := h.RefreshUsers(&users, &api.NodeInfo{NodeType: C.TypeAnyTLS}); err != nil {
		t.Fatal(err)
	}
	assertAnyTLSPasswordAccepted(t, h, "uuid-1001", true)
	assertAnyTLSPasswordAccepted(t, h, "unused-passwd", false)
	assertAnyTLSPasswordAccepted(t, h, "passwd-1002", true)

	rotated := []api.UserInfo{{UID: 1001, UUID: "uuid-1001-rotated"}}
	if err := h.AddUsers(&rotated, &api.NodeInfo{NodeType: C.TypeAnyTLS}); err != nil {
		t.Fatal(err)
	}
	assertAnyTLSPasswordAccepted(t, h, "uuid-1001", false)
	assertAnyTLSPasswordAccepted(t, h, "uuid-1001-rotated", true)

	if err := h.RemoveUsers([]string{"u1002"}); err != nil {
		t.Fatal(err)
	}
	assertAnyTLSPasswordAccepted(t, h, "passwd-1002", false)
}

func TestAnyTLSConfigureFromPanelNodePreStart(t *testing.T) {
	initialPort := testFreeTCPPort(t)
	panelPort := testFreeTCPPort(t)
	h := newTestAnyTLSInbound(t, initialPort, "initial.example.com")
	defer h.Close()

	if err := h.ConfigureFromPanelNode(&api.NodeInfo{
		NodeType: C.TypeAnyTLS,
		Port:     uint32(panelPort),
		Host:     "panel.example.com",
	}); err != nil {
		t.Fatal(err)
	}
	if h.options.ListenPort != uint16(panelPort) {
		t.Fatalf("listen port = %d, want %d", h.options.ListenPort, panelPort)
	}
	if h.options.TLS == nil || h.options.TLS.ServerName != "panel.example.com" {
		t.Fatalf("TLS options = %#v, want panel SNI", h.options.TLS)
	}
	if err := h.Start(adapter.StartStateStart); err != nil {
		t.Fatal(err)
	}
	assertTCPPortBound(t, panelPort, true)
	assertTCPPortBound(t, initialPort, false)
}

func TestAnyTLSConfigureFromPanelNodeHotReloadAndRollback(t *testing.T) {
	initialPort := testFreeTCPPort(t)
	reloadPort := testFreeTCPPort(t)
	h := newTestAnyTLSInbound(t, initialPort, "initial.example.com")
	if err := h.Start(adapter.StartStateStart); err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	oldListener := h.listener
	oldTLS := h.tlsConfig
	if err := h.ConfigureFromPanelNode(&api.NodeInfo{
		NodeType: C.TypeAnyTLS,
		Port:     uint32(reloadPort),
		Host:     "reloaded.example.com",
	}); err != nil {
		t.Fatal(err)
	}
	if h.listener == oldListener {
		t.Fatal("listener was not replaced after port reload")
	}
	if h.tlsConfig == oldTLS {
		t.Fatal("TLS config was not replaced after SNI reload")
	}
	if h.options.ListenPort != uint16(reloadPort) || h.options.TLS.ServerName != "reloaded.example.com" {
		t.Fatalf("effective options after reload = %#v", h.options)
	}
	assertTCPPortBound(t, reloadPort, true)
	assertTCPPortBound(t, initialPort, false)

	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	occupiedPort := occupied.Addr().(*net.TCPAddr).Port
	stableListener := h.listener
	stableTLS := h.tlsConfig
	if err := h.ConfigureFromPanelNode(&api.NodeInfo{
		NodeType: C.TypeAnyTLS,
		Port:     uint32(occupiedPort),
		Host:     "must-not-commit.example.com",
	}); err == nil {
		t.Fatal("expected occupied-port reload to fail")
	}
	if h.listener != stableListener || h.tlsConfig != stableTLS {
		t.Fatal("failed reload replaced live listener or TLS config")
	}
	if h.options.ListenPort != uint16(reloadPort) || h.options.TLS.ServerName != "reloaded.example.com" {
		t.Fatalf("failed reload committed options: %#v", h.options)
	}
	assertTCPPortBound(t, reloadPort, true)
}

func TestAnyTLSConfigureFromPanelNodeRejectsInvalidPort(t *testing.T) {
	h := newTestAnyTLSInbound(t, testFreeTCPPort(t), "initial.example.com")
	defer h.Close()
	oldOptions := h.options

	for _, port := range []uint32{0, 65536} {
		if err := h.ConfigureFromPanelNode(&api.NodeInfo{NodeType: C.TypeAnyTLS, Port: port}); err == nil {
			t.Fatalf("port %d: expected validation error", port)
		}
		if h.options.ListenPort != oldOptions.ListenPort || h.options.TLS.ServerName != oldOptions.TLS.ServerName {
			t.Fatalf("port %d: invalid update changed options", port)
		}
	}
}

func TestAnyTLSConfigureFromPanelNodeNoOpPreservesComponents(t *testing.T) {
	port := testFreeTCPPort(t)
	h := newTestAnyTLSInbound(t, port, "initial.example.com")
	if err := h.Start(adapter.StartStateStart); err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	oldListener, oldTLS := h.listener, h.tlsConfig
	if err := h.ConfigureFromPanelNode(&api.NodeInfo{NodeType: C.TypeAnyTLS, Port: uint32(port), Host: "initial.example.com"}); err != nil {
		t.Fatal(err)
	}
	if h.listener != oldListener || h.tlsConfig != oldTLS {
		t.Fatal("identical panel configuration rebuilt live components")
	}
}

func TestAnyTLSConfigureFromPanelNodeConcurrentReloadAndUsers(t *testing.T) {
	portA, portB := testFreeTCPPort(t), testFreeTCPPort(t)
	h := newTestAnyTLSInbound(t, portA, "initial.example.com")
	if err := h.Start(adapter.StartStateStart); err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	start := make(chan struct{})
	errs := make(chan error, 2)
	var waitGroup sync.WaitGroup
	waitGroup.Add(2)
	go func() {
		defer waitGroup.Done()
		<-start
		ports := []int{portB, portA}
		for index := 0; index < 30; index++ {
			if err := h.ConfigureFromPanelNode(&api.NodeInfo{NodeType: C.TypeAnyTLS, Port: uint32(ports[index%2]), Host: "reloaded.example.com"}); err != nil {
				errs <- err
				return
			}
		}
	}()
	go func() {
		defer waitGroup.Done()
		<-start
		for index := 0; index < 60; index++ {
			users := []api.UserInfo{{UID: 1001, UUID: fmt.Sprintf("uuid-%d", index)}, {UID: 1002, Passwd: "passwd-1002"}}
			switch index % 3 {
			case 0:
				if err := h.AddUsers(&users, &api.NodeInfo{NodeType: C.TypeAnyTLS}); err != nil {
					errs <- err
					return
				}
			case 1:
				if err := h.RemoveUsers([]string{"u1002"}); err != nil {
					errs <- err
					return
				}
			default:
				if err := h.RefreshUsers(&users, &api.NodeInfo{NodeType: C.TypeAnyTLS}); err != nil {
					errs <- err
					return
				}
			}
		}
	}()
	close(start)
	waitGroup.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}

func TestAnyTLSInboundHandlerPreservesAuthenticatedUser(t *testing.T) {
	h := newTestAnyTLSInbound(t, testFreeTCPPort(t), "initial.example.com")
	defer h.Close()
	router := &captureAnyTLSRouter{}
	h.router = router
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()
	source := M.ParseSocksaddr("192.0.2.1:1234")
	destination := M.ParseSocksaddr("example.com:443")
	ctx := auth.ContextWithUser(context.Background(), "u40493")
	(*inboundHandler)(h).NewConnectionEx(ctx, server, source, destination, nil)

	metadata := router.metadata
	if metadata.User != "u40493" || metadata.Inbound != "anytls-in" || metadata.InboundType != C.TypeAnyTLS {
		t.Fatalf("metadata identity = %#v", metadata)
	}
	if metadata.Source != source || metadata.Destination != destination.Unwrap() {
		t.Fatalf("metadata addresses = source %v destination %v", metadata.Source, metadata.Destination)
	}
}

func TestIsExpectedAnyTLSHandshakeFailure(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "EOF", err: io.EOF, want: true},
		{name: "deadline", err: context.DeadlineExceeded, want: true},
		{name: "plain HTTP", err: errors.New("tls: client sent an HTTP request to an HTTPS server"), want: true},
		{name: "bad first record", err: errors.New("tls: first record does not look like a TLS handshake"), want: true},
		{name: "unexpected certificate failure", err: errors.New("certificate signing failure"), want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := isExpectedAnyTLSHandshakeFailure(test.err); got != test.want {
				t.Fatalf("isExpectedAnyTLSHandshakeFailure(%v) = %v, want %v", test.err, got, test.want)
			}
		})
	}
}

type captureAnyTLSRouter struct {
	adapter.ConnectionRouterEx
	metadata adapter.InboundContext
}

func (r *captureAnyTLSRouter) RouteConnection(context.Context, net.Conn, adapter.InboundContext) error {
	return nil
}

func (r *captureAnyTLSRouter) RoutePacketConnection(context.Context, N.PacketConn, adapter.InboundContext) error {
	return nil
}

func (r *captureAnyTLSRouter) RouteConnectionEx(_ context.Context, _ net.Conn, metadata adapter.InboundContext, _ N.CloseHandlerFunc) {
	r.metadata = metadata
}

func (r *captureAnyTLSRouter) RoutePacketConnectionEx(context.Context, N.PacketConn, adapter.InboundContext, N.CloseHandlerFunc) {
}

func newTestAnyTLSInbound(t *testing.T, port int, serverName string) *Inbound {
	t.Helper()
	certPEM, keyPEM := testAnyTLSSelfSignedCertificate(t)
	listenAddr := badoption.Addr(netip.MustParseAddr("127.0.0.1"))
	inbound, err := NewInbound(context.Background(), nil, log.NewNOPFactory().Logger(), "anytls-in", option.AnyTLSInboundOptions{
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
	return inbound.(*Inbound)
}

func assertAnyTLSPasswordAccepted(t *testing.T, h *Inbound, password string, want bool) {
	t.Helper()
	server, client := net.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- h.service.NewConnection(context.Background(), server, M.Socksaddr{}, nil)
	}()
	hash := sha256.Sum256([]byte(password))
	if _, err := client.Write(hash[:]); err != nil {
		t.Fatal(err)
	}
	_ = client.Close()
	err := <-done
	_ = server.Close()
	accepted := err != nil && !strings.Contains(err.Error(), "unknown user password")
	if accepted != want {
		t.Fatalf("password %q accepted = %v, want %v (error: %v)", password, accepted, want, err)
	}
}

func assertTCPPortBound(t *testing.T, port int, want bool) {
	t.Helper()
	probe, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", stringPort(port)))
	bound := err != nil
	if probe != nil {
		_ = probe.Close()
	}
	if bound != want {
		t.Fatalf("TCP port %d bound = %v, want %v (probe error: %v)", port, bound, want, err)
	}
}

func stringPort(port int) string {
	return fmt.Sprint(port)
}

func testFreeTCPPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}

func testAnyTLSSelfSignedCertificate(t *testing.T) (string, string) {
	t.Helper()
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
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
