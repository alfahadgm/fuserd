package store

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fuseronline/internal/mainstage"
)

func TestMainStageEventsAndHeadliners(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "mainstage.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	t0 := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	a := mainstage.Event{UUID: strings.ToUpper(mainstage.NewUUID()), Name: "Opening", Type: mainstage.TypeSpecialEvent,
		Start: t0, End: t0.Add(30 * time.Minute), SpecialName: "Launch", Slots: 3, TurnMinutes: 8}
	if err := s.AddMainStageEvent(ctx, a); err != nil {
		t.Fatal(err)
	}
	clash := mainstage.Event{UUID: mainstage.NewUUID(), Name: "Clash", Type: mainstage.TypeMaintenance,
		Start: t0.Add(29 * time.Minute), End: t0.Add(time.Hour)}
	if err := s.AddMainStageEvent(ctx, clash); !errors.Is(err, ErrMainStageOverlap) {
		t.Fatalf("overlapping event: %v", err)
	}
	clash.Start = t0.Add(30 * time.Minute) // touching is fine
	if err := s.AddMainStageEvent(ctx, clash); err != nil {
		t.Fatal(err)
	}
	if err := s.AddMainStageEvent(ctx, mainstage.Event{UUID: "x"}); err == nil {
		t.Fatal("invalid event stored")
	}
	got, err := s.MainStageEvents(ctx, t0.Add(-time.Hour), t0.Add(2*time.Hour))
	if err != nil || len(got) != 2 || got[0].UUID != strings.ToLower(a.UUID) || got[0].SpecialName != "Launch" ||
		!got[0].Start.Equal(t0) || got[1].Type != mainstage.TypeMaintenance {
		t.Fatalf("events %+v %v", got, err)
	}
	if got, _ := s.MainStageEvents(ctx, t0.Add(time.Hour), t0.Add(2*time.Hour)); len(got) != 0 {
		t.Fatalf("window after both: %+v", got)
	}

	var ids []int64
	for _, u := range []string{"u1", "u2", "u3", "u4"} {
		p, err := s.LoginPlayer(ctx, "epic", u, u)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, p.ID)
	}
	if n, err := s.AddMainStageHeadliner(ctx, a.UUID, 3, ids[0], 0, HeadlinerPurchase); err != nil || n != 1 {
		t.Fatalf("first slot %d %v", n, err)
	}
	if _, err := s.AddMainStageHeadliner(ctx, a.UUID, 3, ids[0], 0, HeadlinerPurchase); !errors.Is(err, ErrAlreadyHeadliner) {
		t.Fatalf("second slot for the same player: %v", err)
	}
	if n, err := s.AddMainStageHeadliner(ctx, a.UUID, 3, ids[1], 3, HeadlinerAdmin); err != nil || n != 3 {
		t.Fatalf("admin slot 3: %d %v", n, err)
	}
	if _, err := s.AddMainStageHeadliner(ctx, a.UUID, 3, ids[2], 3, HeadlinerAdmin); !errors.Is(err, ErrSlotTaken) {
		t.Fatalf("taken slot: %v", err)
	}
	if n, err := s.AddMainStageHeadliner(ctx, a.UUID, 3, ids[2], 0, HeadlinerPurchase); err != nil || n != 2 {
		t.Fatalf("first free slot %d %v", n, err)
	}
	if _, err := s.AddMainStageHeadliner(ctx, a.UUID, 3, ids[3], 0, HeadlinerPurchase); !errors.Is(err, ErrEventFull) {
		t.Fatalf("full event: %v", err)
	}
	if _, err := s.AddMainStageHeadliner(ctx, a.UUID, 3, ids[3], 4, HeadlinerAdmin); err == nil {
		t.Fatal("slot beyond the event's slots accepted")
	}
	hl, err := s.MainStageHeadliners(ctx, []string{a.UUID, clash.UUID})
	list := hl[strings.ToLower(a.UUID)]
	if err != nil || len(list) != 3 || list[0].PlayerID != ids[0] || list[1].PlayerID != ids[2] || list[2].Source != HeadlinerAdmin {
		t.Fatalf("headliners %+v %v", hl, err)
	}
	if err := s.RemoveMainStageHeadliner(ctx, a.UUID, ids[2]); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveMainStageHeadliner(ctx, a.UUID, ids[2]); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second removal: %v", err)
	}
	// Rotation events aren't stored, but their headliners are.
	rot := mainstage.RotationUUID(t0.Add(4 * time.Hour))
	if _, err := s.AddMainStageHeadliner(ctx, rot, 3, ids[3], 0, HeadlinerAdmin); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveMainStageEvent(ctx, a.UUID); err != nil {
		t.Fatal(err)
	}
	if hl, _ := s.MainStageHeadliners(ctx, []string{a.UUID, rot}); len(hl) != 1 || len(hl[rot]) != 1 {
		t.Fatalf("after removing the event: %+v", hl)
	}
	if err := s.RemoveMainStageEvent(ctx, a.UUID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second event removal: %v", err)
	}
	if p, err := s.PlayerByHypeUsername(ctx, strings.ToUpper(mustPlayer(t, s, ids[0]).HypeUsername)); err != nil || p.ID != ids[0] {
		t.Fatalf("by hype username: %+v %v", p, err)
	}
}

func mustPlayer(t *testing.T, s *Store, id int64) *Player {
	t.Helper()
	p, err := s.Player(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// allow_stock is stored and read back, on a new database and on one created before the column existed.
func TestMainStageEventAllowStock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "allow.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	// An older database: the column is missing until Open migrates it.
	if _, err := s.db.Exec(`ALTER TABLE mainstage_events DROP COLUMN allow_stock`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if s, err = Open(path); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	t0 := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	e := mainstage.Event{UUID: mainstage.NewUUID(), Name: "Open stage", Type: mainstage.TypeEvent, Start: t0,
		End: t0.Add(30 * time.Minute), Slots: 2, AllowStock: true}
	if err := s.AddMainStageEvent(ctx, e); err != nil {
		t.Fatal(err)
	}
	got, err := s.MainStageEvents(ctx, t0, t0.Add(time.Hour))
	if err != nil || len(got) != 1 || !got[0].AllowStock {
		t.Fatalf("read back %+v %v", got, err)
	}
	e.AllowStock = false
	if err := s.ControlEventSave(ctx, e, []ControlSlot{{}, {}}, []ControlSlot{{}, {}}); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.MainStageEvents(ctx, t0, t0.Add(time.Hour)); len(got) != 1 || got[0].AllowStock {
		t.Fatalf("after the control save %+v", got)
	}
}
