package poet

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing/common/buf"
	"github.com/sagernet/sing/common/bufio"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"

	"github.com/sagernet/sing-box/poet/controller"
	"github.com/sagernet/sing-box/poet/panel"
	SS "github.com/sagernet/sing-box/poet/shortcuts"

	"github.com/spf13/viper"
)

var (
	panelAccess sync.Mutex
	activePanel *panel.Panel
)

// getConfig reads the panel config file.
//
// viper stays on purpose: it decodes through mapstructure with weak typing
// (a quoted "NodeID": "41" still lands in an int) and picks the format from
// the file extension, and panel.json files written by hand or by old
// installers may depend on either. Swapping in encoding/json would turn such
// a file into a node that no longer starts after an update.
func getConfig(cfgFile string) (*viper.Viper, error) {
	config := viper.New()

	// Set custom path and name
	if cfgFile != "" {
		configName := path.Base(cfgFile)
		configFileExt := path.Ext(cfgFile)
		configNameOnly := strings.TrimSuffix(configName, configFileExt)
		configPath := path.Dir(cfgFile)
		config.SetConfigName(configNameOnly)
		config.SetConfigType(strings.TrimPrefix(configFileExt, "."))
		config.AddConfigPath(configPath)
	} else {
		// Set default config path
		config.SetConfigName("panel")
		config.SetConfigType("json")
		config.AddConfigPath(".")

	}

	if err := config.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("read panel config %s: %w", cfgFile, err)
	}
	return config, nil
}

// LoadPanelConfig reads and parses a poet panel config file. Exported so
// the sing-box run command can inspect the referenced inbound tags before
// constructing the box (see filterInboundsByPanel in cmd_run.go).
func LoadPanelConfig(cfgFile string) (*panel.Config, error) {
	config, err := getConfig(cfgFile)
	if err != nil {
		return nil, err
	}
	panelConfig := &panel.Config{}
	if err := config.Unmarshal(panelConfig); err != nil {
		return nil, fmt.Errorf("parse panel config %s: %w", cfgFile, err)
	}
	return panelConfig, nil
}

// Start loads the panel config and starts every panel node. A box cannot
// run without its panel, so every failure here is returned and fails the
// box's startup (the process exits non-zero through the normal path).
func Start() error {
	ShowVersion()

	cc, _ := SS.GetObject("poetConfigPath")
	configPath, _ := cc.(string)
	if configPath == "" {
		return errors.New("empty panel config path: run with -p <panel.json>")
	}

	panelConfig, err := LoadPanelConfig(configPath)
	if err != nil {
		return err
	}

	p := panel.NewPanel(panelConfig)
	if err := p.Start(); err != nil {
		return err
	}

	panelAccess.Lock()
	if activePanel != nil && activePanel.Running {
		activePanel.Close()
	}
	activePanel = p
	panelAccess.Unlock()

	return nil
}

// Stop stops the running panel. It also forgets the inbounds registered so
// far: after a config reload the next box registers its own, and an inbound
// that is gone from server.json must not be found by a panel node that still
// names its tag (it would drive a closed inbound instead of failing with
// "no Node with InTag found").
func Stop() {
	panelAccess.Lock()
	defer panelAccess.Unlock()
	SS.Singleton().ClearInbounds()
	if activePanel == nil {
		return
	}
	activePanel.Close()
	activePanel = nil
}

// 设置日志
func SetLogger(logFactory log.Factory) {
	inst := SS.Singleton()
	inst.Logger = logFactory.NewLogger("singr")
}

// SetInbound registers an inbound with the panel layer. Called by the
// inbound manager for every inbound it creates.
func SetInbound(in *adapter.Inbound, tag string) {
	SS.Singleton().SetInbound(in, tag)
}

// trafficDebug is enabled by SINGR_TRAFFIC_DEBUG=1 and turns on per-conn /
// per-user / per-report logs that let us verify byte accounting against
// ground truth in sim. Off in production = zero overhead.
var trafficDebug = os.Getenv("SINGR_TRAFFIC_DEBUG") == "1"

func TrafficDebug() bool { return trafficDebug }

