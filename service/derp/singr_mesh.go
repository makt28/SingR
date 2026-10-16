//go:build with_gvisor

package derp

// SingR-only additions to the DERP service (not in upstream):
//
//   - verify_client_loopback: ordinary clients are admitted only from the
//     loopback address (mesh peers bypass verification upstream, see
//     derpserver.Server.verifyClient).
//   - registry_url: the node reports itself (port, certificate pin, its own
//     public IPv4 / IPv6) to sspapi's tailnet.php every minute and gets the
//     mesh PSK plus every other relay back; peers are added and removed in
//     place while running.
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

// Variables only so tests can shorten them.
var (
	defaultSyncInterval   = 60 * time.Second
	minSyncInterval       = 10 * time.Second
	addressCheckInterval  = 5 * time.Second
	addressChangeDebounce = 2 * time.Second
	peerFailureResync     = 10 * time.Second
	publicIPRetryInterval = 30 * time.Second
)

const (
	publicIPLookupTimeout    = 5 * time.Second
	registryRequestTimeout   = 15 * time.Second
	registryMaxResponseBytes = 1 << 20
	registryCacheName        = "mesh-registry.cache.json"
	meshPeerErrorLogInterval = time.Minute

	// advertise_ipv4 / advertise_ipv6 value that stops reporting the family.
	advertiseNone = "none"

	// Never dialed: loopbackAdmitTransport answers it in-process.
	loopbackAdmitURL = "http://loopback-admit.singr.invalid/admit"
)

// The kernel is asked which source address it would use towards these; a
// connected UDP socket sends nothing, so this costs no traffic.
var routeProbeTargets = [2]string{"1.1.1.1:53", "[2606:4700:4700::1111]:53"}

// Asked once at startup (until it answers) when the machine sits behind NAT.
var publicIPLookupURLs = [2]string{"https://api-ipv4.ip.sb/ip", "https://api-ipv6.ip.sb/ip"}

