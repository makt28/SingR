package controller

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/poet/api"
	"github.com/sagernet/sing-box/poet/common/serverstatus"
	"github.com/sagernet/sing-box/poet/common/task"
)

type Controller struct {
	config     *Config
	clientInfo api.ClientInfo
	apiClient  api.API
	nodeInfo   *api.NodeInfo
	Tag        string
	// userList   *[]api.UserInfo
	usersMap map[string]*api.UserInfo

	tasks []periodicTask
	// limitedUsers map[api.UserInfo]LimitInfo
	// warnedUsers  map[api.UserInfo]int
	// Panel type: SSpanel, NewV2board, PMpanel, Proxypanel, V2RaySocks, GoV2Panel
	panelType string
	// stm          stats.Manager
	// dispatcher   *mydispatcher.DefaultDispatcher
	startAt time.Time
	inbound *adapter.Inbound
	logger  log.Logger

	//用户统计与验证
	author *Authenticator
	ctx    context.Context

	// Audit / detect rules pulled from SSPanel `/mod_mu/func/detect_rules`.
	// Stored as an atomic pointer so RoutedConnection's match path is
	// lock-free; nodeInfoMonitor swaps the slice atomically when the
	// panel returns a new ETag.
	detectRules atomic.Pointer[[]api.DetectRule]
	// detectResults accumulates audit hits between report cycles. The
	// dedup map keys (uid, ruleID) → expiry time so a single user
	// scanning across hosts that all match the same rule does not
	// produce thousands of identical reports.
	detectMu       sync.Mutex
	detectResults  []api.DetectResult
	detectDedup    map[detectKey]time.Time
	detectGetWarns int

	// reloadPending is set when ConfigureFromPanelNode fails so the next
	// nodeInfoMonitor tick retries even if the panel answers 304
	// NodeNotModified. Without it a transient reload failure (port
	// briefly occupied, UDP socket not yet released) would never be
	// retried — the `nodeInfoChanged` gate stays false under 304s — and
	// an inbound stuck on stale config (or fully down, for hysteria2's
	// SNI-only rebuild which closes the old service before starting the
	// new one) would stay that way until the panel data happens to
	// change. Only touched from the nodeInfoMonitor goroutine; no lock
	// needed.
	reloadPending bool

	// environment = development testing staging production
	env string
}

// detectKey identifies a unique (user, rule) pair for audit dedup.
type detectKey struct {
	UID    int
	RuleID int
}

// detectDedupTTL is the cooldown window during which repeat hits of the
// same (uid, ruleID) are suppressed. 60 s mirrors XrayR's de-facto
// behavior of "one report per cycle" at the typical 60 s report
// interval.
const detectDedupTTL = 60 * time.Second

type periodicTask struct {
	tag string
	*task.Periodic
}

// New return a Controller service with default parameters.
func New(ctx context.Context, config Config, inbound *adapter.Inbound, apiClient api.API, logger log.Logger) *Controller {

	controller := &Controller{
		panelType: config.PanelType,
		config:    &config,
		apiClient: apiClient,
		// userList:  &[]api.UserInfo{},
		usersMap: make(map[string]*api.UserInfo),

		startAt: time.Now(),
		inbound: inbound,

		ctx:         ctx,
		logger:      logger,
		detectDedup: make(map[detectKey]time.Time),
	}
	author, err := NewAuthenticator(ctx)
	if err != nil {
		// NewAuthenticator currently never returns an error, but if it ever
		// does we must abort: the controller is unusable without the
		// authenticator (every Add/Del/Load user call would nil-deref).
		// logger.Panic only logs at panic level, so use the real builtin.
		logger.Error("err author init fail: ", err)
		panic("singr: failed to initialize authenticator: " + err.Error())
	}
	controller.author = author

	return controller
}

// Start implement the Start() function of the service interface
func (c *Controller) Start() error {

	// log := c.config.Logger

	c.clientInfo = c.apiClient.Describe()

	// First fetch Node Info
	newNodeInfo, err := c.apiClient.GetNodeInfo()
	if err != nil {
		return err
	}
	if newNodeInfo.Port == 0 {
		return errors.New("server port must > 0")
	}

	//简单验证信息
	// if newNodeInfo.NodeType != c.clientInfo.NodeType {
	// 	return errors.New("node type must be same")
	// }

	c.nodeInfo = newNodeInfo
	c.Tag = c.buildNodeTag() //== inTag

	if configurable, ok := (*c.inbound).(adapter.PStartupConfigurableInbound); ok {
		if err := configurable.ConfigureFromPanelNode(c.nodeInfo); err != nil {
			return err
		}
	}

	//sync controller user list
	err = c.syncUserList()
	if err != nil {
		return err
	}

	// Add periodic tasks
	c.tasks = append(c.tasks,
		periodicTask{
			tag: "node monitor",
			Periodic: &task.Periodic{
				Interval: time.Duration(c.config.UpdatePeriodic) * time.Second,
				Execute:  c.nodeInfoMonitor,
			}},
		periodicTask{
			tag: "user monitor",
			Periodic: &task.Periodic{
				Interval: time.Duration(c.config.UpdatePeriodic) * time.Second,
				Execute:  c.userInfoMonitor,
			}},
	)

	// Start periodic tasks
	for i := range c.tasks {
		c.log("start periodic task: "+c.tasks[i].tag, "info")
		go c.tasks[i].Start()
	}
	return nil
}

