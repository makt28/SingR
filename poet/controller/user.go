package controller

//https://github.com/bobby4k/trojan-go/blob/master/statistic/memory/memory.go
//atomic

import (
	"context"
	"fmt"
	"sync"

	// "sync/atomic"
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
	maxIPNum    int

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

func (u *User) AddIP(ip string) bool {
	//先加入ip
	_, found := u.ipTable.Load(ip)
	if found {
		return true //无需限制, 再table中
	}

	//判断是否受限
	if u.maxIPNum > 0 {
		ipn := u.ipNum.Load()
		if int(ipn)+1 > u.maxIPNum {
			return false //超过ip限制
		}
	}

	u.ipTable.Store(ip, true)
	u.ipNum.Add(1)

	return true
}

func (u *User) DelIP(ip string) bool {
	//先载入
	_, found := u.ipTable.Load(ip)
	if !found {
		return false
	}

	// if u.maxIPNum <= 0 {
	// 	return true
	// }

	u.ipTable.Delete(ip)
	u.ipNum.Add(-1)
	return true
}

func (u *User) GetIPNumber() int {
	return int(u.ipNum.Load())
}

func (u *User) SetIPLimit(n int) {
	u.maxIPNum = n
}

func (u *User) GetIPLimit() int {
	return u.maxIPNum
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
	// atomic.StoreUint64(&u.sent, send)
	// atomic.StoreUint64(&u.recv, recv)
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

// // 对应下面携程模式的快速修改
// func (u *User) TardyAddTraffic(send, recv int64) {
// 	if recv > 0 {
// 		atomic.AddUint64(&u.recv, recv)
// 	}
// 	if send > 0 {
// 		atomic.AddUint64(&u.sent, send)
// 	}
// }

// func (u *User) speedUpdater() {
// 	ticker := time.NewTicker(time.Second)
// 	for {
// 		select {
// 		case <-u.ctx.Done():
// 			return
// 		case <-ticker.C:
// 			sent, recv := u.GetTraffic()
// 			atomic.StoreUint64(&u.sendSpeed, sent-u.lastSent)
// 			atomic.StoreUint64(&u.recvSpeed, recv-u.lastRecv)
// 			atomic.StoreUint64(&u.lastSent, sent)
// 			atomic.StoreUint64(&u.lastRecv, recv)
// 		}
// 	}
// }

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

// func (a *Authenticator) AuthUser(hash string) (bool, statistic.User) {
// 	if user, found := a.users.Load(hash); found {
// 		return true, user.(*User)
// 	}
// 	return false, nil
// }

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
	// go meter.speedUpdater()
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

// func (a *Authenticator) AddTraffic(hash string, send, recv uint64) error {
// 	meter, found := a.users.Load(hash)
// 	if !found {
// 		return fmt.Errorf("hash " + hash + " not found")
// 	}
// 	meter.(*User).TardyAddTraffic(send, recv)
// 	return nil
// }

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
