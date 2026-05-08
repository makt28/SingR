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
	"github.com/sagernet/sing/common/bufio"
	N "github.com/sagernet/sing/common/network"

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

	sendPtr, recvPtr, err := contrl.LoadOrCreateUserCounter(ctx, metadata)
	if err != nil {
		ss.Logger.Warn(fmt.Sprintf("RecordUserTraffic: inbound:%s user:%s counter pointer not found", metadata.Inbound, metadata.User))
		return conn
	}
	readCounter = append(readCounter, sendPtr)
	writeCounter = append(writeCounter, recvPtr)

	if trafficDebug {
		perRead := &atomic.Int64{}
		perWrite := &atomic.Int64{}
		readCounter = append(readCounter, perRead)
		writeCounter = append(writeCounter, perWrite)
		tag := fmt.Sprintf("user=%s inbound=%s src=%s dst=%s", metadata.User, metadata.Inbound, metadata.Source.AddrString(), metadata.Destination.AddrString())
		ss.Logger.Debug(fmt.Sprintf("[TRAFFIC] conn wrap %s sendPtrPre=%d recvPtrPre=%d", tag, sendPtr.Load(), recvPtr.Load()))
		wrapped := bufio.NewInt64CounterConn(conn, readCounter, writeCounter)
		return &loggingConn{Conn: wrapped, perRead: perRead, perWrite: perWrite, tag: tag}
	}

	return bufio.NewInt64CounterConn(conn, readCounter, writeCounter)
}
func RoutedPacketConnection(ctx context.Context, conn N.PacketConn, metadata adapter.InboundContext) N.PacketConn {
	ss := SS.Singleton()
	contrl := ss.GetContrlWithInTag(metadata.Inbound)
	if contrl == nil {
		ss.Logger.Warn(fmt.Sprintf("RecordPacketTraffic: inbound:%s contrl not found", metadata.Inbound))
		return conn
	}

	sendPtr, recvPtr, err := contrl.LoadOrCreateUserCounter(ctx, metadata)
	if err != nil {
		ss.Logger.Warn(fmt.Sprintf("RecordPacketTraffic: inbound:%s user:%s counter pointer not found", metadata.Inbound, metadata.User))
		return conn
	}

	return bufio.NewInt64CounterPacketConn(conn, []*atomic.Int64{sendPtr}, nil, []*atomic.Int64{recvPtr}, nil)
}

// debug
func Debug(msg string) {
	SS.Singleton().Logger.Debug(msg)
}
