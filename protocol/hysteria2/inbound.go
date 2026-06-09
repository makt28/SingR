package hysteria2

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/adapter/inbound"
	"github.com/sagernet/sing-box/common/listener"
	"github.com/sagernet/sing-box/common/tls"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing-box/poet/api"
	"github.com/sagernet/sing-quic/hysteria"
	"github.com/sagernet/sing-quic/hysteria2"
	"github.com/sagernet/sing/common"
	"github.com/sagernet/sing/common/atomic"
	"github.com/sagernet/sing/common/auth"
	E "github.com/sagernet/sing/common/exceptions"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
)

func RegisterInbound(registry *inbound.Registry) {
	inbound.Register[option.Hysteria2InboundOptions](registry, C.TypeHysteria2, NewInbound)
}

// Inbound is the SingR hysteria2 inbound.
//
// Differences from upstream sing-box:
//   - The hysteria2 Service is parameterized as Service[string], with the
//     user identity (U) being the runtime user name "u<UID>". The service
//     hands that string back through the auth context, so metadata.User is
//     set directly to "u<UID>" with no index/name side table. This keeps it
//     consistent with the AnyTLS inbound and the accounting hash
//     (controller.buildUserHash → "u%d") with no extra bookkeeping.
//   - Panel-driven user refresh (full + incremental) via pinbound.go.
//   - ConfigureFromPanelNode hot-reloads panel-derived port/SNI. Because
//     hysteria2 runs over QUIC and the TLS config is baked into the QUIC
//     Service, a port or SNI change requires rebuilding the whole Service
//     (unlike AnyTLS, where TLS lives outside the service and only the
//     listener/TLS need swapping). The current user set is mirrored in
//     `currentUsers` so it can be re-applied to the rebuilt service.
type Inbound struct {
	inbound.Adapter
	ctx    context.Context
	router adapter.Router
	logger log.ContextLogger

	// reloadMu serializes ConfigureFromPanelNode against itself and Close.
	reloadMu sync.Mutex
	// started is set true after Start has begun listening. Before Start,
	// ConfigureFromPanelNode just stashes new options and rebuilds the
	// not-yet-started components; after Start it performs a live rebuild.
	started atomic.Bool

	// options is the current effective option set (guarded by reloadMu).
	options option.Hysteria2InboundOptions

	// listener/tlsConfig/service are the live components. They are swapped
	// under reloadMu during a hot reload. service is additionally guarded by
	// usersMu so the panel user-refresh path always sees a consistent
	// pointer when applying deltas.
	listener  *listener.Listener
	tlsConfig tls.ServerConfig
	service   *hysteria2.Service[string]

	// usersMu guards currentUsers (the user mirror) and serializes panel
	// user updates against the rebuild's user re-apply + service swap.
	usersMu      sync.Mutex
	currentUsers map[string]string // name "u<UID>" -> password
}

