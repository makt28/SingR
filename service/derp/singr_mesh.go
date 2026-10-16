//go:build with_gvisor

package derp

// SingR-only additions to the DERP service (not in upstream):
//
//   - verify_client_loopback: ordinary clients are admitted only from the
//     loopback address (mesh peers bypass verification upstream, see
//     derpserver.Server.verifyClient).
//   - control_url: the node registers with the Linbei headscale fork and gets
//     the mesh PSK plus every other relay from it; peers are added and
//     removed in place while running.
//   - probe_listen_port: a loopback plain-HTTP 204 that clients probe through
//     each proxy node to find the ones that have a relay.
//
// Hooks in service.go are kept to a few lines so a core resync stays easy.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/common/dialer"
	"github.com/sagernet/sing-box/common/tls"
	"github.com/sagernet/sing-box/option"
	E "github.com/sagernet/sing/common/exceptions"
	"github.com/sagernet/sing/common/logger"
	M "github.com/sagernet/sing/common/metadata"
	"github.com/sagernet/tailscale/derp"
	"github.com/sagernet/tailscale/derp/derphttp"
	"github.com/sagernet/tailscale/derp/derpserver"
	"github.com/sagernet/tailscale/net/netmon"
	"github.com/sagernet/tailscale/tailcfg"
)

const (
	defaultControlInterval   = 5 * time.Minute
	minControlInterval       = 30 * time.Second
	controlMaxResponseBytes  = 1 << 20
	controlRequestTimeout    = 20 * time.Second
	controlCacheName         = "mesh-control.cache.json"
	meshPeerErrorLogInterval = time.Minute

	// Never dialed: loopbackAdmitTransport answers it in-process.
	loopbackAdmitURL = "http://loopback-admit.singr.invalid/admit"
)

func validateSingRDERPOptions(options option.DERPServiceOptions) error {
	if options.VerifyClientLoopback && (len(options.VerifyClientURL) > 0 || len(options.VerifyClientEndpoint) > 0) {
		return E.New("verify_client_loopback conflicts with verify_client_url and verify_client_endpoint")
	}
	if options.ProbeListenPort != 0 && options.ProbeListenPort == options.ListenPort {
		return E.New("probe_listen_port must differ from listen_port")
	}
	if options.ControlURL == "" {
		return nil
	}
	parsed, err := url.Parse(options.ControlURL)
	if err != nil {
		return E.Cause(err, "invalid control_url")
	}
	// The control server decides whom we hand our mesh key to, so the
	// channel to it must not be tamperable.
	if parsed.Scheme != "https" || parsed.Host == "" {
		return E.New("control_url must be an https URL")
	}
	if options.ControlTokenPath == "" {
		return E.New("control_url requires control_token_path")
	}
	if options.MeshPSK != "" || options.MeshPSKFile != "" {
		return E.New("control_url provides the mesh PSK; remove mesh_psk / mesh_psk_file")
	}
	// We register our port, and list entries without a port use it.
	if options.ListenPort == 0 {
		return E.New("control_url requires listen_port")
	}
	if options.TLS == nil || (len(options.TLS.Certificate) == 0 && options.TLS.CertificatePath == "") {
		return E.New("control_url requires a fixed TLS certificate (its pin is what other relays verify)")
	}
	if interval := time.Duration(options.ControlInterval); interval != 0 && interval < minControlInterval {
		return E.New("control_interval must be at least ", minControlInterval)
	}
	return nil
}

// startSingR runs during the DERP service's Start stage, before the server
// begins serving.
func (d *Service) startSingR(server *derpserver.Server, scope *adapter.Scope) error {
	d.applyLoopbackVerifier(server)
	if d.singr.ProbeListenPort != 0 {
		err := d.startProbe(scope)
		if err != nil {
			return err
		}
	}
	if d.singr.ControlURL == "" {
		return nil
	}
	control, err := d.newMeshControl()
	if err != nil {
		return err
	}
	d.singrControl = control
	// The mesh key has to be in place before the server serves: changing it
	// later would race with connection handshakes. `singr derp enable`
	// pre-fetches it into the cache; without a cache we ask once here.
	psk := control.initialPSK()
	if psk == "" {
		d.logger.Error("no mesh PSK from control server or cache; relaying works, meshing stays off until restart")
		return nil
	}
	return server.SetMeshKey(psk)
}

