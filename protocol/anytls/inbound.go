package anytls

import (
	"context"
	"net"
	"strings"
	"sync"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/adapter/inbound"
	"github.com/sagernet/sing-box/common/listener"
	"github.com/sagernet/sing-box/common/tls"
	"github.com/sagernet/sing-box/common/uot"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing-box/poet/api"
	"github.com/sagernet/sing/common"
	"github.com/sagernet/sing/common/atomic"
	"github.com/sagernet/sing/common/auth"
	E "github.com/sagernet/sing/common/exceptions"
	"github.com/sagernet/sing/common/logger"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"

	anytls "github.com/makt28/sing-anytls"
	"github.com/makt28/sing-anytls/padding"
)

func RegisterInbound(registry *inbound.Registry) {
	inbound.Register[option.AnyTLSInboundOptions](registry, C.TypeAnyTLS, NewInbound)
}

type Inbound struct {
	inbound.Adapter
	tlsConfig tls.ServerConfig
	router    adapter.ConnectionRouterEx
	logger    logger.ContextLogger
	listener  *listener.Listener
	service   *anytls.Service
	ctx       context.Context
	options   option.AnyTLSInboundOptions

	// reloadMu serializes ConfigureFromPanelNode and Close.
	reloadMu sync.Mutex
	// tlsMu guards reads of tlsConfig from the connection accept path
	// against hot-reload swaps.
	tlsMu sync.RWMutex
	// started is set true after Start has successfully begun listening.
	// Before Start, ConfigureFromPanelNode just stashes new options;
	// after Start, ConfigureFromPanelNode performs a live listener/TLS swap.
	started atomic.Bool
}

func NewInbound(ctx context.Context, router adapter.Router, logger log.ContextLogger, tag string, options option.AnyTLSInboundOptions) (adapter.Inbound, error) {
	inbound := &Inbound{
		Adapter: inbound.NewAdapter(C.TypeAnyTLS, tag),
		router:  uot.NewRouter(router, logger),
		logger:  logger,
		ctx:     ctx,
		options: options,
	}

	if options.TLS != nil && options.TLS.Enabled {
		tlsConfig, err := tls.NewServer(ctx, logger, common.PtrValueOrDefault(options.TLS))
		if err != nil {
			return nil, err
		}
		inbound.tlsConfig = tlsConfig
	}

	paddingScheme := padding.DefaultPaddingScheme
	if len(options.PaddingScheme) > 0 {
		paddingScheme = []byte(strings.Join(options.PaddingScheme, "\n"))
	}

	service, err := anytls.NewService(anytls.ServiceConfig{
		Users: common.Map(options.Users, func(it option.AnyTLSUser) anytls.User {
			return (anytls.User)(it)
		}),
		PaddingScheme: paddingScheme,
		Handler:       (*inboundHandler)(inbound),
		Logger:        logger,
	})
	if err != nil {
		return nil, err
	}
	inbound.service = service
	inbound.listener = listener.New(listener.Options{
		Context:           ctx,
		Logger:            logger,
		Network:           []string{N.NetworkTCP},
		Listen:            options.ListenOptions,
		ConnectionHandler: inbound,
	})
	return inbound, nil
}

