package naive

import (
	"sync"

	"github.com/sagernet/sing/common/auth"
)

// userTable maps naive Basic-auth passwords to SingR runtime user names
// ("u<UID>"). All methods are safe for concurrent use.
//
// SingR divergence from upstream, which authenticates through
// sing/common/auth.Authenticator: that map is written without a lock and
// only at construction, while SingR refreshes users live from the panel.
// Lookup is by password only — the client-supplied username is ignored, the
// same model as the anytls and hysteria2 inbounds — so subscriptions need
// not know the u<UID> naming. The same shape as the sing-quic fork's
// hysteria2 userTable.
type userTable struct {
	access     sync.RWMutex
	byPassword map[string]string // password -> name
	byName     map[string]string // name -> password (reverse index for incremental updates)
}

func newUserTable(users []auth.User) *userTable {
	t := &userTable{
		byPassword: make(map[string]string),
		byName:     make(map[string]string),
	}
	names := make([]string, len(users))
	passwords := make([]string, len(users))
	for i, user := range users {
		names[i] = user.Username
		passwords[i] = user.Password
	}
	t.add(names, passwords)
	return t
}

// replace atomically swaps in an entirely new user set.
func (t *userTable) replace(names []string, passwords []string) {
	byPassword := make(map[string]string, len(names))
	byName := make(map[string]string, len(names))
	for i, name := range names {
		byPassword[passwords[i]] = name
		byName[name] = passwords[i]
	}
	t.access.Lock()
	t.byPassword = byPassword
	t.byName = byName
	t.access.Unlock()
}

// add inserts or updates users. If a user already exists its password is
// rotated (the old password entry is removed first); if a password was
// previously bound to a different user, that stale reverse entry is dropped.
func (t *userTable) add(names []string, passwords []string) {
	if len(names) == 0 {
		return
	}
	t.access.Lock()
	defer t.access.Unlock()
	for i, name := range names {
		password := passwords[i]
		if oldPassword, ok := t.byName[name]; ok && oldPassword != password {
			delete(t.byPassword, oldPassword)
		}
		if oldName, ok := t.byPassword[password]; ok && oldName != name {
			delete(t.byName, oldName)
		}
		t.byPassword[password] = name
		t.byName[name] = password
	}
}

// remove drops users by name. Unknown names are ignored.
func (t *userTable) remove(names []string) {
	if len(names) == 0 {
		return
	}
	t.access.Lock()
	defer t.access.Unlock()
	for _, name := range names {
		if password, ok := t.byName[name]; ok {
			delete(t.byPassword, password)
			delete(t.byName, name)
		}
	}
}

// lookup resolves the client-supplied password to a runtime user name.
func (t *userTable) lookup(password string) (string, bool) {
	t.access.RLock()
	name, loaded := t.byPassword[password]
	t.access.RUnlock()
	return name, loaded
}