// startSingRMesh runs in PostStart, when the server is up.
func (d *Service) startSingRMesh(scope *adapter.Scope) error {
	if d.singrControl == nil {
		return nil
	}
	d.singrControl.start(d.server.HasMeshKey())
	scope.Add(d.singrControl.Close)
	return nil
}

func (d *Service) applyLoopbackVerifier(server *derpserver.Server) {
	if !d.singr.VerifyClientLoopback {
		return
	}
	server.SetVerifyClientHTTPClient([]*http.Client{{Transport: loopbackAdmitTransport{}}})
	server.SetVerifyClientURL([]string{loopbackAdmitURL})
}

// ---- probe ----

func (d *Service) startProbe(scope *adapter.Scope) error {
	listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(int(d.singr.ProbeListenPort))))
	if err != nil {
		return E.Cause(err, "listen probe port")
	}
	server := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		}),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go server.Serve(listener)
	scope.Add(server.Close)
	return nil
}

// ---- pin ----

// certificatePin returns base64(SHA-256(PKIX public key)) of the first
// certificate, the form sing-box's certificate_public_key_sha256 checks.
func certificatePin(pemContent []byte) (string, error) {
	block, _ := pem.Decode(pemContent)
	if block == nil || block.Type != "CERTIFICATE" {
		return "", E.New("no PEM certificate")
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "", err
	}
	publicKey, err := x509.MarshalPKIXPublicKey(certificate.PublicKey)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(publicKey)
	return base64.StdEncoding.EncodeToString(sum[:]), nil
}

// ---- verify_client_loopback ----

// loopbackAdmitTransport plays the admission controller in-process, so the
// existing upstream verify_client_url machinery enforces the rule without a
// listener or a network round trip.
type loopbackAdmitTransport struct{}

func (loopbackAdmitTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	var admit tailcfg.DERPAdmitClientRequest
	decodeErr := json.NewDecoder(io.LimitReader(request.Body, 4<<10)).Decode(&admit)
	request.Body.Close()
	content, err := json.Marshal(tailcfg.DERPAdmitClientResponse{
		Allow: decodeErr == nil && isLoopbackSource(admit.Source),
	})
	if err != nil {
		return nil, err
	}
	return &http.Response{
		Status:        "200 OK",
		StatusCode:    http.StatusOK,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        http.Header{"Content-Type": []string{"application/json"}},
		Body:          io.NopCloser(bytes.NewReader(content)),
		ContentLength: int64(len(content)),
		Request:       request,
	}, nil
}

// A dual-stack listener reports IPv4 loopback as ::ffff:127.0.0.1.
func isLoopbackSource(address netip.Addr) bool {
	return address.IsValid() && address.Unmap().IsLoopback()
}

// ---- mesh peer list ----

type meshTarget struct {
	Host string
	Port uint16
	// Pin is the SHA-256 of the peer certificate's public key (PKIX). When
	// set, mesh TLS is verified against it alone; otherwise the certificate
	// must be valid for Host under the system roots. Never unverified.
	Pin []byte
}

func (t meshTarget) String() string {
	return net.JoinHostPort(t.Host, strconv.Itoa(int(t.Port)))
}

// id identifies a running peer connection: a changed pin must reconnect.
func (t meshTarget) id() string {
	if len(t.Pin) == 0 {
		return t.String()
	}
	return t.String() + " pin=" + base64.StdEncoding.EncodeToString(t.Pin)
}

type meshPeerListFile struct {
	// Each entry is either a string "host" / "host:port" ("[v6]:port" for
	// IPv6 with a port), or an object {"host": "...", "pin": "<base64>"}.
	// Entries without a port use this node's own DERP listen port. Unknown
	// fields are ignored, at the top level and inside entries: the same file
	// is read by clients, which may need more than the mesh does.
	Nodes []json.RawMessage `json:"nodes"`
}

