package hysteria2

import (
	"fmt"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/poet/api"
)

// Inbound implements both the full-refresh and the incremental panel user
// interfaces. The controller prefers the incremental path.
var (
	_ adapter.PInbound                = (*Inbound)(nil)
	_ adapter.PIncrementalUserInbound = (*Inbound)(nil)
)

// RefreshUsers replaces the entire user set (fallback / full-refresh path).
func (h *Inbound) RefreshUsers(users *[]api.UserInfo, nodeInfo *api.NodeInfo) error {
	names, passwords := buildHysteria2Users(*users)
	h.usersMu.Lock()
	defer h.usersMu.Unlock()
	next := make(map[string]string, len(names))
	for i, name := range names {
		next[name] = passwords[i]
	}
	h.currentUsers = next
	h.service.UpdateUsers(names, passwords)
	return nil
}

// AddUsers incrementally adds or updates users (added ∪ changed).
func (h *Inbound) AddUsers(users *[]api.UserInfo, nodeInfo *api.NodeInfo) error {
	names, passwords := buildHysteria2Users(*users)
	h.usersMu.Lock()
	defer h.usersMu.Unlock()
	for i, name := range names {
		h.currentUsers[name] = passwords[i]
	}
	h.service.AddUsers(names, passwords)
	return nil
}

// RemoveUsers incrementally removes users by runtime name, e.g. "u40493".
func (h *Inbound) RemoveUsers(names []string) error {
	h.usersMu.Lock()
	defer h.usersMu.Unlock()
	for _, name := range names {
		delete(h.currentUsers, name)
	}
	h.service.RemoveUsers(names)
	return nil
}

func buildHysteria2Users(users []api.UserInfo) (names []string, passwords []string) {
	names = make([]string, len(users))
	passwords = make([]string, len(users))
	for i, user := range users {
		names[i] = fmt.Sprintf("u%d", user.UID)
		passwords[i] = buildHysteria2Password(user)
	}
	return names, passwords
}

func buildHysteria2Password(user api.UserInfo) string {
	if user.UUID != "" {
		return user.UUID
	}
	return user.Passwd
}
