package shortcuts

import (
	"context"
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

// type singPoet struct {
// }

// var instance *singPoet

// func init() {
// 	instance = new(singPoet)
// }

// func singleton() *singPoet {
// 	return instance
// }

// 获取配置文件
func getConfig(cfgFile string) *viper.Viper {
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
		fmt.Println("\tErr: load json file fail> ", err)
	}

	// config.WatchConfig() // Watch the config

	return config
}

// 服务开始
func Start() error {
	ShowVersion()

	cc, _ := SS.GetObject("poetConfigPath")
	if cc == nil {
		fmt.Println("\tErr: empty panel config path !")
		os.Exit(1)
	}

	config := getConfig(cc.(string))
	panelConfig := &panel.Config{}
	if err := config.Unmarshal(panelConfig); err != nil {
		return fmt.Errorf("parse config file %v failed: %s ", cc, err)
	}

	p := panel.NewPanel(panelConfig)

	p.Start()

	panelAccess.Lock()
	if activePanel != nil && activePanel.Running {
		activePanel.Close()
	}
	activePanel = p
	panelAccess.Unlock()

	return nil

}

func Stop() {
	panelAccess.Lock()
	defer panelAccess.Unlock()
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

// 设置 inbound
func SetInboud(in *adapter.Inbound, tag string) {
	SS.Singleton().SetInboud(in, tag)
}

// 设置 outbound
// func SetOutbound() {}

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

// rateLimitedConn wraps a net.Conn with a per-direction token bucket. It
// is layered OUTSIDE bufio.NewInt64CounterConn so that the limiter waits
// happen after the bytes are already counted — the limiter only paces
// the caller, it never under-reports. user.WaitN with a nil limiter
// returns immediately, so non-limited users have ~zero overhead per
// Read/Write (one map-free RLock).
type rateLimitedConn struct {
	net.Conn
	user *controller.User
	ctx  context.Context
}

func (c *rateLimitedConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if n > 0 {
		// Bytes from client → outbound = upload from the user's perspective.
		_ = c.user.WaitN(c.ctx, n, 0)
	}
	return n, err
}

func (c *rateLimitedConn) Write(b []byte) (int, error) {
	if len(b) > 0 {
		// Bytes from outbound → client = download from the user's perspective.
		_ = c.user.WaitN(c.ctx, 0, len(b))
	}
	return c.Conn.Write(b)
}

// rateLimitedPacketConn wraps a PacketConn the same way for UDP. Without
// this, an unlimited UDP path can fully bypass per-user speed control.
type rateLimitedPacketConn struct {
	N.PacketConn
	user *controller.User
	ctx  context.Context
}

func (c *rateLimitedPacketConn) ReadPacket(buffer *buf.Buffer) (M.Socksaddr, error) {
	dest, err := c.PacketConn.ReadPacket(buffer)
	if buffer.Len() > 0 {
		_ = c.user.WaitN(c.ctx, buffer.Len(), 0)
	}
	return dest, err
}

func (c *rateLimitedPacketConn) WritePacket(buffer *buf.Buffer, destination M.Socksaddr) error {
	if buffer.Len() > 0 {
		_ = c.user.WaitN(c.ctx, 0, buffer.Len())
	}
	return c.PacketConn.WritePacket(buffer, destination)
}

// blockedConn is returned by RoutedConnection when an audit rule rejects
// the destination. The underlying conn is closed up front; any further
// Read/Write surfaces a sentinel error so the dispatcher unwinds cleanly
// without dialing the outbound and pumping bytes.
type blockedConn struct {
	net.Conn
	reason error
}

func (c *blockedConn) Read(b []byte) (int, error)  { return 0, c.reason }
func (c *blockedConn) Write(b []byte) (int, error) { return 0, c.reason }

var errAuditBlocked = fmt.Errorf("singr: connection blocked by audit rule")

// 计费
func RoutedConnection(ctx context.Context, conn net.Conn, metadata adapter.InboundContext) net.Conn {
	var readCounter []*atomic.Int64
	var writeCounter []*atomic.Int64

	ss := SS.Singleton()
	contrl := ss.GetContrlWithInTag(metadata.Inbound)
	if contrl == nil {
		ss.Logger.Warn(fmt.Sprintf("RecordUserTraffic: inbound:%s contrl not found", metadata.Inbound))
		return conn
	}

	sendPtr, recvPtr, user, err := contrl.LoadOrCreateUserCounter(ctx, metadata)
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
		if matched, ruleID := contrl.MatchAudit(user.UID, metadata.Destination.Fqdn); matched {
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
	contrl := ss.GetContrlWithInTag(metadata.Inbound)
	if contrl == nil {
		ss.Logger.Warn(fmt.Sprintf("RecordPacketTraffic: inbound:%s contrl not found", metadata.Inbound))
		return conn
	}

	sendPtr, recvPtr, user, err := contrl.LoadOrCreateUserCounter(ctx, metadata)
	if err != nil {
		ss.Logger.Warn(fmt.Sprintf("RecordPacketTraffic: inbound:%s user:%s counter pointer not found", metadata.Inbound, metadata.User))
		return conn
	}

	counted := bufio.NewInt64CounterPacketConn(conn, []*atomic.Int64{sendPtr}, nil, []*atomic.Int64{recvPtr}, nil)
	return &rateLimitedPacketConn{PacketConn: counted, user: user, ctx: ctx}
}

// debug
func Debug(msg string) {
	SS.Singleton().Logger.Debug(msg)
}