type meshPeerListEntry struct {
	Host string `json:"host"`
	Pin  string `json:"pin"`
}

// parseMeshPeerList rejects the whole list on any bad entry, so a typo keeps
// the current peers instead of quietly dropping one. An empty list is
// rejected too: the registry always lists us, so an empty one means it lost
// its state, and that must not tear the mesh down.
func parseMeshPeerList(content []byte, defaultPort uint16) ([]meshTarget, error) {
	var file meshPeerListFile
	err := json.Unmarshal(content, &file)
	if err != nil {
		return nil, E.Cause(err, "decode mesh peer list")
	}
	if len(file.Nodes) == 0 {
		return nil, E.New("mesh peer list has no nodes")
	}
	var targets []meshTarget
	seen := make(map[string]meshTarget)
	for index, raw := range file.Nodes {
		target, parseErr := parseMeshEntry(raw, defaultPort)
		if parseErr != nil {
			return nil, E.Cause(parseErr, "nodes[", index, "]")
		}
		if previous, duplicate := seen[target.String()]; duplicate {
			if previous.id() != target.id() {
				return nil, E.New("nodes[", index, "]: ", target.String(), " listed twice with different pins")
			}
			continue
		}
		seen[target.String()] = target
		targets = append(targets, target)
	}
	return targets, nil
}

func parseMeshEntry(raw json.RawMessage, defaultPort uint16) (meshTarget, error) {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return parseMeshTarget(text, defaultPort)
	}
	var entry meshPeerListEntry
	err := json.Unmarshal(raw, &entry)
	if err != nil {
		return meshTarget{}, E.New("want a string or {\"host\", \"pin\"} object")
	}
	target, err := parseMeshTarget(entry.Host, defaultPort)
	if err != nil {
		return meshTarget{}, err
	}
	if pin := strings.TrimSpace(entry.Pin); pin != "" {
		decoded, decodeErr := base64.StdEncoding.DecodeString(pin)
		if decodeErr != nil || len(decoded) != sha256.Size {
			return meshTarget{}, E.New("pin of ", target.String(), " must be a base64 SHA-256 (44 characters)")
		}
		target.Pin = decoded
	}
	return target, nil
}

func parseMeshTarget(entry string, defaultPort uint16) (meshTarget, error) {
	entry = strings.TrimSpace(entry)
	if entry == "" {
		return meshTarget{}, E.New("empty host")
	}
	if strings.Contains(entry, "://") || strings.ContainsAny(entry, "/?#@ \t") {
		return meshTarget{}, E.New("want host or host:port, got ", strconv.Quote(entry))
	}
	if address, err := netip.ParseAddr(entry); err == nil {
		if address.Zone() != "" {
			return meshTarget{}, E.New("zoned address not allowed: ", entry)
		}
		return meshTarget{Host: address.String(), Port: defaultPort}, nil
	}
	if !strings.Contains(entry, ":") {
		return meshTarget{Host: strings.ToLower(strings.TrimSuffix(entry, ".")), Port: defaultPort}, nil
	}
	host, portText, err := net.SplitHostPort(entry)
	if err != nil {
		return meshTarget{}, E.Cause(err, "parse ", strconv.Quote(entry))
	}
	port, err := strconv.ParseUint(portText, 10, 16)
	if err != nil || port == 0 {
		return meshTarget{}, E.New("invalid port in ", strconv.Quote(entry))
	}
	if host == "" {
		return meshTarget{}, E.New("missing host in ", strconv.Quote(entry))
	}
	if address, addrErr := netip.ParseAddr(host); addrErr == nil {
		if address.Zone() != "" {
			return meshTarget{}, E.New("zoned address not allowed: ", entry)
		}
		host = address.String()
	} else {
		host = strings.ToLower(strings.TrimSuffix(host, "."))
	}
	return meshTarget{Host: host, Port: uint16(port)}, nil
}

// meshPeerSet owns the running peer connections; apply diffs against them.
type meshPeerSet struct {
	start func(meshTarget) (stop func(), err error)
	peers map[string]func()
}