// Close implement the Close() function of the service interface
func (c *Controller) Close() error {
	log := c.config.Logger

	for i := range c.tasks {
		if c.tasks[i].Periodic != nil {
			if err := c.tasks[i].Periodic.Close(); err != nil {
				// log.Panic("%s %s periodic task close failed: %s", c.logPrefix(), c.tasks[i].tag, err)
				log.Panic("periodic task close failed: %s", err)
				return err
			}
		}
	}

	return nil
}

func (c *Controller) buildNodeTag() string {
	return fmt.Sprintf("%s_%s_%d", c.nodeInfo.NodeType, c.config.ListenIP, c.nodeInfo.Port)
}

func (c *Controller) Env() string {
	if c.env == "" {
		if os.Getenv("WSL_DISTRO_NAME") != "" {
			c.env = "development"
		} else {
			c.env = "staging"
		}
	}
	return c.env
}

func (c *Controller) log(message any, level string) {
	message = fmt.Sprintf("Node[%d] %v", c.nodeInfo.NodeID, message)

	switch level {
	case "debug":
		if c.Env() == "development" {
			fmt.Println(time.Now().Format("01-02 15:04:05 "), message) //std output
		}
		c.logger.Debug(message)
	case "warn":
		c.logger.Warn(message)
	case "error":
		c.logger.Error(message)
	case "panic":
		c.logger.Panic(message)
	default:
		c.logger.Info(message)
	}
}

func (c *Controller) nodeInfoMonitor() (err error) {
	// delay to start
	if time.Since(c.startAt) < time.Duration(c.config.UpdatePeriodic)*time.Second {
		return nil
	}

	// First fetch Node Info
	var nodeInfoChanged = true
	newNodeInfo, err := c.apiClient.GetNodeInfo()
	if err != nil {
		if err.Error() == api.NodeNotModified {
			nodeInfoChanged = false
			newNodeInfo = c.nodeInfo
		} else {
			c.log(fmt.Sprintf("nodeInfoMonitor>>GetNodeInfo %v", err), "error")
			return nil
		}
	}
	if newNodeInfo.Port == 0 {
		return errors.New("server port must > 0")
	}
	//new Node Info
	prevNodeSpeedLimit := uint64(0)
	if c.nodeInfo != nil {
		prevNodeSpeedLimit = c.nodeInfo.SpeedLimit
	}
	c.nodeInfo = newNodeInfo

	// Update User
	err = c.syncUserList()
	if err != nil {
		c.log(fmt.Sprintf("nodeInfoMonitor>>syncUserList %v", err), "error")
		return nil
	}

	// If the node-wide speed limit changed, recompute every user's limiter
	// against the new value. syncUserList already reapplies limiters for
	// added/changed users; this catches everyone else. Cheap O(N).
	if newNodeInfo.SpeedLimit != prevNodeSpeedLimit {
		c.log(fmt.Sprintf("node speed limit changed %d -> %d Bps; refreshing %d users", prevNodeSpeedLimit, newNodeInfo.SpeedLimit, len(c.usersMap)), "info")
		c.refreshAllSpeedLimits()
	}

	// Refresh audit rule list. RuleNotModified / network errors are
	// non-fatal — keep whatever we had. A panel without `/mod_mu/func/detect_rules`
	// will return an error here on every cycle; we log once at debug so
	// it doesn't dominate logs.
	c.refreshDetectRules()

	// If nodeInfo changed — or a previous reload attempt failed — ask the
	// inbound to hot-reload its panel-derived settings (port, SNI). The
	// inbound is responsible for diffing and no-op'ing if nothing actually
	// changed. A failed reload leaves the inbound's own options
	// uncommitted, so retrying with the same nodeInfo re-detects the diff
	// and re-attempts the swap; reloadPending keeps that retry alive
	// across 304 NodeNotModified cycles.
	if nodeInfoChanged || c.reloadPending {
		if configurable, ok := (*c.inbound).(adapter.PStartupConfigurableInbound); ok {
			if err := configurable.ConfigureFromPanelNode(c.nodeInfo); err != nil {
				c.reloadPending = true
				c.log(fmt.Sprintf("nodeInfoMonitor>>ConfigureFromPanelNode %v (will retry next cycle)", err), "error")
			} else {
				c.reloadPending = false
			}
		}
	}

	return nil
}

