// Package fdstoken issues the player session tokens FuserDedicatedServer (FDS) containers check at login.
// A token is the player session id fuserd hands the client, which
// the client presents as ?playerSessionId= when it connects:
//
//	psess-fds1.<base64url(claims JSON)>.<base64url(Ed25519 signature over "fds1.<base64url(claims JSON)>")>
//
// The "psess-" prefix keeps the shape of GameLift's player session ids. fuserd signs with a key derived from its
// token_secret, so tokens stay valid across a fuserd restart; containers get only the public key.
package fdstoken

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

const (
	// Prefix is what every token starts with.
	Prefix = "psess-fds1."
	// Lifetime is how long a token may be used to connect after it is issued.
	Lifetime = 10 * time.Minute
)

// Claims bind a token to one container slot, one game session and one player.
type Claims struct {
	Version       int    `json:"v"`
	Slot          string `json:"slot"` // the container's slot id (FDS_SLOT_ID): what the container checks first
	GameSession   string `json:"gsid"`
	PlayerSession string `json:"psid"` // random: one live connection per token
	User          int64  `json:"uid"`  // the Hype user id the client logs in with
	Expires       int64  `json:"exp"`  // Unix seconds
}

var b64 = base64.RawURLEncoding

// KeyFromSecret derives the signing key from fuserd's token_secret.
func KeyFromSecret(secret string) ed25519.PrivateKey {
	seed := sha256.Sum256([]byte("fuserd fds session token v1\x00" + secret))
	return ed25519.NewKeyFromSeed(seed[:])
}

// PublicKeyString is the public key as a container receives it (FDS_TOKEN_PUBLIC_KEY): 32 bytes, base64url.
func PublicKeyString(key ed25519.PrivateKey) string {
	return b64.EncodeToString(key.Public().(ed25519.PublicKey))
}

// Issue signs a token for a player joining a game session on a container slot.
func Issue(key ed25519.PrivateKey, slot, gameSession string, user int64, now time.Time) (string, error) {
	nonce := make([]byte, 12)
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	c := Claims{Version: 1, Slot: slot, GameSession: gameSession, PlayerSession: hex.EncodeToString(nonce), User: user,
		Expires: now.Add(Lifetime).Unix()}
	return Sign(key, c)
}

// Sign encodes and signs claims.
func Sign(key ed25519.PrivateKey, c Claims) (string, error) {
	payload, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	signed := "fds1." + b64.EncodeToString(payload)
	return "psess-" + signed + "." + b64.EncodeToString(ed25519.Sign(key, []byte(signed))), nil
}

var (
	ErrMalformed = errors.New("malformed token")
	ErrSignature = errors.New("bad signature")
	ErrExpired   = errors.New("expired token")
)

// Verify checks a token's signature and expiry and returns its claims. FDS does the same in C++
// (FDSSessionToken.cpp); this is for fuserd's own tests and tools.
func Verify(pub ed25519.PublicKey, token string, now time.Time) (Claims, error) {
	var c Claims
	rest, ok := strings.CutPrefix(token, "psess-")
	if !ok {
		return c, ErrMalformed
	}
	dot := strings.LastIndexByte(rest, '.')
	if dot < 0 || !strings.HasPrefix(rest, "fds1.") {
		return c, ErrMalformed
	}
	signed, sigText := rest[:dot], rest[dot+1:]
	sig, err := b64.DecodeString(sigText)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return c, ErrMalformed
	}
	if !ed25519.Verify(pub, []byte(signed), sig) {
		return c, ErrSignature
	}
	payload, err := b64.DecodeString(strings.TrimPrefix(signed, "fds1."))
	if err != nil || json.Unmarshal(payload, &c) != nil || c.Version != 1 {
		return c, ErrMalformed
	}
	if now.Unix() >= c.Expires {
		return c, ErrExpired
	}
	return c, nil
}