func newMeshPeerSet(start func(meshTarget) (func(), error)) *meshPeerSet {
	return &meshPeerSet{start: start, peers: make(map[string]func())}
}

// apply starts peers that are new and stops ones no longer listed. A peer
// that fails to start is left out so the next apply retries it.
func (s *meshPeerSet) apply(targets []meshTarget) (added, removed []string, errs []error) {
	wanted := make(map[string]meshTarget, len(targets))
	for _, target := range targets {
		wanted[target.id()] = target
	}
	for name, stop := range s.peers {
		if _, keep := wanted[name]; !keep {
			stop()
			delete(s.peers, name)
			removed = append(removed, name)
		}
	}
	for name, target := range wanted {
		if _, running := s.peers[name]; running {
			continue
		}
		stop, err := s.start(target)
		if err != nil {
			errs = append(errs, E.Cause(err, "mesh peer ", name))
			continue
		}
		s.peers[name] = stop
		added = append(added, name)
	}
	slices.Sort(added)
	slices.Sort(removed)
	return
}

func (s *meshPeerSet) closeAll() {
	for name, stop := range s.peers {
		stop()
		delete(s.peers, name)
	}
}

// ---- control_url ----

type meshControl struct {
	ctx         context.Context
	cancel      context.CancelFunc
	logger      logger.ContextLogger
	baseURL     string
	token       string
	advertise   string
	pin         string
	listenPort  uint16
	interval    time.Duration
	cachePath   string
	httpClient  *http.Client
	access      sync.Mutex
	peers       *meshPeerSet
	currentPSK  string
	warnedPSK   string
	started     bool
	done        chan struct{}
	startPeerFn func(context.Context, meshTarget) (func(), error)
}

// meshControlResponse is GET /linbei/derp/mesh. The nodes part is parsed by
// parseMeshPeerList.
type meshControlResponse struct {
	PSK string `json:"psk"`
}

func (d *Service) newMeshControl() (*meshControl, error) {
	token, err := os.ReadFile(d.singr.ControlTokenPath)
	if err != nil {
		return nil, E.Cause(err, "read control_token_path")
	}
	certificate := []byte(strings.Join(d.tlsOptions.Certificate, "\n"))
	if d.tlsOptions.CertificatePath != "" {
		certificate, err = os.ReadFile(d.tlsOptions.CertificatePath)
		if err != nil {
			return nil, E.Cause(err, "read DERP certificate")
		}
	}
	pin, err := certificatePin(certificate)
	if err != nil {
		return nil, E.Cause(err, "DERP certificate pin")
	}
	interval := time.Duration(d.singr.ControlInterval)
	if interval == 0 {
		interval = defaultControlInterval
	}
	ctx, cancel := context.WithCancel(context.Background())
	transport := http.DefaultTransport.(*http.Transport).Clone()
	// Never follow proxy environment variables for the control server.
	transport.Proxy = nil
	control := &meshControl{
		ctx:        ctx,
		cancel:     cancel,
		logger:     d.logger,
		baseURL:    strings.TrimSuffix(d.singr.ControlURL, "/"),
		token:      strings.TrimSpace(string(token)),
		advertise:  d.singr.ControlAdvertiseHost,
		pin:        pin,
		listenPort: d.listenPort,
		interval:   interval,
		cachePath:  filepath.Join(filepath.Dir(d.configPath), controlCacheName),
		httpClient: &http.Client{Transport: transport, Timeout: controlRequestTimeout},
		done:       make(chan struct{}),
	}
	control.startPeerFn = d.startMeshPeer
	control.peers = newMeshPeerSet(func(target meshTarget) (func(), error) {
		return control.startPeerFn(ctx, target)
	})
	return control, nil
}

// initialPSK reads the cached response, or fetches once when there is none.
func (m *meshControl) initialPSK() string {
	content, err := os.ReadFile(m.cachePath)
	if err != nil {
		content, err = m.fetch()
		if err != nil {
			m.logger.Warn("fetch mesh from control server: ", err)
			return ""
		}
		m.writeCache(content)
	}
	var response meshControlResponse
	if json.Unmarshal(content, &response) != nil || checkMeshKey(response.PSK) != nil {
		m.logger.Warn("cached mesh response has no valid PSK")
		return ""
	}
	m.currentPSK = response.PSK
	return response.PSK
}

