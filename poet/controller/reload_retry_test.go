package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sagernet/sing-box/adapter"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/poet/api"
)

// reloadRetryTestAPI returns a real node info once, then 304
// NodeNotModified forever — the steady state of a panel whose data
// isn't changing.
type reloadRetryTestAPI struct {
	syncUserListTestAPI
	node   *api.NodeInfo
	served bool
}

func (a *reloadRetryTestAPI) GetNodeInfo() (*api.NodeInfo, error) {
	if !a.served {
		a.served = true
		return a.node, nil
	}
	return nil, errors.New(api.NodeNotModified)
}

// reloadRetryTestInbound fails ConfigureFromPanelNode a fixed number of
// times before succeeding, modeling a transient reload failure (port
// briefly occupied, UDP socket not yet released).
type reloadRetryTestInbound struct {
	incrementalTestInbound
	failuresLeft   int
	configureCalls int
}

func (i *reloadRetryTestInbound) ConfigureFromPanelNode(*api.NodeInfo) error {
	i.configureCalls++
	if i.failuresLeft > 0 {
		i.failuresLeft--
		return errors.New("transient bind failure")
	}
	return nil
}

// TestNodeInfoMonitorRetriesFailedReloadUnder304 guards the
// reloadPending semantics: a failed ConfigureFromPanelNode must be
// retried on subsequent nodeInfoMonitor ticks even though the panel
// answers 304 NodeNotModified (nodeInfoChanged == false). Without the
// retry, a transiently failed hysteria2 SNI-only rebuild leaves the
// inbound down until the panel data happens to change.
func TestNodeInfoMonitorRetriesFailedReloadUnder304(t *testing.T) {
	node := &api.NodeInfo{NodeID: 1, NodeType: C.TypeAnyTLS, Port: 443}
	inbound := &reloadRetryTestInbound{failuresLeft: 2}
	var adapterInbound adapter.Inbound = inbound

	author, err := NewAuthenticator(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	controller := &Controller{
		config:    &Config{UpdatePeriodic: 1},
		apiClient: &reloadRetryTestAPI{node: node},
		nodeInfo:  node,
		usersMap:  map[string]*api.UserInfo{},
		panelType: "SSpanel",
		inbound:   &adapterInbound,
		logger:    log.NewNOPFactory().Logger(),
		author:    author,
		ctx:       context.Background(),
		startAt:   time.Now().Add(-time.Hour),
	}

	// Tick 1: real node info (nodeInfoChanged), reload fails → pending.
	// Ticks 2-3: 304s; pending must keep retrying until success.
	// Tick 4: 304 after success; no further reload attempts.
	for i := 0; i < 4; i++ {
		if err := controller.nodeInfoMonitor(); err != nil {
			t.Fatalf("tick %d: %v", i+1, err)
		}
	}

	if inbound.configureCalls != 3 {
		t.Fatalf("ConfigureFromPanelNode called %d times; want 3 (fail, fail, succeed — then stop)", inbound.configureCalls)
	}
	if controller.reloadPending {
		t.Fatal("reloadPending still set after successful reload")
	}
}
