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
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"slices"
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
	valid := option.DERPServiceOptions{
		ListenOptions: option.ListenOptions{ListenPort: 8443},
		SingRDERPOptions: option.SingRDERPOptions{
			VerifyClientLoopback: true,
			ControlURL:           "https://hs.example.com",
			ControlTokenPath:     "/etc/singr/derp-control.token",
			ProbeListenPort:      8444,
		},
		InboundTLSOptionsContainer: option.InboundTLSOptionsContainer{
			TLS: &option.InboundTLSOptions{CertificatePath: "/etc/singr/derp.crt"},
		},
	}
	if err := validateSingRDERPOptions(valid); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(*option.DERPServiceOptions){
		"loopback with verify url": func(o *option.DERPServiceOptions) {
			o.VerifyClientURL = []*option.DERPVerifyClientURLOptions{{}}
		},
		"loopback with verify endpoint": func(o *option.DERPServiceOptions) { o.VerifyClientEndpoint = []string{"ts"} },
		"http control url":              func(o *option.DERPServiceOptions) { o.ControlURL = "http://hs.example.com" },
		"control url without host":      func(o *option.DERPServiceOptions) { o.ControlURL = "https:///x" },
		"no token":                      func(o *option.DERPServiceOptions) { o.ControlTokenPath = "" },
		"psk next to control":           func(o *option.DERPServiceOptions) { o.MeshPSK = strings.Repeat("ab", 32) },
		"psk file next to control":      func(o *option.DERPServiceOptions) { o.MeshPSKFile = "/x" },
		"no listen port":                func(o *option.DERPServiceOptions) { o.ListenPort = 0 },
		"no tls":                        func(o *option.DERPServiceOptions) { o.TLS = nil },
		"acme instead of fixed cert": func(o *option.DERPServiceOptions) {
			o.TLS = &option.InboundTLSOptions{}
		},
		"probe on derp port": func(o *option.DERPServiceOptions) { o.ProbeListenPort = 8443 },
		"interval too short": func(o *option.DERPServiceOptions) {
			o.ControlInterval = badoption.Duration(time.Second)
		},
	}
	for name, mutate := range cases {
		options := valid
		tlsCopy := *valid.TLS
		options.TLS = &tlsCopy
		mutate(&options)
		if err := validateSingRDERPOptions(options); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
	// Without a control server only the loopback/probe checks apply.
	if err := validateSingRDERPOptions(option.DERPServiceOptions{ListenOptions: option.ListenOptions{ListenPort: 8443}}); err != nil {
		t.Fatal(err)
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

// The pin we register must be what peers' certificate_public_key_sha256
// check compares against.
func TestCertificatePin(t *testing.T) {
	certificate, key := selfSignedPEM(t)
	pin, err := certificatePin(certificate)
	if err != nil {
		t.Fatal(err)
	}
	spki, _ := x509.MarshalPKIXPublicKey(&key.PublicKey)
	sum := sha256.Sum256(spki)
	if pin != base64.StdEncoding.EncodeToString(sum[:]) {
		t.Fatalf("pin %s does not match the public key", pin)
	}
	if _, err := certificatePin([]byte("not pem")); err == nil {
		t.Fatal("garbage accepted")
	}
}

func selfSignedPEM(t *testing.T) ([]byte, *ecdsa.PrivateKey) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "derp"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), key
}

// fakeControl is the Linbei headscale fork's registry, just enough of it.
type fakeControl struct {
	access     sync.Mutex
	failing    bool
	registered []map[string]any
	response   string
}

