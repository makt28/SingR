package controller

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/poet/api"
)

func (c *Controller) syncUserList() error {
	userInfo, err := c.apiClient.GetUserList()
	if err != nil {
		if err.Error() == api.UserNotModified {
			c.log("GetUserList: 304 UserNotModified", "info")
			return nil
		}
		return err
	}

	nextMap, added, deleted, changed := diffUsers(c.usersMap, *userInfo, c.buildUserHash)

	// Dump traffic for to-be-deleted users before removing them. On
	// failure the bytes are intentionally discarded (same rationale as
	// userInfoMonitor) and the deletion proceeds — keeping the user
	// alive and re-reporting next cycle would re-introduce the
	// quadratic over-report blowup.
	if len(deleted) > 0 {
		if err := c.dumpTrafficForDeleted(deleted); err != nil {
			c.log(fmt.Sprintf("dump traffic before delete failed, discarding bytes: %v", err), "error")
		}
	}
	runtimeUsers := usersFromMap(nextMap)
	deltaAddedChangedUsers := usersForHashes(nextMap, append(added, changed...))
	shouldRefreshRuntimeUsers := len(added) > 0 || len(deleted) > 0 || len(changed) > 0
	c.log(fmt.Sprintf("Sync Users added: %d deleted: %d changed: %d total: %d", len(added), len(deleted), len(changed), len(runtimeUsers)), "info")

	for _, hash := range deleted {
		c.log(fmt.Sprintf("DeleteUser: %s", hash), "debug")
		if err := c.author.DelUser(hash); err != nil {
			c.log(fmt.Sprintf("failed deleteUser: %s %v", hash, err.Error()), "error")
		}
	}
	for _, hash := range added {
		u, ok := nextMap[hash]
		if !ok {
			c.log(fmt.Sprintf("usersMap none hash: %s", hash), "error")
			continue
		}
		if err := c.author.AddUser(hash, u.UID); err != nil {
			c.log(fmt.Sprintf("failed addUser: %s %v", hash, err.Error()), "error")
		}
	}

	c.usersMap = nextMap

	// Only refresh profile/aliases for users whose material changed or were
	// just added. At 2000 users it's wasteful to rewrite every author entry
	// when nothing about that user has changed.
	for _, hash := range added {
		u, ok := nextMap[hash]
		if !ok || u == nil {
			continue
		}
		c.author.SetUserProfile(hash, *u)
		c.author.SetUserAliases(hash, u.UUID, u.Passwd, u.Email)
		c.applyUserSpeedLimit(hash, u.SpeedLimit)
	}
	for _, hash := range changed {
		u, ok := nextMap[hash]
		if !ok || u == nil {
			continue
		}
		c.author.SetUserProfile(hash, *u)
		c.author.SetUserAliases(hash, u.UUID, u.Passwd, u.Email)
		c.applyUserSpeedLimit(hash, u.SpeedLimit)
	}

	if shouldRefreshRuntimeUsers {
		in := *c.inbound
		if incremental, ok := in.(adapter.PIncrementalUserInbound); ok {
			if len(deleted) > 0 {
				if err := incremental.RemoveUsers(deleted); err != nil {
					c.log(fmt.Sprintf("Failed to remove users for node type %s, error: %v", c.nodeInfo.NodeType, err), "error")
					return err
				}
			}
			if len(deltaAddedChangedUsers) > 0 {
				if err := incremental.AddUsers(&deltaAddedChangedUsers, c.nodeInfo); err != nil {
					c.log(fmt.Sprintf("Failed to add/update users for node type %s, error: %v", c.nodeInfo.NodeType, err), "error")
					return err
				}
			}
			return nil
		}

		refresher, ok := in.(adapter.PInbound)
		if !ok {
			c.log(fmt.Sprintf("unsupported node type: %s", c.nodeInfo.NodeType), "error")
			return errors.New("inbound type does not support user refresh")
		}
		if err := refresher.RefreshUsers(&runtimeUsers, c.nodeInfo); err != nil {
			c.log(fmt.Sprintf("Failed to refresh users for node type %s, error: %v", c.nodeInfo.NodeType, err), "error")
			return err
		}
	}

	return nil
}

// dumpTrafficForDeleted reports any pending bytes for users that are about
// to be removed. On report failure the bytes are discarded (no restore) —
// see the rationale in userInfoMonitor.
func (c *Controller) dumpTrafficForDeleted(deleted []string) error {
	var report []api.UserTraffic
	for _, hash := range deleted {
		u, found := c.author.LoadUser(hash)
		if !found {
			continue
		}
		sent, recv := u.ResetTraffic()
		if sent == 0 && recv == 0 {
			continue
		}
		up, down := trafficForSSPanel(sent, recv)
		report = append(report, api.UserTraffic{
			UID:      u.UID,
			Email:    u.Email,
			Upload:   up,
			Download: down,
		})
	}
	if len(report) == 0 {
		return nil
	}
	c.log(fmt.Sprintf("dumping %d deleted users' traffic before removal", len(report)), "info")
	if err := c.apiClient.ReportUserTraffic(&report); err != nil {
		return err
	}
	return nil
}

func trafficForSSPanel(sent, recv int64) (up, down int64) {
	// Old SSPanel/XrayR semantics report raw transfer bytes. The panel owns
	// node traffic_rate accounting, so multiplying here double-charges users.
	return sent, recv
}

