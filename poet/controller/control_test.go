package controller

import (
	"fmt"
	"testing"

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

func testUserHash(user *api.UserInfo) string {
	return fmt.Sprintf("u%d", user.UID)
}
