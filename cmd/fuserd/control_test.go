package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fuseronline/internal/config"
	"fuseronline/internal/store"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestControlEnrollmentCommand(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.TokenSecret = "12345678901234567890123456789012"
	cfg.DatabasePath = filepath.Join(dir, "test.db")
	cfg.SongManifest.SongsFile = ""
	cfg.SongManifest.PakFile = ""
	cfg.Control = config.ControlConfig{Enabled: true, Origin: "http://localhost:8000"}
	path := filepath.Join(dir, "config.json")
	raw, _ := json.Marshal(cfg)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := controlCommand([]string{"enroll", "-config", path, "-name", "Operator", "-expires", "10m"}, &out); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 17 {
		t.Fatal("token was not 16 hex plus newline")
	}
	st, err := store.Open(cfg.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	token := out.String()[:16]
	if pt, err := st.ControlTokenLookup(context.Background(), store.ControlHash(token)); err != nil || pt.Name != "Operator" || pt.CreatedBy != store.ControlCLI {
		t.Fatal("command did not persist token")
	}
	for _, ttl := range []string{"0s", "25h", "-1m"} {
		if err := controlCommand([]string{"enroll", "-config", path, "-name", "Operator", "-expires", ttl}, &out); err == nil {
			t.Fatalf("accepted expiry %s", ttl)
		}
	}
}

func TestControlListAndRevokeCommands(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.TokenSecret = "12345678901234567890123456789012"
	cfg.DatabasePath = filepath.Join(dir, "test.db")
	cfg.SongManifest.SongsFile = ""
	cfg.SongManifest.PakFile = ""
	cfg.Control = config.ControlConfig{Enabled: true, Origin: "http://localhost:8000"}
	path := filepath.Join(dir, "config.json")
	raw, _ := json.Marshal(cfg)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) (string, error) {
		var out bytes.Buffer
		err := controlCommand(append(args, "-config", path), &out)
		return out.String(), err
	}
	token, err := run("enroll", "-name", "Operator")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	st, err := store.Open(cfg.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.ControlEnroll(ctx, store.ControlHash(strings.TrimSpace(token)), store.ControlAdmin{ID: "admin-1", Name: "Operator", CredentialID: "c", Credential: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	_, tokenID, err := st.ControlToken(ctx, "Pending", time.Hour, "admin-1")
	st.Close()
	if err != nil {
		t.Fatal(err)
	}
	out, err := run("list")
	if err != nil || !strings.Contains(out, "admin-1") || !strings.Contains(out, tokenID) || !strings.Contains(out, "Pending") || !strings.Contains(out, "command line") {
		t.Fatalf("list: %v\n%s", err, out)
	}
	if _, err := run("revoke-token", "-id", tokenID); err != nil {
		t.Fatal(err)
	}
	if _, err := run("revoke-token", "-id", tokenID); err == nil {
		t.Fatal("cancelled a token twice")
	}
	if _, err := run("revoke", "-id", "admin-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := run("revoke", "-id", "admin-1"); err == nil {
		t.Fatal("revoked an administrator twice")
	}
	out, _ = run("list")
	if strings.Contains(out, "admin-1") || strings.Contains(out, tokenID) {
		t.Fatalf("revoked entries still listed:\n%s", out)
	}
	st, _ = store.Open(cfg.DatabasePath)
	defer st.Close()
	entries, _ := st.ControlAudit(ctx, 10)
	actions := []string{}
	for _, e := range entries {
		if e.AdminID != store.ControlCLI {
			t.Fatalf("command-line entry not attributed: %+v", e)
		}
		actions = append(actions, e.Action)
	}
	if strings.Join(actions, ",") != "admins.revoke,admins.token_cancel,admins.token_create" {
		t.Fatalf("audit actions %v", actions)
	}
}
