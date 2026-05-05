package adapter

import "github.com/sagernet/sing-box/poet/api"

// UserRefresher 接口定义用户刷新功能
// 这是一个可选接口，不是所有 Inbound 都需要实现
type PInbound interface {
	// RefreshUsers 更新用户数据
	RefreshUsers(users *[]api.UserInfo, nodeInfo *api.NodeInfo) error
}

// PIncrementalUserInbound is implemented by inbounds that can apply panel
// user changes without rebuilding their entire runtime user table.
type PIncrementalUserInbound interface {
	AddUsers(users *[]api.UserInfo, nodeInfo *api.NodeInfo) error
	RemoveUsers(names []string) error
}

// PStartupConfigurableInbound lets the panel layer apply node settings before
// the sing-box inbound starts listening.
type PStartupConfigurableInbound interface {
	ConfigureFromPanelNode(nodeInfo *api.NodeInfo) error
}