// determineRate mirrors XrayR `common/limiter` `determineRate`: SSPanel's
// "node speed limit" is NOT a shared node-wide bucket — it's a per-user
// fallback / ceiling. Each user gets their own limiter with rate equal
// to:
//
//	node=0, user=0  → 0  (no limit)
//	node=0, user=X  → X  (user-specific)
//	node=X, user=0  → X  (node default applied per user)
//	node=X, user=Y  → min(X, Y)
//
// Keeping bug-for-bug parity with XrayR avoids surprising operators who
// migrate from XrayR to SingR and expect the same panel field semantics.
func determineRate(nodeLimit, userLimit uint64) uint64 {
	if nodeLimit == 0 || userLimit == 0 {
		if nodeLimit > userLimit {
			return nodeLimit
		}
		return userLimit
	}
	if nodeLimit < userLimit {
		return nodeLimit
	}
	return userLimit
}

// applyUserSpeedLimit installs (or clears) the per-user limiter using
// the XrayR `determineRate` rule. Called for added / changed users in
// syncUserList and (via refreshAllSpeedLimits) when nodeInfo.SpeedLimit
// changes.
func (c *Controller) applyUserSpeedLimit(hash string, userLimit uint64) {
	nodeLimit := uint64(0)
	if c.nodeInfo != nil {
		nodeLimit = c.nodeInfo.SpeedLimit
	}
	c.author.SetUserSpeedLimit(hash, determineRate(nodeLimit, userLimit))
}

// refreshAllSpeedLimits recomputes every existing user's limiter against
// the current nodeInfo.SpeedLimit. Cheap O(N) walk; only called from
// nodeInfoMonitor when the node speed limit actually changed.
func (c *Controller) refreshAllSpeedLimits() {
	if c.usersMap == nil {
		return
	}
	for hash, u := range c.usersMap {
		if u == nil {
			continue
		}
		c.applyUserSpeedLimit(hash, u.SpeedLimit)
	}
}

// 记录流量
//
// Returns the user's send/recv counter pointers along with the User
// pointer itself so the caller (poet.RoutedConnection) can additionally
// wrap with a rate-limit conn that calls user.WaitN. Callers that only
// need counters can ignore the user return.
func (c *Controller) LoadOrCreateUserCounter(ctx context.Context, metadata adapter.InboundContext) (send, recv *atomic.Int64, user *User, err error) {
	hash := metadata.User
	u, found := c.author.LoadUser(hash)
	if !found {
		return nil, nil, nil, fmt.Errorf("hash:%s is not in user map", hash)
	}
	if hash != u.hash {
		c.log(fmt.Sprintf("traffic user alias mapped alias=%s canonical=%s UID=%d email=%s", hash, u.hash, u.UID, u.Email), "debug")
	}
	if u.MarkCounterAttached() {
		c.log(fmt.Sprintf("traffic counter attached UID=%d email=%s user=%s inbound=%s source=%s", u.UID, u.Email, u.hash, metadata.Inbound, metadata.Source.AddrString()), "debug")
	}

	u.AddIP(metadata.Source.AddrString())

	sendPtr, recvPtr := u.GetTrafficPointer()
	return sendPtr, recvPtr, u, nil
}

// diffUsers is a pure function: it does not mutate inputs. It returns a
// fresh next-state map plus the lists of hashes added, deleted, and changed
// relative to old. Duplicate hashes inside newList are dropped (first-wins).
func diffUsers(
	old map[string]*api.UserInfo,
	newList []api.UserInfo,
	hashFn func(*api.UserInfo) string,
) (next map[string]*api.UserInfo, added []string, deleted []string, changed []string) {
	next = make(map[string]*api.UserInfo, len(newList))
	for i := range newList {
		// Take a value copy so storing &u doesn't alias the loop variable
		// or the slice element memory.
		u := newList[i]
		hash := hashFn(&u)
		if _, dup := next[hash]; dup {
			continue
		}
		next[hash] = &u
		if oldUser, ok := old[hash]; !ok {
			added = append(added, hash)
		} else if oldUser == nil || userMaterialChanged(*oldUser, u) {
			changed = append(changed, hash)
		}
	}
	for hash := range old {
		if _, ok := next[hash]; !ok {
			deleted = append(deleted, hash)
		}
	}
	return next, added, deleted, changed
}

// userMaterialChanged reports whether the fields that actually affect
// authentication, accounting or limits have changed. Other fields (e.g.
// AlterID, Method, Flow) are ignored to avoid panel-side flicker forcing
// pointless full RefreshUsers rebuilds.
func userMaterialChanged(a, b api.UserInfo) bool {
	return a.UUID != b.UUID ||
		a.Passwd != b.Passwd ||
		a.Email != b.Email ||
		a.Port != b.Port ||
		a.SpeedLimit != b.SpeedLimit ||
		a.DeviceLimit != b.DeviceLimit
}

func usersFromMap(users map[string]*api.UserInfo) []api.UserInfo {
	result := make([]api.UserInfo, 0, len(users))
	for _, user := range users {
		if user == nil {
			continue
		}
		result = append(result, *user)
	}
	return result
}

func usersForHashes(users map[string]*api.UserInfo, hashes []string) []api.UserInfo {
	result := make([]api.UserInfo, 0, len(hashes))
	for _, hash := range hashes {
		user, ok := users[hash]
		if !ok || user == nil {
			continue
		}
		result = append(result, *user)
	}
	return result
}
