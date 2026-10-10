package store

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fuseronline/internal/mainstage"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func controlStore(t *testing.T) *Store {
	t.Helper()
	s, e := Open(filepath.Join(t.TempDir(), "control.db"))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { s.Close() })
	return s
}
func TestControlEnrollmentSingleUseAndRevocation(t *testing.T) {
	s := controlStore(t)
	ctx := context.Background()
	token, _, e := s.ControlToken(ctx, "operator", time.Minute, ControlCLI)
	if e != nil {
		t.Fatal(e)
	}
	if b, e := hex.DecodeString(token); e != nil || len(b) != 8 {
		t.Fatalf("expected 16 hex: %q", token)
	}
	hash := ControlHash(token)
	if pt, e := s.ControlTokenLookup(ctx, hash); e != nil || pt.Name != "operator" || pt.CreatedBy != ControlCLI {
		t.Fatalf("token lookup: %+v %v", pt, e)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	success := 0
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := s.ControlEnroll(ctx, hash, ControlAdmin{ID: "admin", Name: "operator", CredentialID: "credential", Credential: []byte(`{}`)})
			if err == nil {
				mu.Lock()
				success++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if success != 1 {
		t.Fatalf("enrolled %d times", success)
	}
	if _, e := s.ControlTokenLookup(ctx, hash); e == nil {
		t.Fatal("used token still valid")
	}
	if e := s.ControlSession(ctx, "session", "admin", time.Now().Add(time.Hour), ""); e != nil {
		t.Fatal(e)
	}
	if _, e := s.ControlSessionAdmin(ctx, "session"); e != nil {
		t.Fatal(e)
	}
	if e := s.ControlRevoke(ctx, "admin"); e != nil {
		t.Fatal(e)
	}
	if _, e := s.ControlSessionAdmin(ctx, "session"); e == nil {
		t.Fatal("revoked admin session remained valid")
	}
	token, _, e = s.ControlToken(ctx, "other", time.Minute, ControlCLI)
	if e != nil {
		t.Fatal(e)
	}
	hash = ControlHash(token)
	s.db.Exec(`UPDATE control_tokens SET expires=0 WHERE hash=?`, hash)
	if e := s.ControlEnroll(ctx, hash, ControlAdmin{ID: "other", Name: "other", CredentialID: "other", Credential: []byte(`{}`)}); e == nil {
		t.Fatal("expired token enrolled")
	}
}
func TestControlDocumentsExemptionsAndSlots(t *testing.T) {
	s := controlStore(t)
	ctx := context.Background()
	version, _ := s.ControlDocumentVersion(ctx, "items")
	if e := s.ControlSaveDocument(ctx, "items", []int{1}, version); e != nil {
		t.Fatal(e)
	}
	if e := s.ControlSaveDocument(ctx, "items", []int{2}, version); e == nil {
		t.Fatal("stale document replaced current data")
	}
	ex := ControlExemption{Username: "DJ", Key: "epic1", Notes: "note"}
	if e := s.ControlExemptionSave(ctx, "", &ex); e != nil {
		t.Fatal(e)
	}
	ex.Key = "epic2"
	if e := s.ControlExemptionSave(ctx, "epic1", &ex); e != nil {
		t.Fatal(e)
	}
	all, e := s.ControlExemptions(ctx)
	if e != nil || len(all) != 1 || all[0].Key != "epic2" {
		t.Fatalf("exemptions %+v %v", all, e)
	}
	s.MarkEpicVerified(ctx, "epic2")
	if e := s.ControlExemptionSave(ctx, "epic2", &ex); e == nil {
		t.Fatal("verified identity changed")
	}
	p, e := s.LoginPlayer(ctx, "steam", "p", "DJ")
	if e != nil {
		t.Fatal(e)
	}
	event := mainstage.Event{UUID: mainstage.NewUUID(), Name: "test", Type: "event", Start: time.Now().Add(time.Hour), End: time.Now().Add(2 * time.Hour), Slots: 2}
	slots := []ControlSlot{{Blocked: true}, {}}
	if e := s.ControlEventSave(ctx, event, slots, nil); e != nil {
		t.Fatal(e)
	}
	if n, e := s.AddMainStageHeadliner(ctx, event.UUID, 2, p.ID, 0, HeadlinerPurchase); e != nil || n != 2 {
		t.Fatalf("blocked slot selected: %d %v", n, e)
	}
	if e := s.ControlEventSave(ctx, event, slots, slots); e == nil {
		t.Fatal("stale edit erased purchased slot")
	}
	heads, _ := s.MainStageHeadliners(ctx, []string{event.UUID})
	if len(heads[event.UUID]) != 1 {
		t.Fatal("stale save partially modified bookings")
	}
	if e := s.ControlEventRemove(ctx, event.UUID); e != nil {
		t.Fatal(e)
	}
	if _, err := s.AddMainStageHeadliner(ctx, event.UUID, 2, p.ID, 0, HeadlinerPurchase); !errors.Is(err, ErrEventFull) {
		t.Fatal("removed occurrence accepted a stale purchase")
	}
	removed, _ := s.MainStageRemoved(ctx)
	if !removed[event.UUID] {
		t.Fatal("rotation not suppressed")
	}
	if e := s.ControlUserSave(ctx, p.ID, p.HypeUsername, true, "moderated"); e != nil {
		t.Fatal(e)
	}
	if e := s.ControlEventSave(ctx, event, []ControlSlot{{User: &p.ID}, {}}, nil); e == nil {
		t.Fatal("banned headliner accepted")
	}
	if _, e := s.Player(ctx, p.ID); errors.Is(e, ErrNotFound) {
		t.Fatal("ban deleted user")
	}
}

func TestControlTimes(t *testing.T) {
	want := time.Date(2026, 10, 9, 0, 29, 10, 0, time.UTC)
	london, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Fatal(err)
	}
	for _, loc := range []*time.Location{time.UTC, london} {
		got, err := ControlParseWall(ControlWall(want, loc), loc)
		if err != nil || !got.Equal(want) {
			t.Fatalf("%s: wall time lost seconds or shifted: %v %v", loc, got, err)
		}
	}
	if got, err := ControlParseWall("2026-10-09T08:00", london); err != nil || !got.Equal(time.Date(2026, 10, 9, 7, 0, 0, 0, time.UTC)) {
		t.Fatalf("minute input in BST: %v %v", got, err)
	}
	if got, err := ControlTime(ControlStored(want)); err != nil || !got.Equal(want) {
		t.Fatalf("stored time: %v %v", got, err)
	}
	// Documents written before the timezone setting hold Perth wall times without an offset.
	if got, err := ControlTime("2026-10-09T08:29:10"); err != nil || !got.Equal(want) {
		t.Fatalf("legacy Perth time: %v %v", got, err)
	}
	if got, err := ControlTime("2026-10-09T08:29"); err != nil || !got.Equal(want.Add(-10*time.Second)) {
		t.Fatalf("legacy minute time: %v %v", got, err)
	}
}
func TestControlMigrationTokensAndRevocation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	old, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	// The control tables as first released, before token IDs, creators and sign-in times.
	_, err = old.Exec(`CREATE TABLE control_tokens(hash TEXT PRIMARY KEY, name TEXT NOT NULL, expires INTEGER NOT NULL);
CREATE TABLE control_admins(id TEXT PRIMARY KEY, name TEXT NOT NULL, credential_id TEXT NOT NULL UNIQUE, credential BLOB NOT NULL);
INSERT INTO control_tokens VALUES('h1','pending',9999999999);
INSERT INTO control_admins VALUES('legacy','Legacy','cred','{}');`)
	old.Close()
	if err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	tokens, err := s.ControlTokens(ctx)
	if err != nil || len(tokens) != 1 || len(tokens[0].ID) != 16 || tokens[0].Name != "pending" {
		t.Fatalf("migrated tokens %+v %v", tokens, err)
	}
	admins, err := s.ControlAdmins(ctx)
	if err != nil || len(admins) != 1 || admins[0].ID != "legacy" || !admins[0].Created.IsZero() {
		t.Fatalf("migrated admins %+v %v", admins, err)
	}

	// A token records its creator, enrollment credits it, and removing the creator cancels the tokens they issued.
	token, id, err := s.ControlToken(ctx, "new", time.Minute, "legacy")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ControlEnroll(ctx, ControlHash(token), ControlAdmin{ID: "new", Name: "new", CredentialID: "c2", Credential: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	a, err := s.ControlAdmin(ctx, "new")
	if err != nil || a.CreatedBy != "legacy" || a.Created.IsZero() || a.LastLogin.IsZero() {
		t.Fatalf("enrolled admin %+v %v", a, err)
	}
	if err := s.ControlTokenRevoke(ctx, id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("used token cancelled: %v", err)
	}
	_, id, _ = s.ControlToken(ctx, "issued by legacy", time.Minute, "legacy")
	_, other, _ := s.ControlToken(ctx, "issued by cli", time.Minute, ControlCLI)
	if err := s.ControlRevoke(ctx, "legacy"); err != nil {
		t.Fatal(err)
	}
	tokens, _ = s.ControlTokens(ctx)
	for _, tk := range tokens {
		if tk.ID == id {
			t.Fatal("removed administrator's token survived")
		}
	}
	if err := s.ControlTokenRevoke(ctx, other); err != nil {
		t.Fatal(err)
	}
	s.db.Exec(`UPDATE control_tokens SET expires=1`)
	tokens, _ = s.ControlTokens(ctx)
	var n int
	s.db.QueryRow(`SELECT COUNT(*) FROM control_tokens`).Scan(&n)
	if len(tokens) != 0 || n != 0 {
		t.Fatalf("expired tokens listed or kept: %+v (%d rows)", tokens, n)
	}

	if err := s.ControlAuditAdd(ctx, ControlAuditEntry{At: time.Now(), AdminID: "new", AdminName: "new", Action: "first"}); err != nil {
		t.Fatal(err)
	}
	s.ControlAuditAdd(ctx, ControlAuditEntry{At: time.Now(), AdminID: "new", AdminName: "new", Action: "second"})
	entries, err := s.ControlAudit(ctx, 1)
	if err != nil || len(entries) != 1 || entries[0].Action != "second" {
		t.Fatalf("audit %+v %v", entries, err)
	}
}