// buildComponents constructs the TLS config, UDP listener and hysteria2
// service from a set of options, without starting them or applying any
// users. It is used both by NewInbound and by the hot-reload rebuild path.
func buildComponents(ctx context.Context, logger log.ContextLogger, options option.Hysteria2InboundOptions, handler hysteria2.ServerHandler) (tls.ServerConfig, *listener.Listener, *hysteria2.Service[string], error) {
	options.UDPFragmentDefault = true
	if options.TLS == nil || !options.TLS.Enabled {
		return nil, nil, nil, C.ErrTLSRequired
	}
	tlsConfig, err := tls.NewServer(ctx, logger, common.PtrValueOrDefault(options.TLS))
	if err != nil {
		return nil, nil, nil, err
	}
	var salamanderPassword string
	if options.Obfs != nil {
		switch options.Obfs.Type {
		case hysteria2.ObfsTypeSalamander:
			// SingR divergence from upstream (which errors on an empty obfs
			// password): an empty password falls back to the TLS SNI
			// (server_name). The panel delivers the SNI via host=, so this
			// lets SSPanel effectively drive the node-wide-shared obfs secret
			// without a dedicated field. An explicit password wins. Clients
			// must use the same value as their obfs password.
			//
			// Note: because obfs is rebuilt with the service, the SNI-derived
			// password follows an SNI hot-reload; an explicit password stays
			// fixed.
			salamanderPassword = options.Obfs.Password
			if salamanderPassword == "" && options.TLS != nil {
				salamanderPassword = options.TLS.ServerName
			}
		default:
			return nil, nil, nil, E.New("unknown obfs type: ", options.Obfs.Type)
		}
	}
	masqueradeHandler, err := buildMasqueradeHandler(options)
	if err != nil {
		return nil, nil, nil, err
	}
	lis := listener.New(listener.Options{
		Context: ctx,
		Logger:  logger,
		Listen:  options.ListenOptions,
	})
	var udpTimeout time.Duration
	if options.UDPTimeout != 0 {
		udpTimeout = time.Duration(options.UDPTimeout)
	} else {
		udpTimeout = C.UDPTimeout
	}
	service, err := hysteria2.NewService[string](hysteria2.ServiceOptions{
		Context:               ctx,
		Logger:                logger,
		BrutalDebug:           options.BrutalDebug,
		SendBPS:               uint64(options.UpMbps * hysteria.MbpsToBps),
		ReceiveBPS:            uint64(options.DownMbps * hysteria.MbpsToBps),
		SalamanderPassword:    salamanderPassword,
		TLSConfig:             tlsConfig,
		IgnoreClientBandwidth: options.IgnoreClientBandwidth,
		UDPTimeout:            udpTimeout,
		Handler:               handler,
		MasqueradeHandler:     masqueradeHandler,
	})
	if err != nil {
		_ = common.Close(tlsConfig)
		return nil, nil, nil, err
	}
	return tlsConfig, lis, service, nil
}

func buildMasqueradeHandler(options option.Hysteria2InboundOptions) (http.Handler, error) {
	if options.Masquerade == nil || options.Masquerade.Type == "" {
		return nil, nil
	}
	switch options.Masquerade.Type {
	case C.Hysterai2MasqueradeTypeFile:
		return http.FileServer(http.Dir(options.Masquerade.FileOptions.Directory)), nil
	case C.Hysterai2MasqueradeTypeProxy:
		masqueradeURL, err := url.Parse(options.Masquerade.ProxyOptions.URL)
		if err != nil {
			return nil, E.Cause(err, "parse masquerade URL")
		}
		return &httputil.ReverseProxy{
			Rewrite: func(r *httputil.ProxyRequest) {
				r.SetURL(masqueradeURL)
				if !options.Masquerade.ProxyOptions.RewriteHost {
					r.Out.Host = r.In.Host
				}
			},
			ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
				w.WriteHeader(http.StatusBadGateway)
			},
		}, nil
	case C.Hysterai2MasqueradeTypeString:
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if options.Masquerade.StringOptions.StatusCode != 0 {
				w.WriteHeader(options.Masquerade.StringOptions.StatusCode)
			}
			for key, values := range options.Masquerade.StringOptions.Headers {
				for _, value := range values {
					w.Header().Add(key, value)
				}
			}
			w.Write([]byte(options.Masquerade.StringOptions.Content))
		}), nil
	default:
		return nil, E.New("unknown masquerade type: ", options.Masquerade.Type)
	}
}

func NewInbound(ctx context.Context, router adapter.Router, logger log.ContextLogger, tag string, options option.Hysteria2InboundOptions) (adapter.Inbound, error) {
	h := &Inbound{
		Adapter:      inbound.NewAdapter(C.TypeHysteria2, tag),
		ctx:          ctx,
		router:       router,
		logger:       logger,
		options:      options,
		currentUsers: make(map[string]string),
	}
	tlsConfig, lis, service, err := buildComponents(ctx, logger, options, h)
	if err != nil {
		return nil, err
	}
	h.tlsConfig = tlsConfig
	h.listener = lis
	h.service = service

	// Seed any users defined statically in the config file (SingR normally
	// drives users from the panel, but honor static entries too).
	names := make([]string, 0, len(options.Users))
	passwords := make([]string, 0, len(options.Users))
	for _, user := range options.Users {
		h.currentUsers[user.Name] = user.Password
		names = append(names, user.Name)
		passwords = append(passwords, user.Password)
	}
	service.UpdateUsers(names, passwords)
	return h, nil
}