// start brings the cached peers up first, so a restart while the control
// server is unreachable still rebuilds the mesh, then keeps registering and
// refreshing. Without a mesh key we still register (other relays should know
// us) but start no peers.
func (m *meshControl) start(meshEnabled bool) {
	if !meshEnabled {
		m.peers = nil
	}
	content, err := os.ReadFile(m.cachePath)
	if err == nil {
		m.apply(content, "cache")
	}
	m.started = true
	go m.loop()
}

func (m *meshControl) loop() {
	defer close(m.done)
	m.refresh()
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-ticker.C:
			m.refresh()
		}
	}
}

func (m *meshControl) refresh() {
	err := m.register()
	if err != nil && m.ctx.Err() == nil {
		m.logger.Warn("register with control server: ", err)
	}
	content, err := m.fetch()
	if err != nil {
		if m.ctx.Err() == nil {
			m.logger.Warn("fetch mesh from control server (keeping current peers): ", err)
		}
		return
	}
	if m.apply(content, "control") {
		m.writeCache(content)
	}
}

func (m *meshControl) register() error {
	body, err := json.Marshal(map[string]any{
		"host": m.advertise,
		"port": m.listenPort,
		"pin":  m.pin,
	})
	if err != nil {
		return err
	}
	_, err = m.do(http.MethodPost, "/linbei/derp/register", body)
	return err
}

func (m *meshControl) fetch() ([]byte, error) {
	return m.do(http.MethodGet, "/linbei/derp/mesh", nil)
}

func (m *meshControl) do(method, path string, body []byte) ([]byte, error) {
	request, err := http.NewRequestWithContext(m.ctx, method, m.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+m.token)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := m.httpClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	content, err := io.ReadAll(io.LimitReader(response.Body, controlMaxResponseBytes+1))
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		return nil, E.New(method, " ", path, ": ", response.Status, ": ", strings.TrimSpace(string(content)))
	}
	if len(content) > controlMaxResponseBytes {
		return nil, E.New("response larger than ", controlMaxResponseBytes, " bytes")
	}
	return content, nil
}

// apply updates peers from a mesh response and reports whether it was usable.
// A bad or empty response keeps the current peers.
func (m *meshControl) apply(content []byte, source string) bool {
	var response meshControlResponse
	err := json.Unmarshal(content, &response)
	if err != nil || checkMeshKey(response.PSK) != nil {
		m.logger.Warn("bad mesh response from ", source, " (keeping current peers)")
		return false
	}
	targets, err := parseMeshPeerList(content, m.listenPort)
	if err != nil {
		m.logger.Warn("bad mesh peer list from ", source, " (keeping current peers): ", err)
		return false
	}
	m.access.Lock()
	defer m.access.Unlock()
	if m.ctx.Err() != nil {
		return false
	}
	if response.PSK != m.currentPSK && response.PSK != m.warnedPSK {
		// The key is fixed once the server serves; the new one is cached and
		// takes effect on the next start.
		m.warnedPSK = response.PSK
		m.logger.Warn("mesh PSK changed on the control server; restart SingR to apply it")
	}
	if m.peers == nil {
		return true
	}
	added, removed, errs := m.peers.apply(targets)
	for _, err := range errs {
		m.logger.Error(err)
	}
	if len(added) > 0 {
		m.logger.Info("mesh peers added (", source, "): ", strings.Join(added, ", "))
	}
	if len(removed) > 0 {
		m.logger.Info("mesh peers removed (", source, "): ", strings.Join(removed, ", "))
	}
	return true
}

func (m *meshControl) writeCache(content []byte) {
	current, err := os.ReadFile(m.cachePath)
	if err == nil && bytes.Equal(current, content) {
		return
	}
	temporary := m.cachePath + ".tmp"
	// The cache holds the mesh PSK.
	err = os.WriteFile(temporary, content, 0o600)
	if err == nil {
		err = os.Rename(temporary, m.cachePath)
	}
	if err != nil {
		os.Remove(temporary)
		m.logger.Warn("write mesh cache: ", err)
	}
}

