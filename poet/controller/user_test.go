package controller

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/sagernet/sing-box/poet/api"
)

func TestAuthenticatorUserAliasesResolveToCanonicalUser(t *testing.T) {
	authenticator, err := NewAuthenticator(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	if err := authenticator.AddUser("u1", 1); err != nil {
		t.Fatal(err)
	}
	authenticator.SetUserAliases("u1", "uuid-1", "passwd-1", "user@example.com")

	canonicalUser, found := authenticator.LoadUser("u1")
	if !found {
		t.Fatal("canonical user not found")
	}
	for _, alias := range []string{"uuid-1", "passwd-1", "user@example.com"} {
		aliasUser, found := authenticator.LoadUser(alias)
		if !found {
			t.Fatalf("alias %q did not resolve", alias)
		}
		if aliasUser != canonicalUser {
			t.Fatalf("alias %q resolved to a different user", alias)
		}
	}
}

func TestAuthenticatorUserProfile(t *testing.T) {
	authenticator, err := NewAuthenticator(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	if err := authenticator.AddUser("u1", 1); err != nil {
		t.Fatal(err)
	}
	authenticator.SetUserProfile("u1", api.UserInfo{
		UID:    1,
		Email:  "user@example.com",
		UUID:   "uuid-1",
		Passwd: "passwd-1",
	})

	user, found := authenticator.LoadUser("u1")
	if !found {
		t.Fatal("user not found")
	}
	if user.Email != "user@example.com" {
		t.Fatalf("Email = %q, want user@example.com", user.Email)
	}
	if user.UUID != "uuid-1" {
		t.Fatalf("UUID = %q, want uuid-1", user.UUID)
	}
	if user.Passwd != "passwd-1" {
		t.Fatalf("Passwd = %q, want passwd-1", user.Passwd)
	}
}

func TestAuthenticatorUserAliasesRefresh(t *testing.T) {
	authenticator, err := NewAuthenticator(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	if err := authenticator.AddUser("u1", 1); err != nil {
		t.Fatal(err)
	}
	authenticator.SetUserAliases("u1", "old-uuid")
	authenticator.SetUserAliases("u1", "new-uuid")

	if _, found := authenticator.LoadUser("old-uuid"); found {
		t.Fatal("old alias still resolves after refresh")
	}
	if _, found := authenticator.LoadUser("new-uuid"); !found {
		t.Fatal("new alias did not resolve")
	}
}

// AddIP's check-then-store must be atomic: many connections arriving at once
// from distinct addresses may not all slip under the limit.
//
// The window between "is there room" and "take the slot" is a few
// instructions wide, so one burst rarely hits it: release every goroutine of
// a round from one barrier, and run many rounds against a limit of 1 — the
// tightest case, where a single extra admission is already a violation.
func TestUserAddIPEnforcesLimitUnderConcurrency(t *testing.T) {
	const (
		rounds  = 400
		callers = 16
	)
	for round := 0; round < rounds; round++ {
		user := &User{}
		user.SetIPLimit(1)

		var admitted atomic.Int32
		var wait sync.WaitGroup
		start := make(chan struct{})
		for i := 0; i < callers; i++ {
			wait.Add(1)
			go func(i int) {
				defer wait.Done()
				<-start
				if user.AddIP(fmt.Sprintf("192.0.2.%d", i)) {
					admitted.Add(1)
				}
			}(i)
		}
		close(start)
		wait.Wait()
		if got := admitted.Load(); got != 1 {
			t.Fatalf("round %d: admitted %d distinct addresses, want 1", round, got)
		}
		if got := user.GetIPNumber(); got != 1 {
			t.Fatalf("round %d: tracked addresses = %d, want 1", round, got)
		}
	}
}

// A report cycle drains the table and frees the slots.
func TestUserResetIPTableFreesDeviceSlots(t *testing.T) {
	const limit = 3
	user := &User{}
	user.SetIPLimit(limit)
	for i := 0; i < limit; i++ {
		if !user.AddIP(fmt.Sprintf("192.0.2.%d", i)) {
			t.Fatalf("address %d refused below the limit", i)
		}
	}
	if user.AddIP("198.51.100.1") {
		t.Fatal("address admitted above the limit")
	}
	if !user.AddIP("192.0.2.0") {
		t.Fatal("an already registered address was refused")
	}
	if drained := user.ResetIPTable(); len(drained) != limit {
		t.Fatalf("drained %d addresses, want %d", len(drained), limit)
	}
	if !user.AddIP("198.51.100.1") {
		t.Fatal("address refused after the table was drained")
	}
}

// Limit 0 (EnableDeviceLimit off) admits everyone.
func TestUserAddIPWithoutLimitAdmitsAll(t *testing.T) {
	user := &User{}
	for i := 0; i < 16; i++ {
		if !user.AddIP(fmt.Sprintf("192.0.2.%d", i)) {
			t.Fatalf("address %d refused although no limit is set", i)
		}
	}
}
