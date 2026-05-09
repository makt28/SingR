package controller

import (
	"context"
	"regexp"
	"sync"
	"testing"
	"time"

	"github.com/sagernet/sing-box/adapter"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/poet/api"
)

func TestDetermineRateMatchesXrayR(t *testing.T) {
	cases := []struct {
		node uint64
		user uint64
		want uint64
	}{
		{0, 0, 0},
		{0, 100, 100},
		{200, 0, 200},
		{100, 200, 100},
		{200, 100, 100},
		{100, 100, 100},
	}
	for _, tc := range cases {
		if got := determineRate(tc.node, tc.user); got != tc.want {
			t.Errorf("determineRate(node=%d, user=%d)=%d; want %d", tc.node, tc.user, got, tc.want)
		}
	}
}

func TestSyncUserListAppliesPerUserSpeedLimit(t *testing.T) {
	users := []api.UserInfo{
		{UID: 1, SpeedLimit: 0},     // 0 → fall back to node = 50
		{UID: 2, SpeedLimit: 30},    // user < node → 30
		{UID: 3, SpeedLimit: 9999},  // user > node → 50
	}
	c, _ := newSpeedLimitTestController(t, &api.NodeInfo{NodeID: 1, NodeType: C.TypeAnyTLS, SpeedLimit: 50}, users)

	if err := c.syncUserList(); err != nil {
		t.Fatal(err)
	}

	expected := map[int]int{1: 50, 2: 30, 3: 50}
	for _, u := range c.author.ListUsers() {
		send, recv := u.GetSpeedLimit()
		if send != expected[u.UID] || recv != expected[u.UID] {
			t.Errorf("UID=%d send=%d recv=%d; want %d/%d", u.UID, send, recv, expected[u.UID], expected[u.UID])
		}
	}
}

func TestRefreshAllSpeedLimitsOnNodeRateChange(t *testing.T) {
	users := []api.UserInfo{
		{UID: 1, SpeedLimit: 0}, // tracks node
		{UID: 2, SpeedLimit: 1000},
	}
	c, _ := newSpeedLimitTestController(t, &api.NodeInfo{NodeID: 1, NodeType: C.TypeAnyTLS, SpeedLimit: 100}, users)
	if err := c.syncUserList(); err != nil {
		t.Fatal(err)
	}

	// Now the panel raises the node speed limit — the user-with-no-limit
	// should pick that up; the user-with-1000 still wins min(1000, 500)=500.
	c.nodeInfo.SpeedLimit = 500
	c.refreshAllSpeedLimits()

	expected := map[int]int{1: 500, 2: 500}
	for _, u := range c.author.ListUsers() {
		send, _ := u.GetSpeedLimit()
		if send != expected[u.UID] {
			t.Errorf("UID=%d send=%d; want %d", u.UID, send, expected[u.UID])
		}
	}
}

func TestUserWaitNZeroLimiterIsNoop(t *testing.T) {
	user := &User{}
	user.SetSpeedLimit(0, 0)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := user.WaitN(ctx, 1<<20, 1<<20); err != nil {
		t.Fatalf("WaitN with zero limit returned err: %v", err)
	}
}

func TestUserWaitNHonorsBurstChunking(t *testing.T) {
	user := &User{}
	// 1 MB/s rate, burst = 1 MB. Asking for 4 MB should chunk into 4
	// burst-sized waits and complete in a few seconds.
	user.SetSpeedLimit(1<<20, 1<<20)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	start := time.Now()
	if err := user.WaitN(ctx, 4<<20, 0); err != nil {
		t.Fatalf("WaitN failed: %v", err)
	}
	elapsed := time.Since(start)
	// Token bucket starts full (1 burst of credit). 4 MB at 1 MB/s with
	// 1 MB burst → ~3 s after the initial burst is consumed. Allow slack.
	if elapsed < 2*time.Second || elapsed > 5*time.Second {
		t.Errorf("WaitN(4MiB) elapsed=%v; expected ~3s", elapsed)
	}
}

func TestMatchAuditDedupsRepeatedHits(t *testing.T) {
	c, _ := newSpeedLimitTestController(t, &api.NodeInfo{NodeID: 1, NodeType: C.TypeAnyTLS}, nil)
	rules := []api.DetectRule{
		{ID: 7, Pattern: regexp.MustCompile(`(?i)example\.com$`)},
	}
	c.detectRules.Store(&rules)

	if matched, id := c.MatchAudit(42, "www.example.com"); !matched || id != 7 {
		t.Fatalf("first match: matched=%v id=%d; want true,7", matched, id)
	}
	if matched, id := c.MatchAudit(42, "other.example.com"); !matched || id != 7 {
		t.Fatalf("repeat match: matched=%v id=%d; want true,7", matched, id)
	}
	if matched, _ := c.MatchAudit(42, "ok.org"); matched {
		t.Fatalf("non-matching host should not match")
	}

	hits := c.drainDetectResults()
	if len(hits) != 1 {
		t.Fatalf("dedup failed: drainDetectResults=%v; want 1 entry", hits)
	}
	if hits[0].UID != 42 || hits[0].RuleID != 7 {
		t.Fatalf("hit=%+v; want {UID:42 RuleID:7}", hits[0])
	}
	if again := c.drainDetectResults(); len(again) != 0 {
		t.Fatalf("drain should clear; got %v", again)
	}
}

