package panel

import (
	"context"
	"fmt"
	"sync"

	"dario.cat/mergo"

	"github.com/sagernet/sing-box/poet/api"
	"github.com/sagernet/sing-box/poet/api/sspanel"
	"github.com/sagernet/sing-box/poet/controller"
	SS "github.com/sagernet/sing-box/poet/shortcuts"
)

// Panel Structure
type Panel struct {
	access      sync.Mutex
	panelConfig *Config
	Running     bool
	// Service
	Nodes []*controller.Controller
}

func NewPanel(panelConfig *Config) *Panel {
	p := &Panel{
		panelConfig: panelConfig,
		Running:     false,
	}
	return p
}

// Start builds one controller per panel node and starts them all.
//
// Any failure is returned, not handled by exiting: the caller (Box.preStart)
// turns it into a failed startup, so the process still ends non-zero — which
// is what the management scripts' backup → restart → verify → rollback relies
// on — but through sing-box's normal shutdown path instead of an os.Exit in
// the middle of a library call. On failure the nodes that did start are
// stopped again.
func (p *Panel) Start() error {
	p.access.Lock()
	defer p.access.Unlock()

	ss := SS.Singleton()
	ctx := context.Background()
	log := ss.Logger

	log.Info("\n\tStart the panel..")

	// Load Nodes config
	for _, nodeConfig := range p.panelConfig.NodesConfig {
		if nodeConfig == nil || nodeConfig.ApiConfig == nil {
			return fmt.Errorf("panel config has a node without ApiConfig")
		}
		log.Info("processing NodeId: ", nodeConfig.ApiConfig.NodeID)

		inTag := nodeConfig.InTag
		inbound := ss.Inbound(inTag)
		if inbound == nil {
			log.Warn("unknown inbound with Tag: ", inTag)
			continue
		}

		// panel webapi
		var apiClient api.API
		switch nodeConfig.PanelType {
		case "SSpanel":
			apiClient = sspanel.New(nodeConfig.ApiConfig)
		default:
			log.Error("unsupported panel type: ", nodeConfig.PanelType)
			continue
		}

		// Register controller service
		controllerConfig := getDefaultControllerConfig()
		controllerConfig.PanelType = nodeConfig.PanelType
		if nodeConfig.ControllerConfig != nil {
			if err := mergo.Merge(controllerConfig, nodeConfig.ControllerConfig, mergo.WithOverride); err != nil {
				log.Error("read ControllerConfig of NodeId ", nodeConfig.ApiConfig.NodeID, " failed: ", err)
				continue
			}
		}
		controllerService := controller.New(ctx, *controllerConfig, inbound, apiClient, log)
		p.Nodes = append(p.Nodes, controllerService)

		ss.SetController(controllerService, inTag)
	}

	if len(p.Nodes) == 0 {
		// The management scripts and the docker entrypoint quote this
		// message; keep the wording.
		return fmt.Errorf("no Node with InTag found")
	}

	// Start all the nodes
	for i, node := range p.Nodes {
		if err := node.Start(); err != nil {
			for _, started := range p.Nodes[:i] {
				_ = started.Close()
			}
			return fmt.Errorf("panel node on inbound %s failed to start: %w", node.InboundTag(), err)
		}
	}
	p.Running = true
	return nil
}

// Close the panel
func (p *Panel) Close() {
	p.access.Lock()
	defer p.access.Unlock()
	for _, node := range p.Nodes {
		node.Close()
	}
	p.Running = false
}
