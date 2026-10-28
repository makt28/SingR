package naive

import (
	"fmt"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/poet/api"
)

// Inbound implements both the full-refresh and the incremental panel user
// interfaces. The controller prefers the incremental path.
//
// Unlike hysteria2 there is no currentUsers mirror: the user table belongs
// to the Inbound, not to a service that hot reload rebuilds, so it survives
// port and SNI changes as is.
var (
	_ adapter.PInbound                = (*Inbound)(nil)
	_ adapter.PIncrementalUserInbound = (*Inbound)(nil)
)

// RefreshUsers replaces the entire user set (fallback / full-refresh path).
func (n *Inbound) RefreshUsers(users *[]api.UserInfo, nodeInfo *api.NodeInfo) error {
	n.users.replace(buildNaiveUsers(*users))
	return nil
}

// AddUsers incrementally adds or updates users (added ∪ changed).
func (n *Inbound) AddUsers(users *[]api.UserInfo, nodeInfo *api.NodeInfo) error {
	n.users.add(buildNaiveUsers(*users))
	return nil
}

// RemoveUsers incrementally removes users by runtime name, e.g. "u40493".
func (n *Inbound) RemoveUsers(names []string) error {
	n.users.remove(names)
	return nil
}

func buildNaiveUsers(users []api.UserInfo) (names []string, passwords []string) {
	names = make([]string, len(users))
	passwords = make([]string, len(users))
	for i, user := range users {
		names[i] = fmt.Sprintf("u%d", user.UID)
		passwords[i] = buildNaivePassword(user)
	}
	return names, passwords
}

func buildNaivePassword(user api.UserInfo) string {
	if user.UUID != "" {
		return user.UUID
	}
	return user.Passwd
}
