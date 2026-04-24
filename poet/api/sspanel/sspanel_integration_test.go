package sspanel

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/sagernet/sing-box/poet/api"
)

type integrationPanelConfig struct {
	Nodes []struct {
		APIConfig integrationAPIConfig `json:"apiconfig"`
	} `json:"nodes"`
}

type integrationAPIConfig struct {
	APIHost             string  `json:"apihost"`
	NodeID              int     `json:"nodeid"`
	Key                 string  `json:"apikey"`
	NodeType            string  `json:"nodetype"`
	EnableVless         bool    `json:"enablevless"`
	VlessFlow           string  `json:"vlessflow"`
	Timeout             int     `json:"timeout"`
	SpeedLimit          float64 `json:"speedlimit"`
	DeviceLimit         int     `json:"devicelimit"`
	RuleListPath        string  `json:"rulelistpath"`
	DisableCustomConfig bool    `json:"disablecustomconfig"`
}

func (c integrationAPIConfig) apiConfig() *api.Config {
	return &api.Config{
		APIHost:             c.APIHost,
		NodeID:              c.NodeID,
		Key:                 c.Key,
		NodeType:            c.NodeType,
		EnableVless:         c.EnableVless,
		VlessFlow:           c.VlessFlow,
		Timeout:             c.Timeout,
		SpeedLimit:          c.SpeedLimit,
		DeviceLimit:         c.DeviceLimit,
		RuleListPath:        c.RuleListPath,
		DisableCustomConfig: c.DisableCustomConfig,
	}
}

func TestIntegrationGetNodeInfo(t *testing.T) {
	configPath := os.Getenv("SINGR_SSPANEL_CONFIG")
	if configPath == "" {
		t.Skip("set SINGR_SSPANEL_CONFIG to run live SSPanel integration test")
	}

	configContent, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}

	var panelConfig integrationPanelConfig
	if err := json.Unmarshal(configContent, &panelConfig); err != nil {
		t.Fatal(err)
	}
	if len(panelConfig.Nodes) == 0 {
		t.Fatal("missing nodes[0].apiconfig")
	}

	apiConfig := panelConfig.Nodes[0].APIConfig.apiConfig()
	client := New(apiConfig)
	nodeInfo, err := client.GetNodeInfo()
	if err != nil {
		t.Fatal(err)
	}
	if nodeInfo.NodeID != apiConfig.NodeID {
		t.Fatalf("node id = %d, want %d", nodeInfo.NodeID, apiConfig.NodeID)
	}
	if nodeInfo.NodeType != "anytls" {
		t.Fatalf("node type = %q, want anytls", nodeInfo.NodeType)
	}
	if nodeInfo.Port == 0 {
		t.Fatal("node port is zero")
	}
}