func TestMatchAuditNoRulesNoMatch(t *testing.T) {
	c, _ := newSpeedLimitTestController(t, &api.NodeInfo{NodeID: 1, NodeType: C.TypeAnyTLS}, nil)
	if matched, _ := c.MatchAudit(1, "anything.com"); matched {
		t.Fatalf("should not match when rule list is unset")
	}
	empty := []api.DetectRule{}
	c.detectRules.Store(&empty)
	if matched, _ := c.MatchAudit(1, "anything.com"); matched {
		t.Fatalf("should not match when rule list is empty")
	}
}

func TestRefreshDetectRulesPreservesOnNotModified(t *testing.T) {
	api1 := &auditTestAPI{
		rules: []api.DetectRule{{ID: 1, Pattern: regexp.MustCompile(`bad\.com`)}},
	}
	c, _ := newSpeedLimitTestControllerWithAPI(t, &api.NodeInfo{NodeID: 1, NodeType: C.TypeAnyTLS}, nil, api1)
	c.refreshDetectRules()
	if got := c.detectRules.Load(); got == nil || len(*got) != 1 {
		t.Fatalf("first refresh should install 1 rule")
	}

	// Second call returns RuleNotModified — the existing list should be retained.
	api1.notModified = true
	c.refreshDetectRules()
	if got := c.detectRules.Load(); got == nil || len(*got) != 1 || (*got)[0].ID != 1 {
		t.Fatalf("RuleNotModified should preserve existing rules; got=%v", got)
	}
}

func TestUserInfoMonitorReportsAuditHits(t *testing.T) {
	api1 := &auditTestAPI{}
	c, _ := newSpeedLimitTestControllerWithAPI(t, &api.NodeInfo{NodeID: 1, NodeType: C.TypeAnyTLS}, nil, api1)
	// Make userInfoMonitor's "delay to start" check pass.
	c.startAt = time.Now().Add(-2 * time.Hour)
	c.config = &Config{UpdatePeriodic: 60}

	// Pre-load some audit hits.
	c.recordDetect(11, 22)
	c.recordDetect(33, 44)

	if err := c.userInfoMonitor(); err != nil {
		t.Fatalf("userInfoMonitor: %v", err)
	}
	if len(api1.illegal) != 2 {
		t.Fatalf("ReportIllegal got %d hits; want 2 (%v)", len(api1.illegal), api1.illegal)
	}
	// Drain should be empty after report.
	if got := c.drainDetectResults(); len(got) != 0 {
		t.Fatalf("hits not drained: %v", got)
	}
}

// --- helpers ---

type auditTestAPI struct {
	syncUserListTestAPI
	rules       []api.DetectRule
	notModified bool
	illegal     []api.DetectResult
	mu          sync.Mutex
}

func (a *auditTestAPI) GetNodeRule() (*[]api.DetectRule, error) {
	if a.notModified {
		return nil, errRuleNotModified
	}
	cp := append([]api.DetectRule(nil), a.rules...)
	return &cp, nil
}

func (a *auditTestAPI) ReportIllegal(hits *[]api.DetectResult) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.illegal = append(a.illegal, (*hits)...)
	return nil
}

// errRuleNotModified mirrors the sentinel returned by sspanel.APIClient.
type ruleNotModifiedErr struct{}

func (ruleNotModifiedErr) Error() string { return api.RuleNotModified }

var errRuleNotModified error = ruleNotModifiedErr{}

func newSpeedLimitTestController(t *testing.T, node *api.NodeInfo, users []api.UserInfo) (*Controller, *incrementalTestInbound) {
	t.Helper()
	return newSpeedLimitTestControllerWithAPI(t, node, users, &syncUserListTestAPI{users: users})
}

func newSpeedLimitTestControllerWithAPI(t *testing.T, node *api.NodeInfo, users []api.UserInfo, apiClient api.API) (*Controller, *incrementalTestInbound) {
	t.Helper()
	inbound := &incrementalTestInbound{}
	var adapterInbound adapter.Inbound = inbound
	author, err := NewAuthenticator(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	c := &Controller{
		apiClient:   apiClient,
		nodeInfo:    node,
		usersMap:    map[string]*api.UserInfo{},
		panelType:   "SSpanel",
		inbound:     &adapterInbound,
		logger:      log.NewNOPFactory().Logger(),
		author:      author,
		ctx:         context.Background(),
		detectDedup: map[detectKey]time.Time{},
	}
	return c, inbound
}
