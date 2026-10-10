package publichost

import (
	"errors"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"
)

func newFDSManager(t *testing.T, maxServers, maxPlayers int) *Manager {
	t.Helper()
	m := New(Config{Image: "fds-server:test", GameRoot: "/srv/fuser", Address: "198.51.100.10",
		MaxServers: maxServers, BasePort: 7777, PortStep: 200, MaxPlayers: maxPlayers, RegisterURL: "http://127.0.0.1:8000/",
		PakKey: "secret-pak-key", Heartbeat: 10 * time.Second}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.start = func(*Slot) { m.runWG.Done() } // no Docker in tests
	m.runningPorts = func() map[int]bool { return map[int]bool{} }
	return m
}

// A new FDS slot gets a random registration token, the creator's venue and (valid) time of day.
func TestFDSReserveStartsSlotForCreatorVenue(t *testing.T) {
	m := newFDSManager(t, 2, 4)
	s, err := m.ReserveAnyFor([]Key{{Rounds: 1, Measures: 32}}, "VenueC", "night")
	if err != nil {
		t.Fatal(err)
	}
	if s.Venue != "VenueC" || s.TimeOfDay != "night" || len(s.regToken) != 64 {
		t.Fatalf("slot: venue %q tod %q token length %d", s.Venue, s.TimeOfDay, len(s.regToken))
	}
	other, err := m.ReserveAnyFor([]Key{{Rounds: 3, Measures: 128}}, "VenueA", "noon")
	if err != nil {
		t.Fatal(err)
	}
	if !TimeOfDayValid(other.TimeOfDay) || other.regToken == s.regToken {
		t.Fatalf("second slot: tod %q, token reused %v", other.TimeOfDay, other.regToken == s.regToken)
	}
}

// Register, status and exit drive the slot, and only with the slot's own token.
func TestFDSRegistrationLifecycle(t *testing.T) {
	m := newFDSManager(t, 1, 3)
	s, err := m.ReserveAnyFor([]Key{{Rounds: 1, Measures: 32}}, "VenueC", "day")
	if err != nil {
		t.Fatal(err)
	}
	token := s.regToken
	if err := m.Register("no-such-slot", token, Registration{}); !errors.Is(err, ErrUnknownSlot) {
		t.Fatalf("unknown slot: %v", err)
	}
	if err := m.Register(s.ID, "wrong", Registration{}); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("wrong token: %v", err)
	}
	if err := m.Register(s.ID, "", Registration{}); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("empty token: %v", err)
	}
	if err := m.UpdateStatus(s.ID, token, Status{Players: 1}); err == nil {
		t.Fatal("status accepted before register")
	}
	if err := m.Register(s.ID, token, Registration{Port: s.Port + 1}); err == nil {
		t.Fatal("register accepted another port")
	}
	if err := m.Register(s.ID, token, Registration{Port: s.Port, Venue: "VenueC", Rounds: 1, TurnMeasure: 64}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-s.Ready():
	default:
		t.Fatal("registered slot not ready")
	}
	if s.actual != (Key{Rounds: 1, Measures: 64}) {
		t.Fatalf("reported set length: %+v", s.actual)
	}
	if err := m.Register(s.ID, token, Registration{Port: s.Port}); err != nil {
		t.Fatalf("repeated register: %v", err)
	}
	// The creating reservation is consumed when its player arrives; an empty server that had players drains.
	if err := m.UpdateStatus(s.ID, token, Status{Players: 1, DJs: 1, Stage: 1}); err != nil {
		t.Fatal(err)
	}
	if s.reserved != 0 || s.remote != 1 || !m.Available(s.ID) {
		t.Fatalf("after arrival: reserved %d remote %d available %v", s.reserved, s.remote, m.Available(s.ID))
	}
	if err := m.UpdateStatus(s.ID, token, Status{Players: 0, Stage: 1}); err != nil {
		t.Fatal(err)
	}
	if m.Available(s.ID) {
		t.Fatal("empty server still available")
	}
	if err := m.ReportExit(s.ID, token, "last player left"); err != nil {
		t.Fatal(err)
	}
	m.stop(s.ID, nil)
	if err := m.UpdateStatus(s.ID, token, Status{}); !errors.Is(err, ErrUnknownSlot) {
		t.Fatalf("token still accepted after the slot stopped: %v", err)
	}
}

// A slot without a registration token can't be registered, even with an empty token.
func TestFDSRegistrationRefusesTokenlessSlot(t *testing.T) {
	m := New(Config{MaxServers: 1, MaxPlayers: 2}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s := &Slot{ID: "slot", Port: 7777, ready: make(chan struct{})}
	m.slots[s.ID] = s
	if err := m.Register(s.ID, "", Registration{}); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("tokenless slot registered: %v", err)
	}
}

