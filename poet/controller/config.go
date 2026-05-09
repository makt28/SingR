package controller

import "github.com/sagernet/sing-box/log"

type Config struct {
	PanelType      string `mapstructure:"PanelType"`
	UpdatePeriodic int    `mapstructure:"UpdatePeriodic"`

	ListenIP string `mapstructure:"ListenIP"`
	SendIP   string `mapstructure:"SendIP"`

	// EnableDeviceLimit gates SSPanel `node_connector` / per-user
	// `DeviceLimit` enforcement. Default false: device count is
	// observed (alive IPs are reported back to the panel for display)
	// but NOT used to reject new connections. When true, a user
	// connecting from more than DeviceLimit unique source IPs has
	// further connections rejected at User.AddIP time.
	//
	// XrayR enforces device limit unconditionally; SingR makes it
	// opt-in because some SSPanel deployments use the field as a
	// soft display hint rather than a hard cap, and rejecting real
	// connections without operator opt-in would silently break those
	// nodes after upgrade.
	EnableDeviceLimit bool `mapstructure:"EnableDeviceLimit"`

	Logger log.Logger
}
