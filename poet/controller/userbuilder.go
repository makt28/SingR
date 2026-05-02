package controller

import (
	"fmt"

	"github.com/sagernet/sing-box/poet/api"
)

// buildUserHash returns the canonical runtime hash for a panel user.
//
// Only the SSpanel branch is exercised today; SingR is intentionally
// SSPanel-only (see CLAUDE.md / AGENTS.md). The default branch is kept as
// a safety net but must be audited together with
// protocol/anytls/pinbound.go RefreshUsers (which hardcodes "u%d") before
// a non-SSpanel panel type is enabled.
func (c *Controller) buildUserHash(user *api.UserInfo) string {
	switch c.panelType {
	case "SSpanel":
		return fmt.Sprintf("u%d", user.UID)
	default:
		return user.UUID
	}
}
