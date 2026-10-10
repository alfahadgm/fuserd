package api

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fuseronline/internal/config"
	"fuseronline/internal/logx"
	"fuseronline/internal/platform"
	"fuseronline/internal/store"
	"fuseronline/internal/token"
)

func TestEpicTieredAdmission(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	const sub, dn = "0123456789abcdef0123456789abcdef", "DJ Name"
	jwk := map[string]any{"keys": []map[string]string{{"kty": "RSA", "kid": "test-key",
		"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
		"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes())}}}
	jwks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(jwk)
	}))
	defer jwks.Close()
	path := filepath.Join(t.TempDir(), "auth.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	admin, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	if _, err := admin.Exec(`INSERT INTO epic_auth_policy (sub, dn, state) VALUES (?, ?, 'allowed')`, sub, dn); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Auth.AllowEpicFailedSignatures = true
	cfg.Auth.Epic.JWKSURL = jwks.URL
	cfg.Auth.Epic.Issuer = "https://issuer.example"
	cfg.Auth.Epic.Audience = "test-audience"
	logs, err := logx.New(slog.NewTextHandler(io.Discard, nil), nil, []byte("test key"))
	if err != nil {
		t.Fatal(err)
	}
	v := platform.NewJWTVerifier(platform.Epic, cfg.Auth.Epic)
	s := New(cfg, st, token.NewIssuer(strings.Repeat("k", 32), time.Hour),
		map[string]platform.Verifier{platform.Epic: v}, logs)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	claims := map[string]any{"iss": cfg.Auth.Epic.Issuer, "aud": cfg.Auth.Epic.Audience,
		"sub": sub, "dn": dn, "exp": time.Now().Add(time.Hour).Unix()}
	header, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": "test-key"})
	body, _ := json.Marshal(claims)
	signing := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(body)
	fake := signing + "." + strings.Repeat("a", 64)
	// The same emulator token with padded segments, as some emulators send. Grow a filler claim until the claims
	// segment really needs padding.
	padded := ""
	for filler := ""; !strings.HasSuffix(padded, "="); filler += "x" {
		claims["filler"] = filler
		b, _ := json.Marshal(claims)
		padded = base64.URLEncoding.EncodeToString(b)
	}
	delete(claims, "filler")
	paddedFake := base64.URLEncoding.EncodeToString(header) + "." + padded + "." + strings.Repeat("a", 64)
	digest := sha256.Sum256([]byte(signing))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	valid := signing + "." + base64.RawURLEncoding.EncodeToString(signature)
	login := func(jwt string) (int, envelope) {
		return call(t, srv, "POST", "auth/jwt/", "", authRequest{Platform: platform.Epic,
			PlatformUserID: sub, PlatformUsername: dn, Token: jwt})
	}
	disabled := cfg
	disabled.Auth.AllowEpicFailedSignatures = false
	disabledServer := httptest.NewServer(New(disabled, st, token.NewIssuer(strings.Repeat("k", 32), time.Hour),
		map[string]platform.Verifier{platform.Epic: v}, logs).Handler())
	if status, _ := call(t, disabledServer, "POST", "auth/jwt/", "", authRequest{Platform: platform.Epic,
		PlatformUserID: sub, PlatformUsername: dn, Token: fake}); status != http.StatusUnauthorized {
		t.Fatalf("disabled fallback: HTTP %d", status)
	}
	disabledServer.Close()
	if status, _ := login(fake); status != http.StatusOK {
		t.Fatalf("allowlisted fallback: HTTP %d", status)
	}
	if status, _ := login(paddedFake); status != http.StatusOK {
		t.Fatalf("padded allowlisted fallback: HTTP %d", status)
	}
	status, env := login(valid)
	if status != http.StatusOK {
		t.Fatalf("verified upgrade: HTTP %d", status)
	}
	var session authResponse
	if err := json.Unmarshal(env.Results, &session); err != nil {
		t.Fatal(err)
	}
	if allowed, err := st.AllowEpicFallback(context.Background(), sub, dn); err != nil || allowed {
		t.Fatalf("verified account could downgrade: %v %v", allowed, err)
	}
	if status, _ := login(fake); status != http.StatusUnauthorized {
		t.Fatalf("downgrade: HTTP %d", status)
	}
	if _, err := admin.Exec(`UPDATE epic_auth_policy SET state = 'blocked' WHERE sub = ?`, sub); err != nil {
		t.Fatal(err)
	}
	if status, _ := login(valid); status != http.StatusUnauthorized {
		t.Fatalf("blocked valid JWT: HTTP %d", status)
	}
	if status, _ := call(t, srv, "GET", "players/ping/", "JWT "+session.Token, nil); status != http.StatusUnauthorized {
		t.Fatalf("blocked existing session: HTTP %d", status)
	}
}