// An exiting FDS server takes no more players.
func TestFDSExitDrains(t *testing.T) {
	m := newFDSManager(t, 1, 3)
	s, _ := m.ReserveAnyFor([]Key{{Rounds: 1, Measures: 32}}, "", "")
	if err := m.Register(s.ID, s.regToken, Registration{Port: s.Port}); err != nil {
		t.Fatal(err)
	}
	if err := m.ReportExit(s.ID, s.regToken, "idle"); err != nil {
		t.Fatal(err)
	}
	if m.Available(s.ID) {
		t.Fatal("exiting server available")
	}
	if _, err := m.ReserveExisting(); !errors.Is(err, ErrCapacity) {
		t.Fatalf("viewer placed on an exiting server: %v", err)
	}
}

// Three missed heartbeats (at least 30 s) fail the slot.
func TestFDSHeartbeatStall(t *testing.T) {
	m := newFDSManager(t, 1, 3)
	s, _ := m.ReserveAnyFor([]Key{{Rounds: 1, Measures: 32}}, "", "")
	if err := m.Register(s.ID, s.regToken, Registration{Port: s.Port}); err != nil {
		t.Fatal(err)
	}
	if reason := m.unhealthy(s.ID); reason != "" {
		t.Fatalf("fresh slot unhealthy: %s", reason)
	}
	s.lastStatus = time.Now().Add(-31 * time.Second)
	if reason := m.unhealthy(s.ID); reason != "status stalled" {
		t.Fatalf("stalled heartbeat: %q", reason)
	}
}

// The docker command line carries no secrets: the token and pak key are bare -e names.
func TestFDSRunArgs(t *testing.T) {
	m := newFDSManager(t, 1, 3)
	s := &Slot{ID: "fuserd-public-1-7777", Key: Key{Rounds: 1, Measures: 32}, Port: 7777, Venue: "VenueC", TimeOfDay: "night",
		regToken: "0123456789abcdef"}
	args := fdsRunArgs(s, m.cfg, m.cfg.Heartbeat)
	line := strings.Join(args, " ")
	for _, want := range []string{"run -d --rm --name fuserd-public-1-7777 --network host",
		"type=bind,src=/srv/fuser,dst=/fuser,readonly", "-e FDS_PORT=7777", "-e FDS_SLOT_ID=fuserd-public-1-7777",
		"-e FDS_REGISTER_URL=http://127.0.0.1:8000 ", "-e FDS_HEARTBEAT_SECONDS=10", "-e FDS_ROUNDS=1",
		"-e FDS_TURN_MEASURES=32", "-e FDS_SERVER_ACCESS=public", "-e FDS_ADDRESS=198.51.100.10", "-e FDS_VENUE=VenueC", "-e FDS_TIME_OF_DAY=night",
		"-e FDS_REGISTER_TOKEN -e FDS_PAK_KEY fds-server:test"} {
		if !strings.Contains(line+" ", want) {
			t.Errorf("missing %q in %q", want, line)
		}
	}
	if strings.Contains(line, s.regToken) || strings.Contains(line, "secret-pak-key") {
		t.Fatalf("secret on the command line: %q", line)
	}
	s.Venue, s.TimeOfDay = "Venue C; rm -rf /", "midnight"
	line = strings.Join(fdsRunArgs(s, m.cfg, m.cfg.Heartbeat), " ")
	if strings.Contains(line, "FDS_VENUE") || strings.Contains(line, "FDS_TIME_OF_DAY") {
		t.Fatalf("unchecked venue or time of day passed: %q", line)
	}
}

