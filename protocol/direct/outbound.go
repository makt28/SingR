package direct

import (
	"context"
	"net"
	"net/netip"
	"reflect"
	"sync"
	"time"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/adapter/outbound"
	"github.com/sagernet/sing-box/common/dialer"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing-tun"
	"github.com/sagernet/sing-tun/ping"
	"github.com/sagernet/sing/common"
	"github.com/sagernet/sing/common/atomic"
	E "github.com/sagernet/sing/common/exceptions"
	"github.com/sagernet/sing/common/logger"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	"github.com/sagernet/sing/service"
)

func RegisterOutbound(registry *outbound.Registry) {
	outbound.Register[option.DirectOutboundOptions](registry, C.TypeDirect, NewOutbound)
}

var (
	_ N.ParallelDialer             = (*Outbound)(nil)
	_ dialer.ParallelNetworkDialer = (*Outbound)(nil)
	_ dialer.DirectDialer          = (*Outbound)(nil)
	_ adapter.DirectRouteOutbound  = (*Outbound)(nil)
)

// firstHitCap bounds the per-outbound (user, destHost) memo so a long-running
// process doesn't accumulate unbounded entries. Once the cap is reached every
// further connection is logged at debug level only.
const firstHitCap = 16384

type Outbound struct {
	outbound.Adapter
	ctx            context.Context
	logger         logger.ContextLogger
	network        adapter.NetworkManager
	dialer         dialer.ParallelInterfaceDialer
	domainStrategy C.DomainStrategy
	fallbackDelay  time.Duration
	isEmpty        bool
	myAddresses    common.TypedValue[[]netip.Prefix]

	// firstHitSeen memoizes (user, destination-host) pairs we've already
	// logged at Info. Subsequent connections in the same pair fall through
	// to Debug, which keeps the log volume bounded under load while still
	// surfacing "what does this user reach" at a glance.
	firstHitSeen  sync.Map
	firstHitCount atomic.Int64
}

func NewOutbound(ctx context.Context, router adapter.Router, logger log.ContextLogger, tag string, options option.DirectOutboundOptions) (adapter.Outbound, error) {
	options.UDPFragmentDefault = true
	if options.Detour != "" {
		return nil, E.New("`detour` is not supported in direct context")
	}
	outboundDialer, err := dialer.NewWithOptions(dialer.Options{
		Context:        ctx,
		Options:        options.DialerOptions,
		RemoteIsDomain: true,
		DirectOutbound: true,
	})
	if err != nil {
		return nil, err
	}
	outbound := &Outbound{
		Adapter: outbound.NewAdapterWithDialerOptions(C.TypeDirect, tag, []string{N.NetworkTCP, N.NetworkUDP, N.NetworkICMP}, options.DialerOptions),
		ctx:     ctx,
		logger:  logger,
		network: service.FromContext[adapter.NetworkManager](ctx),
		//nolint:staticcheck
		domainStrategy: C.DomainStrategy(options.DomainStrategy),
		fallbackDelay:  time.Duration(options.FallbackDelay),
		dialer:         outboundDialer.(dialer.ParallelInterfaceDialer),
		isEmpty:        reflect.DeepEqual(options.DialerOptions, option.DialerOptions{UDPFragmentDefault: true}),
	}
	//nolint:staticcheck
	if options.ProxyProtocol != 0 {
		return nil, E.New("Proxy Protocol is deprecated and removed in sing-box 1.6.0")
	}
	return outbound, nil
}

// shouldLogFirstHit returns true exactly once for each (user, destHost) pair
// observed on this outbound (until firstHitCap is reached). Caller is
// expected to log at Info on true and Debug otherwise.
func (h *Outbound) shouldLogFirstHit(user string, dest M.Socksaddr) bool {
	key := user + "|" + dest.AddrString()
	if _, ok := h.firstHitSeen.Load(key); ok {
		return false
	}
	if h.firstHitCount.Load() >= firstHitCap {
		return false
	}
	if _, loaded := h.firstHitSeen.LoadOrStore(key, struct{}{}); loaded {
		return false
	}
	h.firstHitCount.Add(1)
	return true
}

// logConn emits the per-connection outbound line. It picks Info for the very
// first hit of a (user, destHost) pair and Debug otherwise, keeping the
// production log readable without going dark.
func (h *Outbound) logConn(ctx context.Context, network string, destination M.Socksaddr) {
	user := ""
	if md := adapter.ContextFrom(ctx); md != nil {
		user = md.User
	}
	var msg string
	switch network {
	case N.NetworkTCP:
		msg = "outbound connection to "
	case N.NetworkUDP:
		msg = "outbound packet connection to "
	default:
		return
	}
	if h.shouldLogFirstHit(user, destination) {
		h.logger.InfoContext(ctx, msg, destination)
	} else {
		h.logger.DebugContext(ctx, msg, destination)
	}
}

func (h *Outbound) Start(stage adapter.StartStage) error {
	switch stage {
	case adapter.StartStatePostStart, adapter.StartStateStarted:
		h.fetchMyAddresses()
	}
	return nil
}

