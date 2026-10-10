package platform

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"fuseronline/internal/config"
)

const (
	jwksMaxAge     = time.Hour
	jwksMinRefetch = time.Minute
	jwtClockLeeway = time.Minute
)

// JWTVerifier validates RS256 JWTs signed by a key in a platform's published JWKS: the Epic account ID token and
// the Nintendo BaaS ID token.
type JWTVerifier struct {
	platform string
	cfg      config.JWTConfig
	client   *http.Client
	now      func() time.Time

	mu        sync.Mutex
	keys      map[string]*rsa.PublicKey
	fetchedAt time.Time
}

func NewJWTVerifier(platform string, cfg config.JWTConfig) *JWTVerifier {
	return &JWTVerifier{
		platform: platform,
		cfg:      cfg,
		client:   &http.Client{Timeout: 10 * time.Second},
		now:      time.Now,
	}
}

func (v *JWTVerifier) Verify(ctx context.Context, userID, tok string) (Identity, error) {
	parts, hdr, _, err := v.parseClaims(userID, tok)
	if err != nil {
		return Identity{}, err
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	// Even a 1024-bit RSA signature takes 128 bytes. The emulator's 64-character
	// hex-looking suffix decodes to only 48 bytes as base64url, so its failure
	// can be established without depending on Epic's JWKS being reachable.
	if err != nil || len(sig) < 128 {
		return Identity{}, errors.Join(ErrInvalid, ErrSignature)
	}
	key, err := v.key(ctx, hdr.Kid)
	if err != nil {
		return Identity{}, err
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], sig) != nil {
		return Identity{}, errors.Join(ErrInvalid, ErrSignature)
	}
	return Identity{Platform: v.platform, UserID: userID}, nil
}

// FallbackDisplayName only parses and validates claims. Call it solely after Verify
// returned ErrSignature; its result is not proof of platform ownership.
func (v *JWTVerifier) FallbackDisplayName(userID, tok string) (string, error) {
	if v.platform != Epic {
		return "", ErrInvalid
	}
	_, _, claims, err := v.parseClaims(userID, tok)
	if err != nil {
		return "", err
	}
	dn, _ := claims["dn"].(string)
	if dn == "" || len(dn) > 128 {
		return "", ErrInvalid
	}
	return dn, nil
}

type jwtHeader struct {
	Alg string `json:"alg"`
	Kid string `json:"kid"`
}

func (v *JWTVerifier) parseClaims(userID, tok string) ([]string, jwtHeader, map[string]any, error) {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return nil, jwtHeader{}, nil, fmt.Errorf("%w: not a JWT", ErrInvalid)
	}
	var hdr jwtHeader
	if err := decodeSegment(parts[0], &hdr); err != nil || hdr.Alg != "RS256" || hdr.Kid == "" {
		return nil, jwtHeader{}, nil, fmt.Errorf("%w: unsupported JWT header", ErrInvalid)
	}
	var claims map[string]any
	if err := decodeSegment(parts[1], &claims); err != nil {
		return nil, jwtHeader{}, nil, fmt.Errorf("%w: bad claims", ErrInvalid)
	}
	if err := v.checkClaims(claims, userID); err != nil {
		return nil, jwtHeader{}, nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	return parts, hdr, claims, nil
}

func (v *JWTVerifier) checkClaims(c map[string]any, userID string) error {
	if s, _ := c["iss"].(string); s != v.cfg.Issuer {
		return fmt.Errorf("issuer %q", s)
	}
	if !audienceContains(c["aud"], v.cfg.Audience) {
		return fmt.Errorf("audience %v", c["aud"])
	}
	if s, _ := c["sub"].(string); s != userID {
		return fmt.Errorf("token subject is not the claimed user")
	}
	now := v.now()
	exp, ok := c["exp"].(float64)
	if !ok || now.After(time.Unix(int64(exp), 0).Add(jwtClockLeeway)) {
		return fmt.Errorf("token expired")
	}
	if nbf, ok := c["nbf"].(float64); ok && now.Add(jwtClockLeeway).Before(time.Unix(int64(nbf), 0)) {
		return fmt.Errorf("token not yet valid")
	}
	for path, want := range v.cfg.RequiredClaims {
		if got := claimAt(c, path); got != want {
			return fmt.Errorf("claim %s = %q", path, got)
		}
	}
	return nil
}

