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
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sagernet/sing-box/common/tls"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/logger"
	"github.com/sagernet/tailscale/derp"
	"github.com/sagernet/tailscale/derp/derphttp"
	"github.com/sagernet/tailscale/derp/derpserver"
	"github.com/sagernet/tailscale/net/netmon"
	"github.com/sagernet/tailscale/types/key"
)

func TestMeshTargets(t *testing.T) {
	pin := func(seed byte) string { return base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{seed}, 32)) }
	nodes := []registryNode{
		{Pin: pin(1), Port: 8443, IPv4: "203.0.113.1", IPv6: "2a01:4f8::1"}, // self
		{Pin: pin(2), Port: 8443, IPv4: "203.0.113.2", IPv6: "2a01:4f8::2"}, // dual stack
		{Pin: pin(3), Port: 9443, IPv6: "2a01:4f8::3"},                      // IPv6 only
		{Pin: pin(4), Port: 8443, IPv4: "203.0.113.4"},                      // IPv4 only
	}
	hosts := func(targets []meshTarget) []string {
		var out []string
		for _, target := range targets {
			out = append(out, target.String())
		}
		return out
	}
	v4 := netip.MustParseAddr("203.0.113.1")
	v6 := netip.MustParseAddr("2a01:4f8::1")

	// Dual stack: IPv4 when both have it, IPv6 for the IPv6-only peer.
	targets, unreachable, err := meshTargets(nodes, pin(1), [2]netip.Addr{v4, v6})
	if err != nil {
		t.Fatal(err)
	}
	if got := hosts(targets); !slices.Equal(got, []string{"203.0.113.2:8443", "[2a01:4f8::3]:9443", "203.0.113.4:8443"}) || len(unreachable) != 0 {
		t.Fatalf("dual stack: %v unreachable=%v", got, unreachable)
	}
	if !bytes.Equal(targets[0].Pin, bytes.Repeat([]byte{2}, 32)) {
		t.Fatal("pin not carried")
	}

	// IPv6-only self: the IPv4-only peer is unreachable, the others over IPv6.
	targets, unreachable, _ = meshTargets(nodes, pin(1), [2]netip.Addr{{}, v6})
	if got := hosts(targets); !slices.Equal(got, []string{"[2a01:4f8::2]:8443", "[2a01:4f8::3]:9443"}) || !slices.Equal(unreachable, []string{pin(4)}) {
		t.Fatalf("v6-only self: %v unreachable=%v", got, unreachable)
	}

	// IPv4-only self.
	targets, unreachable, _ = meshTargets(nodes, pin(1), [2]netip.Addr{v4, {}})
	if got := hosts(targets); !slices.Equal(got, []string{"203.0.113.2:8443", "203.0.113.4:8443"}) || !slices.Equal(unreachable, []string{pin(3)}) {
		t.Fatalf("v4-only self: %v unreachable=%v", got, unreachable)
	}

	// Any bad entry rejects the whole list (keeps the current peers).
	for name, bad := range map[string]registryNode{
		"bad pin":       {Pin: "AAAA", Port: 8443, IPv4: "203.0.113.9"},
		"bad port":      {Pin: pin(9), Port: 0, IPv4: "203.0.113.9"},
		"v6 in v4 slot": {Pin: pin(9), Port: 8443, IPv4: "2a01:4f8::9"},
		"garbage":       {Pin: pin(9), Port: 8443, IPv6: "not-an-ip"},
	} {
		if _, _, err := meshTargets(append(slices.Clone(nodes), bad), pin(1), [2]netip.Addr{v4, v6}); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestParseRegistryResponse(t *testing.T) {
	psk := strings.Repeat("ab", 32)
	good := `{"status":"success","code":"OK","psk":"` + psk + `","nodes":[{"pin":"x","port":1}],"interval":60}`
	if _, err := parseRegistryResponse([]byte(good)); err != nil {
		t.Fatal(err)
	}
	for name, bad := range map[string]string{
		"error status": `{"status":"error","code":"DERP_UNAUTHORIZED","message":"节点 token 不对"}`,
		"no psk":       `{"status":"success","nodes":[{"pin":"x"}]}`,
		"empty list":   `{"status":"success","psk":"` + psk + `","nodes":[]}`,
		"not json":     `<html>`,
	} {
		if _, err := parseRegistryResponse([]byte(bad)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestIsPublicAddress(t *testing.T) {
	for text, want := range map[string]bool{
		"203.0.113.10": true, "8.8.8.8": true, "2a01:4f8::1": true, "2606:4700::1111": true,
		"10.0.0.1": false, "192.168.1.1": false, "172.16.0.1": false, "100.64.1.1": false, "127.0.0.1": false,
		"169.254.1.1": false, "fd00::1": false, "fe80::1": false, "::1": false, "2001:db8::1": false,
	} {
		if got := isPublicAddress(netip.MustParseAddr(text)); got != want {
			t.Errorf("%s: got %v", text, got)
		}
	}
}

// What we report, per family: pinned wins; "none" reports nothing; auto uses
// the kernel's source address when public, and behind NAT asks ip.sb once,
// retrying every publicIPRetryInterval until it answers.
func TestOwnAddresses(t *testing.T) {
	log := logger.NOP()
	ctx := context.Background()
	local := [2]netip.Addr{netip.MustParseAddr("10.0.0.5"), netip.MustParseAddr("2a01:4f8::5")}
	lookups := 0
	lookupErr := errors.New("ip.sb down")
	addresses := ownAddresses{
		local: func(family int) netip.Addr { return local[family] },
		lookup: func(_ context.Context, family int) (netip.Addr, error) {
			lookups++
			if lookupErr != nil {
				return netip.Addr{}, lookupErr
			}
			return netip.MustParseAddr("203.0.113.5"), nil
		},
	}
	now := time.Unix(1_800_000_000, 0)

	// IPv4 behind NAT, ip.sb down: no IPv4 yet; IPv6 public on the NIC.
	if !addresses.refresh(ctx, now, log) || addresses.current[0].IsValid() || addresses.current[1] != local[1] || lookups != 1 {
		t.Fatalf("first: %v lookups=%d", addresses.current, lookups)
	}
	// No retry before the interval.
	addresses.refresh(ctx, now.Add(5*time.Second), log)
	if lookups != 1 {
		t.Fatalf("retried too early: %d", lookups)
	}
	// Retry after the interval succeeds.
	lookupErr = nil
	if !addresses.refresh(ctx, now.Add(publicIPRetryInterval), log) || addresses.current[0] != netip.MustParseAddr("203.0.113.5") || lookups != 2 {
		t.Fatalf("retry: %v lookups=%d", addresses.current, lookups)
	}
	// Answered once: never asked again; nothing changed.
	if addresses.refresh(ctx, now.Add(10*publicIPRetryInterval), log) || lookups != 2 {
		t.Fatalf("asked again: %d", lookups)
	}
	// The NIC's IPv6 changes: noticed on the next check.
	local[1] = netip.MustParseAddr("2a01:4f8::6")
	if !addresses.refresh(ctx, now.Add(11*publicIPRetryInterval), log) || addresses.current[1] != local[1] {
		t.Fatalf("v6 change missed: %v", addresses.current)
	}
	// The NIC gains a public IPv4: it wins over the ip.sb answer.
	local[0] = netip.MustParseAddr("198.51.100.7")
	addresses.refresh(ctx, now.Add(12*publicIPRetryInterval), log)
	if addresses.current[0] != local[0] {
		t.Fatalf("public NIC address not preferred: %v", addresses.current)
	}
	// Pinned and "none".
	addresses.pinned = [2]string{"192.0.2.44", advertiseNone}
	addresses.refresh(ctx, now.Add(13*publicIPRetryInterval), log)
	if addresses.current[0] != netip.MustParseAddr("192.0.2.44") || addresses.current[1].IsValid() {
		t.Fatalf("pinned/none: %v", addresses.current)
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
			RegistryURL:          "https://api.example.com/tailnet.php",
			RegistryTokenPath:    "/etc/singr/derp/registry.token",
			ProbeListenPort:      8444,
		},
		InboundTLSOptionsContainer: option.InboundTLSOptionsContainer{
			TLS: &option.InboundTLSOptions{CertificatePath: "/etc/singr/derp.crt"},
		},
	}
	if err := validateSingRDERPOptions(valid); err != nil {
		t.Fatal(err)
	}
	pinned := valid
	pinned.AdvertiseIPv4, pinned.AdvertiseIPv6 = "203.0.113.5", advertiseNone
	if err := validateSingRDERPOptions(pinned); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(*option.DERPServiceOptions){
		"loopback with verify url": func(o *option.DERPServiceOptions) {
			o.VerifyClientURL = []*option.DERPVerifyClientURLOptions{{}}
		},
		"loopback with verify endpoint": func(o *option.DERPServiceOptions) { o.VerifyClientEndpoint = []string{"ts"} },
		"http registry":                 func(o *option.DERPServiceOptions) { o.RegistryURL = "http://api.example.com/tailnet.php" },
		"registry without host":         func(o *option.DERPServiceOptions) { o.RegistryURL = "https:///x" },
		"no token":                      func(o *option.DERPServiceOptions) { o.RegistryTokenPath = "" },
		"psk next to registry":          func(o *option.DERPServiceOptions) { o.MeshPSK = strings.Repeat("ab", 32) },
		"psk file next to registry":     func(o *option.DERPServiceOptions) { o.MeshPSKFile = "/x" },
		"no listen port":                func(o *option.DERPServiceOptions) { o.ListenPort = 0 },
		"no tls":                        func(o *option.DERPServiceOptions) { o.TLS = nil },
		"acme instead of fixed cert": func(o *option.DERPServiceOptions) {
			o.TLS = &option.InboundTLSOptions{}
		},
		"probe on derp port": func(o *option.DERPServiceOptions) { o.ProbeListenPort = 8443 },
		"v6 in ipv4":         func(o *option.DERPServiceOptions) { o.AdvertiseIPv4 = "2a01:4f8::1" },
		"v4 in ipv6":         func(o *option.DERPServiceOptions) { o.AdvertiseIPv6 = "203.0.113.5" },
		"garbage ipv4":       func(o *option.DERPServiceOptions) { o.AdvertiseIPv4 = "example.com" },
		"both none":          func(o *option.DERPServiceOptions) { o.AdvertiseIPv4, o.AdvertiseIPv6 = advertiseNone, advertiseNone },
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
	// Without a registry only the loopback/probe checks apply.
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

// fakeRegistry is sspapi's tailnet.php, just enough of it.
type fakeRegistry struct {
	access   sync.Mutex
	failing  bool
	syncs    []url.Values
	psk      string
	nodes    []registryNode
	interval int
}

func (f *fakeRegistry) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.access.Lock()
	defer f.access.Unlock()
	r.ParseForm()
	if r.PostForm.Get("node_token") != "node-token" {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"status":"error","code":"DERP_UNAUTHORIZED","message":"bad token"}`))
		return
	}
	if f.failing {
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	if r.PostForm.Get("action") != "derp_sync" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	f.syncs = append(f.syncs, r.PostForm)
	json.NewEncoder(w).Encode(map[string]any{
		"status": "success", "code": "OK", "psk": f.psk, "nodes": f.nodes, "interval": f.interval,
	})
}

func (f *fakeRegistry) syncCount() int {
	f.access.Lock()
	defer f.access.Unlock()
	return len(f.syncs)
}

func (f *fakeRegistry) lastSync() url.Values {
	f.access.Lock()
	defer f.access.Unlock()
	return f.syncs[len(f.syncs)-1]
}

func (f *fakeRegistry) set(fn func(*fakeRegistry)) {
	f.access.Lock()
	defer f.access.Unlock()
	fn(f)
}

type runningPeers struct {
	access    sync.Mutex
	set       map[string]bool
	onFailure func()
}

func (r *runningPeers) start(_ context.Context, target meshTarget, onFailure func()) (func(), error) {
	r.access.Lock()
	defer r.access.Unlock()
	r.set[target.String()] = true
	r.onFailure = onFailure
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

func testPin(seed byte) string {
	return base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{seed}, 32))
}

// shortTimings speeds the control loop up for a test.
func shortTimings(t *testing.T) {
	t.Helper()
	saved := [5]time.Duration{addressCheckInterval, addressChangeDebounce, peerFailureResync, defaultSyncInterval, minSyncInterval}
	addressCheckInterval, addressChangeDebounce, peerFailureResync = 20*time.Millisecond, 20*time.Millisecond, 50*time.Millisecond
	defaultSyncInterval, minSyncInterval = time.Hour, time.Hour
	t.Cleanup(func() {
		addressCheckInterval, addressChangeDebounce, peerFailureResync, defaultSyncInterval, minSyncInterval =
			saved[0], saved[1], saved[2], saved[3], saved[4]
	})
}

// testAddresses are what the kernel reports in a test; tests change them.
type testAddresses struct {
	access sync.Mutex
	local  [2]netip.Addr
}

func (a *testAddresses) get(family int) netip.Addr {
	a.access.Lock()
	defer a.access.Unlock()
	return a.local[family]
}

func (a *testAddresses) setV4(text string) {
	a.access.Lock()
	defer a.access.Unlock()
	a.local[0] = netip.MustParseAddr(text)
}

func newTestControl(t *testing.T, registryURL string, client *http.Client, cachePath string, running *runningPeers, local *testAddresses) *meshControl {
	t.Helper()
	m := newMeshControl(logger.NOP(), registryURL, "node-token", testPin(1), 8443, cachePath, [2]string{})
	m.httpClient = client
	m.startPeerFn = running.start
	m.addresses.local = local.get
	m.addresses.lookup = func(context.Context, int) (netip.Addr, error) { return netip.Addr{}, errors.New("no lookup in tests") }
	return m
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

// Report port, pin and own addresses; follow the registry; a failing registry
// keeps the current peers; peers that leave are dropped; the cache (0600,
// holds the PSK) follows the registry.
func TestMeshControlReportsAndFollowsRegistry(t *testing.T) {
	shortTimings(t)
	psk := strings.Repeat("ab", 32)
	registry := &fakeRegistry{psk: psk, interval: 3600, nodes: []registryNode{
		{Pin: testPin(1), Port: 8443, IPv4: "203.0.113.1"},
		{Pin: testPin(2), Port: 8443, IPv4: "203.0.113.2", IPv6: "2a01:4f8::2"},
		{Pin: testPin(3), Port: 9443, IPv6: "2a01:4f8::3"},
	}}
	server := httptest.NewTLSServer(registry)
	defer server.Close()
	cachePath := t.TempDir() + "/" + registryCacheName
	running := &runningPeers{set: map[string]bool{}}
	local := &testAddresses{local: [2]netip.Addr{netip.MustParseAddr("203.0.113.1"), netip.MustParseAddr("2a01:4f8::1")}}
	m := newTestControl(t, server.URL, server.Client(), cachePath, running, local)

	if got := m.initialPSK(); got != psk {
		t.Fatalf("initial PSK %q", got)
	}
	info, err := os.Stat(cachePath)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("cache must exist with 0600 (holds the PSK): %v %v", info, err)
	}
	first := registry.lastSync()
	if first.Get("port") != "8443" || first.Get("pin") != testPin(1) || first.Get("ipv4") != "203.0.113.1" || first.Get("ipv6") != "2a01:4f8::1" {
		t.Fatalf("sync form: %v", first)
	}

	m.start(true)
	defer m.Close()
	waitFor(t, "peers", func() bool {
		return slices.Equal(running.snapshot(), []string{"203.0.113.2:8443", "[2a01:4f8::3]:9443"})
	})

	// Syncs only ever run on the control loop; a peer failure asks it for one.
	resync := func() {
		time.Sleep(peerFailureResync)
		m.notePeerFailure()
	}

	registry.set(func(f *fakeRegistry) { f.failing = true })
	resync()
	time.Sleep(100 * time.Millisecond)
	if got := running.snapshot(); len(got) != 2 {
		t.Fatalf("failing registry dropped peers: %v", got)
	}

	registry.set(func(f *fakeRegistry) { f.failing = false; f.nodes = f.nodes[:2] })
	resync()
	waitFor(t, "node removal", func() bool { return slices.Equal(running.snapshot(), []string{"203.0.113.2:8443"}) })
	cached, _ := os.ReadFile(cachePath)
	if strings.Contains(string(cached), "2a01:4f8::3") {
		t.Fatalf("cache not updated: %s", cached)
	}
}

// Our address changes: reported right away (after the debounce), not on the
// next minute; a peer that fails triggers an early refetch, throttled.
func TestMeshControlReactsToAddressChangeAndPeerFailure(t *testing.T) {
	shortTimings(t)
	registry := &fakeRegistry{psk: strings.Repeat("ab", 32), interval: 3600, nodes: []registryNode{
		{Pin: testPin(1), Port: 8443, IPv4: "203.0.113.1"},
		{Pin: testPin(2), Port: 8443, IPv4: "203.0.113.2"},
	}}
	server := httptest.NewTLSServer(registry)
	defer server.Close()
	running := &runningPeers{set: map[string]bool{}}
	local := &testAddresses{local: [2]netip.Addr{netip.MustParseAddr("203.0.113.1")}}
	m := newTestControl(t, server.URL, server.Client(), t.TempDir()+"/"+registryCacheName, running, local)
	m.initialPSK()
	m.start(true)
	defer m.Close()
	waitFor(t, "first sync", func() bool { return registry.syncCount() >= 2 })

	local.setV4("203.0.113.99")
	waitFor(t, "address change reported", func() bool {
		return registry.syncCount() >= 3 && registry.lastSync().Get("ipv4") == "203.0.113.99"
	})

	// The peer moved: its connection fails, we refetch and follow it.
	registry.set(func(f *fakeRegistry) { f.nodes[1].IPv4 = "203.0.113.22" })
	before := registry.syncCount()
	running.access.Lock()
	onFailure := running.onFailure
	running.access.Unlock()
	onFailure()
	waitFor(t, "refetch after peer failure", func() bool {
		return slices.Equal(running.snapshot(), []string{"203.0.113.22:8443"})
	})
	// Throttled: a burst of failures right after does not hammer the registry.
	for range 20 {
		onFailure()
	}
	time.Sleep(20 * time.Millisecond)
	if n := registry.syncCount() - before; n > 2 {
		t.Fatalf("peer failures not throttled: %d syncs", n)
	}
}

// A restart while the registry is down rebuilds the mesh from the cache, PSK
// included.
func TestMeshControlStartsFromCacheWhenRegistryUnreachable(t *testing.T) {
	shortTimings(t)
	psk := strings.Repeat("cd", 32)
	cachePath := t.TempDir() + "/" + registryCacheName
	cache := `{"status":"success","code":"OK","psk":"` + psk + `","nodes":[` +
		`{"pin":"` + testPin(1) + `","port":8443,"ipv4":"203.0.113.1","ipv6":""},` +
		`{"pin":"` + testPin(2) + `","port":9443,"ipv4":"203.0.113.2","ipv6":""}],"interval":60}`
	if err := os.WriteFile(cachePath, []byte(cache), 0o600); err != nil {
		t.Fatal(err)
	}
	running := &runningPeers{set: map[string]bool{}}
	local := &testAddresses{local: [2]netip.Addr{netip.MustParseAddr("203.0.113.1")}}
	m := newTestControl(t, "https://127.0.0.1:1/tailnet.php", &http.Client{Timeout: time.Second}, cachePath, running, local)
	if got := m.initialPSK(); got != psk {
		t.Fatalf("PSK from cache %q", got)
	}
	m.start(true)
	time.Sleep(100 * time.Millisecond) // let the first (failing) sync run
	if got := running.snapshot(); !slices.Equal(got, []string{"203.0.113.2:9443"}) {
		t.Fatalf("peers from cache: %v", got)
	}
	m.Close()
	if got := running.snapshot(); len(got) != 0 {
		t.Fatalf("peers left after Close: %v", got)
	}
}

// Without a mesh key the node still reports but starts no peers.
func TestMeshControlWithoutMeshKeyStartsNoPeers(t *testing.T) {
	shortTimings(t)
	registry := &fakeRegistry{psk: strings.Repeat("ab", 32), interval: 3600, nodes: []registryNode{
		{Pin: testPin(2), Port: 8443, IPv4: "203.0.113.2"},
	}}
	server := httptest.NewTLSServer(registry)
	defer server.Close()
	running := &runningPeers{set: map[string]bool{}}
	local := &testAddresses{local: [2]netip.Addr{netip.MustParseAddr("203.0.113.1")}}
	m := newTestControl(t, server.URL, server.Client(), t.TempDir()+"/"+registryCacheName, running, local)
	m.start(false)
	waitFor(t, "report", func() bool { return registry.syncCount() == 1 })
	m.Close()
	if got := running.snapshot(); len(got) != 0 {
		t.Fatalf("peers started without a mesh key: %v", got)
	}
}

// Nothing public to report (NAT and ip.sb unreachable): no request with an
// empty address set is sent.
func TestMeshControlWithoutAddressDoesNotReport(t *testing.T) {
	shortTimings(t)
	registry := &fakeRegistry{psk: strings.Repeat("ab", 32)}
	server := httptest.NewTLSServer(registry)
	defer server.Close()
	running := &runningPeers{set: map[string]bool{}}
	local := &testAddresses{local: [2]netip.Addr{netip.MustParseAddr("10.0.0.5")}}
	m := newTestControl(t, server.URL, server.Client(), t.TempDir()+"/"+registryCacheName, running, local)
	if got := m.initialPSK(); got != "" {
		t.Fatalf("got a PSK without reporting: %q", got)
	}
	if registry.syncCount() != 0 {
		t.Fatal("sent a sync without an address")
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
