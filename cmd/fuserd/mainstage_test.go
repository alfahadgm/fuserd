package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fuseronline/internal/mainstage"
	"fuseronline/internal/store"
)

func TestMainStageCommand(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "fuseronline.db")
	cfgPath := filepath.Join(dir, "config.json")
	os.WriteFile(filepath.Join(dir, "dlc_songs.json"), []byte(`["countingstars"]`), 0o644)
	cfg := `{"token_secret": "` + strings.Repeat("k", 32) + `", "database_path": ` + jsonString(db) +
		`, "song_manifest": {"songs_file": "dlc_songs.json"}, "main_stage": {"enabled": true,
		"rotation": {"anchor": "2026-10-08T12:00:00Z"}}}`
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 8, 11, 0, 0, 0, time.UTC)
	run := func(args ...string) (string, error) {
		var out strings.Builder
		err := mainStageCommandAt(append([]string{args[0], "-config", cfgPath}, args[1:]...), &out,
			func() time.Time { return now })
		return out.String(), err
	}

	out, err := run("maintenance", "-start", "2026-10-08T14:00:00Z", "-minutes", "60")
	if err != nil || !strings.Contains(out, "maintenance \"Maintenance\"") || !strings.Contains(out, "replaces 1 rotation event") {
		t.Fatalf("maintenance: %q %v", out, err)
	}
	maint := strings.Fields(out)[1]
	if _, err := run("add", "-name", "Clash", "-start", "2026-10-08T14:30:00Z"); err == nil ||
		!strings.Contains(err.Error(), "overlaps") {
		t.Fatalf("overlapping add: %v", err)
	}
	if _, err := run("add", "-name", "Past", "-start", "2026-10-08T09:00:00Z", "-minutes", "30"); err == nil {
		t.Fatal("an event that has already ended was added")
	}
	out, err = run("add", "-name", "Launch Party", "-type", "special_event", "-start", "2026-10-08T17:00:00Z",
		"-slots", "2", "-turn-minutes", "10")
	if err != nil || !strings.Contains(out, "special_event \"Launch Party\" 2026-10-08T17:00:00Z - 2026-10-08T17:29:10Z") {
		t.Fatalf("add: %q %v", out, err)
	}
	special := strings.Fields(out)[1]

	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	p1, _ := st.LoginPlayer(context.Background(), "epic", "a", "Alpha")
	p2, _ := st.LoginPlayer(context.Background(), "epic", "b", "Bravo")
	p3, _ := st.LoginPlayer(context.Background(), "epic", "c", "Charlie")
	st.Close()

	rotation := mainstage.RotationUUID(time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC))
	if out, err := run("assign", "-event", rotation, "-player", p1.HypeUsername); err != nil ||
		!strings.Contains(out, "slot 1 of 4") {
		t.Fatalf("assign by name: %q %v", out, err)
	}
	if _, err := run("assign", "-event", special, "-player", "999"); err == nil {
		t.Fatal("unknown player assigned")
	}
	if _, err := run("assign", "-event", maint, "-player", p1.HypeUsername); err == nil {
		t.Fatal("assigned to maintenance")
	}
	for _, p := range []*store.Player{p2, p3} {
		if _, err := run("assign", "-event", special, "-player", p.HypeUsername); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := run("assign", "-event", special, "-player", p1.HypeUsername); err == nil ||
		!strings.Contains(err.Error(), "every headliner slot is taken") {
		t.Fatalf("third headliner on a 2-slot event: %v", err)
	}
	out, err = run("list", "-hours", "8")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"NEXT_OPEN", "Main Stage (rotation)", "slot 1: " + p1.HypeUsername + " (admin)",
		"maintenance", "2/2", "Launch Party"} {
		if !strings.Contains(out, want) {
			t.Errorf("list lacks %q:\n%s", want, out)
		}
	}
	if out, err := run("unassign", "-event", rotation, "-player", p1.HypeUsername); err != nil || !strings.HasPrefix(out, "unassigned") {
		t.Fatalf("unassign: %q %v", out, err)
	}
	// Between sets (the 12:00 rotation event ended at 12:29:10, the next starts at 13:00) the ended event is listed
	// first, as held for the game until the next start.
	now = time.Date(2026, 10, 8, 12, 40, 0, 0, time.UTC)
	out, err = run("list", "-hours", "2")
	if err != nil || !strings.Contains(out, "HELD until 2026-10-08T13:00:00Z") || !strings.Contains(out, rotation) ||
		!strings.Contains(out, "Maintenance windows are never held") {
		t.Fatalf("list between sets: %q %v", out, err)
	}
	if _, err := run("remove", "-event", maint); err != nil {
		t.Fatal(err)
	}
	if _, err := run("remove", "-event", maint); err == nil {
		t.Fatal("removed twice")
	}
	if _, err := run("bogus"); err == nil || !strings.Contains(err.Error(), "usage") {
		t.Fatalf("unknown command: %v", err)
	}
}
