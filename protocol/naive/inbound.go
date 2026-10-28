package naive

import (
	"context"
	"io"
	"net"
	"net/http"
	"sync"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/adapter/inbound"
	"github.com/sagernet/sing-box/common/listener"
	"github.com/sagernet/sing-box/common/tls"
	"github.com/sagernet/sing-box/common/uot"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing-box/transport/v2rayhttp"
	"github.com/sagernet/sing/common"
	"github.com/sagernet/sing/common/atomic"
	"github.com/sagernet/sing/common/buf"
	"github.com/sagernet/sing/common/bufio"
	E "github.com/sagernet/sing/common/exceptions"
	"github.com/sagernet/sing/common/logger"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	sHttp "github.com/sagernet/sing/protocol/http"
)

var (
	ConfigureHTTP3ListenerFunc func(ctx context.Context, logger logger.Logger, listener *listener.Listener, handler http.Handler, tlsConfig tls.ServerConfig, options option.NaiveInboundOptions) (io.Closer, error)
	WrapError                  func(error) error
)

func RegisterInbound(registry *inbound.Registry) {
	inbound.Register[option.NaiveInboundOptions](registry, C.TypeNaive, NewInbound)
}

type Inbound struct {
	inbound.Adapter
	ctx              context.Context
	router           adapter.ConnectionRouterEx
	logger           logger.ContextLogger
	options          option.NaiveInboundOptions
	listener         *listener.Listener
	network          []string
	networkIsDefault bool
	users            *userTable
	tlsConfig        tls.ServerConfig
	httpServer       *http.Server

	// SingR panel hot reload, see singr_reload.go.
	//
	// reloadMu serializes Start, Close and ConfigureFromPanelNode. tlsMu
	// guards tlsConfig against the accept paths (tlsListener for TCP,
	// tlsSwitch for QUIC), which read it per connection so an SNI change
	// takes effect without rebinding. detour is captured at construction so
	// the request path never reads options/listener while a reload swaps
	// them.
	reloadMu  sync.Mutex
	tlsMu     sync.RWMutex
	started   atomic.Bool
	closed    bool
	h3Server  io.Closer
	tlsSwitch *tlsSwitch
	detour    string
}

func NewInbound(ctx context.Context, router adapter.Router, logger log.ContextLogger, tag string, options option.NaiveInboundOptions) (adapter.Inbound, error) {
	inbound := &Inbound{
		Adapter: inbound.NewAdapter(C.TypeNaive, tag),
		ctx:     ctx,
		router:  uot.NewRouter(router, logger),
		logger:  logger,
		options: options,
		listener: listener.New(listener.Options{
			Context: ctx,
			Logger:  logger,
			Listen:  options.ListenOptions,
		}),
		networkIsDefault: options.Network == "",
		network:          options.Network.Build(),
		users:            newUserTable(options.Users),
		//nolint:staticcheck
		detour: options.ListenOptions.Detour,
	}
	inbound.tlsSwitch = newTLSSwitch(inbound)
	if common.Contains(inbound.network, N.NetworkUDP) {
		if options.TLS == nil || !options.TLS.Enabled {
			return nil, E.New("TLS is required for QUIC server")
		}
	}
	// SingR: no "missing users" check — a panel-driven inbound starts with
	// `users: []` and is filled by the panel before Start.
	if options.TLS != nil {
		tlsConfig, err := newNaiveTLS(ctx, logger, options, inbound.network)
		if err != nil {
			return nil, err
		}
		inbound.tlsConfig = tlsConfig
	}
	return inbound, nil
}

func (n *Inbound) Start(stage adapter.StartStage, scope *adapter.Scope) error {
	if stage != adapter.StartStateStart {
		return nil
	}
	// Hold reloadMu so a concurrent pre-Start ConfigureFromPanelNode cannot
	// swap listener/TLS out from under us. Register our own Close rather
	// than upstream's scope.Add(n.listener.Close) / scope.Add(n.httpServer.Close):
	// those method values bind what exists now, but hot reload swaps them,
	// so shutdown would close the stale ones and leak the live ones.
	// Registered first so a partial Start is still cleaned up.
	n.reloadMu.Lock()
	defer n.reloadMu.Unlock()
	scope.Add(n.Close)
	if n.tlsConfig != nil {
		err := n.tlsConfig.Start()
		if err != nil {
			return E.Cause(err, "create TLS config")
		}
	}
	httpServer, h3Server, err := n.serve(n.listener)
	if err != nil {
		return err
	}
	n.httpServer, n.h3Server = httpServer, h3Server
	n.started.Store(true)
	return nil
}

