package api

import (
	"fmt"

	"github.com/sagernet/sing-box/option"
)

// ValidatePanelNode checks that a panel node may drive an inbound of the
// given type: the node must resolve to that protocol and carry a usable
// listen port. Shared by every panel-driven inbound's ConfigureFromPanelNode.
//
// The port check is deliberately strict: a value outside (0, 65535] is an
// error and the caller leaves its configuration in place. A panel that
// briefly answers 0 must not silently rewrite the listen port.
func ValidatePanelNode(inboundType string, nodeInfo *NodeInfo) error {
	if nodeInfo.NodeType != inboundType {
		return NodeTypeMismatch(inboundType, nodeInfo.NodeType)
	}
	if nodeInfo.Port == 0 || nodeInfo.Port > 65535 {
		return fmt.Errorf("invalid %s listen port from panel: %d", inboundType, nodeInfo.Port)
	}
	return nil
}

// ListenUpdate is the result of laying a panel node's settings over an
// inbound's current listen port and TLS options.
type ListenUpdate struct {
	// Port is the listen port the panel asks for.
	Port uint16
	// TLS is the TLS option set to use from now on. When the panel carries a
	// host it is a copy with ServerName replaced (or a fresh, enabled block
	// if the inbound had none) — the current options are never modified in
	// place. Without a host it is the current pointer, unchanged.
	TLS *option.InboundTLSOptions
	// SNI is the server name in TLS ("" without TLS).
	SNI string

	PortChanged bool
	SNIChanged  bool
}

// Changed reports whether the panel asks for anything different.
func (u ListenUpdate) Changed() bool {
	return u.PortChanged || u.SNIChanged
}

// PlanListenUpdate computes what a (validated) panel node changes about an
// inbound that currently listens on currentPort with currentTLS. Only the
// port and the TLS server name are panel-deliverable; an empty panel host
// keeps the configured server name.
func PlanListenUpdate(nodeInfo *NodeInfo, currentPort uint16, currentTLS *option.InboundTLSOptions) ListenUpdate {
	update := ListenUpdate{
		Port: uint16(nodeInfo.Port),
		TLS:  currentTLS,
	}
	if nodeInfo.Host != "" {
		if currentTLS == nil {
			update.TLS = &option.InboundTLSOptions{Enabled: true}
		} else {
			tlsCopy := *currentTLS
			update.TLS = &tlsCopy
		}
		update.TLS.ServerName = nodeInfo.Host
	}
	oldSNI := ""
	if currentTLS != nil {
		oldSNI = currentTLS.ServerName
	}
	if update.TLS != nil {
		update.SNI = update.TLS.ServerName
	}
	update.PortChanged = update.Port != currentPort
	update.SNIChanged = update.SNI != oldSNI
	return update
}
