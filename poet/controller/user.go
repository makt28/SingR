package controller

//https://github.com/bobby4k/trojan-go/blob/master/statistic/memory/memory.go
//atomic

import (
	"context"
	"fmt"
	"sync"

	"github.com/sagernet/sing/common/atomic"

	"github.com/sagernet/sing-box/poet/api"

	"golang.org/x/time/rate"
)

const Name = "MEMORY"

type User struct {
	// WARNING: do not change the order of these fields.
	// 64-bit fields that use `sync/atomic` package functions
	// must be 64-bit aligned on 32-bit systems.
	// Reference: https://github.com/golang/go/issues/599
	// Solution: https://github.com/golang/go/issues/11891#issuecomment-433623786
	sent      atomic.Int64
	recv      atomic.Int64
	lastSent  atomic.Int64
	lastRecv  atomic.Int64
	sendSpeed atomic.Int64
	recvSpeed atomic.Int64

	hash string
	UID  int

	Email  string
	UUID   string
	Passwd string

	ipTable     sync.Map
	ipNum       atomic.Int32
	counterSeen atomic.Bool
	// maxIPNum is written by the panel sync goroutine (SetIPLimit) and read
	// on every routed connection (AddIP). ipAccess makes AddIP's
	// check-then-store atomic, so two connections from two new addresses
	// cannot both slip under the limit.
	maxIPNum atomic.Int32
	ipAccess sync.Mutex

	// limiter is a SINGLE token bucket shared across both directions.
	// XrayR/SSPanel semantics: `node_speedlimit` and per-user
	// `SpeedLimit` describe the user's TOTAL bandwidth, not per-
	// direction. Wrapping inbound conn Read (upload) and Write
	// (download) against the same bucket means simultaneous up+down
	// competes for the same budget — same end-user experience as
	// XrayR. nil = unlimited.
	limiterLock sync.RWMutex
	limiter     *rate.Limiter

	ctx    context.Context
	cancel context.CancelFunc
}

func (u *User) Close() error {
	u.ResetTraffic()
	u.cancel()
	return nil
}

// AddIP registers a source address for the user and reports whether the
// connection may proceed. It returns false only when a device limit is set
// (SetIPLimit > 0) and this address would be one more than the limit; an
// address already registered always passes.
//
// The table is drained by ResetIPTable on every report cycle, so the limit
// counts distinct source addresses that opened a connection within one cycle
// (UpdatePeriodic, 60 s by default) — a connection that is already running
// is never cut off, only new ones are refused.
func (u *User) AddIP(ip string) bool {
	if _, found := u.ipTable.Load(ip); found {
		return true
	}

	u.ipAccess.Lock()
	defer u.ipAccess.Unlock()
	if _, found := u.ipTable.Load(ip); found {
		return true
	}
	if limit := u.maxIPNum.Load(); limit > 0 && u.ipNum.Load()+1 > limit {
		return false
	}
	u.ipTable.Store(ip, true)
	u.ipNum.Add(1)
	return true
}

func (u *User) DelIP(ip string) bool {
	u.ipAccess.Lock()
	defer u.ipAccess.Unlock()
	_, found := u.ipTable.Load(ip)
	if !found {
		return false
	}

	u.ipTable.Delete(ip)
	u.ipNum.Add(-1)
	return true
}

func (u *User) GetIPNumber() int {
	return int(u.ipNum.Load())
}

func (u *User) SetIPLimit(n int) {
	if n < 0 {
		n = 0
	}
	u.maxIPNum.Store(int32(n))
}

func (u *User) GetIPLimit() int {
	return int(u.maxIPNum.Load())
}

// WaitN blocks until the user's shared rate limiter allows `n` bytes.
// It does NOT mutate the byte counters — counter accounting is handled
// by `bufio.NewInt64CounterConn` wrapped underneath the rate-limited
// conn in poet.RoutedConnection.
//
// Both directions share one bucket (XrayR semantics): a user with
// SpeedLimit=10 MB/s can do 10 MB/s in one direction OR 5+5 MB/s split,
// not 10+10 MB/s.
//
// A nil limiter (limit == 0 → unlimited) returns immediately. A WaitN
// larger than the limiter burst is split into burst-sized chunks so the
// `golang.org/x/time/rate` rejection of `n > burst` does not surface
// to the caller (this is stricter than XrayR, which silently drops the
// limiter for oversized buffers — see CLAUDE.md).
func (u *User) WaitN(ctx context.Context, n int) error {
	if n <= 0 {
		return nil
	}
	u.limiterLock.RLock()
	l := u.limiter
	u.limiterLock.RUnlock()

	if l == nil {
		return nil
	}
	return waitChunked(ctx, l, n)
}

func waitChunked(ctx context.Context, l *rate.Limiter, n int) error {
	burst := l.Burst()
	if burst <= 0 {
		return nil
	}
	for n > 0 {
		chunk := n
		if chunk > burst {
			chunk = burst
		}
		if err := l.WaitN(ctx, chunk); err != nil {
			return err
		}
		n -= chunk
	}
	return nil
}

// SetSpeedLimit installs the user's shared rate limiter. bytesPerSec is
// the user's TOTAL bandwidth (up + down compete for the same bucket).
// <= 0 disables limiting (clears the bucket). Burst is `int(limit)` ≈
// one second of bytes (XrayR parity), so a single TCP buffer (≤ 64 KiB
// in practice) does not need chunking unless the user limit is below
// 64 KiB/s.
func (u *User) SetSpeedLimit(bytesPerSec int64) {
	u.limiterLock.Lock()
	defer u.limiterLock.Unlock()

	if bytesPerSec <= 0 {
		u.limiter = nil
		return
	}
	u.limiter = rate.NewLimiter(rate.Limit(bytesPerSec), int(bytesPerSec))
}