func (n *Inbound) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	ctx := log.ContextWithNewID(request.Context())
	if request.Method != "CONNECT" {
		rejectHTTP(writer, http.StatusBadRequest)
		n.badRequest(ctx, request, E.New("not CONNECT request"))
		return
	} else if request.Header.Get("Padding") == "" {
		rejectHTTP(writer, http.StatusBadRequest)
		n.badRequest(ctx, request, E.New("missing naive padding"))
		return
	}
	// SingR: authenticate by password only and take the identity from the
	// user table, never from the client-supplied username — metadata.User
	// drives accounting.
	var userName string
	_, password, authOk := sHttp.ParseBasicAuth(request.Header.Get("Proxy-Authorization"))
	if authOk {
		userName, authOk = n.users.lookup(password)
	}
	if !authOk {
		rejectHTTP(writer, http.StatusProxyAuthRequired)
		n.badRequest(ctx, request, E.New("authorization failed"))
		return
	}
	writer.Header().Set("Padding", generatePaddingHeader())
	writer.WriteHeader(http.StatusOK)
	writer.(http.Flusher).Flush()

	hostPort := request.Header.Get("-connect-authority")
	if hostPort == "" {
		hostPort = request.URL.Host
		if hostPort == "" {
			hostPort = request.Host
		}
	}
	source := sHttp.SourceAddress(request)
	destination := M.ParseSocksaddr(hostPort).Unwrap()

	if hijacker, isHijacker := writer.(http.Hijacker); isHijacker {
		conn, reader, err := hijacker.Hijack()
		if err != nil {
			n.badRequest(ctx, request, E.New("hijack failed"))
			return
		}
		if cacheLen := reader.Reader.Buffered(); cacheLen > 0 {
			cache := buf.NewSize(cacheLen)
			_, err = cache.ReadFullFrom(reader.Reader, cacheLen)
			if err != nil {
				cache.Release()
				conn.Close()
				n.badRequest(ctx, request, E.Cause(err, "read cache"))
				return
			}
			conn = bufio.NewCachedConn(conn, cache)
		}
		n.newConnection(ctx, false, &naiveConn{Conn: conn}, userName, source, destination)
	} else {
		n.newConnection(ctx, true, &naiveH2Conn{
			reader:        request.Body,
			writer:        writer,
			flusher:       writer.(http.Flusher),
			remoteAddress: source,
		}, userName, source, destination)
	}
}

func (n *Inbound) newConnection(ctx context.Context, waitForClose bool, conn net.Conn, userName string, source M.Socksaddr, destination M.Socksaddr) {
	if userName != "" {
		n.logger.DebugContext(ctx, "[", userName, "] inbound connection from ", source)
		n.logger.DebugContext(ctx, "[", userName, "] inbound connection to ", destination)
	} else {
		n.logger.DebugContext(ctx, "inbound connection from ", source)
		n.logger.DebugContext(ctx, "inbound connection to ", destination)
	}
	var metadata adapter.InboundContext
	metadata.Inbound = n.Tag()
	metadata.InboundType = n.Type()
	//nolint:staticcheck
	metadata.InboundDetour = n.detour
	//nolint:staticcheck
	metadata.Source = source
	metadata.Destination = destination
	metadata.OriginDestination = M.SocksaddrFromNet(conn.LocalAddr()).Unwrap()
	metadata.User = userName
	if !waitForClose {
		n.router.RouteConnectionEx(ctx, conn, metadata, nil)
	} else {
		done := make(chan struct{})
		wrapper := v2rayhttp.NewHTTP2Wrapper(conn)
		n.router.RouteConnectionEx(ctx, wrapper, metadata, N.OnceClose(func(it error) {
			close(done)
		}))
		<-done
		wrapper.CloseWrapper()
	}
}

// badRequest covers probes and misconfigured clients (not CONNECT, no
// padding, wrong password), so SingR logs it at debug, like anytls'
// pre-auth handshake noise.
func (n *Inbound) badRequest(ctx context.Context, request *http.Request, err error) {
	n.logger.DebugContext(ctx, E.Cause(err, "process connection from ", request.RemoteAddr))
}

func rejectHTTP(writer http.ResponseWriter, statusCode int) {
	hijacker, ok := writer.(http.Hijacker)
	if !ok {
		writer.WriteHeader(statusCode)
		return
	}
	conn, _, err := hijacker.Hijack()
	if err != nil {
		writer.WriteHeader(statusCode)
		return
	}
	if tcpConn, isTCP := common.Cast[*net.TCPConn](conn); isTCP {
		tcpConn.SetLinger(0)
	}
	conn.Close()
}
