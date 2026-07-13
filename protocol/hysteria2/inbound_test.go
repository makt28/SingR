package hysteria2

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/netip"
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

func TestConfigureFromPanelNodeConcurrentReloadAndUsers(t *testing.T) {
	certPEM, keyPEM := testSelfSignedCertificate(t)
	portA := testFreeUDPPort(t)
	portB := testFreeUDPPort(t)
	listenAddr := badoption.Addr(netip.MustParseAddr("127.0.0.1"))

	inbound, err := NewInbound(context.Background(), nil, log.NewNOPFactory().Logger(), "hysteria2-in", option.Hysteria2InboundOptions{
		ListenOptions: option.ListenOptions{
			Listen:     &listenAddr,
			ListenPort: uint16(portA),
		},
		InboundTLSOptionsContainer: option.InboundTLSOptionsContainer{
			TLS: &option.InboundTLSOptions{
				Enabled:     true,
				ServerName:  "initial.example.com",
				Certificate: badoption.Listable[string]{certPEM},
				Key:         badoption.Listable[string]{keyPEM},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	h := inbound.(*Inbound)
	if err := h.Start(adapter.StartStateStart); err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	start := make(chan struct{})
	errs := make(chan error, 64)
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		ports := []uint32{uint32(portB), uint32(portA)}
		hosts := []string{"reload-a.example.com", "reload-b.example.com"}
		for i := 0; i < 40; i++ {
			if err := h.ConfigureFromPanelNode(&api.NodeInfo{
				NodeType: C.TypeHysteria2,
				Port:     ports[i%len(ports)],
				Host:     hosts[i%len(hosts)],
			}); err != nil {
				errs <- err
				return
			}
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		for i := 0; i < 80; i++ {
			users := []api.UserInfo{
				{UID: 1001, UUID: "uuid-1001", Passwd: "passwd-1001"},
				{UID: 1002, UUID: "uuid-1002", Passwd: "passwd-1002"},
				{UID: 1003, UUID: "uuid-1003", Passwd: "passwd-1003"},
			}
			switch i % 3 {
			case 0:
				if err := h.AddUsers(&users, &api.NodeInfo{NodeType: C.TypeHysteria2}); err != nil {
					errs <- err
					return
				}
			case 1:
				if err := h.RemoveUsers([]string{"u1002"}); err != nil {
					errs <- err
					return
				}
			default:
				users[0].UUID = "uuid-1001-rotated"
				if err := h.RefreshUsers(&users, &api.NodeInfo{NodeType: C.TypeHysteria2}); err != nil {
					errs <- err
					return
				}
			}
		}
	}()

	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestBuildHysteria2UsersUsesCanonicalNameAndUUIDFallback(t *testing.T) {
	names, passwords := buildHysteria2Users([]api.UserInfo{
		{UID: 1001, UUID: "uuid-1001", Passwd: "passwd-1001"},
		{UID: 1002, Passwd: "passwd-1002"},
	})
	if len(names) != 2 || len(passwords) != 2 {
		t.Fatalf("got %d names and %d passwords, want 2 each", len(names), len(passwords))
	}
	if names[0] != "u1001" || passwords[0] != "uuid-1001" {
		t.Fatalf("first user = (%q, %q), want canonical name with UUID password", names[0], passwords[0])
	}
	if names[1] != "u1002" || passwords[1] != "passwd-1002" {
		t.Fatalf("second user = (%q, %q), want canonical name with passwd fallback", names[1], passwords[1])
	}
}

func TestHysteria2PanelUserUpdatesMaintainMirror(t *testing.T) {
	h := newTestHysteria2Inbound(t, testFreeUDPPort(t), "initial.example.com")
	defer h.Close()

	users := []api.UserInfo{
		{UID: 1001, UUID: "uuid-1001", Passwd: "unused-passwd"},
		{UID: 1002, Passwd: "passwd-1002"},
	}
	if err := h.RefreshUsers(&users, &api.NodeInfo{NodeType: C.TypeHysteria2}); err != nil {
		t.Fatal(err)
	}
	assertHysteria2UserMirror(t, h, map[string]string{
		"u1001": "uuid-1001",
		"u1002": "passwd-1002",
	})

	rotated := []api.UserInfo{{UID: 1001, UUID: "uuid-1001-rotated"}}
	if err := h.AddUsers(&rotated, &api.NodeInfo{NodeType: C.TypeHysteria2}); err != nil {
		t.Fatal(err)
	}
	assertHysteria2UserMirror(t, h, map[string]string{
		"u1001": "uuid-1001-rotated",
		"u1002": "passwd-1002",
	})

	if err := h.RemoveUsers([]string{"u1002"}); err != nil {
		t.Fatal(err)
	}
	assertHysteria2UserMirror(t, h, map[string]string{"u1001": "uuid-1001-rotated"})
}

func TestHysteria2ConfigureFromPanelNodePreStart(t *testing.T) {
	initialPort := testFreeUDPPort(t)
	panelPort := testFreeUDPPort(t)
	h := newTestHysteria2Inbound(t, initialPort, "initial.example.com")
	defer h.Close()

	if err := h.ConfigureFromPanelNode(&api.NodeInfo{
		NodeType: C.TypeHysteria2,
		Port:     uint32(panelPort),
		Host:     "reload-a.example.com",
	}); err != nil {
		t.Fatal(err)
	}
	if h.options.ListenPort != uint16(panelPort) {
		t.Fatalf("listen port = %d, want %d", h.options.ListenPort, panelPort)
	}
	if h.options.TLS == nil || h.options.TLS.ServerName != "reload-a.example.com" {
		t.Fatalf("TLS options = %#v, want panel SNI", h.options.TLS)
	}
	if err := h.Start(adapter.StartStateStart); err != nil {
		t.Fatal(err)
	}
	assertUDPPortBound(t, panelPort, true)
	assertUDPPortBound(t, initialPort, false)
}

func TestHysteria2ConfigureFromPanelNodeHotReloadAndRollback(t *testing.T) {
	initialPort := testFreeUDPPort(t)
	reloadPort := testFreeUDPPort(t)
	h := newTestHysteria2Inbound(t, initialPort, "initial.example.com")
	if err := h.Start(adapter.StartStateStart); err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	oldService := h.service
	if err := h.ConfigureFromPanelNode(&api.NodeInfo{
		NodeType: C.TypeHysteria2,
		Port:     uint32(reloadPort),
		Host:     "reload-a.example.com",
	}); err != nil {
		t.Fatal(err)
	}
	if h.service == oldService {
		t.Fatal("QUIC service was not rebuilt after port/SNI reload")
	}
	if h.options.ListenPort != uint16(reloadPort) || h.options.TLS.ServerName != "reload-a.example.com" {
		t.Fatalf("effective options after reload = %#v", h.options)
	}
	assertUDPPortBound(t, reloadPort, true)
	assertUDPPortBound(t, initialPort, false)

	occupied, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	occupiedPort := occupied.LocalAddr().(*net.UDPAddr).Port
	stableService := h.service
	stableListener := h.listener.Load()
	if err := h.ConfigureFromPanelNode(&api.NodeInfo{
		NodeType: C.TypeHysteria2,
		Port:     uint32(occupiedPort),
		Host:     "reload-b.example.com",
	}); err == nil {
		t.Fatal("expected occupied-port reload to fail")
	}
	if h.service != stableService || h.listener.Load() != stableListener {
		t.Fatal("failed reload replaced live QUIC service or listener")
	}
	if h.options.ListenPort != uint16(reloadPort) || h.options.TLS.ServerName != "reload-a.example.com" {
		t.Fatalf("failed reload committed options: %#v", h.options)
	}
	assertUDPPortBound(t, reloadPort, true)
}

func TestHysteria2ConfigureFromPanelNodeSNIOnlyRebuildsService(t *testing.T) {
	port := testFreeUDPPort(t)
	h := newTestHysteria2Inbound(t, port, "initial.example.com")
	if err := h.Start(adapter.StartStateStart); err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	oldService := h.service
	if err := h.ConfigureFromPanelNode(&api.NodeInfo{
		NodeType: C.TypeHysteria2,
		Port:     uint32(port),
		Host:     "reload-a.example.com",
	}); err != nil {
		t.Fatal(err)
	}
	if h.service == oldService {
		t.Fatal("QUIC service was not rebuilt after SNI-only reload")
	}
	if h.options.TLS.ServerName != "reload-a.example.com" {
		t.Fatalf("SNI = %q, want reload-a.example.com", h.options.TLS.ServerName)
	}
	assertUDPPortBound(t, port, true)
}

func TestHysteria2ConfigureFromPanelNodeRejectsInvalidPort(t *testing.T) {
	h := newTestHysteria2Inbound(t, testFreeUDPPort(t), "initial.example.com")
	defer h.Close()
	oldOptions := h.options

	for _, port := range []uint32{0, 65536} {
		if err := h.ConfigureFromPanelNode(&api.NodeInfo{NodeType: C.TypeHysteria2, Port: port}); err == nil {
			t.Fatalf("port %d: expected validation error", port)
		}
		if h.options.ListenPort != oldOptions.ListenPort || h.options.TLS.ServerName != oldOptions.TLS.ServerName {
			t.Fatalf("port %d: invalid update changed options", port)
		}
	}
}

func TestHysteria2ConfigureFromPanelNodeNoOpPreservesComponents(t *testing.T) {
	port := testFreeUDPPort(t)
	h := newTestHysteria2Inbound(t, port, "initial.example.com")
	if err := h.Start(adapter.StartStateStart); err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	oldListener, oldTLS, oldService := h.listener.Load(), h.tlsConfig, h.service
	if err := h.ConfigureFromPanelNode(&api.NodeInfo{NodeType: C.TypeHysteria2, Port: uint32(port), Host: "initial.example.com"}); err != nil {
		t.Fatal(err)
	}
	if h.listener.Load() != oldListener || h.tlsConfig != oldTLS || h.service != oldService {
		t.Fatal("identical panel configuration rebuilt live QUIC components")
	}
}

func TestHysteria2HandlersPreserveAuthenticatedUser(t *testing.T) {
	h := newTestHysteria2Inbound(t, testFreeUDPPort(t), "initial.example.com")
	defer h.Close()
	router := &captureHysteria2Router{}
	h.router = router
	ctx := auth.ContextWithUser(context.Background(), "u40493")
	source := M.ParseSocksaddr("192.0.2.1:1234")
	destination := M.ParseSocksaddr("example.com:443")
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	h.NewConnectionEx(ctx, server, source, destination, nil)
	if router.connectionMetadata.User != "u40493" || router.connectionMetadata.Inbound != "hysteria2-in" || router.connectionMetadata.InboundType != C.TypeHysteria2 {
		t.Fatalf("TCP metadata identity = %#v", router.connectionMetadata)
	}
	if router.connectionMetadata.Source != source || router.connectionMetadata.Destination != destination {
		t.Fatalf("TCP metadata addresses = source %v destination %v", router.connectionMetadata.Source, router.connectionMetadata.Destination)
	}

	h.NewPacketConnectionEx(ctx, nil, source, destination, nil)
	if router.packetMetadata.User != "u40493" || router.packetMetadata.Inbound != "hysteria2-in" || router.packetMetadata.InboundType != C.TypeHysteria2 {
		t.Fatalf("UDP metadata identity = %#v", router.packetMetadata)
	}
	if router.packetMetadata.Source != source || router.packetMetadata.Destination != destination {
		t.Fatalf("UDP metadata addresses = source %v destination %v", router.packetMetadata.Source, router.packetMetadata.Destination)
	}
}

type captureHysteria2Router struct {
	adapter.Router
	connectionMetadata adapter.InboundContext
	packetMetadata     adapter.InboundContext
}

func (r *captureHysteria2Router) RouteConnection(context.Context, net.Conn, adapter.InboundContext) error {
	return nil
}

func (r *captureHysteria2Router) RoutePacketConnection(context.Context, N.PacketConn, adapter.InboundContext) error {
	return nil
}

func (r *captureHysteria2Router) RouteConnectionEx(_ context.Context, _ net.Conn, metadata adapter.InboundContext, _ N.CloseHandlerFunc) {
	r.connectionMetadata = metadata
}

func (r *captureHysteria2Router) RoutePacketConnectionEx(_ context.Context, _ N.PacketConn, metadata adapter.InboundContext, _ N.CloseHandlerFunc) {
	r.packetMetadata = metadata
}

func newTestHysteria2Inbound(t *testing.T, port int, serverName string) *Inbound {
	t.Helper()
	certPEM, keyPEM := testSelfSignedCertificate(t)
	listenAddr := badoption.Addr(netip.MustParseAddr("127.0.0.1"))
	inbound, err := NewInbound(context.Background(), nil, log.NewNOPFactory().Logger(), "hysteria2-in", option.Hysteria2InboundOptions{
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

func assertHysteria2UserMirror(t *testing.T, h *Inbound, want map[string]string) {
	t.Helper()
	h.usersMu.Lock()
	defer h.usersMu.Unlock()
	if len(h.currentUsers) != len(want) {
		t.Fatalf("user mirror = %#v, want %#v", h.currentUsers, want)
	}
	for name, password := range want {
		if h.currentUsers[name] != password {
			t.Fatalf("user mirror = %#v, want %#v", h.currentUsers, want)
		}
	}
}

func assertUDPPortBound(t *testing.T, port int, want bool) {
	t.Helper()
	probe, err := net.ListenPacket("udp", net.JoinHostPort("127.0.0.1", fmt.Sprint(port)))
	bound := err != nil
	if probe != nil {
		_ = probe.Close()
	}
	if bound != want {
		t.Fatalf("UDP port %d bound = %v, want %v (probe error: %v)", port, bound, want, err)
	}
}

func testFreeUDPPort(t *testing.T) int {
	t.Helper()
	packetConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer packetConn.Close()
	return packetConn.LocalAddr().(*net.UDPAddr).Port
}

func testSelfSignedCertificate(t *testing.T) (string, string) {
	t.Helper()
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			CommonName: "test.example.com",
		},
		NotBefore: time.Now().Add(-time.Hour),
		NotAfter:  time.Now().Add(time.Hour),
		DNSNames:  []string{"initial.example.com", "reload-a.example.com", "reload-b.example.com"},
		KeyUsage:  x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{
			x509.ExtKeyUsageServerAuth,
		},
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
