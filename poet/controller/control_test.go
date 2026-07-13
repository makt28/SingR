package controller

import (
	"context"
	"errors"
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

func TestSyncUserListRetriesIncrementalAddBeforeCommit(t *testing.T) {
	oldUsers := []api.UserInfo{{UID: 1, UUID: "uuid-1"}}
	nextUsers := []api.UserInfo{{UID: 1, UUID: "uuid-1"}, {UID: 2, UUID: "uuid-2"}}
	inbound := &incrementalTestInbound{addFailures: 1}
	controller := newSyncFailureTestController(t, oldUsers, nextUsers, inbound)
	defer controller.author.Close()

	if err := controller.syncUserList(); err == nil {
		t.Fatal("expected injected AddUsers failure")
	}
	if _, committed := controller.usersMap["u2"]; committed {
		t.Fatal("failed AddUsers was committed to usersMap")
	}
	if _, exists := controller.author.LoadUser("u2"); exists {
		t.Fatal("failed AddUsers was committed to authenticator")
	}

	if err := controller.syncUserList(); err != nil {
		t.Fatalf("retry sync: %v", err)
	}
	if inbound.addCalls != 2 {
		t.Fatalf("AddUsers calls = %d, want 2", inbound.addCalls)
	}
	if _, committed := controller.usersMap["u2"]; !committed {
		t.Fatal("successful retry did not commit usersMap")
	}
	if _, exists := controller.author.LoadUser("u2"); !exists {
		t.Fatal("successful retry did not commit authenticator")
	}
}

func TestSyncUserListRetriesIncrementalRemoveBeforeCommit(t *testing.T) {
	oldUsers := []api.UserInfo{{UID: 1, UUID: "uuid-1"}, {UID: 2, UUID: "uuid-2"}}
	nextUsers := []api.UserInfo{{UID: 1, UUID: "uuid-1"}}
	inbound := &incrementalTestInbound{removeFailures: 1}
	controller := newSyncFailureTestController(t, oldUsers, nextUsers, inbound)
	defer controller.author.Close()

	if err := controller.syncUserList(); err == nil {
		t.Fatal("expected injected RemoveUsers failure")
	}
	if _, committed := controller.usersMap["u2"]; !committed {
		t.Fatal("failed RemoveUsers deleted user from usersMap")
	}
	if _, exists := controller.author.LoadUser("u2"); !exists {
		t.Fatal("failed RemoveUsers deleted user from authenticator")
	}

	if err := controller.syncUserList(); err != nil {
		t.Fatalf("retry sync: %v", err)
	}
	if inbound.removeCalls != 2 {
		t.Fatalf("RemoveUsers calls = %d, want 2", inbound.removeCalls)
	}
	if _, committed := controller.usersMap["u2"]; committed {
		t.Fatal("successful retry did not delete user from usersMap")
	}
	if _, exists := controller.author.LoadUser("u2"); exists {
		t.Fatal("successful retry did not delete user from authenticator")
	}
}

func TestSyncUserListRetriesWholeDeltaAfterPartialIncrementalFailure(t *testing.T) {
	oldUsers := []api.UserInfo{{UID: 1, UUID: "uuid-1"}, {UID: 2, UUID: "uuid-2"}}
	nextUsers := []api.UserInfo{{UID: 1, UUID: "uuid-1-rotated"}, {UID: 3, UUID: "uuid-3"}}
	inbound := &incrementalTestInbound{addFailures: 1}
	controller := newSyncFailureTestController(t, oldUsers, nextUsers, inbound)
	defer controller.author.Close()

	if err := controller.syncUserList(); err == nil {
		t.Fatal("expected injected AddUsers failure after successful RemoveUsers")
	}
	if _, oldStillCommitted := controller.usersMap["u2"]; !oldStillCommitted {
		t.Fatal("partial runtime update committed controller map")
	}
	if err := controller.syncUserList(); err != nil {
		t.Fatalf("retry sync: %v", err)
	}
	if inbound.removeCalls != 2 || inbound.addCalls != 2 {
		t.Fatalf("retry calls remove=%d add=%d, want 2 each", inbound.removeCalls, inbound.addCalls)
	}
	if _, exists := controller.usersMap["u2"]; exists {
		t.Fatal("deleted user remained after successful retry")
	}
	if _, exists := controller.usersMap["u3"]; !exists {
		t.Fatal("added user missing after successful retry")
	}
}

func TestSyncUserListRetriesFullRefreshBeforeCommit(t *testing.T) {
	oldUsers := []api.UserInfo{{UID: 1, UUID: "uuid-1"}}
	nextUsers := []api.UserInfo{{UID: 1, UUID: "uuid-1"}, {UID: 2, UUID: "uuid-2"}}
	inbound := &fullRefreshTestInbound{failures: 1}
	controller := newSyncFailureTestController(t, oldUsers, nextUsers, inbound)
	defer controller.author.Close()

	if err := controller.syncUserList(); err == nil {
		t.Fatal("expected injected RefreshUsers failure")
	}
	if _, committed := controller.usersMap["u2"]; committed {
		t.Fatal("failed RefreshUsers was committed")
	}
	if err := controller.syncUserList(); err != nil {
		t.Fatalf("retry sync: %v", err)
	}
	if inbound.calls != 2 {
		t.Fatalf("RefreshUsers calls = %d, want 2", inbound.calls)
	}
	if _, committed := controller.usersMap["u2"]; !committed {
		t.Fatal("successful full refresh retry was not committed")
	}
}

func TestDumpTrafficForDeletedReportsOnceAndResetsCounters(t *testing.T) {
	author, err := NewAuthenticator(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer author.Close()
	for _, profile := range []api.UserInfo{
		{UID: 101, Email: "one@example.com"},
		{UID: 102, Email: "two@example.com"},
	} {
		hash := testUserHash(&profile)
		if err := author.AddUser(hash, profile.UID); err != nil {
			t.Fatal(err)
		}
		author.SetUserProfile(hash, profile)
	}
	user1, _ := author.LoadUser("u101")
	user2, _ := author.LoadUser("u102")
	user1.SetTraffic(123, 456)
	user2.SetTraffic(789, 987)

	apiClient := &syncUserListTestAPI{}
	controller := &Controller{apiClient: apiClient, nodeInfo: &api.NodeInfo{NodeID: 1}, author: author, logger: log.NewNOPFactory().Logger()}
	if err := controller.dumpTrafficForDeleted([]string{"u101", "missing", "u102"}); err != nil {
		t.Fatal(err)
	}
	if len(apiClient.trafficReports) != 1 {
		t.Fatalf("report calls = %d, want 1", len(apiClient.trafficReports))
	}
	report := apiClient.trafficReports[0]
	if len(report) != 2 {
		t.Fatalf("report = %#v, want 2 users", report)
	}
	byUID := make(map[int]api.UserTraffic, len(report))
	for _, traffic := range report {
		byUID[traffic.UID] = traffic
	}
	if byUID[101] != (api.UserTraffic{UID: 101, Email: "one@example.com", Upload: 123, Download: 456}) {
		t.Fatalf("UID 101 traffic = %#v", byUID[101])
	}
	if byUID[102] != (api.UserTraffic{UID: 102, Email: "two@example.com", Upload: 789, Download: 987}) {
		t.Fatalf("UID 102 traffic = %#v", byUID[102])
	}
	if sent, recv := user1.GetTraffic(); sent != 0 || recv != 0 {
		t.Fatalf("UID 101 counters after report = (%d, %d), want zero", sent, recv)
	}
	if sent, recv := user2.GetTraffic(); sent != 0 || recv != 0 {
		t.Fatalf("UID 102 counters after report = (%d, %d), want zero", sent, recv)
	}

	if err := controller.dumpTrafficForDeleted([]string{"u101", "u102"}); err != nil {
		t.Fatal(err)
	}
	if len(apiClient.trafficReports) != 1 {
		t.Fatalf("zero traffic caused another report: %d calls", len(apiClient.trafficReports))
	}
}

func TestDumpTrafficForDeletedDiscardsSwappedBytesOnReportFailure(t *testing.T) {
	author, err := NewAuthenticator(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer author.Close()
	profile := api.UserInfo{UID: 101, Email: "one@example.com"}
	if err := author.AddUser("u101", profile.UID); err != nil {
		t.Fatal(err)
	}
	author.SetUserProfile("u101", profile)
	user, _ := author.LoadUser("u101")
	user.SetTraffic(123, 456)

	reportErr := errors.New("panel rejected traffic")
	apiClient := &syncUserListTestAPI{trafficReportErr: reportErr}
	controller := &Controller{apiClient: apiClient, nodeInfo: &api.NodeInfo{NodeID: 1}, author: author, logger: log.NewNOPFactory().Logger()}
	if err := controller.dumpTrafficForDeleted([]string{"u101"}); !errors.Is(err, reportErr) {
		t.Fatalf("error = %v, want %v", err, reportErr)
	}
	if len(apiClient.trafficReports) != 1 || len(apiClient.trafficReports[0]) != 1 {
		t.Fatalf("traffic reports = %#v, want one attempted report", apiClient.trafficReports)
	}
	if sent, recv := user.GetTraffic(); sent != 0 || recv != 0 {
		t.Fatalf("counters after failed report = (%d, %d), want discarded zero", sent, recv)
	}
}

func testUserHash(user *api.UserInfo) string {
	return fmt.Sprintf("u%d", user.UID)
}

type syncUserListTestAPI struct {
	users            []api.UserInfo
	trafficReports   [][]api.UserTraffic
	trafficReportErr error
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

func (a *syncUserListTestAPI) ReportUserTraffic(traffic *[]api.UserTraffic) error {
	a.trafficReports = append(a.trafficReports, append([]api.UserTraffic(nil), (*traffic)...))
	return a.trafficReportErr
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
	added          []api.UserInfo
	removed        []string
	fullRefreshes  [][]api.UserInfo
	addFailures    int
	removeFailures int
	addCalls       int
	removeCalls    int
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
	i.addCalls++
	if i.addFailures > 0 {
		i.addFailures--
		return errors.New("injected AddUsers failure")
	}
	i.added = append(i.added, (*users)...)
	return nil
}

func (i *incrementalTestInbound) RemoveUsers(names []string) error {
	i.removeCalls++
	if i.removeFailures > 0 {
		i.removeFailures--
		return errors.New("injected RemoveUsers failure")
	}
	i.removed = append(i.removed, names...)
	return nil
}

func (i *incrementalTestInbound) RefreshUsers(users *[]api.UserInfo, nodeInfo *api.NodeInfo) error {
	i.fullRefreshes = append(i.fullRefreshes, append([]api.UserInfo(nil), (*users)...))
	return nil
}

type fullRefreshTestInbound struct {
	calls    int
	failures int
}

func (*fullRefreshTestInbound) Type() string                   { return C.TypeAnyTLS }
func (*fullRefreshTestInbound) Tag() string                    { return "anytls-in" }
func (*fullRefreshTestInbound) Start(adapter.StartStage) error { return nil }
func (*fullRefreshTestInbound) Close() error                   { return nil }
func (i *fullRefreshTestInbound) RefreshUsers(*[]api.UserInfo, *api.NodeInfo) error {
	i.calls++
	if i.failures > 0 {
		i.failures--
		return errors.New("injected RefreshUsers failure")
	}
	return nil
}

func newSyncFailureTestController(t *testing.T, oldUsers, nextUsers []api.UserInfo, inbound adapter.Inbound) *Controller {
	t.Helper()
	author, err := NewAuthenticator(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	oldMap := make(map[string]*api.UserInfo, len(oldUsers))
	for index := range oldUsers {
		user := oldUsers[index]
		hash := testUserHash(&user)
		oldMap[hash] = &oldUsers[index]
		if err := author.AddUser(hash, user.UID); err != nil {
			author.Close()
			t.Fatal(err)
		}
		author.SetUserProfile(hash, user)
	}
	adapterInbound := inbound
	return &Controller{
		apiClient: &syncUserListTestAPI{users: nextUsers},
		nodeInfo:  &api.NodeInfo{NodeID: 1, NodeType: C.TypeAnyTLS},
		usersMap:  oldMap,
		panelType: "SSpanel",
		inbound:   &adapterInbound,
		logger:    log.NewNOPFactory().Logger(),
		author:    author,
		ctx:       context.Background(),
	}
}

func userUIDSet(users []api.UserInfo) map[int]bool {
	result := make(map[int]bool, len(users))
	for _, user := range users {
		result[user.UID] = true
	}
	return result
}