func validateSingRDERPOptions(options option.DERPServiceOptions) error {
	if options.VerifyClientLoopback && (len(options.VerifyClientURL) > 0 || len(options.VerifyClientEndpoint) > 0) {
		return E.New("verify_client_loopback conflicts with verify_client_url and verify_client_endpoint")
	}
	if options.ProbeListenPort != 0 && options.ProbeListenPort == options.ListenPort {
		return E.New("probe_listen_port must differ from listen_port")
	}
	if options.RegistryURL == "" {
		return nil
	}
	parsed, err := url.Parse(options.RegistryURL)
	if err != nil {
		return E.Cause(err, "invalid registry_url")
	}
	// The registry decides whom we hand our mesh key to, so the channel to it
	// must not be tamperable.
	if parsed.Scheme != "https" || parsed.Host == "" {
		return E.New("registry_url must be an https URL")
	}
	if options.RegistryTokenPath == "" {
		return E.New("registry_url requires registry_token_path")
	}
	if options.MeshPSK != "" || options.MeshPSKFile != "" {
		return E.New("registry_url provides the mesh PSK; remove mesh_psk / mesh_psk_file")
	}
	if options.ListenPort == 0 {
		return E.New("registry_url requires listen_port")
	}
	if options.TLS == nil || (len(options.TLS.Certificate) == 0 && options.TLS.CertificatePath == "") {
		return E.New("registry_url requires a fixed TLS certificate (its pin is what other relays verify)")
	}
	for family, value := range [2]string{options.AdvertiseIPv4, options.AdvertiseIPv6} {
		if value == "" || value == advertiseNone {
			continue
		}
		address, parseErr := netip.ParseAddr(value)
		if parseErr != nil || address.Zone() != "" || address.Is4() != (family == 0) {
			return E.New("advertise_ipv", []string{"4", "6"}[family], ": want an IPv", []string{"4", "6"}[family],
				" address or \"none\", got ", strconv.Quote(value))
		}
	}
	if options.AdvertiseIPv4 == advertiseNone && options.AdvertiseIPv6 == advertiseNone {
		return E.New("advertise_ipv4 and advertise_ipv6 are both \"none\": nothing to report")
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
	if d.singr.RegistryURL == "" {
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
		d.logger.Error("no mesh PSK from the registry or cache; relaying works, meshing stays off until restart")
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

// ---- mesh peers ----

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

// id identifies a running peer connection: a changed address or pin must
// reconnect.
func (t meshTarget) id() string {
	if len(t.Pin) == 0 {
		return t.String()
	}
	return t.String() + " pin=" + base64.StdEncoding.EncodeToString(t.Pin)
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

// ---- own addresses ----

// isPublicAddress: what the registry accepts and other relays can dial.
func isPublicAddress(address netip.Addr) bool {
	address = address.Unmap()
	if !address.IsValid() || !address.IsGlobalUnicast() || address.IsPrivate() {
		return false
	}
	if address.Is4() {
		return !netip.MustParsePrefix("100.64.0.0/10").Contains(address)
	}
	return netip.MustParsePrefix("2000::/3").Contains(address) &&
		!netip.MustParsePrefix("2001:db8::/32").Contains(address)
}

// localSourceAddress asks the kernel which address it would send from towards
// a public host (0 = IPv4, 1 = IPv6). Invalid when the family has no route.
func localSourceAddress(family int) netip.Addr {
	conn, err := net.Dial([]string{"udp4", "udp6"}[family], routeProbeTargets[family])
	if err != nil {
		return netip.Addr{}
	}
	defer conn.Close()
	addressPort, err := netip.ParseAddrPort(conn.LocalAddr().String())
	if err != nil {
		return netip.Addr{}
	}
	return addressPort.Addr().Unmap()
}

// lookupPublicIP asks ip.sb, over the given family only, which address we
// come from. Used when the machine's own address is not public (NAT).
func lookupPublicIP(ctx context.Context, family int) (netip.Addr, error) {
	network := []string{"tcp4", "tcp6"}[family]
	var netDialer net.Dialer
	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, _, address string) (net.Conn, error) {
			return netDialer.DialContext(ctx, network, address)
		},
		TLSHandshakeTimeout: publicIPLookupTimeout,
	}
	defer transport.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(ctx, publicIPLookupTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, publicIPLookupURLs[family], nil)
	if err != nil {
		return netip.Addr{}, err
	}
	request.Header.Set("User-Agent", "SingR")
	response, err := (&http.Client{Transport: transport}).Do(request)
	if err != nil {
		return netip.Addr{}, err
	}
	defer response.Body.Close()
	content, err := io.ReadAll(io.LimitReader(response.Body, 256))
	if err != nil {
		return netip.Addr{}, err
	}
	if response.StatusCode != http.StatusOK {
		return netip.Addr{}, E.New(publicIPLookupURLs[family], ": ", response.Status)
	}
	address, err := netip.ParseAddr(strings.TrimSpace(string(content)))
	if err != nil || address.Is4() != (family == 0) || !isPublicAddress(address) {
		return netip.Addr{}, E.New(publicIPLookupURLs[family], ": unexpected answer ", strconv.Quote(strings.TrimSpace(string(content))))
	}
	return address, nil
}

// ownAddresses decides what we report, per family:
//   - advertise_ipv4 / advertise_ipv6 set to an address: that, always;
//   - set to "none": nothing;
//   - empty (auto): the address the kernel would send from, when it is
//     public (most VPS); behind NAT, what ip.sb said once at startup.
type ownAddresses struct {
	pinned   [2]string
	local    func(family int) netip.Addr
	lookup   func(ctx context.Context, family int) (netip.Addr, error)
	lookedUp [2]netip.Addr
	lastTry  [2]time.Time
	current  [2]netip.Addr
}

// needsLookup: auto family, a route exists, but the source address is not
// public and ip.sb has not answered yet.
func (a *ownAddresses) needsLookup(family int, local netip.Addr) bool {
	return a.pinned[family] == "" && local.IsValid() && !isPublicAddress(local) && !a.lookedUp[family].IsValid()
}

// refresh recomputes both families and reports whether anything changed.
// ip.sb is asked at most every publicIPRetryInterval and only until it answers.
func (a *ownAddresses) refresh(ctx context.Context, now time.Time, log logger.ContextLogger) bool {
	changed := false
	for family := range 2 {
		var next netip.Addr
		switch pinned := a.pinned[family]; pinned {
		case advertiseNone:
		case "":
			local := a.local(family)
			if a.needsLookup(family, local) && now.Sub(a.lastTry[family]) >= publicIPRetryInterval {
				a.lastTry[family] = now
				address, err := a.lookup(ctx, family)
				if err != nil {
					log.Warn("public IPv", 4+2*family, " lookup (retrying in ", publicIPRetryInterval, "): ", err)
				} else {
					a.lookedUp[family] = address
				}
			}
			if isPublicAddress(local) {
				next = local
			} else {
				next = a.lookedUp[family]
			}
		default:
			next = netip.MustParseAddr(pinned)
		}
		if next != a.current[family] {
			if a.current[family].IsValid() || next.IsValid() {
				log.Info("reported IPv", 4+2*family, ": ", addressText(a.current[family]), " -> ", addressText(next))
			}
			a.current[family] = next
			changed = true
		}
	}
	return changed
}

func addressText(address netip.Addr) string {
	if !address.IsValid() {
		return "none"
	}
	return address.String()
}

// ---- registry ----

type registryNode struct {
	Pin  string `json:"pin"`
	Port int    `json:"port"`
	IPv4 string `json:"ipv4"`
	IPv6 string `json:"ipv6"`
}

// registryResponse is tailnet.php's derp_sync answer (also what the cache
// holds, byte for byte).
type registryResponse struct {
	Status   string         `json:"status"`
	Code     string         `json:"code"`
	Message  string         `json:"message"`
	PSK      string         `json:"psk"`
	Nodes    []registryNode `json:"nodes"`
	Interval int            `json:"interval"`
}

func parseRegistryResponse(content []byte) (registryResponse, error) {
	var response registryResponse
	err := json.Unmarshal(content, &response)
	if err != nil {
		return response, E.Cause(err, "decode registry response")
	}
	if response.Status != "success" {
		return response, E.New("registry: ", response.Code, " ", response.Message)
	}
	if checkMeshKey(response.PSK) != nil {
		return response, E.New("registry response has no valid PSK")
	}
	// The registry always lists us, so an empty list means it lost its
	// state; that must not tear the mesh down.
	if len(response.Nodes) == 0 {
		return response, E.New("registry lists no nodes")
	}
	return response, nil
}

// meshTargets picks, for every other node, the address to mesh over: IPv4
// when both sides have one, else IPv6 when both do. A node sharing no family
// with us is returned in unreachable. The whole list is rejected on any bad
// entry, so a broken registry keeps the current peers.
func meshTargets(nodes []registryNode, selfPin string, self [2]netip.Addr) (targets []meshTarget, unreachable []string, err error) {
	for index, node := range nodes {
		pin, decodeErr := base64.StdEncoding.DecodeString(node.Pin)
		if decodeErr != nil || len(pin) != sha256.Size {
			return nil, nil, E.New("nodes[", index, "]: bad pin")
		}
		if node.Port < 1 || node.Port > 65535 {
			return nil, nil, E.New("nodes[", index, "]: bad port")
		}
		var peer [2]netip.Addr
		for family, text := range [2]string{node.IPv4, node.IPv6} {
			if text == "" {
				continue
			}
			address, parseErr := netip.ParseAddr(text)
			if parseErr != nil || address.Zone() != "" || address.Is4() != (family == 0) {
				return nil, nil, E.New("nodes[", index, "]: bad IPv", 4+2*family, " ", strconv.Quote(text))
			}
			peer[family] = address
		}
		if node.Pin == selfPin {
			continue
		}
		chosen := -1
		for family := range 2 {
			if self[family].IsValid() && peer[family].IsValid() {
				chosen = family
				break
			}
		}
		if chosen < 0 {
			unreachable = append(unreachable, node.Pin)
			continue
		}
		targets = append(targets, meshTarget{Host: peer[chosen].String(), Port: uint16(node.Port), Pin: pin})
	}
	return targets, unreachable, nil
}

type meshControl struct {
	ctx         context.Context
	cancel      context.CancelFunc
	logger      logger.ContextLogger
	registryURL string
	token       string
	pin         string
	listenPort  uint16
	cachePath   string
	httpClient  *http.Client
	access      sync.Mutex
	peers       *meshPeerSet
	addresses   ownAddresses
	interval    time.Duration
	currentPSK  string
	warnedPSK   string
	warned      map[string]bool
	peerFailed  chan struct{}
	started     bool
	done        chan struct{}
	startPeerFn func(context.Context, meshTarget, func()) (func(), error)
}

func (d *Service) newMeshControl() (*meshControl, error) {
	token, err := os.ReadFile(d.singr.RegistryTokenPath)
	if err != nil {
		return nil, E.Cause(err, "read registry_token_path")
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
	control := newMeshControl(d.logger, d.singr.RegistryURL, strings.TrimSpace(string(token)), pin, d.listenPort,
		filepath.Join(filepath.Dir(d.configPath), registryCacheName),
		[2]string{d.singr.AdvertiseIPv4, d.singr.AdvertiseIPv6})
	control.startPeerFn = d.startMeshPeer
	return control, nil
}

func newMeshControl(log logger.ContextLogger, registryURL, token, pin string, listenPort uint16, cachePath string, pinned [2]string) *meshControl {
	ctx, cancel := context.WithCancel(context.Background())
	transport := http.DefaultTransport.(*http.Transport).Clone()
	// Never follow proxy environment variables for the registry.
	transport.Proxy = nil
	control := &meshControl{
		ctx:         ctx,
		cancel:      cancel,
		logger:      log,
		registryURL: registryURL,
		token:       token,
		pin:         pin,
		listenPort:  listenPort,
		cachePath:   cachePath,
		httpClient:  &http.Client{Transport: transport, Timeout: registryRequestTimeout},
		addresses:   ownAddresses{pinned: pinned, local: localSourceAddress, lookup: lookupPublicIP},
		interval:    defaultSyncInterval,
		warned:      make(map[string]bool),
		peerFailed:  make(chan struct{}, 1),
		done:        make(chan struct{}),
	}
	control.peers = newMeshPeerSet(func(target meshTarget) (func(), error) {
		return control.startPeerFn(ctx, target, control.notePeerFailure)
	})
	return control
}

// initialPSK reads the cached response, or registers once when there is none.
func (m *meshControl) initialPSK() string {
	content, err := os.ReadFile(m.cachePath)
	if err != nil {
		m.addresses.refresh(m.ctx, time.Now(), m.logger)
		content, err = m.syncRequest()
		if err != nil {
			m.logger.Warn("register with the registry: ", err)
			return ""
		}
		m.writeCache(content)
	}
	response, err := parseRegistryResponse(content)
	if err != nil {
		m.logger.Warn("cached registry response unusable: ", err)
		return ""
	}
	m.currentPSK = response.PSK
	return response.PSK
}

// start brings the cached peers up first, so a restart while the registry is
// unreachable still rebuilds the mesh, then keeps reporting and refreshing.
// Without a mesh key we still report (other relays should know us) but start
// no peers.
func (m *meshControl) start(meshEnabled bool) {
	if !meshEnabled {
		m.peers = nil
	}
	m.addresses.refresh(m.ctx, time.Now(), m.logger)
	content, err := os.ReadFile(m.cachePath)
	if err == nil {
		m.apply(content, "cache")
	}
	m.started = true
	go m.loop()
}

func (m *meshControl) notePeerFailure() {
	select {
	case m.peerFailed <- struct{}{}:
	default:
	}
}

func (m *meshControl) loop() {
	defer close(m.done)
	m.sync()
	addressTicker := time.NewTicker(addressCheckInterval)
	defer addressTicker.Stop()
	syncTimer := time.NewTimer(m.interval)
	defer syncTimer.Stop()
	var debounce <-chan time.Time
	var lastFailureSync time.Time
	resync := func() {
		m.sync()
		syncTimer.Reset(m.interval)
	}
	for {
		select {
		case <-m.ctx.Done():
			return
		case now := <-addressTicker.C:
			if m.addresses.refresh(m.ctx, now, m.logger) && debounce == nil {
				debounce = time.After(addressChangeDebounce)
			}
		case <-debounce:
			debounce = nil
			resync()
		case <-m.peerFailed:
			// A peer that changed address fails until we learn the new one.
			if time.Since(lastFailureSync) >= peerFailureResync {
				lastFailureSync = time.Now()
				resync()
			}
		case <-syncTimer.C:
			resync()
		}
	}
}

func (m *meshControl) sync() {
	content, err := m.syncRequest()
	if err != nil {
		if m.ctx.Err() == nil {
			m.logger.Warn("sync with the registry (keeping current peers): ", err)
		}
		return
	}
	if m.apply(content, "registry") {
		m.writeCache(content)
	}
}

func (m *meshControl) syncRequest() ([]byte, error) {
	self := m.addresses.current
	if !self[0].IsValid() && !self[1].IsValid() {
		return nil, E.New("no public IPv4 or IPv6 to report (set advertise_ipv4 / advertise_ipv6)")
	}
	form := url.Values{
		"action":     {"derp_sync"},
		"node_token": {m.token},
		"port":       {strconv.Itoa(int(m.listenPort))},
		"pin":        {m.pin},
	}
	if self[0].IsValid() {
		form.Set("ipv4", self[0].String())
	}
	if self[1].IsValid() {
		form.Set("ipv6", self[1].String())
	}
	request, err := http.NewRequestWithContext(m.ctx, http.MethodPost, m.registryURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := m.httpClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	content, err := io.ReadAll(io.LimitReader(response.Body, registryMaxResponseBytes+1))
	if err != nil {
		return nil, err
	}
	if len(content) > registryMaxResponseBytes {
		return nil, E.New("response larger than ", registryMaxResponseBytes, " bytes")
	}
	if response.StatusCode != http.StatusOK {
		var failure registryResponse
		_ = json.Unmarshal(content, &failure)
		if failure.Code != "" {
			return nil, E.New(response.Status, ": ", failure.Code, " ", failure.Message)
		}
		return nil, E.New(response.Status)
	}
	return content, nil
}

// apply updates peers from a registry response and reports whether it was
// usable. A bad or empty response keeps the current peers.
func (m *meshControl) apply(content []byte, source string) bool {
	response, err := parseRegistryResponse(content)
	if err != nil {
		m.logger.Warn("bad registry response from ", source, " (keeping current peers): ", err)
		return false
	}
	targets, unreachable, err := meshTargets(response.Nodes, m.pin, m.addresses.current)
	if err != nil {
		m.logger.Warn("bad node list from ", source, " (keeping current peers): ", err)
		return false
	}
	m.access.Lock()
	defer m.access.Unlock()
	if m.ctx.Err() != nil {
		return false
	}
	if source == "registry" && response.Interval > 0 {
		m.interval = max(time.Duration(response.Interval)*time.Second, minSyncInterval)
	}
	if response.PSK != m.currentPSK && response.PSK != m.warnedPSK {
		// The key is fixed once the server serves; the new one is cached and
		// takes effect on the next start.
		m.warnedPSK = response.PSK
		m.logger.Warn("mesh PSK changed in the registry; restart SingR to apply it")
	}
	for _, pin := range unreachable {
		if !m.warned[pin] {
			m.warned[pin] = true
			m.logger.Warn("relay ", pin, " shares no address family with us (one side IPv4-only, the other IPv6-only); not meshing with it")
		}
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
		m.logger.Warn("write registry cache: ", err)
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
func (d *Service) startMeshPeer(parent context.Context, target meshTarget, onFailure func()) (func(), error) {
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
		onFailure()
		logAccess.Lock()
		defer logAccess.Unlock()
		if now := time.Now(); now.Sub(lastErrorAt) >= meshPeerErrorLogInterval {
			lastErrorAt = now
			d.logger.Warn("mesh(", name, "): ", err)
		}
	}
	add := func(m derp.PeerPresentMessage) { d.server.AddPacketForwarder(m.Key, meshClient) }
	remove := func(m derp.PeerGoneMessage) { d.server.RemovePacketForwarder(m.Peer, meshClient) }

	// Our own entry is skipped by pin; should our address still show up (a
	// stale entry), RunWatchConnectionLoop detects the self-connect by server
	// key and goes idle.
	go meshClient.RunWatchConnectionLoop(ctx, d.server.PublicKey(), debugf, add, remove, notifyError)
	return func() {
		cancel()
		meshClient.Close()
	}, nil
}
