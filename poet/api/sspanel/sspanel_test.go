package sspanel

import (
	"encoding/json"
	"testing"

	"github.com/sagernet/sing-box/poet/api"
)

func newTestClient(nodeType string) *APIClient {
	return New(&api.Config{
		APIHost:  "http://127.0.0.1",
		Key:      "test-key",
		NodeID:   3,
		NodeType: nodeType,
	})
}

func TestParseV2rayNodeResponseAnyTLSAlias(t *testing.T) {
	client := newTestClient("V2ray")
	nodeInfo, err := client.ParseV2rayNodeResponse(&NodeInfoResponse{
		RawServerString: "sa.akanyoni.com;14555;0;ws;;path=/anytls|host=xxxx.com|relay_server=other.agstores99.vip|relay_port=42132",
		TrafficRate:     1,
	})
	if err != nil {
		t.Fatal(err)
	}

	if nodeInfo.NodeType != "anytls" {
		t.Fatalf("NodeType = %q, want anytls", nodeInfo.NodeType)
	}
	if nodeInfo.PanelNodeType != "V2ray" {
		t.Fatalf("PanelNodeType = %q, want V2ray", nodeInfo.PanelNodeType)
	}
	if nodeInfo.Port != 14555 {
		t.Fatalf("Port = %d, want 14555", nodeInfo.Port)
	}
	if nodeInfo.TransportProtocol != "tcp" {
		t.Fatalf("TransportProtocol = %q, want tcp", nodeInfo.TransportProtocol)
	}
	if nodeInfo.Path != "/anytls" {
		t.Fatalf("Path = %q, want /anytls", nodeInfo.Path)
	}
	if nodeInfo.Host != "xxxx.com" {
		t.Fatalf("Host = %q, want xxxx.com", nodeInfo.Host)
	}
	if nodeInfo.RelayServer != "other.agstores99.vip" {
		t.Fatalf("RelayServer = %q, want other.agstores99.vip", nodeInfo.RelayServer)
	}
	if nodeInfo.RelayPort != 42132 {
		t.Fatalf("RelayPort = %d, want 42132", nodeInfo.RelayPort)
	}
}

func TestParseV2rayNodeResponseAnyTLSAliasWithoutSlash(t *testing.T) {
	client := newTestClient("v2ray")
	nodeInfo, err := client.ParseV2rayNodeResponse(&NodeInfoResponse{
		RawServerString: "example.com;14555;0;ws;;path=anytls",
	})
	if err != nil {
		t.Fatal(err)
	}

	if nodeInfo.NodeType != "anytls" {
		t.Fatalf("NodeType = %q, want anytls", nodeInfo.NodeType)
	}
	if nodeInfo.TransportProtocol != "tcp" {
		t.Fatalf("TransportProtocol = %q, want tcp", nodeInfo.TransportProtocol)
	}
}

func TestParseV2rayNodeResponseHysteria2Alias(t *testing.T) {
	client := newTestClient("V2ray")
	nodeInfo, err := client.ParseV2rayNodeResponse(&NodeInfoResponse{
		RawServerString: "hy2.example.com;14555;0;ws;;path=/hy2|host=sni.example.com",
	})
	if err != nil {
		t.Fatal(err)
	}

	if nodeInfo.NodeType != "hysteria2" {
		t.Fatalf("NodeType = %q, want hysteria2", nodeInfo.NodeType)
	}
	if nodeInfo.PanelNodeType != "V2ray" {
		t.Fatalf("PanelNodeType = %q, want V2ray", nodeInfo.PanelNodeType)
	}
	if nodeInfo.Port != 14555 {
		t.Fatalf("Port = %d, want 14555", nodeInfo.Port)
	}
	if nodeInfo.TransportProtocol != "udp" {
		t.Fatalf("TransportProtocol = %q, want udp", nodeInfo.TransportProtocol)
	}
	if nodeInfo.Path != "/hy2" {
		t.Fatalf("Path = %q, want /hy2", nodeInfo.Path)
	}
	if nodeInfo.Host != "sni.example.com" {
		t.Fatalf("Host = %q, want sni.example.com", nodeInfo.Host)
	}
}

func TestParseSSPanelNodeInfoHysteria2AliasWithoutSlash(t *testing.T) {
	client := newTestClient("V2ray")
	customConfig, err := json.Marshal(CustomConfig{
		OffsetPortNode: "14555",
		Network:        "ws",
		Path:           "hy2",
		Host:           "sni.example.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	nodeInfo, err := client.ParseSSPanelNodeInfo(&NodeInfoResponse{
		CustomConfig: customConfig,
	})
	if err != nil {
		t.Fatal(err)
	}

	if nodeInfo.NodeType != "hysteria2" {
		t.Fatalf("NodeType = %q, want hysteria2", nodeInfo.NodeType)
	}
	if nodeInfo.PanelNodeType != "V2ray" {
		t.Fatalf("PanelNodeType = %q, want V2ray", nodeInfo.PanelNodeType)
	}
	if nodeInfo.Port != 14555 {
		t.Fatalf("Port = %d, want 14555", nodeInfo.Port)
	}
	if nodeInfo.TransportProtocol != "udp" {
		t.Fatalf("TransportProtocol = %q, want udp", nodeInfo.TransportProtocol)
	}
	if nodeInfo.Path != "hy2" {
		t.Fatalf("Path = %q, want hy2", nodeInfo.Path)
	}
	if nodeInfo.Host != "sni.example.com" {
		t.Fatalf("Host = %q, want sni.example.com", nodeInfo.Host)
	}
}

func TestParseV2rayNodeResponseKeepsNormalWebSocket(t *testing.T) {
	client := newTestClient("V2ray")
	nodeInfo, err := client.ParseV2rayNodeResponse(&NodeInfoResponse{
		RawServerString: "example.com;10001;0;ws;;path=/vmess|host=cdn.example.com",
	})
	if err != nil {
		t.Fatal(err)
	}

	if nodeInfo.NodeType != "V2ray" {
		t.Fatalf("NodeType = %q, want V2ray", nodeInfo.NodeType)
	}
	if nodeInfo.TransportProtocol != "ws" {
		t.Fatalf("TransportProtocol = %q, want ws", nodeInfo.TransportProtocol)
	}
	if nodeInfo.Host != "cdn.example.com" {
		t.Fatalf("Host = %q, want cdn.example.com", nodeInfo.Host)
	}
}

func TestParseUserListResponseOldNodeConnector(t *testing.T) {
	client := newTestClient("V2ray")
	users, err := client.ParseUserListResponse(&[]UserResponse{
		{
			ID:            1,
			Email:         "user@example.com",
			Passwd:        "password",
			UUID:          "00000000-0000-0000-0000-000000000001",
			NodeConnector: 2,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(*users) != 1 {
		t.Fatalf("len(users) = %d, want 1", len(*users))
	}
	user := (*users)[0]
	if user.Email != "user@example.com" {
		t.Fatalf("Email = %q, want user@example.com", user.Email)
	}
	if user.DeviceLimit != 2 {
		t.Fatalf("DeviceLimit = %d, want 2", user.DeviceLimit)
	}
}