// A private FDS slot is its creator's alone: public tickets and viewers never land on it, invites still can.
func TestFDSPrivateSlotIsNotShared(t *testing.T) {
	m := newFDSManager(t, 3, 4)
	m.cfg.TokenPublicKey = "pubkey"
	priv, err := m.ReservePrivate(Key{Rounds: 1, Measures: 32}, "VenueB", "day")
	if err != nil {
		t.Fatal(err)
	}
	if !priv.Private || !strings.HasPrefix(priv.ID, "fuserd-private-") {
		t.Fatalf("private slot: %+v", priv)
	}
	if err := m.Register(priv.ID, priv.regToken, Registration{Port: priv.Port}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.ReserveExisting(); !errors.Is(err, ErrCapacity) {
		t.Fatalf("viewer placed on a private slot: %v", err)
	}
	pub, err := m.ReserveAnyFor([]Key{{Rounds: 1, Measures: 32}}, "VenueB", "day")
	if err != nil || pub.ID == priv.ID || pub.Private {
		t.Fatalf("public ticket shared the private slot: %+v %v", pub, err)
	}
	if err := m.ReserveSlot(priv.ID); err != nil {
		t.Fatalf("invite reservation refused: %v", err)
	}
	line := strings.Join(fdsRunArgs(priv, m.cfg, m.cfg.Heartbeat), " ")
	if !strings.Contains(line, "-e FDS_SERVER_ACCESS=private") || !strings.Contains(line, "-e FDS_TOKEN_PUBLIC_KEY=pubkey") {
		t.Fatalf("private container args: %q", line)
	}
}

// "Join a public session" (a DJ ticket) only joins a server with a free performer slot - reported DJs plus DJs
// being placed below MaxDJs - and starts a new one otherwise; "View a public session" only needs MaxPlayers.
func TestDJTicketsNeedAFreePerformerSlot(t *testing.T) {
	m := newFDSManager(t, 2, 11)
	m.cfg.MaxDJs = 4
	keys := []Key{{Rounds: 1, Measures: 32}}
	a, err := m.ReserveAnyForExcept(keys, "", "", nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Register(a.ID, a.regToken, Registration{Port: a.Port}); err != nil {
		t.Fatal(err)
	}
	for i := 2; i <= 4; i++ { // DJs 2-4 fill a's performer slots while still being placed
		if got, err := m.ReserveAnyForExcept(keys, "", "", nil, true); err != nil || got.ID != a.ID {
			t.Fatalf("DJ %d: %+v %v", i, got, err)
		}
	}
	b, err := m.ReserveAnyForExcept(keys, "", "", nil, true)
	if err != nil || b.ID == a.ID {
		t.Fatalf("a fifth DJ should start a new server: %+v %v", b, err)
	}
	m.Drain(b.ID) // only a is left to choose from

	// The four arrive: still full for DJs, but a viewer is placed there.
	if err := m.UpdateStatus(a.ID, a.regToken, Status{Players: 4, DJs: 4}); err != nil {
		t.Fatal(err)
	}
	if len(a.djReservedAt) != 0 || a.performers() != 4 {
		t.Fatalf("DJ reservations not consumed: %d left, performers %d", len(a.djReservedAt), a.performers())
	}
	if _, err := m.ReserveAnyForExcept(keys, "", "", nil, true); !errors.Is(err, ErrCapacity) {
		t.Fatalf("a DJ placed in a full lobby (max_servers reached): %v", err)
	}
	if got, err := m.ReserveExistingExcept(nil); err != nil || got.ID != a.ID {
		t.Fatalf("viewer: %+v %v", got, err)
	}

	// A DJ turns viewer: the slot is free again.
	if err := m.UpdateStatus(a.ID, a.regToken, Status{Players: 5, DJs: 3, Viewers: 2}); err != nil {
		t.Fatal(err)
	}
	if got, err := m.ReserveAnyForExcept(keys, "", "", nil, true); err != nil || got.ID != a.ID {
		t.Fatalf("DJ with a free performer slot: %+v %v", got, err)
	}
}

// The Main Stage server is one persistent slot: never handed to tickets or viewers, never drained when empty, started
// in Main Stage mode without session tokens.
func TestFDSMainStageSlot(t *testing.T) {
	m := newFDSManager(t, 3, 4)
	m.cfg.TokenPublicKey = "public-key"
	s, err := m.EnsureMainStage("VenueB", "night", 50)
	if err != nil {
		t.Fatal(err)
	}
	if again, _ := m.EnsureMainStage("VenueB", "night", 50); again != s {
		t.Fatal("a second main stage server was started")
	}
	if !strings.HasPrefix(s.ID, "fuserd-mainstage-") || s.reserved != 0 || s.MaxPlayers != 50 {
		t.Fatalf("slot %+v", s)
	}
	args := strings.Join(fdsRunArgs(s, m.cfg, m.cfg.Heartbeat), " ")
	for _, want := range []string{"FDS_MAIN_STAGE=1", "FDS_EXIT_WHEN_EMPTY=0", "FDS_MAX_PLAYERS=50", "FDS_VENUE=VenueB",
		"FDS_TIME_OF_DAY=night"} {
		if !strings.Contains(args, want) {
			t.Errorf("run args lack %s: %s", want, args)
		}
	}
	if strings.Contains(args, "FDS_TOKEN_PUBLIC_KEY") {
		t.Error("the main stage server must not require session tokens")
	}
	if _, _, ready := m.MainStageServer(); ready {
		t.Fatal("ready before registering")
	}
	if err := m.Register(s.ID, s.regToken, Registration{Port: s.Port}); err != nil {
		t.Fatal(err)
	}
	if addr, port, ready := m.MainStageServer(); !ready || addr != "198.51.100.10" || port != s.Port {
		t.Fatalf("main stage server %s:%d ready %v", addr, port, ready)
	}
	if ms, err := m.Authorize(s.ID, s.regToken); err != nil || !ms {
		t.Fatalf("authorize: %v %v", ms, err)
	}
	if _, err := m.Authorize(s.ID, "wrong"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("wrong token: %v", err)
	}
	// Tickets and viewers get their own servers.
	other, err := m.ReserveAnyFor([]Key{{Rounds: 1, Measures: 32}}, "VenueA", "day")
	if err != nil || other == s {
		t.Fatalf("ticket placed on the main stage: %v", err)
	}
	if v, err := m.ReserveExisting(); err != nil || v == s {
		t.Fatalf("viewer placed on the main stage: %v", err)
	}
	if err := m.ReserveSlot(s.ID); err == nil {
		t.Fatal("ReserveSlot accepted the main stage")
	}
	// Players come and go; the empty server keeps running.
	m.UpdateStatus(s.ID, s.regToken, Status{Players: 2, Viewers: 2, Stage: 1})
	m.UpdateStatus(s.ID, s.regToken, Status{Players: 0, Stage: 1})
	if s.draining || m.unhealthy(s.ID) != "" {
		t.Fatalf("empty main stage drains: draining %v, unhealthy %q", s.draining, m.unhealthy(s.ID))
	}
	if _, ok := m.SiteSnapshot()[s.ID]; ok {
		t.Error("the main stage is listed with the public sessions")
	}
	m.stop(s.ID, nil)
	if again, err := m.EnsureMainStage("VenueB", "night", 50); err != nil || again == s {
		t.Fatalf("not restarted after stopping: %v", err)
	}
}

// At fuserd's exit only the Main Stage server is stopped; the session containers are left running, and a restarted
// fuserd keeps off their ports.
func TestCloseLeavesSessionsRunning(t *testing.T) {
	m := newFDSManager(t, 4, 4)
	pub, _ := m.ReserveAnyFor([]Key{{Rounds: 1, Measures: 32}}, "VenueA", "day")
	priv, _ := m.ReservePrivate(Key{Rounds: 1, Measures: 32}, "VenueC", "night")
	ms, _ := m.EnsureMainStage("VenueB", "night", 50)
	stop, left := m.closeSplit()
	if len(stop) != 1 || stop[0] != ms.ID {
		t.Fatalf("stopped %v, want only the main stage %s", stop, ms.ID)
	}
	if len(left) != 2 || !slices.Contains(left, pub.ID) || !slices.Contains(left, priv.ID) {
		t.Fatalf("left running %v", left)
	}
	if m.leftRunning(ms) != nil || m.leftRunning(pub) == nil {
		t.Fatal("only the session supervisors return at exit")
	}

	ports := slotPorts(pub.ID + "\n" + priv.ID + "\nsomething-else-7000\nfuserd-public-1-notaport\n")
	if len(ports) != 2 || !ports[pub.Port] || !ports[priv.Port] {
		t.Fatalf("ports %v", ports)
	}
	next := newFDSManager(t, 4, 4)
	next.runningPorts = func() map[int]bool { return slotPorts(pub.ID + "\n" + priv.ID) }
	s, err := next.ReserveAnyFor([]Key{{Rounds: 1, Measures: 32}}, "VenueA", "day")
	if err != nil || s.Port == pub.Port || s.Port == priv.Port {
		t.Fatalf("new slot on port %d (left running: %d, %d): %v", s.Port, pub.Port, priv.Port, err)
	}
}

// The Main Stage server's status carries its members; MainStageLive returns the newest report.
func TestFDSMainStageLive(t *testing.T) {
	m := newFDSManager(t, 2, 4)
	s, _ := m.EnsureMainStage("VenueB", "night", 50)
	if _, ok := m.MainStageLive(); ok {
		t.Fatal("live before any report")
	}
	m.Register(s.ID, s.regToken, Registration{Port: s.Port})
	st := Status{Players: 2, DJs: 1, Viewers: 1, Members: []Member{{UserID: 7, Team: "dj"}, {UserID: 9, Team: "spectator"}},
		CurrentDJ: 7, Event: "e1"}
	if err := m.UpdateStatus(s.ID, s.regToken, st); err != nil {
		t.Fatal(err)
	}
	live, ok := m.MainStageLive()
	if !ok || len(live.Members) != 2 || live.CurrentDJ != 7 || live.Event != "e1" {
		t.Fatalf("live %+v %v", live, ok)
	}
}
