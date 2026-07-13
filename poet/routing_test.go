package shortcuts

import (
	"context"
	"errors"
	"net"
	"regexp"
	"testing"
	"time"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/poet/api"
	"github.com/sagernet/sing-box/poet/controller"
	SS "github.com/sagernet/sing-box/poet/shortcuts"
	"github.com/sagernet/sing/common/buf"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
)

func TestRoutedConnectionReturnsOriginalWithoutControllerOrUser(t *testing.T) {
	logger := log.NewNOPFactory().Logger()
	SS.Singleton().Logger = logger
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	missingControllerMetadata := adapter.InboundContext{Inbound: "missing-controller", User: "u1"}
	if got := RoutedConnection(context.Background(), server, missingControllerMetadata); got != server {
		t.Fatal("missing controller should return original connection")
	}

	controllerService, cleanup := installRoutingTestController(t, "known-inbound")
	defer cleanup()
	unknownUserMetadata := adapter.InboundContext{Inbound: "known-inbound", User: "unknown"}
	if got := RoutedConnection(context.Background(), server, unknownUserMetadata); got != server {
		t.Fatal("unknown user should return original connection")
	}
	if _, exists := controllerService.Author().LoadUser("unknown"); exists {
		t.Fatal("routing lookup unexpectedly created unknown user")
	}
}

func TestRoutedConnectionMapsAliasAndCountsDirections(t *testing.T) {
	controllerService, cleanup := installRoutingTestController(t, "traffic-in")
	defer cleanup()
	user := addRoutingTestUser(t, controllerService)
	server, client := net.Pipe()
	defer client.Close()
	metadata := adapter.InboundContext{
		Inbound: "traffic-in",
		User:    "uuid-101",
		Source:  M.ParseSocksaddr("192.0.2.10:1234"),
	}
	wrapped := RoutedConnection(context.Background(), server, metadata)
	if wrapped == server {
		t.Fatal("known alias was not wrapped")
	}

	down := []byte("download")
	writeDone := make(chan error, 1)
	go func() {
		_, err := wrapped.Write(down)
		writeDone <- err
	}()
	received := make([]byte, len(down))
	if _, err := client.Read(received); err != nil {
		t.Fatal(err)
	}
	if err := <-writeDone; err != nil {
		t.Fatal(err)
	}

	up := []byte("upload-data")
	go func() { _, _ = client.Write(up) }()
	read := make([]byte, len(up))
	if _, err := wrapped.Read(read); err != nil {
		t.Fatal(err)
	}
	_ = wrapped.Close()
	sent, recv := user.GetTraffic()
	if sent != int64(len(up)) || recv != int64(len(down)) {
		t.Fatalf("counters = upload %d download %d, want %d/%d", sent, recv, len(up), len(down))
	}
	if _, exists := controllerService.Author().LoadUser("uuid-101"); !exists {
		t.Fatal("UUID alias no longer resolves to canonical user")
	}
}

func TestRoutedPacketConnectionMapsAliasAndCountsDirections(t *testing.T) {
	controllerService, cleanup := installRoutingTestController(t, "packet-in")
	defer cleanup()
	user := addRoutingTestUser(t, controllerService)
	packetConn := &routingTestPacketConn{readData: []byte("packet-upload")}
	metadata := adapter.InboundContext{
		Inbound: "packet-in",
		User:    "one@example.com",
		Source:  M.ParseSocksaddr("192.0.2.11:4321"),
	}
	wrapped := RoutedPacketConnection(context.Background(), packetConn, metadata)

	readBuffer := buf.NewPacket()
	defer readBuffer.Release()
	if _, err := wrapped.ReadPacket(readBuffer); err != nil {
		t.Fatal(err)
	}
	writeBuffer := buf.NewPacket()
	defer writeBuffer.Release()
	_, _ = writeBuffer.Write([]byte("packet-download"))
	if err := wrapped.WritePacket(writeBuffer, M.ParseSocksaddr("198.51.100.1:53")); err != nil {
		t.Fatal(err)
	}
	sent, recv := user.GetTraffic()
	if sent != int64(len(packetConn.readData)) || recv != int64(len("packet-download")) {
		t.Fatalf("packet counters = upload %d download %d", sent, recv)
	}
}

