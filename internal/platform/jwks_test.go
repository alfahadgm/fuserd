package platform

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"fuseronline/internal/config"
)

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func signJWT(t *testing.T, key *rsa.PrivateKey, kid string, claims map[string]any) string {
	t.Helper()
	h, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": kid, "typ": "JWT"})
	c, _ := json.Marshal(claims)
	signing := b64(h) + "." + b64(c)
	digest := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return signing + "." + b64(sig)
}

func TestEpicFallbackOnlyForInvalidSignature(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	srv := jwksServer(t, key, "epic-key")
	v := NewJWTVerifier(Epic, config.JWTConfig{JWKSURL: srv.URL, Issuer: "https://issuer.example", Audience: "client-1"})
	claims := map[string]any{"iss": "https://issuer.example", "aud": "client-1", "sub": "user-1",
		"dn": "DJ Name", "exp": time.Now().Add(time.Hour).Unix()}
	valid := signJWT(t, key, "epic-key", claims)
	if _, err := v.Verify(context.Background(), "user-1", valid); err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(valid, ".")
	shortSignature := parts[0] + "." + parts[1] + "." + strings.Repeat("a", 64)
	if _, err := v.Verify(context.Background(), "user-1", shortSignature); !errors.Is(err, ErrSignature) {
		t.Fatalf("expected signature failure, got %v", err)
	}
	if dn, err := v.FallbackDisplayName("user-1", shortSignature); err != nil || dn != "DJ Name" {
		t.Fatalf("fallback claims: dn=%q err=%v", dn, err)
	}
	if _, err := v.FallbackDisplayName("other-user", shortSignature); !errors.Is(err, ErrInvalid) {
		t.Fatalf("subject mismatch accepted: %v", err)
	}
	claims["exp"] = time.Now().Add(-time.Hour).Unix()
	expired := signJWT(t, key, "epic-key", claims)
	if _, err := v.Verify(context.Background(), "user-1", expired); !errors.Is(err, ErrInvalid) || errors.Is(err, ErrSignature) {
		t.Fatalf("expired token must not enter fallback: %v", err)
	}
}

func jwksServer(t *testing.T, key *rsa.PrivateKey, kid string) *httptest.Server {
	doc := map[string]any{"keys": []map[string]string{{
		"kty": "RSA", "kid": kid, "alg": "RS256", "use": "sig",
		"n": b64(key.N.Bytes()), "e": b64(big.NewInt(int64(key.E)).Bytes()),
	}}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(doc)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestJWTVerify(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	other, _ := rsa.GenerateKey(rand.Reader, 2048)
	srv := jwksServer(t, key, "k1")
	v := NewJWTVerifier(Nintendo, config.JWTConfig{
		Enabled: true, JWKSURL: srv.URL, Issuer: "https://issuer.example", Audience: "client-1",
		RequiredClaims: map[string]string{"nintendo.ai": "0100e1f013674000"},
	})

	base := func() map[string]any {
		return map[string]any{
			"iss": "https://issuer.example", "aud": "client-1", "sub": "user-1",
			"exp": time.Now().Add(time.Hour).Unix(), "nintendo": map[string]any{"ai": "0100e1f013674000"},
		}
	}
	with := func(k string, val any) map[string]any { c := base(); c[k] = val; return c }

	cases := []struct {
		name   string
		key    *rsa.PrivateKey
		kid    string
		claims map[string]any
		ok     bool
	}{
		{"valid", key, "k1", base(), true},
		{"audience array", key, "k1", with("aud", []string{"x", "client-1"}), true},
		{"wrong issuer", key, "k1", with("iss", "https://evil.example"), false},
		{"wrong audience", key, "k1", with("aud", "other-game"), false},
		{"other user", key, "k1", with("sub", "user-2"), false},
		{"expired", key, "k1", with("exp", time.Now().Add(-time.Hour).Unix()), false},
		{"required claim mismatch", key, "k1", with("nintendo", map[string]any{"ai": "someothertitle"}), false},
		{"forged signature", other, "k1", base(), false},
		{"unknown kid", key, "k9", base(), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := v.Verify(context.Background(), "user-1", signJWT(t, c.key, c.kid, c.claims))
			if c.ok && err != nil {
				t.Fatalf("rejected: %v", err)
			}
			if !c.ok && !errors.Is(err, ErrInvalid) {
				t.Fatalf("want ErrInvalid, got %v", err)
			}
		})
	}

	// alg=none must never pass.
	h, _ := json.Marshal(map[string]string{"alg": "none", "kid": "k1"})
	cl, _ := json.Marshal(base())
	if _, err := v.Verify(context.Background(), "user-1", b64(h)+"."+b64(cl)+"."); !errors.Is(err, ErrInvalid) {
		t.Fatalf("alg=none accepted: %v", err)
	}
}

// Emulators pad their segments (and may use the standard alphabet); both must decode like a spec JWT segment.
func TestDecodeSegmentPaddingAndAlphabet(t *testing.T) {
	for filler := ""; len(filler) < 64; filler += "?" {
		raw, _ := json.Marshal(map[string]string{"dn": "DJ Name", "f": filler})
		for name, seg := range map[string]string{
			"raw url":      base64.RawURLEncoding.EncodeToString(raw),
			"padded url":   base64.URLEncoding.EncodeToString(raw),
			"padded std":   base64.StdEncoding.EncodeToString(raw),
			"unpadded std": base64.RawStdEncoding.EncodeToString(raw),
		} {
			var got map[string]string
			if err := decodeSegment(seg, &got); err != nil || got["dn"] != "DJ Name" || got["f"] != filler {
				t.Fatalf("%s %q: %v %v", name, seg, got, err)
			}
		}
	}
	var v map[string]any
	if err := decodeSegment("not*base64", &v); err == nil {
		t.Fatal("decoded a segment that is not base64")
	}
}

// The emulator's encoder drops the final partial 3-byte group, losing the claims' closing `}` or `"}`.
func TestDecodeSegmentRestoresDroppedTail(t *testing.T) {
	cuts := map[int]bool{}
	for filler := ""; len(filler) < 8; filler += "x" {
		raw, _ := json.Marshal(map[string]string{"dn": "DJ Name", "jti": filler})
		kept := raw[:len(raw)-len(raw)%3]
		cuts[len(raw)%3] = true
		var got map[string]string
		if err := decodeSegment(base64.RawURLEncoding.EncodeToString(kept), &got); err != nil ||
			got["dn"] != "DJ Name" || got["jti"] != filler {
			t.Fatalf("dropped %d bytes of %s: %v %v", len(raw)%3, raw, got, err)
		}
	}
	if len(cuts) != 3 {
		t.Fatalf("not every remainder was tried: %v", cuts)
	}
	// Anything that closing characters can't repair is still refused.
	for _, bad := range []string{`["a"`, `{"dn":`, `{"dn":"DJ Name",`, `not json`} {
		var v map[string]any
		if err := decodeSegment(base64.RawURLEncoding.EncodeToString([]byte(bad)), &v); err == nil {
			t.Fatalf("accepted %s as %v", bad, v)
		}
	}
}
