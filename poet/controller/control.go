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

	nextMap, added, deleted := diffUsers(c.usersMap, *userInfo, c.buildUserHash)
	c.log(fmt.Sprintf("Sync Users added: %d deleted: %d", len(added), len(deleted)), "info")

	// Dump traffic for to-be-deleted users before removing them; if the
	// report fails, keep the users this cycle so the unreported bytes
	// aren't lost. Next sync will retry.
	if len(deleted) > 0 {
		if err := c.dumpTrafficForDeleted(deleted); err != nil {
			c.log(fmt.Sprintf("dump traffic before delete failed, deferring deletion: %v", err), "error")
			for _, hash := range deleted {
				if u, ok := c.usersMap[hash]; ok {
					nextMap[hash] = u
				}
			}
			deleted = nil
		}
	}

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

	for _, user := range *userInfo {
		hash := c.buildUserHash(&user)
		c.author.SetUserProfile(hash, user)
		c.author.SetUserAliases(hash, user.UUID, user.Passwd, user.Email)
	}

	in := *c.inbound
	refresher, ok := in.(adapter.PInbound)
	if !ok {
		c.log(fmt.Sprintf("unsupported node type: %s", c.nodeInfo.NodeType), "error")
		return errors.New("inbound type does not support user refresh")
	}
	if err := refresher.RefreshUsers(userInfo, c.nodeInfo); err != nil {
		c.log(fmt.Sprintf("Failed to refresh users for node type %s, error: %v", c.nodeInfo.NodeType, err), "error")
		return err
	}

	c.log(fmt.Sprintf("final UsersMap: %d \tuserInfo: %d ", len(c.usersMap), len(*userInfo)), "info")
	return nil
}

// dumpTrafficForDeleted reports any pending bytes for users that are about
// to be removed. On report failure the bytes are restored to the user's
// counters so the next regular cycle can retry, and the caller is expected
// to defer deletion.
func (c *Controller) dumpTrafficForDeleted(deleted []string) error {
	type pending struct {
		user *User
		sent int64
		recv int64
	}
	var pendings []pending
	var report []api.UserTraffic
	rate := c.nodeInfo.TrafficRate
	if rate <= 0 {
		rate = 1
	}
	for _, hash := range deleted {
		u, found := c.author.LoadUser(hash)
		if !found {
			continue
		}
		sent, recv := u.ResetTraffic()
		if sent == 0 && recv == 0 {
			continue
		}
		var up, down int64
		if rate == 1 {
			up, down = sent, recv
		} else {
			up = int64(rate * float64(sent))
			down = int64(rate * float64(recv))
		}
		pendings = append(pendings, pending{user: u, sent: sent, recv: recv})
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
		for _, p := range pendings {
			p.user.RestoreTraffic(p.sent, p.recv)
		}
		return err
	}
	return nil
}

// 记录流量
func (c *Controller) LoadOrCreateUserCounter(ctx context.Context, metadata adapter.InboundContext) (send, recv *atomic.Int64, err error) {
	hash := metadata.User
	user, found := c.author.LoadUser(hash)
	if !found {
		return nil, nil, fmt.Errorf("hash:%s is not in user map", hash)
	}
	if hash != user.hash {
		c.log(fmt.Sprintf("traffic user alias mapped alias=%s canonical=%s UID=%d email=%s", hash, user.hash, user.UID, user.Email), "debug")
	}
	if user.MarkCounterAttached() {
		c.log(fmt.Sprintf("traffic counter attached UID=%d email=%s user=%s inbound=%s source=%s", user.UID, user.Email, user.hash, metadata.Inbound, metadata.Source.AddrString()), "info")
	}

	user.AddIP(metadata.Source.AddrString())

	sendPtr, recvPtr := user.GetTrafficPointer()
	return sendPtr, recvPtr, nil
}

// diffUsers is a pure function: it does not mutate inputs. It returns a
// fresh next-state map plus the lists of hashes added and deleted relative
// to old. Duplicate hashes inside newList are dropped (first-wins).
func diffUsers(
	old map[string]*api.UserInfo,
	newList []api.UserInfo,
	hashFn func(*api.UserInfo) string,
) (next map[string]*api.UserInfo, added []string, deleted []string) {
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
		if _, ok := old[hash]; !ok {
			added = append(added, hash)
		}
	}
	for hash := range old {
		if _, ok := next[hash]; !ok {
			deleted = append(deleted, hash)
		}
	}
	return next, added, deleted
}
