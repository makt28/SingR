package naive

import (
	"context"
	stdtls "crypto/tls"
	"errors"
	"io"
	stdlog "log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/sagernet/sing-box/common/listener"
	"github.com/sagernet/sing-box/common/tls"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing-box/poet/api"
	"github.com/sagernet/sing/common"
	E "github.com/sagernet/sing/common/exceptions"
	"github.com/sagernet/sing/common/logger"
	N "github.com/sagernet/sing/common/network"
	aTLS "github.com/sagernet/sing/common/tls"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c" //nolint:staticcheck
)

// This file holds SingR's panel hot reload for the naive inbound and has no
// upstream counterpart. inbound.go keeps only the hooks.
//
// One naive inbound serves HTTP/2 (TCP) and HTTP/3 (QUIC) on the same port
// number. Both read the TLS config per connection — the TCP accept path
// through tlsListener, QUIC through tlsSwitch's GetConfigForClient — so an
// SNI change swaps the config without rebinding either socket. That is the
// difference from hysteria2, whose SNI-only change has to close the old UDP
// listener before the new one can bind.

// newNaiveTLS builds the server TLS config and sets the ALPN list that
// upstream sets in Start (TCP: h2 first plus http/1.1; QUIC: h3), so every
// config hot reload swaps in negotiates the same protocols.
func newNaiveTLS(ctx context.Context, logger logger.ContextLogger, options option.NaiveInboundOptions, network []string) (tls.ServerConfig, error) {
	tlsConfig, err := tls.NewServer(ctx, logger, common.PtrValueOrDefault(options.TLS))
	if err != nil || tlsConfig == nil {
		return tlsConfig, err
	}
	if common.Contains(network, N.NetworkTCP) {
		if len(tlsConfig.NextProtos()) == 0 {
			tlsConfig.SetNextProtos([]string{http2.NextProtoTLS, "http/1.1"})
		} else if !common.Contains(tlsConfig.NextProtos(), http2.NextProtoTLS) {
			tlsConfig.SetNextProtos(append([]string{http2.NextProtoTLS}, tlsConfig.NextProtos()...))
		}
	}
	if common.Contains(network, N.NetworkUDP) && !common.Contains(tlsConfig.NextProtos(), "h3") {
		tlsConfig.SetNextProtos(append(append([]string{}, tlsConfig.NextProtos()...), "h3"))
	}
	return tlsConfig, nil
}

func (n *Inbound) currentTLS() tls.ServerConfig {
	n.tlsMu.RLock()
	defer n.tlsMu.RUnlock()
	return n.tlsConfig
}

// serve brings HTTP/2 and/or HTTP/3 up on lis. Unlike upstream, an HTTP/3
// failure is fatal even when TCP is also enabled: a reload that silently
// left QUIC behind would never be retried. On error the caller closes lis.
func (n *Inbound) serve(lis *listener.Listener) (*http.Server, io.Closer, error) {
	var httpServer *http.Server
	if common.Contains(n.network, N.NetworkTCP) {
		tcpListener, err := lis.ListenTCP()
		if err != nil {
			return nil, nil, err
		}
		httpServer = &http.Server{
			//nolint:staticcheck
			Handler: h2c.NewHandler(n, &http2.Server{}),
			BaseContext: func(listener net.Listener) context.Context {
				return n.ctx
			},
			// net/http logs TLS handshake failures from probes to the
			// standard logger, i.e. straight into the service log.
			ErrorLog: stdlog.New(debugLogWriter{n.logger}, "", 0),
		}
		go func() {
			sErr := httpServer.Serve(&tlsListener{Listener: tcpListener, inbound: n})
			if sErr != nil && !errors.Is(sErr, http.ErrServerClosed) && !E.IsClosedOrCanceled(sErr) {
				n.logger.Error("http server serve error: ", sErr)
			}
		}()
	}
	var h3Server io.Closer
	if common.Contains(n.network, N.NetworkUDP) {
		var err error
		h3Server, err = ConfigureHTTP3ListenerFunc(n.ctx, n.logger, lis, n, n.tlsSwitch, n.options)
		if err != nil {
			if httpServer != nil {
				_ = httpServer.Close()
			}
			return nil, nil, E.Cause(err, "start naive http3")
		}
	}
	return httpServer, h3Server, nil
}