// GetSpeedLimit returns the user's current effective rate limit in
// bytes/second, or 0 if unlimited.
func (u *User) GetSpeedLimit() int {
	u.limiterLock.RLock()
	defer u.limiterLock.RUnlock()

	if u.limiter == nil {
		return 0
	}
	return int(u.limiter.Limit())
}

func (u *User) Hash() string {
	return u.hash
}

func (u *User) SetTraffic(send, recv int64) {
	u.sent.Store(send)
	u.recv.Store(recv)
}

func (u *User) GetTraffic() (int64, int64) {
	return u.sent.Load(), u.recv.Load()
}

func (u *User) GetTrafficPointer() (send, recv *atomic.Int64) {
	return &u.sent, &u.recv
}

func (u *User) ResetTraffic() (int64, int64) {
	sent := u.sent.Swap(0)
	recv := u.recv.Swap(0)
	u.lastSent.Store(0)
	u.lastRecv.Store(0)
	return sent, recv
}

func (u *User) ResetIPTable() []string {
	var iplist []string

	u.ipAccess.Lock()
	defer u.ipAccess.Unlock()
	u.ipTable.Range(func(key, value interface{}) bool {
		if ip, ok := key.(string); ok {
			iplist = append(iplist, ip)

			//reset
			u.ipTable.Delete(ip)
			u.ipNum.Add(-1)
		}
		return true
	})

	return iplist
}

func (u *User) GetSpeed() (int64, int64) {
	return u.sendSpeed.Load(), u.recvSpeed.Load()
}

func (u *User) MarkCounterAttached() bool {
	return u.counterSeen.CompareAndSwap(false, true)
}

type Authenticator struct {
	users   sync.Map
	aliases sync.Map
	ctx     context.Context
}

func (a *Authenticator) AddUser(hash string, uid int) error {
	if _, found := a.users.Load(hash); found {
		return fmt.Errorf("hash %s already exists", hash)
	}
	ctx, cancel := context.WithCancel(a.ctx)
	meter := &User{
		hash:   hash,
		UID:    uid,
		ctx:    ctx,
		cancel: cancel,
	}
	a.users.Store(hash, meter)
	return nil
}

func (a *Authenticator) DelUser(hash string) error {
	meter, found := a.users.Load(hash)
	if !found {
		return fmt.Errorf("hash %s not found", hash)
	}
	meter.(*User).Close()
	a.users.Delete(hash)
	a.deleteAliases(hash)
	return nil
}

func (a *Authenticator) LoadUser(hash string) (*User, bool) {
	meter, found := a.users.Load(hash)
	if found {
		user, ok := meter.(*User)
		return user, ok
	}

	canonicalHash, found := a.aliases.Load(hash)
	if !found {
		return nil, false
	}
	meter, found = a.users.Load(canonicalHash)
	if !found {
		return nil, false
	}
	user, ok := meter.(*User)
	return user, ok
}

func (a *Authenticator) SetUserAliases(hash string, aliases ...string) {
	a.deleteAliases(hash)
	for _, alias := range aliases {
		if alias == "" || alias == hash {
			continue
		}
		a.aliases.Store(alias, hash)
	}
}

func (a *Authenticator) deleteAliases(hash string) {
	a.aliases.Range(func(key, value interface{}) bool {
		if canonicalHash, ok := value.(string); ok && canonicalHash == hash {
			a.aliases.Delete(key)
		}
		return true
	})
}

func (a *Authenticator) SetUserProfile(hash string, userInfo api.UserInfo) {
	user, found := a.LoadUser(hash)
	if !found {
		return
	}
	user.Email = userInfo.Email
	user.UUID = userInfo.UUID
	user.Passwd = userInfo.Passwd
}

// SetUserSpeedLimit installs the user's shared rate limiter (one bucket
// across both directions, XrayR semantics). bytesPerSec is the user's
// total bandwidth in B/s; 0 removes any existing limiter. Returns
// false if the user is not in the authenticator (e.g. just deleted).
func (a *Authenticator) SetUserSpeedLimit(hash string, bytesPerSec uint64) bool {
	user, found := a.LoadUser(hash)
	if !found {
		return false
	}
	user.SetSpeedLimit(int64(bytesPerSec))
	return true
}

// SetUserDeviceLimit installs the user's per-IP device limit (max
// distinct source IPs that can be tracked concurrently). 0 disables
// the limit. Returns false if the user is not in the authenticator.
func (a *Authenticator) SetUserDeviceLimit(hash string, maxDevices int) bool {
	user, found := a.LoadUser(hash)
	if !found {
		return false
	}
	user.SetIPLimit(maxDevices)
	return true
}

// 返回指针列表 复制sync.Map内部的互斥锁,可能会导致并发问题
func (a *Authenticator) ListUsers() []*User {
	result := make([]*User, 0)

	a.users.Range(func(key, value interface{}) bool {
		if u, ok := value.(*User); ok {
			result = append(result, u)
		}
		return true
	})

	return result
}

func (a *Authenticator) Close() error {
	return nil
}

func NewAuthenticator(ctx context.Context) (*Authenticator, error) {
	u := &Authenticator{
		ctx: ctx,
	}
	return u, nil
}