func (c *Controller) userInfoMonitor() (err error) {
	// delay to start
	if time.Since(c.startAt) < time.Duration(c.config.UpdatePeriodic)*time.Second {
		return nil
	}

	// Get server status
	CPU, Mem, Disk, Uptime, err := serverstatus.GetSystemInfo()
	if err != nil {
		c.log(fmt.Sprintf("userInfoMonitor>>ReportNodeStatus %v", err), "error")
	} else {
		err = c.apiClient.ReportNodeStatus(
			&api.NodeStatus{
				CPU:    CPU,
				Mem:    Mem,
				Disk:   Disk,
				Uptime: Uptime,
			})
		if err != nil {
			c.log(err, "error")
		}
		c.log(fmt.Sprintf("userInfoMonitor>>ReportNodeStatus CPU %.2f, Mem %.2f, Disk %.2f, Uptime %v, err %v", CPU/100, Mem/100, Disk/100, strconv.FormatUint(Uptime, 10), err), "info")
	}

	// Get User traffic
	var userTraffic []api.UserTraffic
	var onlineUsers []api.OnlineUser
	userArr := c.author.ListUsers()
	debug := os.Getenv("SINGR_TRAFFIC_DEBUG") == "1"
	if len(userArr) == 0 {
		c.log("traffic zero online user", "info")
		// Fall through so accumulated audit hits still get reported.
	}
	var sumSent, sumRecv int64
	if debug {
		c.log(fmt.Sprintf("[TRAFFIC] userInfoMonitor begin, ListUsers count=%d", len(userArr)), "debug")
	}
	for _, user := range userArr {
		// 获取用户流量
		sent, recv := user.ResetTraffic()
		if debug && (sent != 0 || recv != 0) {
			c.log(fmt.Sprintf("[TRAFFIC] pre-report user UID=%d hash=%s email=%s sent=%d recv=%d", user.UID, user.Hash(), user.Email, sent, recv), "debug")
			sumSent += sent
			sumRecv += recv
		}
		if sent == 0 && recv == 0 {
			continue
		}

		up, down := trafficForSSPanel(sent, recv)

		// c.log(fmt.Sprintf("get traffic user:%s sent:%d up:%d recv:%d down:%d rate:%f", user.hash, sent, up, recv, down, c.nodeInfo.TrafficRate), "debug")

		userTraffic = append(userTraffic, api.UserTraffic{
			UID:      user.UID,
			Email:    user.Email,
			Upload:   up,
			Download: down})

		// 在线用户
		iplist := user.ResetIPTable()
		for _, ip := range iplist {
			onlineUsers = append(onlineUsers, api.OnlineUser{
				UID: user.UID,
				IP:  ip,
			})

		} //END online user
	}

	userCounter := len(userTraffic)
	ipCounter := len(onlineUsers)
	if debug {
		c.log(fmt.Sprintf("[TRAFFIC] userInfoMonitor sums sumSent=%d sumRecv=%d records=%d", sumSent, sumRecv, userCounter), "debug")
	}
	//report traffic
	if userCounter > 0 {
		c.log(fmt.Sprintf("reporting %d user traffic records; first UID=%d email=%s upload=%d download=%d", userCounter, userTraffic[0].UID, userTraffic[0].Email, userTraffic[0].Upload, userTraffic[0].Download), "info")
		// On failure, the bytes captured by ResetTraffic are intentionally
		// discarded. Restoring them caused a quadratic over-report blowup
		// when the panel processed the request but the client saw a
		// timeout / connection error: each subsequent cycle would
		// re-include the same bytes, and the panel would re-add them.
		// Bounded under-reporting (1 cycle's bytes lost) is preferred
		// over unbounded over-reporting.
		if err = c.apiClient.ReportUserTraffic(&userTraffic); err != nil {
			c.log(fmt.Sprintf("report traffic err:%v (discarding %d records)", err.Error(), userCounter), "error")
		}
	}
	//report online users
	if ipCounter > 0 {
		if err = c.apiClient.ReportNodeOnlineUsers(&onlineUsers); err != nil {
			c.log(fmt.Sprintf("report online users err:%v", err.Error()), "error")
		}
	}
	c.log(fmt.Sprintf("userInfoMonitor>>ReportUserOnline userCounter:%d ipCounter:%d", userCounter, ipCounter), "info")

	// Drain accumulated audit hits and report them. On failure the slice
	// is intentionally discarded — keeping it would let one persistently
	// failing panel grow the buffer unboundedly, and audit dedup already
	// suppresses repeats within `detectDedupTTL` so dropping a cycle's
	// worth is bounded.
	if hits := c.drainDetectResults(); len(hits) > 0 {
		c.log(fmt.Sprintf("reporting %d audit hits", len(hits)), "info")
		if err = c.apiClient.ReportIllegal(&hits); err != nil {
			c.log(fmt.Sprintf("report illegal err:%v (discarding %d records)", err.Error(), len(hits)), "error")
		}
	}

	return nil
}