func (m *meshControl) Close() error {
	m.cancel()
	if m.started {
		<-m.done
	}
	m.access.Lock()
	if m.peers != nil {
		m.peers.closeAll()
	}
	m.access.Unlock()
	return nil
}

// meshTLSOptions never returns an unverified config. The client info sent
// right after the handshake carries our mesh key, sealed to whatever DERP key
// the far end presents, so an unauthenticated TLS peer would simply be handed
// the PSK. The list itself is fetched over verified https, which is what makes
// a pin from it trustworthy.
func meshTLSOptions(target meshTarget) option.OutboundTLSOptions {
	options := option.OutboundTLSOptions{Enabled: true, ServerName: target.Host}
	if len(target.Pin) > 0 {
		// Pinned peers use a fixed self-signed certificate; the pin alone
		// decides (no CA, hostname or expiry check).
		options.CertificatePublicKeySHA256 = [][]byte{target.Pin}
	}
	return options
}

// startMeshPeer mirrors upstream startMeshWithHost, kept separate so the
// connection can be stopped and its failures do not flood the log: a peer
// that is down retries every few seconds forever.
func (d *Service) startMeshPeer(parent context.Context, target meshTarget) (func(), error) {
	meshDialer, err := dialer.NewWithOptions(dialer.Options{
		Context:        d.ctx,
		Options:        option.DialerOptions{},
		RemoteIsDomain: M.ParseSocksaddrHostPort(target.Host, target.Port).IsDomain(),
		NewDialer:      true,
	})
	if err != nil {
		return nil, err
	}
	tlsConfig, err := tls.NewClient(d.ctx, d.logger, target.Host, meshTLSOptions(target))
	if err != nil {
		return nil, err
	}
	stdConfig, err := tlsConfig.STDConfig()
	if err != nil {
		return nil, err
	}
	name := target.String()
	serverURL := "https://" + name + "/derp"
	debugf := func(format string, args ...any) {
		d.logger.Debug("mesh(", name, "): ", fmt.Sprintf(format, args...))
	}
	meshClient, err := derphttp.NewClient(d.server.PrivateKey(), serverURL, debugf, netmon.NewStatic())
	if err != nil {
		return nil, err
	}
	meshClient.TLSConfig = stdConfig
	meshClient.MeshKey = d.server.MeshKey()
	meshClient.WatchConnectionChanges = true
	meshClient.SetURLDialer(func(ctx context.Context, network, addr string) (net.Conn, error) {
		return meshDialer.DialContext(ctx, network, M.ParseSocksaddr(addr))
	})

	// Upstream's "connected; N peers" info line fires from a 2s timer even
	// when the connect failed, so it stays at debug. Failures are what an
	// operator needs to see: at most one warning per minute while a peer is
	// down, and silence once it is back.
	ctx, cancel := context.WithCancel(parent)
	var (
		logAccess   sync.Mutex
		lastErrorAt time.Time
	)
	notifyError := func(err error) {
		if ctx.Err() != nil {
			// Stopped on purpose (removed from the list or shutting down).
			return
		}
		logAccess.Lock()
		defer logAccess.Unlock()
		if now := time.Now(); now.Sub(lastErrorAt) >= meshPeerErrorLogInterval {
			lastErrorAt = now
			d.logger.Warn("mesh(", name, "): ", err)
		}
	}
	add := func(m derp.PeerPresentMessage) { d.server.AddPacketForwarder(m.Key, meshClient) }
	remove := func(m derp.PeerGoneMessage) { d.server.RemovePacketForwarder(m.Peer, meshClient) }

	// Self-connect (our own address in the list) is detected by server key
	// inside RunWatchConnectionLoop, which then returns and stays idle.
	go meshClient.RunWatchConnectionLoop(ctx, d.server.PublicKey(), debugf, add, remove, notifyError)
	return func() {
		cancel()
		meshClient.Close()
	}, nil
}