// ConfigureFromPanelNode applies SSPanel-derived port and SNI.
//
// Before Start it stashes the new options and rebuilds the not-yet-started
// components so Start picks up panel-derived values.
//
// After Start it performs a live rebuild of the QUIC service:
//   - port change: bind the new port and start the new service before
//     closing the old one ("start new before close old"), so a bind
//     failure rolls back cleanly with the old service still serving.
//   - SNI-only change (same UDP port): the old listener must be closed
//     first to free the port (UDP cannot double-bind without SO_REUSEPORT),
//     so there is a brief restart window and no rollback once the old
//     service is closed.
//
// In both cases the current user set is re-applied to the rebuilt service
// from the `currentUsers` mirror.
//
// nodeInfo.Port must be in (0, 65535]; an out-of-range value is rejected
// and the previous configuration is left in place (defensive against a
// panel that briefly returns Port=0).
func (h *Inbound) ConfigureFromPanelNode(nodeInfo *api.NodeInfo) error {
	if nodeInfo == nil || nodeInfo.NodeType != C.TypeHysteria2 {
		return nil
	}
	if nodeInfo.Port <= 0 || nodeInfo.Port > 65535 {
		return E.New("invalid hysteria2 listen port from panel: ", nodeInfo.Port)
	}

	h.reloadMu.Lock()
	defer h.reloadMu.Unlock()

	newOpts := h.options
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

	portChanged := newOpts.ListenPort != h.options.ListenPort
	oldSNI := ""
	if h.options.TLS != nil {
		oldSNI = h.options.TLS.ServerName
	}
	newSNI := ""
	if newOpts.TLS != nil {
		newSNI = newOpts.TLS.ServerName
	}
	sniChanged := newSNI != oldSNI

	if h.started.Load() && !portChanged && !sniChanged {
		return nil
	}

	newTLS, newListener, newService, err := buildComponents(h.ctx, h.logger, newOpts, h)
	if err != nil {
		return err
	}

	// Pre-Start: just stash; Start will bring the components up.
	if !h.started.Load() {
		oldTLS, oldListener := h.tlsConfig, h.listener
		h.usersMu.Lock()
		h.applyUsersLocked(newService)
		h.service = newService
		h.usersMu.Unlock()
		h.tlsConfig = newTLS
		h.listener = newListener
		h.options = newOpts
		_ = common.Close(oldListener, oldTLS)
		return nil
	}

	// Post-Start live rebuild.
	oldTLS, oldListener, oldService := h.tlsConfig, h.listener, h.service

	if !portChanged {
		// Same UDP port: free it before binding the new listener.
		_ = closeComponents(nil, oldListener, oldService)
	}

	if err := startComponents(newTLS, newListener, newService); err != nil {
		_ = closeComponents(newTLS, newListener, newService)
		if !portChanged {
			// Old service was already closed to free the port; we cannot
			// rebind it. The inbound is now down until the next successful
			// reconfigure. Surface the error.
			h.logger.Error(E.Cause(err, "hysteria2 restart after SNI change failed; inbound is down"))
		}
		return E.Cause(err, "start reloaded hysteria2 listener on port ", newOpts.ListenPort)
	}

	h.usersMu.Lock()
	h.applyUsersLocked(newService)
	h.service = newService
	h.usersMu.Unlock()
	h.tlsConfig = newTLS
	h.listener = newListener
	h.options = newOpts

	if portChanged {
		_ = closeComponents(oldTLS, oldListener, oldService)
		h.logger.Info("hysteria2 listener hot-reloaded to port ", newOpts.ListenPort)
	} else {
		_ = common.Close(oldTLS)
		h.logger.Info("hysteria2 TLS hot-reloaded with SNI ", newSNI)
	}
	return nil
}