func TestRoutedConnectionAuditClosesAndBlocks(t *testing.T) {
	controllerService, cleanup := installRoutingTestController(t, "audit-in")
	defer cleanup()
	addRoutingTestUser(t, controllerService)
	controllerService.SetDetectRules([]api.DetectRule{{ID: 7, Pattern: regexp.MustCompile(`blocked\.example`)}})
	server, client := net.Pipe()
	defer client.Close()
	metadata := adapter.InboundContext{
		Inbound:     "audit-in",
		User:        "u101",
		Source:      M.ParseSocksaddr("192.0.2.12:5555"),
		Destination: M.ParseSocksaddr("blocked.example:443"),
	}
	wrapped := RoutedConnection(context.Background(), server, metadata)
	buffer := make([]byte, 1)
	if _, err := wrapped.Read(buffer); !errors.Is(err, errAuditBlocked) {
		t.Fatalf("blocked read error = %v, want errAuditBlocked", err)
	}
	if _, err := wrapped.Write(buffer); !errors.Is(err, errAuditBlocked) {
		t.Fatalf("blocked write error = %v, want errAuditBlocked", err)
	}
	if results := controllerService.DrainDetectResults(); len(results) != 1 || results[0] != (api.DetectResult{UID: 101, RuleID: 7}) {
		t.Fatalf("audit results = %#v", results)
	}
	_ = client.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	if _, err := client.Read(buffer); err == nil {
		t.Fatal("audit did not close underlying connection")
	}
}

func installRoutingTestController(t *testing.T, inboundTag string) (*controller.Controller, func()) {
	t.Helper()
	logger := log.NewNOPFactory().Logger()
	SS.Singleton().Logger = logger
	inbound := adapter.Inbound(&routingTestInbound{tag: inboundTag})
	service := controller.New(context.Background(), controller.Config{PanelType: "SSpanel"}, &inbound, routingTestAPI{}, logger)
	service.SetNodeInfo(&api.NodeInfo{NodeID: 1})
	SS.Singleton().SetContrl(service, inboundTag, "")
	return service, func() {
		delete(SS.Singleton().InContrls, inboundTag)
		_ = service.Close()
	}
}

func addRoutingTestUser(t *testing.T, service *controller.Controller) *controller.User {
	t.Helper()
	profile := api.UserInfo{UID: 101, Email: "one@example.com", UUID: "uuid-101", Passwd: "passwd-101"}
	if err := service.Author().AddUser("u101", profile.UID); err != nil {
		t.Fatal(err)
	}
	service.Author().SetUserProfile("u101", profile)
	service.Author().SetUserAliases("u101", profile.UUID, profile.Passwd, profile.Email)
	user, _ := service.Author().LoadUser("u101")
	return user
}

type routingTestInbound struct{ tag string }

func (*routingTestInbound) Type() string                   { return "test" }
func (i *routingTestInbound) Tag() string                  { return i.tag }
func (*routingTestInbound) Start(adapter.StartStage) error { return nil }
func (*routingTestInbound) Close() error                   { return nil }

type routingTestAPI struct{}

func (routingTestAPI) GetNodeInfo() (*api.NodeInfo, error)           { return nil, nil }
func (routingTestAPI) GetUserList() (*[]api.UserInfo, error)         { return nil, nil }
func (routingTestAPI) ReportNodeStatus(*api.NodeStatus) error        { return nil }
func (routingTestAPI) ReportNodeOnlineUsers(*[]api.OnlineUser) error { return nil }
func (routingTestAPI) ReportUserTraffic(*[]api.UserTraffic) error    { return nil }
func (routingTestAPI) Describe() api.ClientInfo                      { return api.ClientInfo{} }
func (routingTestAPI) GetNodeRule() (*[]api.DetectRule, error)       { return nil, nil }
func (routingTestAPI) ReportIllegal(*[]api.DetectResult) error       { return nil }
func (routingTestAPI) Debug()                                        {}

type routingTestPacketConn struct {
	readData []byte
	written  int
}

func (c *routingTestPacketConn) ReadPacket(buffer *buf.Buffer) (M.Socksaddr, error) {
	_, _ = buffer.Write(c.readData)
	return M.ParseSocksaddr("198.51.100.2:53"), nil
}

func (c *routingTestPacketConn) WritePacket(buffer *buf.Buffer, _ M.Socksaddr) error {
	c.written = buffer.Len()
	return nil
}

func (*routingTestPacketConn) Close() error                     { return nil }
func (*routingTestPacketConn) LocalAddr() net.Addr              { return &net.UDPAddr{} }
func (*routingTestPacketConn) SetDeadline(time.Time) error      { return nil }
func (*routingTestPacketConn) SetReadDeadline(time.Time) error  { return nil }
func (*routingTestPacketConn) SetWriteDeadline(time.Time) error { return nil }

var _ N.PacketConn = (*routingTestPacketConn)(nil)