type loggingConn struct {
	net.Conn
	perRead  *atomic.Int64
	perWrite *atomic.Int64
	tag      string
	closed   atomic.Bool
}

func (c *loggingConn) Close() error {
	err := c.Conn.Close()
	if c.closed.CompareAndSwap(false, true) {
		SS.Singleton().Logger.Debug(fmt.Sprintf("[TRAFFIC] conn close %s read=%d write=%d", c.tag, c.perRead.Load(), c.perWrite.Load()))
	}
	return err
}

// rateLimitedConn wraps a net.Conn with the user's SHARED token bucket
// (XrayR semantics: one bucket gated by both directions, so up+down
// compete for the same budget). It is layered OUTSIDE
// bufio.NewInt64CounterConn so that the limiter waits happen after the
// bytes are already counted — the limiter only paces the caller, it
// never under-reports. user.WaitN with a nil limiter returns
// immediately, so non-limited users have ~zero overhead per Read/Write
// (one RLock).
//
// Each individual byte transits the inbound conn exactly once (Read xor
// Write), so a single byte never double-charges the bucket. Read =
// upload, Write = download.
type rateLimitedConn struct {
	net.Conn
	user *controller.User
	ctx  context.Context
}

func (c *rateLimitedConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if n > 0 {
		_ = c.user.WaitN(c.ctx, n)
	}
	return n, err
}

func (c *rateLimitedConn) Write(b []byte) (int, error) {
	if len(b) > 0 {
		_ = c.user.WaitN(c.ctx, len(b))
	}
	return c.Conn.Write(b)
}

// rateLimitedPacketConn wraps a PacketConn the same way for UDP using
// the same shared bucket. Without this, an unlimited UDP path can fully
// bypass per-user speed control.
type rateLimitedPacketConn struct {
	N.PacketConn
	user *controller.User
	ctx  context.Context
}

func (c *rateLimitedPacketConn) ReadPacket(buffer *buf.Buffer) (M.Socksaddr, error) {
	dest, err := c.PacketConn.ReadPacket(buffer)
	if buffer.Len() > 0 {
		_ = c.user.WaitN(c.ctx, buffer.Len())
	}
	return dest, err
}

func (c *rateLimitedPacketConn) WritePacket(buffer *buf.Buffer, destination M.Socksaddr) error {
	if buffer.Len() > 0 {
		_ = c.user.WaitN(c.ctx, buffer.Len())
	}
	return c.PacketConn.WritePacket(buffer, destination)
}

// blockedConn is returned by RoutedConnection when the connection must not
// go through: an audit rule rejects the destination, or the user is over
// their device limit. The underlying conn is closed up front; any further
// Read/Write surfaces a sentinel error so the dispatcher unwinds without
// pumping bytes.
type blockedConn struct {
	net.Conn
	reason error
}

func (c *blockedConn) Read(b []byte) (int, error)  { return 0, c.reason }
func (c *blockedConn) Write(b []byte) (int, error) { return 0, c.reason }

// blockedPacketConn is the UDP counterpart of blockedConn.
type blockedPacketConn struct {
	N.PacketConn
	reason error
}

func (c *blockedPacketConn) ReadPacket(buffer *buf.Buffer) (M.Socksaddr, error) {
	return M.Socksaddr{}, c.reason
}

func (c *blockedPacketConn) WritePacket(buffer *buf.Buffer, destination M.Socksaddr) error {
	buffer.Release()
	return c.reason
}

var (
	errAuditBlocked = errors.New("singr: connection blocked by audit rule")
	errDeviceLimit  = errors.New("singr: connection blocked by device limit")
)