func (f *fakeControl) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.access.Lock()
	defer f.access.Unlock()
	if r.Header.Get("Authorization") != "Bearer node-token" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if f.failing {
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/linbei/derp/register":
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		f.registered = append(f.registered, body)
		w.Write([]byte(`{}`))
	case r.Method == http.MethodGet && r.URL.Path == "/linbei/derp/mesh":
		w.Write([]byte(f.response))
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

type runningPeers struct {
	access sync.Mutex
	set    map[string]bool
}

func (r *runningPeers) start(_ context.Context, target meshTarget) (func(), error) {
	r.access.Lock()
	defer r.access.Unlock()
	r.set[target.String()] = true
	return func() {
		r.access.Lock()
		defer r.access.Unlock()
		delete(r.set, target.String())
	}, nil
}

func (r *runningPeers) snapshot() []string {
	r.access.Lock()
	defer r.access.Unlock()
	var out []string
	for k := range r.set {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

func newTestControl(t *testing.T, baseURL string, client *http.Client, cachePath string, running *runningPeers) *meshControl {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	control := &meshControl{
		ctx:         ctx,
		cancel:      cancel,
		logger:      log.NewNOPFactory().Logger(),
		baseURL:     baseURL,
		token:       "node-token",
		advertise:   "hk1.example.com",
		pin:         "PIN",
		listenPort:  8443,
		interval:    time.Hour,
		cachePath:   cachePath,
		httpClient:  client,
		done:        make(chan struct{}),
		startPeerFn: running.start,
	}
	control.peers = newMeshPeerSet(func(target meshTarget) (func(), error) {
		return control.startPeerFn(ctx, target)
	})
	return control
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for ", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Register, fetch the PSK and peers, cache them; a failing control server
// keeps the current peers; peers that leave the registry are dropped.
func TestMeshControlRegistersAndFollowsRegistry(t *testing.T) {
	psk := strings.Repeat("ab", 32)
	control := &fakeControl{response: `{"psk":"` + psk + `","nodes":[{"host":"a.example.com:8443","pin":"` +
		base64.StdEncoding.EncodeToString(make([]byte, 32)) + `"},{"host":"b.example.com:9443"}]}`}
	server := httptest.NewTLSServer(control)
	defer server.Close()
	cachePath := t.TempDir() + "/" + controlCacheName
	running := &runningPeers{set: map[string]bool{}}
	m := newTestControl(t, server.URL, server.Client(), cachePath, running)

	if got := m.initialPSK(); got != psk {
		t.Fatalf("initial PSK %q", got)
	}
	info, err := os.Stat(cachePath)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("cache must exist with 0600 (holds the PSK): %v %v", info, err)
	}
	m.start(true)
	defer m.Close()
	waitFor(t, "peers", func() bool { return len(running.snapshot()) == 2 })
	waitFor(t, "registration", func() bool {
		control.access.Lock()
		defer control.access.Unlock()
		return len(control.registered) == 1
	})

	control.access.Lock()
	registered := control.registered
	control.access.Unlock()
	if len(registered) != 1 || registered[0]["host"] != "hk1.example.com" ||
		registered[0]["port"] != float64(8443) || registered[0]["pin"] != "PIN" {
		t.Fatalf("register body: %v", registered)
	}

	control.access.Lock()
	control.failing = true
	control.access.Unlock()
	m.refresh()
	if got := running.snapshot(); len(got) != 2 {
		t.Fatalf("failing control server dropped peers: %v", got)
	}

	control.access.Lock()
	control.failing = false
	control.response = `{"psk":"` + psk + `","nodes":["b.example.com:9443"]}`
	control.access.Unlock()
	m.refresh()
	if got := running.snapshot(); !slices.Equal(got, []string{"b.example.com:9443"}) {
		t.Fatalf("after a node left: %v", got)
	}
	cached, _ := os.ReadFile(cachePath)
	if !strings.Contains(string(cached), `"nodes":["b.example.com:9443"]`) {
		t.Fatalf("cache not updated: %s", cached)
	}
}

// A restart while the control server is down rebuilds the mesh from the
// cache, PSK included.
func TestMeshControlStartsFromCacheWhenControlUnreachable(t *testing.T) {
	psk := strings.Repeat("cd", 32)
	cachePath := t.TempDir() + "/" + controlCacheName
	if err := os.WriteFile(cachePath, []byte(`{"psk":"`+psk+`","nodes":["a.example.com","b.example.com:9443"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	running := &runningPeers{set: map[string]bool{}}
	m := newTestControl(t, "https://127.0.0.1:1", &http.Client{Timeout: time.Second}, cachePath, running)
	if got := m.initialPSK(); got != psk {
		t.Fatalf("PSK from cache %q", got)
	}
	m.start(true)
	time.Sleep(200 * time.Millisecond) // let the first (failing) refresh run
	if got := running.snapshot(); !slices.Equal(got, []string{"a.example.com:8443", "b.example.com:9443"}) {
		t.Fatalf("peers from cache: %v", got)
	}
	m.Close()
	if got := running.snapshot(); len(got) != 0 {
		t.Fatalf("peers left after Close: %v", got)
	}
}

// Without a mesh key the node still registers but starts no peers.
func TestMeshControlWithoutMeshKeyStartsNoPeers(t *testing.T) {
	control := &fakeControl{response: `{"psk":"` + strings.Repeat("ab", 32) + `","nodes":["a.example.com"]}`}
	server := httptest.NewTLSServer(control)
	defer server.Close()
	running := &runningPeers{set: map[string]bool{}}
	m := newTestControl(t, server.URL, server.Client(), t.TempDir()+"/"+controlCacheName, running)
	m.start(false)
	waitFor(t, "registration", func() bool {
		control.access.Lock()
		defer control.access.Unlock()
		return len(control.registered) == 1
	})
	m.Close()
	if got := running.snapshot(); len(got) != 0 {
		t.Fatalf("peers started without a mesh key: %v", got)
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
