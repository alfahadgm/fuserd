package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"fuseronline/internal/store"
)

func TestExemptCommand(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "fuseronline.db")
	cfgPath := filepath.Join(dir, "config.json")
	cfg := `{"token_secret": "` + strings.Repeat("k", 32) + `", "database_path": ` + jsonString(db) +
		`, "song_manifest": {"songs_file": ""}, "auth": {"allow_epic_failed_signatures": true}}`
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	const key = "0123456789abcdef0123456789abcdef"
	run := func(args ...string) (string, error) {
		var out strings.Builder
		err := exemptCommand(append([]string{"-config", cfgPath}, args...), &out)
		return out.String(), err
	}

	if _, err := run("-username", "DJ Name"); err == nil || !strings.Contains(err.Error(), "usage") {
		t.Fatalf("missing key: %v", err)
	}
	if _, err := run("-username", "DJ Name ", "-key", key); err == nil {
		t.Fatal("expected a whitespace error")
	}
	if out, err := run("-username", "DJ Name", "-key", key); err != nil || out != "added exempt login \"DJ Name\"\n" {
		t.Fatalf("add: %q %v", out, err)
	}
	if out, err := run("-username", "New Name", "-key", key); err != nil || !strings.HasPrefix(out, "updated") {
		t.Fatalf("update: %q %v", out, err)
	}

	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if ok, err := st.AllowEpicFallback(ctx, key, "New Name"); err != nil || !ok {
		t.Fatalf("exempt login refused: %v %v", ok, err)
	}
	if ok, _ := st.AllowEpicFallback(ctx, key, "DJ Name"); ok {
		t.Fatal("the old name still works")
	}
	if _, err := st.MarkEpicVerified(ctx, key); err != nil {
		t.Fatal(err)
	}
	st.Close()
	if _, err := run("-username", "DJ Name", "-key", key); err == nil || !strings.Contains(err.Error(), "verified") {
		t.Fatalf("a verified key was exempted: %v", err)
	}
	if out, err := run("-username", "DJ Name", "-key", "not-an-epic-id"); err != nil || !strings.Contains(out, "note: the key") {
		t.Fatalf("odd key: %q %v", out, err)
	}
}

func jsonString(s string) string {
	return `"` + strings.ReplaceAll(s, `\`, `\\`) + `"`
}
