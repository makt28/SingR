//go:build with_gvisor

package derp

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	stdtls "crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"errors"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sagernet/sing-box/common/tls"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/json/badoption"
	"github.com/sagernet/tailscale/derp"
	"github.com/sagernet/tailscale/derp/derphttp"
	"github.com/sagernet/tailscale/derp/derpserver"
	"github.com/sagernet/tailscale/net/netmon"
	"github.com/sagernet/tailscale/types/key"
)

func TestParseMeshPeerList(t *testing.T) {
	targets, err := parseMeshPeerList([]byte(`{
		"nodes": [
			"HK1.Example.com",
			"jp1.example.com:9443",
			"203.0.113.7",
			"2001:db8::1",
			"[2001:db8::2]:9000",
			"hk1.example.com",
			"hk1.example.com."
		],
		"client_only_field": {"anything": true}
	}`), 8443)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, target := range targets {
		got = append(got, target.String())
	}
	want := []string{
		"hk1.example.com:8443",
		"jp1.example.com:9443",
		"203.0.113.7:8443",
		"[2001:db8::1]:8443",
		"[2001:db8::2]:9000",
	}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestParseMeshPeerListPins(t *testing.T) {
	pin := sha256.Sum256([]byte("peer key"))
	pinText := base64.StdEncoding.EncodeToString(pin[:])
	targets, err := parseMeshPeerList([]byte(`{"nodes": [
		{"host": "hk1.example.com", "pin": "`+pinText+`", "label": "ignored"},
		{"host": "jp1.example.com:9443"},
		"sg1.example.com",
		{"host": "hk1.example.com", "pin": "`+pinText+`"}
	]}`), 8443)
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 3 {
		t.Fatalf("want 3 targets (exact duplicate dropped), got %v", targets)
	}
	if targets[0].String() != "hk1.example.com:8443" || !bytes.Equal(targets[0].Pin, pin[:]) {
		t.Fatalf("pinned entry parsed wrong: %+v", targets[0])
	}
	if targets[1].String() != "jp1.example.com:9443" || targets[1].Pin != nil || targets[2].Pin != nil {
		t.Fatalf("unpinned entries parsed wrong: %+v", targets[1:])
	}
	if targets[0].id() == (meshTarget{Host: "hk1.example.com", Port: 8443}).id() {
		t.Fatal("a pin change must change the peer identity (forces a reconnect)")
	}
}

func TestParseMeshPeerListRejectsWholeListOnBadEntry(t *testing.T) {
	for _, content := range []string{
		`{"nodes": []}`,
		`{}`,
		`not json`,
		`{"nodes": ["ok.example.com", ""]}`,
		`{"nodes": ["ok.example.com", "https://bad.example.com"]}`,
		`{"nodes": ["ok.example.com", "bad.example.com/derp"]}`,
		`{"nodes": ["ok.example.com", "bad.example.com:0"]}`,
		`{"nodes": ["ok.example.com", "bad.example.com:70000"]}`,
		`{"nodes": ["ok.example.com", "bad.example.com:x"]}`,
		`{"nodes": ["ok.example.com", ":8443"]}`,
		`{"nodes": ["ok.example.com", "user@bad.example.com"]}`,
		`{"nodes": ["ok.example.com", "fe80::1%eth0"]}`,
		`{"nodes": ["ok.example.com", {"pin": "AAAA"}]}`,
		`{"nodes": ["ok.example.com", {"host": "a.example.com", "pin": "not base64!"}]}`,
		`{"nodes": ["ok.example.com", {"host": "a.example.com", "pin": "AAAA"}]}`,
		`{"nodes": ["ok.example.com", 42]}`,
		`{"nodes": [{"host": "a.example.com", "pin": "` + base64.StdEncoding.EncodeToString(make([]byte, 32)) + `"}, "a.example.com"]}`,
	} {
		if _, err := parseMeshPeerList([]byte(content), 8443); err == nil {
			t.Errorf("expected error for %s", content)
		}
	}
}

func TestLoopbackSource(t *testing.T) {
	for address, want := range map[string]bool{
		"127.0.0.1":        true,
		"127.0.0.53":       true,
		"::1":              true,
		"::ffff:127.0.0.1": true,
		"203.0.113.5":      false,
		"10.0.0.1":         false,
		"::ffff:10.0.0.1":  false,
		"2001:db8::1":      false,
	} {
		if got := isLoopbackSource(netip.MustParseAddr(address)); got != want {
			t.Errorf("%s: got %v, want %v", address, got, want)
		}
	}
	if isLoopbackSource(netip.Addr{}) {
		t.Error("invalid address must not be loopback")
	}
}

func TestValidateSingRDERPOptions(t *testing.T) {
	psk := strings.Repeat("ab", 32)
	valid := option.DERPServiceOptions{
		ListenOptions:    option.ListenOptions{ListenPort: 8443},
		SingRDERPOptions: option.SingRDERPOptions{VerifyClientLoopback: true, MeshWithURL: "https://example.com/derp.json"},
		MeshPSK:          psk,
	}
	if err := validateSingRDERPOptions(valid); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(*option.DERPServiceOptions){
		"loopback with verify url": func(o *option.DERPServiceOptions) {
			o.VerifyClientURL = []*option.DERPVerifyClientURLOptions{{}}
		},
		"loopback with verify endpoint": func(o *option.DERPServiceOptions) { o.VerifyClientEndpoint = []string{"ts"} },
		"http list url":                 func(o *option.DERPServiceOptions) { o.MeshWithURL = "http://example.com/derp.json" },
		"list url without host":         func(o *option.DERPServiceOptions) { o.MeshWithURL = "https:///derp.json" },
		"no psk":                        func(o *option.DERPServiceOptions) { o.MeshPSK = "" },
		"no listen port":                func(o *option.DERPServiceOptions) { o.ListenPort = 0 },
		"interval too short": func(o *option.DERPServiceOptions) {
			o.MeshWithURLInterval = badoption.Duration(time.Second)
		},
	}
	for name, mutate := range cases {
		options := valid
		mutate(&options)
		if err := validateSingRDERPOptions(options); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestMeshPeerSetApply(t *testing.T) {
	running := map[string]bool{}
	failOnce := map[string]bool{"bad.example.com:8443": true}
	set := newMeshPeerSet(func(target meshTarget) (func(), error) {
		name := target.String()
		if failOnce[name] {
			delete(failOnce, name)
			return nil, errors.New("boom")
		}
		if running[name] {
			t.Fatalf("%s started twice", name)
		}
		running[name] = true
		return func() { delete(running, name) }, nil
	})
	targets := func(names ...string) []meshTarget {
		var list []meshTarget
		for _, name := range names {
			list = append(list, meshTarget{Host: name, Port: 8443})
		}
		return list
	}

	added, removed, errs := set.apply(targets("a.example.com", "b.example.com", "bad.example.com"))
	if strings.Join(added, ",") != "a.example.com:8443,b.example.com:8443" || len(removed) != 0 || len(errs) != 1 {
		t.Fatalf("first apply: added=%v removed=%v errs=%v", added, removed, errs)
	}
	// Unchanged peers stay up; the failed one is retried; dropped ones stop.
	added, removed, errs = set.apply(targets("b.example.com", "bad.example.com", "c.example.com"))
	if strings.Join(added, ",") != "bad.example.com:8443,c.example.com:8443" || strings.Join(removed, ",") != "a.example.com:8443" || len(errs) != 0 {
		t.Fatalf("second apply: added=%v removed=%v errs=%v", added, removed, errs)
	}
	if len(running) != 3 || running["a.example.com:8443"] {
		t.Fatalf("running after second apply: %v", running)
	}
	set.closeAll()
	if len(running) != 0 {
		t.Fatalf("running after closeAll: %v", running)
	}
}

// The loopback rule runs through the real derpserver admission path:
// ordinary clients from loopback get in, the same client from a public
// address is rejected, and a mesh peer from a public address still gets in.
func TestLoopbackVerifierWithDERPServer(t *testing.T) {
	meshKey, err := key.ParseDERPMesh(strings.Repeat("cd", 32))
	if err != nil {
		t.Fatal(err)
	}
	server := derpserver.New(key.NewNode(), t.Logf)
	defer server.Close()
	server.SetMeshKey(meshKey.String())
	(&Service{singr: option.SingRDERPOptions{VerifyClientLoopback: true}}).applyLoopbackVerifier(server)

	// derpserver takes the client address from the hijacked connection, so
	// fake it on the accepted net.Conn rather than on the request.
	remoteAddr := make(chan string, 1)
	httpServer := httptest.NewUnstartedServer(derpserver.Handler(server))
	httpServer.Listener = &fakeRemoteListener{Listener: httpServer.Listener, next: remoteAddr}
	httpServer.Start()
	defer httpServer.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	connect := func(from string, mesh bool) *derphttp.Client {
		t.Helper()
		client, err := derphttp.NewClient(key.NewNode(), httpServer.URL+"/derp", t.Logf, netmon.NewStatic())
		if err != nil {
			t.Fatal(err)
		}
		if mesh {
			client.MeshKey = meshKey
			client.WatchConnectionChanges = true
		}
		remoteAddr <- from
		if err := client.Connect(ctx); err != nil {
			t.Fatal(err)
		}
		return client
	}
	recv := func(client *derphttp.Client) (derp.ReceivedMessage, error) {
		type result struct {
			message derp.ReceivedMessage
			err     error
		}
		done := make(chan result, 1)
		go func() {
			message, err := client.Recv()
			done <- result{message, err}
		}()
		select {
		case r := <-done:
			return r.message, r.err
		case <-time.After(5 * time.Second):
			t.Fatal("recv timed out")
			return nil, nil
		}
	}

	sender := connect("127.0.0.1:40001", false)
	defer sender.Close()
	receiver := connect("[::ffff:127.0.0.1]:40002", false)
	defer receiver.Close()
	// The receiver is registered asynchronously after admission; keep sending
	// until it shows up rather than racing it.
	sendCtx, stopSending := context.WithCancel(ctx)
	defer stopSending()
	go func() {
		for sendCtx.Err() == nil {
			sender.Send(receiver.SelfPublicKey(), []byte("hello"))
			time.Sleep(50 * time.Millisecond)
		}
	}()
	for {
		message, err := recv(receiver)
		if err != nil {
			t.Fatalf("loopback client was not admitted: %v", err)
		}
		if packet, ok := message.(derp.ReceivedPacket); ok {
			if string(packet.Data) != "hello" {
				t.Fatalf("unexpected packet %q", packet.Data)
			}
			stopSending()
			break
		}
	}

	outsider := connect("203.0.113.5:40003", false)
	defer outsider.Close()
	if message, err := recv(outsider); err == nil {
		t.Fatalf("public client was admitted: %T %+v", message, message)
	}

	meshPeer := connect("203.0.113.6:40004", true)
	defer meshPeer.Close()
	for {
		message, err := recv(meshPeer)
		if err != nil {
			t.Fatalf("mesh peer was not admitted: %v", err)
		}
		if _, ok := message.(derp.PeerPresentMessage); ok {
			break
		}
	}
}

type fakeRemoteListener struct {
	net.Listener
	next chan string
}

func (l *fakeRemoteListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	select {
	case address := <-l.next:
		return &fakeRemoteConn{Conn: conn, remote: net.TCPAddrFromAddrPort(netip.MustParseAddrPort(address))}, nil
	default:
		return conn, nil
	}
}

type fakeRemoteConn struct {
	net.Conn
	remote net.Addr
}

func (c *fakeRemoteConn) RemoteAddr() net.Addr {
	return c.remote
}

// A restart while the list host is down must rebuild the mesh from the
// cached list, and a failing fetch must not drop those peers.
func TestMeshURLManagerStartsFromCacheWhenListUnreachable(t *testing.T) {
	cachePath := t.TempDir() + "/" + meshPeerCacheName
	if err := os.WriteFile(cachePath, []byte(`{"nodes": ["a.example.com", "b.example.com:9443"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	var access sync.Mutex
	running := map[string]bool{}
	ctx, cancel := context.WithCancel(context.Background())
	manager := &meshURLManager{
		ctx:         ctx,
		cancel:      cancel,
		logger:      log.NewNOPFactory().Logger(),
		url:         "https://127.0.0.1:1/unreachable.json",
		interval:    time.Hour,
		defaultPort: 8443,
		cachePath:   cachePath,
		httpClient:  &http.Client{Timeout: time.Second},
		done:        make(chan struct{}),
	}
	manager.peers = newMeshPeerSet(func(target meshTarget) (func(), error) {
		access.Lock()
		defer access.Unlock()
		running[target.String()] = true
		return func() {
			access.Lock()
			defer access.Unlock()
			delete(running, target.String())
		}, nil
	})
	manager.start()
	time.Sleep(200 * time.Millisecond) // let the first (failing) fetch run
	access.Lock()
	got := len(running) == 2 && running["a.example.com:8443"] && running["b.example.com:9443"]
	access.Unlock()
	if !got {
		t.Fatalf("peers from cache not running: %v", running)
	}
	manager.Close()
	if len(running) != 0 {
		t.Fatalf("peers left after Close: %v", running)
	}
}

// The mesh client sends our PSK right after the handshake, so the handshake
// itself must authenticate the peer: by pin when the list gives one, by the
// system roots otherwise — and never not at all.
func TestMeshTLSOptionsVerifyPeer(t *testing.T) {
	serverKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "derp"},
		DNSNames:     []string{"derp"},
		NotBefore:    time.Now().Add(-time.Hour),
		// Expired on purpose: a pinned peer is identified by its key alone.
		NotAfter: time.Now().Add(-time.Minute),
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &serverKey.PublicKey, serverKey)
	if err != nil {
		t.Fatal(err)
	}
	spki, _ := x509.MarshalPKIXPublicKey(&serverKey.PublicKey)
	goodPin := sha256.Sum256(spki)
	wrongPin := sha256.Sum256([]byte("someone else"))

	listener, err := stdtls.Listen("tcp", "127.0.0.1:0", &stdtls.Config{
		Certificates: []stdtls.Certificate{{Certificate: [][]byte{der}, PrivateKey: serverKey}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			go func() {
				conn.(*stdtls.Conn).Handshake()
				conn.Close()
			}()
		}
	}()

	handshake := func(target meshTarget) error {
		config, err := tls.NewClient(context.Background(), log.NewNOPFactory().Logger(), target.Host, meshTLSOptions(target))
		if err != nil {
			return err
		}
		stdConfig, err := config.STDConfig()
		if err != nil {
			return err
		}
		conn, err := net.Dial("tcp", listener.Addr().String())
		if err != nil {
			return err
		}
		defer conn.Close()
		return stdtls.Client(conn, stdConfig).Handshake()
	}
	host := "hk1.example.com"
	if err := handshake(meshTarget{Host: host, Port: 8443, Pin: goodPin[:]}); err != nil {
		t.Fatalf("matching pin rejected: %v", err)
	}
	if err := handshake(meshTarget{Host: host, Port: 8443, Pin: wrongPin[:]}); err == nil {
		t.Fatal("wrong pin accepted (a man in the middle would get the PSK)")
	}
	if err := handshake(meshTarget{Host: host, Port: 8443}); err == nil {
		t.Fatal("unpinned self-signed peer accepted (must fall back to CA verification, not to none)")
	}
}
