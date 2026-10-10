// Package token issues and verifies the session tokens handed out by auth/jwt/. They are HS256 JWTs; the client
// treats them as opaque and sends them back as "Authorization: JWT <token>".
package token

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"
)

const issuer = "fuseronline/session"

var (
	ErrMalformed = errors.New("malformed token")
	ErrSignature = errors.New("bad token signature")
	ErrExpired   = errors.New("token expired")
)

// Claims identify the player a session token was issued to.
type Claims struct {
	PlayerID       int64  `json:"-"`
	Platform       string `json:"plt"`
	PlatformUserID string `json:"puid"`
	Subject        string `json:"sub"`
	Issuer         string `json:"iss"`
	IssuedAt       int64  `json:"iat"`
	ExpiresAt      int64  `json:"exp"`
}

type Issuer struct {
	secret   []byte
	lifetime time.Duration
	now      func() time.Time
}

func NewIssuer(secret string, lifetime time.Duration) *Issuer {
	return &Issuer{secret: []byte(secret), lifetime: lifetime, now: time.Now}
}

func (i *Issuer) Lifetime() time.Duration { return i.lifetime }

var header = base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))

// Issue signs a token for playerID.
func (i *Issuer) Issue(playerID int64, platform, platformUserID string) (string, error) {
	now := i.now()
	body, err := json.Marshal(Claims{
		Platform:       platform,
		PlatformUserID: platformUserID,
		Subject:        strconv.FormatInt(playerID, 10),
		Issuer:         issuer,
		IssuedAt:       now.Unix(),
		ExpiresAt:      now.Add(i.lifetime).Unix(),
	})
	if err != nil {
		return "", err
	}
	signing := header + "." + base64.RawURLEncoding.EncodeToString(body)
	return signing + "." + i.sign(signing), nil
}

// Verify checks the signature, issuer and expiry of tok.
func (i *Issuer) Verify(tok string) (Claims, error) {
	var c Claims
	parts := strings.Split(tok, ".")
	if len(parts) != 3 || parts[0] != header {
		return c, ErrMalformed
	}
	if !hmac.Equal([]byte(parts[2]), []byte(i.sign(parts[0]+"."+parts[1]))) {
		return c, ErrSignature
	}
	body, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || json.Unmarshal(body, &c) != nil || c.Issuer != issuer {
		return c, ErrMalformed
	}
	if c.PlayerID, err = strconv.ParseInt(c.Subject, 10, 64); err != nil {
		return c, ErrMalformed
	}
	if i.now().Unix() >= c.ExpiresAt {
		return c, ErrExpired
	}
	return c, nil
}

func (i *Issuer) sign(s string) string {
	m := hmac.New(sha256.New, i.secret)
	m.Write([]byte(s))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}
