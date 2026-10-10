package fdstoken

import (
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestIssueAndVerify(t *testing.T) {
	key := KeyFromSecret("test-secret")
	pub := key.Public().(ed25519.PublicKey)
	now := time.Unix(1_800_000_000, 0)
	token, err := Issue(key, "fuserd-public-1-7777", "gsess-abc", 42, now)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(token, Prefix) || strings.ContainsAny(token, "?=#&+/ ") {
		t.Fatalf("token not URL-option safe: %q", token)
	}
	c, err := Verify(pub, token, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if c.Slot != "fuserd-public-1-7777" || c.GameSession != "gsess-abc" || c.User != 42 || len(c.PlayerSession) != 24 {
		t.Fatalf("claims: %+v", c)
	}
	if _, err := Verify(pub, token, now.Add(Lifetime)); !errors.Is(err, ErrExpired) {
		t.Fatalf("expiry: %v", err)
	}
	other := KeyFromSecret("other-secret").Public().(ed25519.PublicKey)
	if _, err := Verify(other, token, now); !errors.Is(err, ErrSignature) {
		t.Fatalf("other key: %v", err)
	}
	// A changed claim breaks the signature.
	parts := strings.Split(strings.TrimPrefix(token, "psess-"), ".")
	forged, _ := json.Marshal(Claims{Version: 1, Slot: "fuserd-public-1-7777", GameSession: "gsess-abc", PlayerSession: c.PlayerSession,
		User: 7, Expires: c.Expires})
	tampered := "psess-fds1." + b64.EncodeToString(forged) + "." + parts[2]
	if _, err := Verify(pub, tampered, now); !errors.Is(err, ErrSignature) {
		t.Fatalf("tampered claims: %v", err)
	}
	for _, bad := range []string{"", "psess-abc", "fds1.x.y", "psess-fds1.e30", "psess-fds1.e30.!!!"} {
		if _, err := Verify(pub, bad, now); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	if KeyFromSecret("test-secret").Public().(ed25519.PublicKey).Equal(pub) != true {
		t.Fatal("key derivation not deterministic")
	}
}

// FDS verifies tokens in C++ (FDSSessionToken.cpp). With
// FDS_WRITE_VECTORS=<file> this writes vectors for its self-test commandlet: RFC 8032's first test (empty message)
// plus tokens that must pass and fail.
func TestWriteVectorsForFDS(t *testing.T) {
	out := os.Getenv("FDS_WRITE_VECTORS")
	if out == "" {
		t.Skip("set FDS_WRITE_VECTORS to write FDS's verification vectors")
	}
	type raw struct {
		PublicKey string `json:"public_key_hex"`
		Message   string `json:"message_hex"`
		Signature string `json:"signature_hex"`
		Valid     bool   `json:"valid"`
	}
	type tok struct {
		PublicKey string `json:"public_key"`
		Token     string `json:"token"`
		Valid     bool   `json:"valid"`
		Slot      string `json:"slot"`
		User      int64  `json:"uid"`
		Expires   int64  `json:"exp"`
	}
	var vectors struct {
		Raw    []raw `json:"raw"`
		Tokens []tok `json:"tokens"`
	}
	// RFC 8032 section 7.1, test 1.
	seed, _ := hex.DecodeString("9d61b19deffd5a60ba844af492ec2cc44449c5697b326919703bac031cae7f60")
	rfc := ed25519.NewKeyFromSeed(seed)
	sig := ed25519.Sign(rfc, nil)
	vectors.Raw = append(vectors.Raw, raw{hex.EncodeToString(rfc.Public().(ed25519.PublicKey)), "", hex.EncodeToString(sig), true})
	sig[10] ^= 1
	vectors.Raw = append(vectors.Raw, raw{hex.EncodeToString(rfc.Public().(ed25519.PublicKey)), "", hex.EncodeToString(sig), false})
	for i, msg := range []string{"72", "af82", strings.Repeat("ab", 1000)} {
		m, _ := hex.DecodeString(msg)
		k := ed25519.NewKeyFromSeed(append(seed[:31:31], byte(i)))
		s := ed25519.Sign(k, m)
		vectors.Raw = append(vectors.Raw, raw{hex.EncodeToString(k.Public().(ed25519.PublicKey)), msg, hex.EncodeToString(s), true})
		m[0] ^= 1
		vectors.Raw = append(vectors.Raw, raw{hex.EncodeToString(k.Public().(ed25519.PublicKey)), hex.EncodeToString(m), hex.EncodeToString(s), false})
	}
	key := KeyFromSecret("vector-secret")
	now := time.Now()
	good, _ := Issue(key, "slot-test", "gsess-vector", 1234, now)
	c, _ := Verify(key.Public().(ed25519.PublicKey), good, now)
	vectors.Tokens = append(vectors.Tokens, tok{PublicKeyString(key), good, true, "slot-test", 1234, c.Expires})
	vectors.Tokens = append(vectors.Tokens, tok{PublicKeyString(KeyFromSecret("wrong")), good, false, "slot-test", 1234, c.Expires})
	body, _ := json.MarshalIndent(vectors, "", "  ")
	if err := os.WriteFile(out, body, 0o644); err != nil {
		t.Fatal(err)
	}
}

// With FDS_ISSUE_OUT=<file>, writes tokens for FDS's test-admission.ps1: a key, a good token for
// slot "slot-test" / user 77, one for another slot, an expired one, a tampered one, and a "short" one that expires
// 75 s after it is written (for the reconnect-after-expiry check).
func TestIssueForFDSAdmissionTest(t *testing.T) {
	out := os.Getenv("FDS_ISSUE_OUT")
	if out == "" {
		t.Skip("set FDS_ISSUE_OUT to write admission-test tokens")
	}
	key := KeyFromSecret("admission-test-secret")
	now := time.Now()
	good, _ := Issue(key, "slot-test", "gsess-admission", 77, now)
	other, _ := Issue(key, "slot-other", "gsess-admission", 77, now)
	expired, _ := Issue(key, "slot-test", "gsess-admission", 77, now.Add(-Lifetime-time.Minute))
	short, _ := Issue(key, "slot-test", "gsess-admission", 77, now.Add(-Lifetime+75*time.Second))
	shortClaims, _ := Verify(key.Public().(ed25519.PublicKey), short, now)
	tampered := good[:len(good)-4] + "AAAA"
	body, _ := json.MarshalIndent(map[string]string{"public_key": PublicKeyString(key), "good": good, "other_slot": other,
		"expired": expired, "tampered": tampered, "short": short,
		"short_exp": strconv.FormatInt(shortClaims.Expires, 10), "slot": "slot-test", "uid": "77"}, "", "  ")
	if err := os.WriteFile(out, body, 0o644); err != nil {
		t.Fatal(err)
	}
}