// ConfigureFromPanelNode applies SSPanel-derived port and SNI to the inbound.
//
// Before Start it just stashes the new options and rebuilds the (not yet
// started) listener/TLS so that Start picks up panel-derived values.
//
// After Start it performs an in-place hot reload:
//   - port change: bring up a new listener first, then close the old one
//     ("start new before close old" — minimizes connection loss but may
//     briefly hold both sockets; on EADDRINUSE the new listener fails and
//     the old one keeps running, so callers get a clean rollback).
//   - SNI change: rebuild tlsConfig and atomically swap. In-flight handshakes
//     keep using the old tlsConfig they captured before the swap.
//
// If neither port nor SNI changed, ConfigureFromPanelNode is a no-op.
//
// nodeInfo.Port must be in (0, 65535]; an out-of-range value is an error and
// the previous configuration is left in place. Defensive against a panel
// that briefly returns Port=0 or other bad values.
func (h *Inbound) ConfigureFromPanelNode(nodeInfo *api.NodeInfo) error {
	if nodeInfo == nil || nodeInfo.NodeType != C.TypeAnyTLS {
		return nil
	}
	if nodeInfo.Port <= 0 || nodeInfo.Port > 65535 {
		return E.New("invalid anytls listen port from panel: ", nodeInfo.Port)
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

	started := h.started.Load()
	if started && !portChanged && !sniChanged {
		return nil
	}

	// Pre-Start path: behave like the previous startup-only override —
	// stash new options and rebuild the not-yet-started listener/TLS.
	if !started {
		if newOpts.TLS != nil && newOpts.TLS.Enabled {
			tlsConfig, err := tls.NewServer(h.ctx, h.logger, common.PtrValueOrDefault(newOpts.TLS))
			if err != nil {
				return err
			}
			if h.tlsConfig != nil {
				_ = common.Close(h.tlsConfig)
			}
			h.tlsConfig = tlsConfig
		}
		h.listener = listener.New(listener.Options{
			Context:           h.ctx,
			Logger:            h.logger,
			Network:           []string{N.NetworkTCP},
			Listen:            newOpts.ListenOptions,
			ConnectionHandler: h,
		})
		h.options = newOpts
		return nil
	}

	// Hot reload path.
	var newTLS tls.ServerConfig
	if sniChanged && newOpts.TLS != nil && newOpts.TLS.Enabled {
		var err error
		newTLS, err = tls.NewServer(h.ctx, h.logger, common.PtrValueOrDefault(newOpts.TLS))
		if err != nil {
			return err
		}
		if err := newTLS.Start(); err != nil {
			_ = common.Close(newTLS)
			return E.Cause(err, "start reloaded anytls tls")
		}
	}

	var newListener *listener.Listener
	if portChanged {
		newListener = listener.New(listener.Options{
			Context:           h.ctx,
			Logger:            h.logger,
			Network:           []string{N.NetworkTCP},
			Listen:            newOpts.ListenOptions,
			ConnectionHandler: h,
		})
		if err := newListener.Start(); err != nil {
			if newTLS != nil {
				_ = common.Close(newTLS)
			}
			return E.Cause(err, "start reloaded anytls listener on port ", newOpts.ListenPort)
		}
	}

	if newListener != nil {
		oldListener := h.listener
		h.listener = newListener
		if oldListener != nil {
			_ = oldListener.Close()
		}
		h.logger.Info("anytls listener hot-reloaded to port ", newOpts.ListenPort)
	}

	if newTLS != nil {
		h.tlsMu.Lock()
		oldTLS := h.tlsConfig
		h.tlsConfig = newTLS
		h.tlsMu.Unlock()
		if oldTLS != nil {
			_ = common.Close(oldTLS)
		}
		h.logger.Info("anytls TLS hot-reloaded with SNI ", newSNI)
	}

	h.options = newOpts
	return nil
}

func (h *Inbound) Start(stage adapter.StartStage) error {
	if stage != adapter.StartStateStart {
		return nil
	}
	if h.tlsConfig != nil {
		err := h.tlsConfig.Start()
		if err != nil {
			return err
		}
	}
	if err := h.listener.Start(); err != nil {
		return err
	}
	h.started.Store(true)
	return nil
}

func (h *Inbound) Close() error {
	h.reloadMu.Lock()
	defer h.reloadMu.Unlock()
	return common.Close(h.listener, h.tlsConfig)
}

func (h *Inbound) NewConnectionEx(ctx context.Context, conn net.Conn, metadata adapter.InboundContext, onClose N.CloseHandlerFunc) {
	h.tlsMu.RLock()
	tlsCfg := h.tlsConfig
	h.tlsMu.RUnlock()
	if tlsCfg != nil {
		tlsConn, err := tls.ServerHandshake(ctx, conn, tlsCfg)
		if err != nil {
			N.CloseOnHandshakeFailure(conn, onClose, err)
			h.logger.ErrorContext(ctx, E.Cause(err, "process connection from ", metadata.Source, ": TLS handshake"))
			return
		}
		conn = tlsConn
	}
	err := h.service.NewConnection(adapter.WithContext(ctx, &metadata), conn, metadata.Source, onClose)
	if err != nil {
		N.CloseOnHandshakeFailure(conn, onClose, err)
		h.logger.ErrorContext(ctx, E.Cause(err, "process connection from ", metadata.Source))
	}
}

type inboundHandler Inbound

func (h *inboundHandler) NewConnectionEx(ctx context.Context, conn net.Conn, source M.Socksaddr, destination M.Socksaddr, onClose N.CloseHandlerFunc) {
	var metadata adapter.InboundContext
	metadata.Inbound = h.Tag()
	metadata.InboundType = h.Type()
	//nolint:staticcheck
	metadata.InboundDetour = h.listener.ListenOptions().Detour
	//nolint:staticcheck
	metadata.Source = source
	metadata.Destination = destination.Unwrap()
	if userName, _ := auth.UserFromContext[string](ctx); userName != "" {
		metadata.User = userName
		h.logger.DebugContext(ctx, "[", userName, "] inbound connection to ", metadata.Destination)
	} else {
		h.logger.DebugContext(ctx, "inbound connection to ", metadata.Destination)
	}
	h.router.RouteConnectionEx(ctx, conn, metadata, onClose)
}
