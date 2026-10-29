package api

import "fmt"

// NodeTypeMismatch is the error a panel-driven inbound returns when the
// panel node bound to it (panel.json intag) resolves to another protocol.
// It used to be a silent no-op, which left the inbound on server.json's
// listen_port 0 — a random port, healthy process, unreachable node.
func NodeTypeMismatch(inboundType, panelNodeType string) error {
	if panelNodeType == "" {
		panelNodeType = "(empty)"
	}
	return fmt.Errorf("panel node resolves to type %s, but its panel.json intag points at a %s inbound: the panel node's ws path (/anytls, /hy2, /naive) picks the protocol",
		panelNodeType, inboundType)
}