func audienceContains(aud any, want string) bool {
	switch a := aud.(type) {
	case string:
		return a == want
	case []any:
		for _, x := range a {
			if s, _ := x.(string); s == want {
				return true
			}
		}
	}
	return false
}

func claimAt(c map[string]any, path string) string {
	var cur any = c
	for _, k := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return ""
		}
		cur = m[k]
	}
	s, _ := cur.(string)
	return s
}

// key returns the signing key for kid, refreshing the JWKS when it is stale or the kid is unknown (at most once
// per jwksMinRefetch, so garbage kids cannot hammer the platform).
func (v *JWTVerifier) key(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	age := v.now().Sub(v.fetchedAt)
	if k, ok := v.keys[kid]; ok && age < jwksMaxAge {
		return k, nil
	}
	if age >= jwksMinRefetch {
		keys, err := v.fetch(ctx)
		if err != nil {
			return nil, fmt.Errorf("%s JWKS: %w", v.platform, err)
		}
		v.keys, v.fetchedAt = keys, v.now()
	}
	if k, ok := v.keys[kid]; ok {
		return k, nil
	}
	return nil, fmt.Errorf("%w: unknown signing key %q", errors.Join(ErrInvalid, ErrSignature), kid)
}

func (v *JWTVerifier) fetch(ctx context.Context) (map[string]*rsa.PublicKey, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.cfg.JWKSURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := v.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return ParseJWKS(resp.Body)
}

// ParseJWKS reads the RSA keys out of a JWKS document.
func ParseJWKS(r io.Reader) (map[string]*rsa.PublicKey, error) {
	var doc struct {
		Keys []struct {
			Kty string `json:"kty"`
			Kid string `json:"kid"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(r).Decode(&doc); err != nil {
		return nil, err
	}
	keys := make(map[string]*rsa.PublicKey)
	for _, k := range doc.Keys {
		if k.Kty != "RSA" {
			continue
		}
		n, errN := base64.RawURLEncoding.DecodeString(k.N)
		e, errE := base64.RawURLEncoding.DecodeString(k.E)
		if errN != nil || errE != nil || len(e) > 4 {
			continue
		}
		keys[k.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
	}
	return keys, nil
}

// decodeSegment decodes a JWT header or claims segment. JWTs use unpadded base64url; padding and the standard
// alphabet are accepted too. Signatures are still checked over the segments exactly as received.
//
// An Epic emulator's base64 encoder drops the final partial 3-byte group, so when the JSON's length is not a
// multiple of 3 its last one or two bytes are missing: `}` or `"}` (e.g. claims ending
// `"jti":"…"` with no `}`). A segment that decodes but does not parse is retried with those restored, and only
// accepted if the result is a JSON object. Only closing characters are added, never claim values. (If a number
// was last and lost a digit, it reads smaller: for exp, the token then counts as expired.)
func decodeSegment(seg string, v any) error {
	seg = strings.TrimRight(seg, "=")
	raw, err := base64.RawURLEncoding.DecodeString(seg)
	if err != nil {
		var stdErr error
		if raw, stdErr = base64.RawStdEncoding.DecodeString(seg); stdErr != nil {
			return err
		}
	}
	err = json.Unmarshal(raw, v)
	if err == nil || len(raw) == 0 || raw[0] != '{' {
		return err
	}
	for _, tail := range []string{"}", `"}`} {
		fixed := append(append([]byte{}, raw...), tail...)
		if json.Valid(fixed) && json.Unmarshal(fixed, v) == nil {
			return nil
		}
	}
	return err
}