func (h *Outbound) fetchMyAddresses() {
	if len(h.myAddresses.Load()) > 0 {
		return
	}
	myInterfaceNames := h.network.InterfaceMonitor().MyInterfaces()
	if len(myInterfaceNames) == 0 {
		return
	}
	var myAddresses []netip.Prefix
	for _, myInterfaceName := range myInterfaceNames {
		myInterface, err := h.network.InterfaceFinder().ByName(myInterfaceName)
		if err != nil {
			continue
		}
		myAddresses = append(myAddresses, myInterface.Addresses...)
	}
	h.myAddresses.Store(myAddresses)
}

func (h *Outbound) isMyLoopbackAddress(addresses ...netip.Addr) bool {
	for _, prefix := range h.myAddresses.Load() {
		for _, address := range addresses {
			if prefix.Addr() != address && prefix.Contains(address) {
				return true
			}
		}
	}
	return false
}

func (h *Outbound) DialContext(ctx context.Context, network string, destination M.Socksaddr) (net.Conn, error) {
	if h.isMyLoopbackAddress(destination.Addr) {
		return nil, E.New("loopback connection to TUN range")
	}
	ctx, metadata := adapter.ExtendContext(ctx)
	metadata.Outbound = h.Tag()
	metadata.Destination = destination
	network = N.NetworkName(network)
	h.logConn(ctx, network, destination)
	return h.dialer.DialContext(ctx, network, destination)
}

func (h *Outbound) ListenPacket(ctx context.Context, destination M.Socksaddr) (net.PacketConn, error) {
	if h.isMyLoopbackAddress(destination.Addr) {
		return nil, E.New("loopback connection to TUN range")
	}
	ctx, metadata := adapter.ExtendContext(ctx)
	metadata.Outbound = h.Tag()
	metadata.Destination = destination
	h.logConn(ctx, N.NetworkUDP, destination)
	conn, err := h.dialer.ListenPacket(ctx, destination)
	if err != nil {
		return nil, err
	}
	return conn, nil
}

func (h *Outbound) NewDirectRouteConnection(metadata adapter.InboundContext, routeContext tun.DirectRouteContext, timeout time.Duration) (tun.DirectRouteDestination, error) {
	ctx := log.ContextWithNewID(h.ctx)
	destination, err := ping.ConnectDestination(ctx, h.logger, common.MustCast[*dialer.DefaultDialer](h.dialer).DialerForICMPDestination(metadata.Destination.Addr).Control, metadata.Destination.Addr, routeContext, timeout)
	if err != nil {
		return nil, err
	}
	h.logger.InfoContext(ctx, "linked ", metadata.Network, " connection from ", metadata.Source.AddrString(), " to ", metadata.Destination.AddrString())
	return destination, nil
}

func (h *Outbound) DialParallel(ctx context.Context, network string, destination M.Socksaddr, destinationAddresses []netip.Addr) (net.Conn, error) {
	if h.isMyLoopbackAddress(destinationAddresses...) {
		return nil, E.New("loopback connection to TUN range")
	}
	ctx, metadata := adapter.ExtendContext(ctx)
	metadata.Outbound = h.Tag()
	metadata.Destination = destination
	network = N.NetworkName(network)
	h.logConn(ctx, network, destination)
	return dialer.DialParallelNetwork(ctx, h.dialer, network, destination, destinationAddresses, len(destinationAddresses) > 0 && destinationAddresses[0].Is6(), nil, nil, nil, h.fallbackDelay)
}

func (h *Outbound) DialParallelNetwork(ctx context.Context, network string, destination M.Socksaddr, destinationAddresses []netip.Addr, networkStrategy *C.NetworkStrategy, networkType []C.InterfaceType, fallbackNetworkType []C.InterfaceType, fallbackDelay time.Duration) (net.Conn, error) {
	if h.isMyLoopbackAddress(destinationAddresses...) {
		return nil, E.New("loopback connection to TUN range")
	}
	ctx, metadata := adapter.ExtendContext(ctx)
	metadata.Outbound = h.Tag()
	metadata.Destination = destination
	network = N.NetworkName(network)
	h.logConn(ctx, network, destination)
	return dialer.DialParallelNetwork(ctx, h.dialer, network, destination, destinationAddresses, len(destinationAddresses) > 0 && destinationAddresses[0].Is6(), networkStrategy, networkType, fallbackNetworkType, fallbackDelay)
}

func (h *Outbound) ListenSerialNetworkPacket(ctx context.Context, destination M.Socksaddr, destinationAddresses []netip.Addr, networkStrategy *C.NetworkStrategy, networkType []C.InterfaceType, fallbackNetworkType []C.InterfaceType, fallbackDelay time.Duration) (net.PacketConn, netip.Addr, error) {
	if h.isMyLoopbackAddress(destinationAddresses...) {
		return nil, netip.Addr{}, E.New("loopback connection to TUN range")
	}
	ctx, metadata := adapter.ExtendContext(ctx)
	metadata.Outbound = h.Tag()
	metadata.Destination = destination
	h.logConn(ctx, N.NetworkUDP, destination)
	conn, newDestination, err := dialer.ListenSerialNetworkPacket(ctx, h.dialer, destination, destinationAddresses, networkStrategy, networkType, fallbackNetworkType, fallbackDelay)
	if err != nil {
		return nil, netip.Addr{}, err
	}
	return conn, newDestination, nil
}

func (h *Outbound) IsEmpty() bool {
	return h.isEmpty
}