// 计费
func RoutedConnection(ctx context.Context, conn net.Conn, metadata adapter.InboundContext) net.Conn {
	var readCounter []*atomic.Int64
	var writeCounter []*atomic.Int64

	ss := SS.Singleton()
	ctrl := ss.ControllerByInTag(metadata.Inbound)
	if ctrl == nil {
		// No logger means the panel was never started (a box without -p
		// routing a connection): nothing to account, nothing to report.
		if ss.Logger != nil {
			ss.Logger.Warn(fmt.Sprintf("RecordUserTraffic: inbound:%s controller not found", metadata.Inbound))
		}
		return conn
	}

	sendPtr, recvPtr, user, err := ctrl.LoadOrCreateUserCounter(ctx, metadata)
	if errors.Is(err, controller.ErrDeviceLimitExceeded) {
		// Must be closed, not returned as is: an unwrapped conn would go
		// through unmetered.
		ss.Logger.Debug(fmt.Sprintf("device limit blocked UID=%d source=%s inbound=%s", user.UID, metadata.Source.AddrString(), metadata.Inbound))
		_ = conn.Close()
		return &blockedConn{Conn: conn, reason: errDeviceLimit}
	}
	if err != nil {
		ss.Logger.Warn(fmt.Sprintf("RecordUserTraffic: inbound:%s user:%s counter pointer not found", metadata.Inbound, metadata.User))
		return conn
	}
	readCounter = append(readCounter, sendPtr)
	writeCounter = append(writeCounter, recvPtr)

	// Audit (TCP only; UDP destinations are usually IPs not FQDNs).
	// Match against the destination FQDN; on hit, close the inbound
	// conn immediately and surface a sentinel error to downstream
	// dispatch. The DetectResult is recorded inside MatchAudit and
	// reported by userInfoMonitor.
	if metadata.Destination.IsFqdn() {
		if matched, ruleID := ctrl.MatchAudit(user.UID, metadata.Destination.Fqdn); matched {
			ss.Logger.Info(fmt.Sprintf("audit blocked UID=%d host=%s ruleID=%d", user.UID, metadata.Destination.Fqdn, ruleID))
			_ = conn.Close()
			return &blockedConn{Conn: conn, reason: errAuditBlocked}
		}
	}

	var wrapped net.Conn
	if trafficDebug {
		perRead := &atomic.Int64{}
		perWrite := &atomic.Int64{}
		readCounter = append(readCounter, perRead)
		writeCounter = append(writeCounter, perWrite)
		tag := fmt.Sprintf("user=%s inbound=%s src=%s dst=%s", metadata.User, metadata.Inbound, metadata.Source.AddrString(), metadata.Destination.AddrString())
		ss.Logger.Debug(fmt.Sprintf("[TRAFFIC] conn wrap %s sendPtrPre=%d recvPtrPre=%d", tag, sendPtr.Load(), recvPtr.Load()))
		counted := bufio.NewInt64CounterConn(conn, readCounter, writeCounter)
		wrapped = &loggingConn{Conn: counted, perRead: perRead, perWrite: perWrite, tag: tag}
	} else {
		wrapped = bufio.NewInt64CounterConn(conn, readCounter, writeCounter)
	}

	// Layer the rate limiter outside the counter so counters are
	// authoritative — limiter waits do not affect what got counted.
	return &rateLimitedConn{Conn: wrapped, user: user, ctx: ctx}
}

func RoutedPacketConnection(ctx context.Context, conn N.PacketConn, metadata adapter.InboundContext) N.PacketConn {
	ss := SS.Singleton()
	ctrl := ss.ControllerByInTag(metadata.Inbound)
	if ctrl == nil {
		if ss.Logger != nil {
			ss.Logger.Warn(fmt.Sprintf("RecordPacketTraffic: inbound:%s controller not found", metadata.Inbound))
		}
		return conn
	}

	sendPtr, recvPtr, user, err := ctrl.LoadOrCreateUserCounter(ctx, metadata)
	if errors.Is(err, controller.ErrDeviceLimitExceeded) {
		ss.Logger.Debug(fmt.Sprintf("device limit blocked UID=%d source=%s inbound=%s (udp)", user.UID, metadata.Source.AddrString(), metadata.Inbound))
		_ = conn.Close()
		return &blockedPacketConn{PacketConn: conn, reason: errDeviceLimit}
	}
	if err != nil {
		ss.Logger.Warn(fmt.Sprintf("RecordPacketTraffic: inbound:%s user:%s counter pointer not found", metadata.Inbound, metadata.User))
		return conn
	}

	counted := bufio.NewInt64CounterPacketConn(conn, []*atomic.Int64{sendPtr}, nil, []*atomic.Int64{recvPtr}, nil)
	return &rateLimitedPacketConn{PacketConn: counted, user: user, ctx: ctx}
}
