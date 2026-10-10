package publichost

import (
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"
)

// markReadyForTest makes a slot ready, as a container's registration does.
func markReadyForTest(m *Manager, s *Slot) {
	m.mu.Lock()
	ev := m.markReady(s)
	m.mu.Unlock()
	m.emit(ev)
}

// statusForTest applies a status report, as a container's heartbeat does.
func statusForTest(m *Manager, s *Slot, players, stage int) {
	m.mu.Lock()
	ev := m.applyStatus(s, players, stage)
	m.mu.Unlock()
	m.emit(ev)
}

func TestCompatibleSlotAndCapacity(t *testing.T) {
	m := New(Config{MaxServers: 1, MaxPlayers: 2}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s := &Slot{ID: "slot", Key: Key{Rounds: 3, Measures: 128}, Address: "198.51.100.10",
		Port: 7777, ready: make(chan struct{})}
	m.slots[s.ID] = s
	markReadyForTest(m, s)
	if !m.Available(s.ID) {
		t.Fatal("ready slot unavailable")
	}
	first, err := m.ReserveAny([]Key{{Rounds: 1, Measures: 32}, {Rounds: 3, Measures: 128}})
	if err != nil || first.ID != s.ID {
		t.Fatalf("compatible reserve: %+v %v", first, err)
	}
	if _, err := m.Reserve(s.Key); err != nil {
		t.Fatal(err)
	}
	if m.Available(s.ID) {
		t.Fatal("reserved slot still advertised as available")
	}
	if _, err := m.Reserve(s.Key); !errors.Is(err, ErrCapacity) {
		t.Fatalf("capacity: %v", err)
	}
	statusForTest(m, s, 2, 1)
	if s.reserved != 0 || s.remote != 2 {
		t.Fatalf("reservations after connect: %+v", s)
	}
	statusForTest(m, s, 0, 0)
	if m.Available(s.ID) {
		t.Fatal("empty draining server accepted a new player")
	}
}

// A viewer never starts a server: nothing running is ErrCapacity; a READY show is preferred to a STARTING one
// whatever its set length.
func TestReserveExistingNeverStarts(t *testing.T) {
	m := New(Config{MaxServers: 2, MaxPlayers: 2}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, err := m.ReserveExisting(); !errors.Is(err, ErrCapacity) || len(m.slots) != 0 {
		t.Fatalf("empty manager: %v, %d slots", err, len(m.slots))
	}
	starting := &Slot{ID: "starting", Key: Key{Rounds: 3, Measures: 128}, Port: 7777, ready: make(chan struct{})}
	ready := &Slot{ID: "ready", Key: Key{Rounds: 1, Measures: 32}, Port: 7778, ready: make(chan struct{})}
	m.slots[starting.ID], m.slots[ready.ID] = starting, ready
	markReadyForTest(m, ready)
	got, err := m.ReserveExisting()
	if err != nil || got.ID != "ready" || len(m.slots) != 2 {
		t.Fatalf("reserve existing: %+v %v (%d slots)", got, err, len(m.slots))
	}
	m.Drain(ready.ID)
	if got, err := m.ReserveExisting(); err != nil || got.ID != "starting" {
		t.Fatalf("draining show skipped: %+v %v", got, err)
	}
}

// A public ticket's turn_length is the round choice's paired option, so slots match on rounds only; the set length
// a container registers with decides later matches.
func TestSetLengthMatchesRoundsAndFollowsRegistration(t *testing.T) {
	m := New(Config{MaxServers: 1, MaxPlayers: 4}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s := &Slot{ID: "slot", Key: Key{Rounds: 1, Measures: 32}, Port: 7777, ready: make(chan struct{}), regToken: "token"}
	m.slots[s.ID] = s
	if err := m.Register(s.ID, "token", Registration{Port: s.Port}); err != nil {
		t.Fatal(err)
	}
	if got, err := m.ReserveAny([]Key{{Rounds: 1, Measures: 32}}); err != nil || got.ID != s.ID {
		t.Fatalf("same rounds: %+v %v", got, err)
	}
	if err := m.Register(s.ID, "token", Registration{Port: s.Port, Rounds: 1, TurnMeasure: 64}); err != nil {
		t.Fatal(err)
	}
	if s.actual != (Key{Rounds: 1, Measures: 64}) {
		t.Fatalf("actual set length %+v", s.actual)
	}
	if got, err := m.ReserveAny([]Key{{Rounds: 1, Measures: 32}}); err != nil || got.ID != s.ID {
		t.Fatalf("paired 1/32 ticket should join the 1/64 server: %+v %v", got, err)
	}
	if _, err := m.ReserveAny([]Key{{Rounds: 2, Measures: 64}}); !errors.Is(err, ErrCapacity) {
		t.Fatalf("2-round ticket joined a 1-round server: %v", err)
	}
	if err := m.Register(s.ID, "token", Registration{Port: s.Port, Rounds: 2, TurnMeasure: 128}); err != nil {
		t.Fatal(err)
	}
	if got, err := m.ReserveAny([]Key{{Rounds: 2, Measures: 64}}); err != nil || got.ID != s.ID {
		t.Fatalf("reported rounds not used for matching: %+v %v", got, err)
	}
}

// A new public server gets one of the four times of day, and over many spawns every one of them.
func TestRandomTimeOfDay(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		tod := randomTimeOfDay()
		if !TimeOfDayValid(tod) {
			t.Fatalf("invalid time of day %q", tod)
		}
		seen[tod] = true
	}
	if len(seen) != len(timesOfDay) {
		t.Fatalf("200 picks covered only %v", seen)
	}
}

func TestSiteSnapshotIncludesFullServersAndExpiresStage(t *testing.T) {
	m := New(Config{MaxPlayers: 2}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	slot := &Slot{ID: "full", Venue: "VenueB", TimeOfDay: "sunset", Key: Key{Rounds: 4, Measures: 32}, ready: make(chan struct{})}
	m.slots[slot.ID] = slot
	markReadyForTest(m, slot)
	statusForTest(m, slot, 2, 6)
	if m.Available(slot.ID) {
		t.Fatal("full server available for assignment")
	}
	got, ok := m.SiteSnapshot()[slot.ID]
	if !ok || got.Stage != 6 || got.Rounds != 4 || got.Bars != 32 || got.Venue != "VenueB" || got.TimeOfDay != "sunset" {
		t.Fatalf("full server missing: %+v", got)
	}
	slot.lastStatus = time.Now().Add(-2 * time.Minute)
	if m.SiteSnapshot()[slot.ID].Stage != -1 {
		t.Fatal("stale stage shown as current")
	}
	m.Drain(slot.ID)
	if len(m.SiteSnapshot()) != 0 {
		t.Fatal("draining server shown")
	}
}

// A slot skip passes over (it holds a player blocked either way) is not offered: a new server starts instead, and
// a viewer gets none.
func TestReserveExcept(t *testing.T) {
	m := New(Config{MaxServers: 1, MaxPlayers: 4}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s := &Slot{ID: "slot", Key: Key{Rounds: 1, Measures: 32}, Port: 7777, ready: make(chan struct{})}
	m.slots[s.ID] = s
	markReadyForTest(m, s)
	skip := func(id string) bool { return id == "slot" }
	if _, err := m.ReserveAnyForExcept([]Key{{Rounds: 1, Measures: 32}}, "", "", skip, true); !errors.Is(err, ErrCapacity) {
		t.Fatalf("skipped slot offered, or a server started beyond max_servers: %v", err)
	}
	if _, err := m.ReserveExistingExcept(skip); !errors.Is(err, ErrCapacity) {
		t.Fatalf("viewer placed in a skipped slot: %v", err)
	}
	if got, err := m.ReserveAnyForExcept([]Key{{Rounds: 1, Measures: 32}}, "", "", nil, true); err != nil || got.ID != "slot" {
		t.Fatalf("without a skip: %+v %v", got, err)
	}
}
