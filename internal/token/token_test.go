package token

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestIssueVerify(t *testing.T) {
	iss := NewIssuer(strings.Repeat("s", 32), time.Hour)
	tok, err := iss.Issue(42, "stm", "76561198000000001")
	if err != nil {
		t.Fatal(err)
	}
	c, err := iss.Verify(tok)
	if err != nil || c.PlayerID != 42 || c.Platform != "stm" || c.PlatformUserID != "76561198000000001" {
		t.Fatalf("verify: %+v %v", c, err)
	}

	if _, err := NewIssuer(strings.Repeat("x", 32), time.Hour).Verify(tok); !errors.Is(err, ErrSignature) {
		t.Fatalf("other secret: %v", err)
	}
	parts := strings.Split(tok, ".")
	forged := parts[0] + "." + strings.TrimRight(parts[1], "A") + "B." + parts[2]
	if _, err := iss.Verify(forged); err == nil {
		t.Fatal("modified claims accepted")
	}

	iss.now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	if _, err := iss.Verify(tok); !errors.Is(err, ErrExpired) {
		t.Fatalf("expired: %v", err)
	}
}
