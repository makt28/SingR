package controller

import (
	"context"
	"fmt"
	"testing"

	"github.com/sagernet/sing-box/adapter"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/poet/api"
)

func TestTrafficForSSPanelReportsRawBytes(t *testing.T) {
	up, down := trafficForSSPanel(123, 456)
	if up != 123 || down != 456 {
		t.Fatalf("trafficForSSPanel() = %d, %d; want raw bytes 123, 456", up, down)
	}
}

func TestDiffUsersDetectsCredentialChanges(t *testing.T) {
	old := map[string]*api.UserInfo{
		"u1": {UID: 1, Email: "user@example.com", UUID: "old-uuid", Passwd: "old-passwd"},
	}
	newList := []api.UserInfo{
		{UID: 1, Email: "user@example.com", UUID: "new-uuid", Passwd: "old-passwd"},
	}

	_, added, deleted, changed := diffUsers(old, newList, testUserHash)
	if len(added) != 0 || len(deleted) != 0 {
		t.Fatalf("added=%v deleted=%v; want none", added, deleted)
	}
	if len(changed) != 1 || changed[0] != "u1" {
		t.Fatalf("changed=%v; want [u1]", changed)
	}
}

func TestDiffUsersNoChange(t *testing.T) {
	user := api.UserInfo{UID: 1, Email: "user@example.com", UUID: "uuid", Passwd: "passwd"}
	old := map[string]*api.UserInfo{"u1": &user}

	_, added, deleted, changed := diffUsers(old, []api.UserInfo{user}, testUserHash)
	if len(added) != 0 || len(deleted) != 0 || len(changed) != 0 {
		t.Fatalf("added=%v deleted=%v changed=%v; want no changes", added, deleted, changed)
	}
}

func TestDiffUsersIgnoresImmaterialFlicker(t *testing.T) {
	old := map[string]*api.UserInfo{
		"u1": {UID: 1, Email: "user@example.com", UUID: "uuid", Passwd: "passwd", AlterID: 0, Method: "aes-128-gcm"},
	}
	newList := []api.UserInfo{
		{UID: 1, Email: "user@example.com", UUID: "uuid", Passwd: "passwd", AlterID: 64, Method: "chacha20"},
	}

	_, added, deleted, changed := diffUsers(old, newList, testUserHash)
	if len(added) != 0 || len(deleted) != 0 || len(changed) != 0 {
		t.Fatalf("added=%v deleted=%v changed=%v; want no changes for AlterID/Method flicker", added, deleted, changed)
	}
}

func TestSyncUserListUsesIncrementalInboundWhenAvailable(t *testing.T) {
	oldUser1 := api.UserInfo{UID: 1, Email: "one@example.com", UUID: "old-uuid", Passwd: "old-pass"}
	oldUser2 := api.UserInfo{UID: 2, Email: "two@example.com", UUID: "uuid-2", Passwd: "pass-2"}
	newUsers := []api.UserInfo{
		{UID: 1, Email: "one@example.com", UUID: "new-uuid", Passwd: "old-pass"},
		{UID: 3, Email: "three@example.com", UUID: "uuid-3", Passwd: "pass-3"},
	}

	inbound := &incrementalTestInbound{}
	var adapterInbound adapter.Inbound = inbound
	author, err := NewAuthenticator(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, user := range []api.UserInfo{oldUser1, oldUser2} {
		hash := testUserHash(&user)
		if err := author.AddUser(hash, user.UID); err != nil {
			t.Fatal(err)
		}
		author.SetUserProfile(hash, user)
	}

	controller := &Controller{
		apiClient: &syncUserListTestAPI{users: newUsers},
		nodeInfo:  &api.NodeInfo{NodeID: 1, NodeType: C.TypeAnyTLS},
		usersMap: map[string]*api.UserInfo{
			"u1": &oldUser1,
			"u2": &oldUser2,
		},
		panelType: "SSpanel",
		inbound:   &adapterInbound,
		logger:    log.NewNOPFactory().Logger(),
		author:    author,
		ctx:       context.Background(),
	}

	if err := controller.syncUserList(); err != nil {
		t.Fatal(err)
	}

	if len(inbound.fullRefreshes) != 0 {
		t.Fatalf("full refresh called %d times; want incremental path only", len(inbound.fullRefreshes))
	}
	if len(inbound.removed) != 1 || inbound.removed[0] != "u2" {
		t.Fatalf("removed=%v; want [u2]", inbound.removed)
	}
	if got := userUIDSet(inbound.added); !got[1] || !got[3] || len(got) != 2 {
		t.Fatalf("added/changed users=%v; want UIDs 1 and 3", inbound.added)
	}
}

func testUserHash(user *api.UserInfo) string {
	return fmt.Sprintf("u%d", user.UID)
}

type syncUserListTestAPI struct {
	users []api.UserInfo
}

func (a *syncUserListTestAPI) GetNodeInfo() (*api.NodeInfo, error) {
	return nil, nil
}

func (a *syncUserListTestAPI) GetUserList() (*[]api.UserInfo, error) {
	return &a.users, nil
}

func (a *syncUserListTestAPI) ReportNodeStatus(*api.NodeStatus) error {
	return nil
}

func (a *syncUserListTestAPI) ReportNodeOnlineUsers(*[]api.OnlineUser) error {
	return nil
}

func (a *syncUserListTestAPI) ReportUserTraffic(*[]api.UserTraffic) error {
	return nil
}

func (a *syncUserListTestAPI) Describe() api.ClientInfo {
	return api.ClientInfo{}
}

func (a *syncUserListTestAPI) GetNodeRule() (*[]api.DetectRule, error) {
	return nil, nil
}

func (a *syncUserListTestAPI) ReportIllegal(*[]api.DetectResult) error {
	return nil
}

func (a *syncUserListTestAPI) Debug() {
}

type incrementalTestInbound struct {
	added         []api.UserInfo
	removed       []string
	fullRefreshes [][]api.UserInfo
}

func (i *incrementalTestInbound) Type() string {
	return C.TypeAnyTLS
}

func (i *incrementalTestInbound) Tag() string {
	return "anytls-in"
}

func (i *incrementalTestInbound) Start(adapter.StartStage) error {
	return nil
}

func (i *incrementalTestInbound) Close() error {
	return nil
}

func (i *incrementalTestInbound) AddUsers(users *[]api.UserInfo, nodeInfo *api.NodeInfo) error {
	i.added = append(i.added, (*users)...)
	return nil
}

func (i *incrementalTestInbound) RemoveUsers(names []string) error {
	i.removed = append(i.removed, names...)
	return nil
}

func (i *incrementalTestInbound) RefreshUsers(users *[]api.UserInfo, nodeInfo *api.NodeInfo) error {
	i.fullRefreshes = append(i.fullRefreshes, append([]api.UserInfo(nil), (*users)...))
	return nil
}

func userUIDSet(users []api.UserInfo) map[int]bool {
	result := make(map[int]bool, len(users))
	for _, user := range users {
		result[user.UID] = true
	}
	return result
}
