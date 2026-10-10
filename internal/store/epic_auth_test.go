package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestEpicAuthPolicy(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "epic.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	const sub = "0123456789abcdef0123456789abcdef"
	if _, err := s.db.ExecContext(ctx, `INSERT INTO epic_auth_policy (sub, dn, state) VALUES (?, ?, 'allowed')`, sub, "DJ Name"); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.AllowEpicFallback(ctx, sub, "DJ Name"); err != nil || !ok {
		t.Fatalf("allowed pair: %v %v", ok, err)
	}
	for _, pair := range [][2]string{{sub, "Other"}, {"other", "DJ Name"}} {
		if ok, err := s.AllowEpicFallback(ctx, pair[0], pair[1]); err != nil || ok {
			t.Fatalf("mismatched pair: %v %v", ok, err)
		}
	}
	if ok, err := s.MarkEpicVerified(ctx, sub); err != nil || !ok {
		t.Fatalf("promote: %v %v", ok, err)
	}
	if ok, err := s.AllowEpicFallback(ctx, sub, "DJ Name"); err != nil || ok {
		t.Fatalf("verified downgrade: %v %v", ok, err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE epic_auth_policy SET state = 'blocked' WHERE sub = ?`, sub); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.MarkEpicVerified(ctx, sub); err != nil || ok {
		t.Fatalf("blocked verified login: %v %v", ok, err)
	}
	if ok, err := s.AllowEpicFallback(ctx, sub, "DJ Name"); err != nil || ok {
		t.Fatalf("blocked fallback: %v %v", ok, err)
	}
}