// closeServing stops accepting on one port. Established HTTP/2 and
// HTTP/1.1 tunnels are hijacked from the http.Server (h2c takes over the
// conn), so they survive it — like anytls, whose old listener close leaves
// accepted conns alone. HTTP/3 sessions die with the UDP socket, like a
// hysteria2 port change.
func closeServing(httpServer *http.Server, h3Server io.Closer, lis *listener.Listener) {
	if httpServer != nil {
		_ = httpServer.Close()
	}
	_ = common.Close(h3Server, lis)
}

// ConfigureFromPanelNode applies SSPanel-derived port and SNI.
//
// Before Start it stashes the new options and rebuilds the not-yet-started
// listener/TLS so Start picks up panel-derived values.
//
// After Start:
//   - port change: bring HTTP/2 and HTTP/3 up on the new port first, then
//     close the old one. If either fails to bind, both are rolled back and
//     the old port keeps serving; reloadPending retries on the next tick.
//   - SNI change: build and start a new TLS config and swap it in under
//     tlsMu. No socket is touched, so there is no downtime window and a
//     failure leaves the old config serving.
//
// nodeInfo.Port must be in (0, 65535]; an out-of-range value is rejected
// and the previous configuration is left in place (defensive against a
// panel that briefly returns Port=0).
//
// A panel node of another type is an error (api.NodeTypeMismatch), not a
// no-op: at startup it fails the node, at runtime reloadPending keeps
// logging it while the current configuration stays in place.
func (n *Inbound) ConfigureFromPanelNode(nodeInfo *api.NodeInfo) error {
	if nodeInfo == nil {
		return nil
	}
	if nodeInfo.NodeType != C.TypeNaive {
		return api.NodeTypeMismatch(C.TypeNaive, nodeInfo.NodeType)
	}
	if nodeInfo.Port <= 0 || nodeInfo.Port > 65535 {
		return E.New("invalid naive listen port from panel: ", nodeInfo.Port)
	}

	n.reloadMu.Lock()
	defer n.reloadMu.Unlock()
	// A panel tick still in flight after shutdown must not bring a new
	// listener up on a closed inbound.
	if n.closed {
		return nil
	}

	newOpts := n.options
	newOpts.ListenPort = uint16(nodeInfo.Port)
	if nodeInfo.Host != "" {
		if newOpts.TLS == nil {
			newOpts.TLS = &option.InboundTLSOptions{Enabled: true}
		} else {
			tlsCopy := *newOpts.TLS
			newOpts.TLS = &tlsCopy
		}
		newOpts.TLS.ServerName = nodeInfo.Host
	}

	portChanged := newOpts.ListenPort != n.options.ListenPort
	oldSNI := ""
	if n.options.TLS != nil {
		oldSNI = n.options.TLS.ServerName
	}
	newSNI := ""
	if newOpts.TLS != nil {
		newSNI = newOpts.TLS.ServerName
	}
	sniChanged := newSNI != oldSNI

	started := n.started.Load()
	if started && !portChanged && !sniChanged {
		return nil
	}

	if !started {
		if newOpts.TLS != nil && newOpts.TLS.Enabled {
			tlsConfig, err := newNaiveTLS(n.ctx, n.logger, newOpts, n.network)
			if err != nil {
				return err
			}
			n.tlsMu.Lock()
			oldTLS := n.tlsConfig
			n.tlsConfig = tlsConfig
			n.tlsMu.Unlock()
			_ = common.Close(oldTLS)
		}
		n.listener = n.newListener(newOpts)
		n.options = newOpts
		return nil
	}

	var newTLS tls.ServerConfig
	if sniChanged && newOpts.TLS != nil && newOpts.TLS.Enabled {
		var err error
		newTLS, err = newNaiveTLS(n.ctx, n.logger, newOpts, n.network)
		if err != nil {
			return err
		}
		if err = newTLS.Start(); err != nil {
			_ = common.Close(newTLS)
			return E.Cause(err, "start reloaded naive tls")
		}
	}

	if portChanged {
		// Until the TLS swap below, the new port serves the old config.
		newListener := n.newListener(newOpts)
		httpServer, h3Server, err := n.serve(newListener)
		if err != nil {
			_ = newListener.Close()
			_ = common.Close(newTLS)
			return E.Cause(err, "start reloaded naive listener on port ", newOpts.ListenPort)
		}
		oldHTTP, oldH3, oldListener := n.httpServer, n.h3Server, n.listener
		n.httpServer, n.h3Server, n.listener = httpServer, h3Server, newListener
		closeServing(oldHTTP, oldH3, oldListener)
		n.logger.Info("naive listener hot-reloaded to port ", newOpts.ListenPort)
	}

	if newTLS != nil {
		n.tlsMu.Lock()
		oldTLS := n.tlsConfig
		n.tlsConfig = newTLS
		n.tlsMu.Unlock()
		_ = common.Close(oldTLS)
		n.logger.Info("naive TLS hot-reloaded with SNI ", newSNI)
	}

	n.options = newOpts
	return nil
}

