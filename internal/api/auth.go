package api

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"fuseronline/internal/logx"
	"fuseronline/internal/platform"
	"fuseronline/internal/store"
)

// FPlayerProfileAuthRequest
type authRequest struct {
	Platform         string `json:"platform"`
	PlatformUserID   string `json:"platform_user_id"`
	PlatformUsername string `json:"platform_username"`
	Token            string `json:"token"`
	Environment      string `json:"environment"`
	ClientID         string `json:"client_id"`
}

// FPlayerProfileAuthResponse
type authResponse struct {
	Token         string `json:"token"`
	Prefix        string `json:"prefix"`
	UserID        int32  `json:"user_id"`
	TokenDuration int32  `json:"token_duration"`
	EncodedUserID string `json:"encoded_user_id"`
}

// authJWT exchanges a genuine platform ticket for one of our session tokens.
func (s *Server) authJWT(q *request) (any, error) {
	var req authRequest
	if err := q.decode(&req); err != nil {
		return nil, err
	}
	if req.Platform == "" || req.PlatformUserID == "" || req.Token == "" {
		return nil, errBadRequest("platform, platform_user_id and token are required")
	}
	lg := s.logs.For(logx.Auth)
	v, ok := s.verifiers[req.Platform]
	if !ok {
		lg.Info("auth rejected", "platform", req.Platform, "reason", platform.ErrUnsupported)
		return nil, errBadRequest("platform not supported by this server")
	}
	id, err := v.Verify(q.Context(), req.PlatformUserID, req.Token)
	if err != nil {
		if req.Platform == platform.Epic && s.cfg.Auth.AllowEpicFailedSignatures &&
			errors.Is(err, platform.ErrSignature) {
			if epic, ok := v.(*platform.JWTVerifier); ok {
				dn, claimErr := epic.FallbackDisplayName(req.PlatformUserID, req.Token)
				if claimErr == nil {
					allowed, dbErr := s.store.AllowEpicFallback(q.Context(), req.PlatformUserID, dn)
					if dbErr != nil {
						return nil, fmt.Errorf("epic fallback lookup: %w", dbErr)
					}
					if allowed {
						id = platform.Identity{Platform: platform.Epic, UserID: req.PlatformUserID}
						req.PlatformUsername = dn
						lg.Info("epic fallback login", "platform_user", s.logs.Tag(req.PlatformUserID))
						err = nil
					}
				}
			}
		}
		if err != nil {
			lg.Info("auth rejected", "platform", req.Platform, "platform_user", s.logs.Tag(req.PlatformUserID),
				"reason", err)
			if errors.Is(err, platform.ErrInvalid) {
				return nil, errUnauthorized("platform ticket was not accepted")
			}
			return nil, err // JWKS fetch trouble is never a fallback trigger.
		}
	} else if id.Platform == platform.Epic {
		allowed, dbErr := s.store.MarkEpicVerified(q.Context(), id.UserID)
		if dbErr != nil {
			return nil, fmt.Errorf("epic verified policy: %w", dbErr)
		}
		if !allowed {
			return nil, errUnauthorized("platform ticket was not accepted")
		}
	}

	p, err := s.store.LoginPlayer(q.Context(), id.Platform, id.UserID, req.PlatformUsername)
	if err != nil {
		return nil, err
	}
	if p.Banned {
		return nil, &apiError{http.StatusForbidden, codeUserBanned, "banned"}
	}
	if id.Platform == platform.Steam {
		if err := s.store.SetSteamOwnedDLC(q.Context(), p.ID, id.OwnedDLC); err != nil {
			return nil, err
		}
	}

	tok, err := s.tokens.Issue(p.ID, id.Platform, id.UserID)
	if err != nil {
		return nil, err
	}
	lg.Info("login", "player", s.logs.TagID(p.ID), "platform", id.Platform, "platform_user", s.logs.Tag(id.UserID),
		"hype_username", s.logs.Tag(p.HypeUsername), "env", req.Environment)
	return authResponse{
		Token:         tok,
		Prefix:        "JWT",
		UserID:        int32(p.ID),
		TokenDuration: int32(s.tokens.Lifetime().Seconds()),
		EncodedUserID: encodeUserID(p.ID),
	}, nil
}

// encodeUserID renders the player ID in Crockford base32. The client shows encoded_user_id as the "FUSER ID" in
// its version popup; the original encoding is unknown, this one is only meant to be short and readable.
func encodeUserID(id int64) string {
	const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	if id <= 0 {
		return "0"
	}
	var b strings.Builder
	for ; id > 0; id /= 32 {
		b.WriteByte(alphabet[id%32])
	}
	r := []byte(b.String())
	for i, j := 0, len(r)-1; i < j; i, j = i+1, j-1 {
		r[i], r[j] = r[j], r[i]
	}
	return string(r)
}

// steamOwnedDLC reports the DLC app IDs the player's last Steam ownership ticket listed (FGetSteamOwnedDlcResponse).
func (s *Server) steamOwnedDLC(q *request) (any, error) {
	owned, err := s.store.SteamOwnedDLC(q.Context(), q.playerID())
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	return map[string]any{"owned_dlc": owned}, nil
}
