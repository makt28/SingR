package anytls

import (
	"fmt"

	anytls "github.com/anytls/sing-anytls"
	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/poet/api"
)

// 确保实现 UserRefresher 接口
var _ adapter.PInbound = (*Inbound)(nil)
var _ adapter.PIncrementalUserInbound = (*Inbound)(nil)

// RefreshUsers 更新用户列表
func (h *Inbound) RefreshUsers(users *[]api.UserInfo, nodeInfo *api.NodeInfo) error {
	// 1. 准备更新参数
	opUsers := buildAnyTLSUsers(*users)

	// 2. 更新服务用户
	h.service.UpdateUsers(opUsers)

	return nil
}

// AddUsers incrementally adds or updates users in the AnyTLS service.
func (h *Inbound) AddUsers(users *[]api.UserInfo, nodeInfo *api.NodeInfo) error {
	h.service.AddUsers(buildAnyTLSUsers(*users))
	return nil
}

// RemoveUsers incrementally removes users by their runtime names, e.g. u40493.
func (h *Inbound) RemoveUsers(names []string) error {
	h.service.RemoveUsers(names)
	return nil
}

func buildAnyTLSUsers(users []api.UserInfo) []anytls.User {
	ulen := len(users)
	opUsers := make([]anytls.User, ulen)

	for i, user := range users {
		opUsers[i] = anytls.User{
			Name:     fmt.Sprintf("u%d", user.UID),
			Password: buildAnyTLSPassword(user),
		}
	}
	return opUsers
}

func buildAnyTLSPassword(user api.UserInfo) string {
	if user.UUID != "" {
		return user.UUID
	}
	return user.Passwd
}
