package controller

import (
	"context"
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