// refreshDetectRules pulls the latest audit rules from the panel and
// atomically swaps `c.detectRules`. RuleNotModified preserves the
// existing list. Other errors are logged once at debug (then suppressed)
// so a panel that doesn't expose the endpoint doesn't dominate logs.
func (c *Controller) refreshDetectRules() {
	rules, err := c.apiClient.GetNodeRule()
	if err != nil {
		if err.Error() == api.RuleNotModified {
			return
		}
		if c.detectGetWarns == 0 {
			c.log(fmt.Sprintf("GetNodeRule failed (suppressing further warnings): %v", err), "debug")
		}
		c.detectGetWarns++
		return
	}
	c.detectGetWarns = 0
	if rules == nil {
		empty := []api.DetectRule{}
		c.detectRules.Store(&empty)
		return
	}
	c.detectRules.Store(rules)
}

// MatchAudit checks `host` against the active rule list and, on a match,
// records a `DetectResult{UID, RuleID}` for the next ReportIllegal cycle.
// Returns (matched, ruleID). Repeats of the same (uid, ruleID) within
// detectDedupTTL are recorded as matched but not re-appended.
//
// host should be the connection's destination FQDN (metadata.Destination.Fqdn).
// IP-only destinations are not matched (XrayR audit semantics: rules are
// regexes meant for hostnames / SNIs).
func (c *Controller) MatchAudit(uid int, host string) (bool, int) {
	if host == "" {
		return false, 0
	}
	rulesPtr := c.detectRules.Load()
	if rulesPtr == nil {
		return false, 0
	}
	rules := *rulesPtr
	for i := range rules {
		if rules[i].Pattern == nil {
			continue
		}
		if rules[i].Pattern.MatchString(host) {
			c.recordDetect(uid, rules[i].ID)
			return true, rules[i].ID
		}
	}
	return false, 0
}

func (c *Controller) recordDetect(uid, ruleID int) {
	now := time.Now()
	key := detectKey{UID: uid, RuleID: ruleID}

	c.detectMu.Lock()
	defer c.detectMu.Unlock()

	if exp, ok := c.detectDedup[key]; ok && now.Before(exp) {
		return
	}
	c.detectDedup[key] = now.Add(detectDedupTTL)
	c.detectResults = append(c.detectResults, api.DetectResult{UID: uid, RuleID: ruleID})

	// Bound dedup map growth: walk and prune expired entries every time
	// the map grows past 4096. Cheap because the loop is O(N) and amortizes
	// against insert frequency.
	if len(c.detectDedup) > 4096 {
		for k, exp := range c.detectDedup {
			if now.After(exp) {
				delete(c.detectDedup, k)
			}
		}
	}
}

func (c *Controller) drainDetectResults() []api.DetectResult {
	c.detectMu.Lock()
	defer c.detectMu.Unlock()
	if len(c.detectResults) == 0 {
		return nil
	}
	out := c.detectResults
	c.detectResults = nil
	return out
}

// Author returns the controller's authenticator. Exposed for end-to-end
// tests in sim/ that need to register users / observe runtime state
// without driving the full panel sync loop.
func (c *Controller) Author() *Authenticator { return c.author }

// SetNodeInfo replaces the controller's view of the panel node info.
// Production code receives this through Start() / nodeInfoMonitor;
// tests use this to install a node config (esp. SpeedLimit) without
// running the full sync loop.
func (c *Controller) SetNodeInfo(info *api.NodeInfo) { c.nodeInfo = info }

// SetDetectRules atomically replaces the controller's audit rule list.
// Production: refreshDetectRules calls this from nodeInfoMonitor with
// rules pulled from /mod_mu/func/detect_rules. Tests: bypass the panel
// pull by installing rules directly.
func (c *Controller) SetDetectRules(rules []api.DetectRule) {
	c.detectRules.Store(&rules)
}

// DrainDetectResults removes and returns all accumulated audit hits.
// userInfoMonitor calls this once per cycle to feed ReportIllegal;
// tests call it to inspect what was recorded by RoutedConnection's
// audit-block path.
func (c *Controller) DrainDetectResults() []api.DetectResult {
	return c.drainDetectResults()
}