func (n *Inbound) newListener(options option.NaiveInboundOptions) *listener.Listener {
	return listener.New(listener.Options{
		Context: n.ctx,
		Logger:  n.logger,
		Listen:  options.ListenOptions,
	})
}

// Close releases whatever is live now, including a hot-reloaded port.
// Idempotent: the inbound registers it with the start scope and tests may
// also call it directly.
func (n *Inbound) Close() error {
	n.reloadMu.Lock()
	defer n.reloadMu.Unlock()
	if n.closed {
		return nil
	}
	n.closed = true
	closeServing(n.httpServer, n.h3Server, n.listener)
	return common.Close(n.currentTLS())
}

// tlsListener wraps accepted TCP conns with the TLS config current at
// accept time, so an SNI swap applies to the next connection.
type tlsListener struct {
	net.Listener
	inbound *Inbound
}

func (l *tlsListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	if tlsConfig := l.inbound.currentTLS(); tlsConfig != nil {
		return aTLS.NewLazyConn(conn, tlsConfig), nil
	}
	return conn, nil
}

// tlsSwitch is the TLS config handed to the HTTP/3 listener. Its standard
// config is a front whose GetConfigForClient returns the inbound's current
// config; quic-go applies that per handshake (recursively, see
// internal/handshake setupConfigForServer), which is the same path the
// certificate file reload already relies on. The QUIC listener therefore
// never needs to be rebuilt for an SNI change. Lifecycle belongs to the
// inbound, so Start/Close are no-ops.
type tlsSwitch struct {
	inbound *Inbound
	front   *stdtls.Config
}

func newTLSSwitch(inbound *Inbound) *tlsSwitch {
	s := &tlsSwitch{inbound: inbound}
	s.front = &stdtls.Config{GetConfigForClient: s.getConfigForClient}
	return s
}

func (s *tlsSwitch) getConfigForClient(info *stdtls.ClientHelloInfo) (*stdtls.Config, error) {
	current := s.inbound.currentTLS()
	if current == nil {
		return nil, E.New("naive: TLS is not configured")
	}
	config, err := current.STDConfig()
	if err != nil {
		return nil, err
	}
	if config.GetConfigForClient != nil {
		return config.GetConfigForClient(info)
	}
	return config, nil
}

func (s *tlsSwitch) ServerName() string {
	if current := s.inbound.currentTLS(); current != nil {
		return current.ServerName()
	}
	return ""
}

func (s *tlsSwitch) SetServerName(serverName string) {
	if current := s.inbound.currentTLS(); current != nil {
		current.SetServerName(serverName)
	}
}

func (s *tlsSwitch) NextProtos() []string {
	if current := s.inbound.currentTLS(); current != nil {
		return current.NextProtos()
	}
	return nil
}

func (s *tlsSwitch) SetNextProtos(nextProto []string) {
	if current := s.inbound.currentTLS(); current != nil {
		current.SetNextProtos(nextProto)
	}
}

func (s *tlsSwitch) HandshakeTimeout() time.Duration {
	if current := s.inbound.currentTLS(); current != nil {
		return current.HandshakeTimeout()
	}
	return 0
}

func (s *tlsSwitch) SetHandshakeTimeout(timeout time.Duration) {
	if current := s.inbound.currentTLS(); current != nil {
		current.SetHandshakeTimeout(timeout)
	}
}

func (s *tlsSwitch) STDConfig() (*tls.STDConfig, error) {
	return s.front, nil
}

func (s *tlsSwitch) Client(conn net.Conn) (tls.Conn, error) {
	return nil, os.ErrInvalid
}

func (s *tlsSwitch) Clone() aTLS.Config {
	return s
}

func (s *tlsSwitch) Start() error {
	return nil
}

func (s *tlsSwitch) Close() error {
	return nil
}

func (s *tlsSwitch) Server(conn net.Conn) (tls.Conn, error) {
	current := s.inbound.currentTLS()
	if current == nil {
		return nil, E.New("naive: TLS is not configured")
	}
	return current.Server(conn)
}

type debugLogWriter struct {
	logger logger.ContextLogger
}

func (w debugLogWriter) Write(p []byte) (int, error) {
	w.logger.Debug(strings.TrimSpace(string(p)))
	return len(p), nil
}
