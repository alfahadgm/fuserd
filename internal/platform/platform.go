// Package platform validates platform tickets presented to auth/jwt/. The optional
// Epic emulator fallback is handled separately by the API and its DB allowlist.
package platform

import (
	"context"
	"errors"
)

// Platform codes as sent in FPlayerProfileAuthRequest.platform (wide-string table in the exe: pc, xbl, psn,
// ntd, stm, epc).
const (
	Steam    = "stm"
	Epic     = "epc"
	Nintendo = "ntd"
)

var (
	ErrUnsupported = errors.New("platform not supported")
	ErrInvalid     = errors.New("ticket failed validation")
	ErrSignature   = errors.New("ticket signature failed validation")
)

// Identity is what a validated ticket proves.
type Identity struct {
	Platform string
	UserID   string
	// OwnedDLC is the Steam DLC app IDs listed in a Steam ownership ticket.
	OwnedDLC []uint32
}

// Verifier validates one platform's tickets. userID is the platform_user_id the client claims; the ticket must
// prove that identity.
type Verifier interface {
	Verify(ctx context.Context, userID, ticket string) (Identity, error)
}