// applyUsersLocked pushes the current user mirror into the given service.
// Caller must hold usersMu.
func (h *Inbound) applyUsersLocked(service *hysteria2.Service[string]) {
	names := make([]string, 0, len(h.currentUsers))
	passwords := make([]string, 0, len(h.currentUsers))
	for name, password := range h.currentUsers {
		names = append(names, name)
		passwords = append(passwords, password)
	}
	service.UpdateUsers(names, passwords)
}

func startComponents(tlsConfig tls.ServerConfig, lis *listener.Listener, service *hysteria2.Service[string]) error {
	if tlsConfig != nil {
		if err := tlsConfig.Start(); err != nil {
			return err
		}
	}
	packetConn, err := lis.ListenUDP()
	if err != nil {
		return err
	}
	return service.Start(packetConn)
}

func closeComponents(tlsConfig tls.ServerConfig, lis *listener.Listener, service *hysteria2.Service[string]) error {
	return common.Close(
		common.PtrOrNil(service),
		lis,
		tlsConfig,
	)
}

func (h *Inbound) NewConnectionEx(ctx context.Context, conn net.Conn, source M.Socksaddr, destination M.Socksaddr, onClose N.CloseHandlerFunc) {
	ctx = log.ContextWithNewID(ctx)
	lis := h.listener
	var metadata adapter.InboundContext
	metadata.Inbound = h.Tag()
	metadata.InboundType = h.Type()
	//nolint:staticcheck
	metadata.InboundDetour = lis.ListenOptions().Detour
	//nolint:staticcheck
	metadata.OriginDestination = lis.UDPAddr()
	metadata.Source = source
	metadata.Destination = destination
	if userName, _ := auth.UserFromContext[string](ctx); userName != "" {
		metadata.User = userName
		h.logger.DebugContext(ctx, "[", userName, "] inbound connection to ", metadata.Destination)
	} else {
		h.logger.DebugContext(ctx, "inbound connection to ", metadata.Destination)
	}
	h.router.RouteConnectionEx(ctx, conn, metadata, onClose)
}

func (h *Inbound) NewPacketConnectionEx(ctx context.Context, conn N.PacketConn, source M.Socksaddr, destination M.Socksaddr, onClose N.CloseHandlerFunc) {
	ctx = log.ContextWithNewID(ctx)
	lis := h.listener
	var metadata adapter.InboundContext
	metadata.Inbound = h.Tag()
	metadata.InboundType = h.Type()
	//nolint:staticcheck
	metadata.InboundDetour = lis.ListenOptions().Detour
	//nolint:staticcheck
	metadata.OriginDestination = lis.UDPAddr()
	metadata.Source = source
	metadata.Destination = destination
	if userName, _ := auth.UserFromContext[string](ctx); userName != "" {
		metadata.User = userName
		h.logger.DebugContext(ctx, "[", userName, "] inbound packet connection to ", metadata.Destination)
	} else {
		h.logger.DebugContext(ctx, "inbound packet connection to ", metadata.Destination)
	}
	h.router.RoutePacketConnectionEx(ctx, conn, metadata, onClose)
}

func (h *Inbound) Start(stage adapter.StartStage) error {
	if stage != adapter.StartStateStart {
		return nil
	}
	h.reloadMu.Lock()
	defer h.reloadMu.Unlock()
	if err := startComponents(h.tlsConfig, h.listener, h.service); err != nil {
		return err
	}
	h.started.Store(true)
	return nil
}

func (h *Inbound) Close() error {
	h.reloadMu.Lock()
	defer h.reloadMu.Unlock()
	return closeComponents(h.tlsConfig, h.listener, h.service)
}

// isExpectedHysteria2HandshakeFailure classifies pre-auth QUIC/TLS noise so
// it can be logged at debug instead of error. Some relay/probe paths open a
// connection that never completes the hysteria2 auth; that is not a server
// fault. Real post-auth errors still surface at error from the service.
func isExpectedHysteria2HandshakeFailure(err error) bool {
	if errors.Is(err, io.EOF) || errors.Is(err, context.DeadlineExceeded) || E.IsClosedOrCanceled(err) {
		return true
	}
	message := err.Error()
	return strings.Contains(message, "first record does not look like a TLS handshake") ||
		strings.Contains(message, "no recent network activity")
}
