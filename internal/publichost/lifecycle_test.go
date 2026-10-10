package publichost

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// A container still starting keeps its reservations; once ready they last two minutes from the later of the
// reservation and readiness. A poll of a PLACING ticket renews one, and Release gives back a DJ's performer slot.
func TestReservationsWaitForTheServer(t *testing.T) {
	m := newFDSManager(t, 2, 4)
	m.cfg.MaxDJs = 4
	s, err := m.ReserveAnyForExcept([]Key{{Rounds: 1, Measures: 32}}, "", "", nil, true)
	if err != nil {
		t.Fatal(err)
	}
	s.reservedAt[0] = time.Now().Add(-10 * time.Minute) // a long boot
	s.djReservedAt[0] = s.reservedAt[0]
	pruneReservations(s)
	if s.reserved != 1 || len(s.djReservedAt) != 1 {
		t.Fatalf("starting server lost its reservation: %d / %d", s.reserved, len(s.djReservedAt))
	}
	if err := m.Register(s.ID, s.regToken, Registration{Port: s.Port, BeaconPort: s.Port + 100}); err != nil {
		t.Fatal(err)
	}
	pruneReservations(s)
	if s.reserved != 1 {
		t.Fatal("reservation dropped as the server became ready")
	}
	s.readyAt = time.Now().Add(-3 * time.Minute)
	m.Refresh(s.ID, true)
	pruneReservations(s)
	if s.reserved != 1 || len(s.djReservedAt) != 1 {
		t.Fatalf("refreshed reservation dropped: %d / %d", s.reserved, len(s.djReservedAt))
	}
	s.reservedAt[0] = time.Now().Add(-3 * time.Minute)
	s.djReservedAt[0] = s.reservedAt[0]
	pruneReservations(s)
	if s.reserved != 0 || len(s.djReservedAt) != 0 {
		t.Fatalf("stale reservation kept: %d / %d", s.reserved, len(s.djReservedAt))
	}

	// Release(dj) gives back the performer slot too; a viewer's release leaves the DJ ones.
	m.ReserveAnyForExcept([]Key{{Rounds: 1, Measures: 32}}, "", "", nil, true)
	m.ReserveExisting()
	if s.reserved != 2 || len(s.djReservedAt) != 1 {
		t.Fatalf("reserved %d, dj %d", s.reserved, len(s.djReservedAt))
	}
	m.Release(s.ID, false)
	if s.reserved != 1 || len(s.djReservedAt) != 1 {
		t.Fatalf("viewer release: reserved %d, dj %d", s.reserved, len(s.djReservedAt))
	}
	m.Release(s.ID, true)
	if s.reserved != 0 || len(s.djReservedAt) != 0 || s.performers() != 0 {
		t.Fatalf("DJ release: reserved %d, dj %d", s.reserved, len(s.djReservedAt))
	}
}

// Three containers that stop before registering pause launches (ErrPaused), longer on each trip; a server that
// registers ends the backoff. A container stopped for a port conflict doesn't count.
func TestLaunchFailureBackoff(t *testing.T) {
	m := newFDSManager(t, 4, 4)
	keys := []Key{{Rounds: 1, Measures: 32}}
	fail := func() {
		t.Helper()
		s, err := m.ReservePrivate(keys[0], "", "")
		if err != nil {
			t.Fatal(err)
		}
		m.stop(s.ID, errors.New("docker run: exit 125"))
	}
	conflict, _ := m.ReservePrivate(keys[0], "", "")
	m.Register(conflict.ID, conflict.regToken, Registration{Port: conflict.Port + 1})
	fail()
	fail()
	if _, err := m.ReservePrivate(keys[0], "", ""); err != nil {
		t.Fatalf("paused after two failures (and a port conflict): %v", err)
	}
	m.mu.Lock()
	for id := range m.slots {
		delete(m.slots, id)
	}
	m.mu.Unlock()
	fail()
	if _, err := m.ReserveAnyFor(keys, "", ""); !errors.Is(err, ErrPaused) {
		t.Fatalf("third failure: %v", err)
	}
	if _, err := m.EnsureMainStage("VenueB", "night", 50); !errors.Is(err, ErrPaused) {
		t.Fatalf("main stage during the pause: %v", err)
	}
	first := time.Until(m.pausedUntil)
	if first < launchPauseBase-time.Second || first > launchPauseBase {
		t.Fatalf("first pause %s", first)
	}
	m.pausedUntil = time.Time{}
	fail()
	fail()
	fail()
	if second := time.Until(m.pausedUntil); second < 2*launchPauseBase-time.Second {
		t.Fatalf("second pause %s, want doubled", second)
	}
	m.pausedUntil = time.Time{}
	s, err := m.ReserveAnyFor(keys, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Register(s.ID, s.regToken, Registration{Port: s.Port}); err != nil {
		t.Fatal(err)
	}
	if m.pauseTrips != 0 || len(m.launchFails) != 0 {
		t.Fatalf("backoff not reset by a registration: trips %d fails %d", m.pauseTrips, len(m.launchFails))
	}
}

// The Main Stage server's restart backs off 30 s, 60 s, 120 s and 300 s after consecutive failures; a clean exit
// after running (its scheduled restart) restarts it at once.
func TestMainStageRelaunchBackoff(t *testing.T) {
	m := newFDSManager(t, 2, 4)
	for i, want := range []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 5 * time.Minute, 5 * time.Minute} {
		s, err := m.EnsureMainStage("VenueB", "night", 50)
		if err != nil {
			t.Fatalf("start %d: %v", i, err)
		}
		m.Register(s.ID, s.regToken, Registration{Port: s.Port})
		m.stop(s.ID, errors.New("container exit 139"))
		if _, err := m.EnsureMainStage("VenueB", "night", 50); !errors.Is(err, ErrPaused) {
			t.Fatalf("failure %d: restarted at once: %v", i+1, err)
		}
		if wait := time.Until(m.mainStageNext); wait > want || wait < want-time.Second {
			t.Fatalf("failure %d: wait %s, want %s", i+1, wait, want)
		}
		m.mainStageNext = time.Time{} // the wait is over
	}
	s, _ := m.EnsureMainStage("VenueB", "night", 50)
	m.Register(s.ID, s.regToken, Registration{Port: s.Port})
	m.stop(s.ID, nil) // scheduled restart: exit 0
	if again, err := m.EnsureMainStage("VenueB", "night", 50); err != nil || again == s {
		t.Fatalf("not restarted at once after a clean exit: %v", err)
	}
	if m.mainStageFails != 0 {
		t.Fatalf("failures %d after a clean exit", m.mainStageFails)
	}
}

// A container that registers on other ports than its slot's is stopped at once and both ports cool down: the next
// server takes another port. A port another program holds is skipped too.
func TestPortConflictStopsAndCoolsDown(t *testing.T) {
	m := newFDSManager(t, 3, 4)
	var stopped []string
	var mu sync.Mutex
	done := make(chan struct{}, 4)
	m.docker = func(_ context.Context, args ...string) ([]byte, error) {
		mu.Lock()
		stopped = append(stopped, strings.Join(args, " "))
		mu.Unlock()
		done <- struct{}{}
		return nil, nil
	}
	keys := []Key{{Rounds: 1, Measures: 32}}
	s, _ := m.ReserveAnyFor(keys, "", "")
	if s.Port != 7777 {
		t.Fatalf("first port %d", s.Port)
	}
	if err := m.Register(s.ID, s.regToken, Registration{Port: s.Port, BeaconPort: s.Port + 101}); err == nil {
		t.Fatal("register on another beacon port accepted")
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("container not stopped")
	}
	mu.Lock()
	if len(stopped) != 1 || stopped[0] != "stop --time 5 "+s.ID {
		t.Fatalf("docker calls %v", stopped)
	}
	mu.Unlock()
	select {
	case <-s.Ready():
	default:
		t.Fatal("ready channel of the stopped slot still open (its tickets would wait for the timeout)")
	}
	if ready, _ := m.State(s.ID); ready || m.portCooldown[7777].IsZero() || m.portCooldown[7877].IsZero() {
		t.Fatalf("slot ready %v, cooldown %v", ready, m.portCooldown)
	}
	next, err := m.ReserveAnyFor(keys, "", "")
	if err != nil || next.Port != 7977 {
		t.Fatalf("next server on %d: %v", next.Port, err)
	}

	// Another program holds 8177's beacon port: skipped, and cooling down.
	m.portFree = func(port int) bool { return port != 8177 }
	if _, err := m.ReservePrivate(keys[0], "", ""); !errors.Is(err, ErrCapacity) {
		t.Fatalf("held port used: %v", err)
	}
	if m.portCooldown[8177].IsZero() {
		t.Fatal("held port not cooling down")
	}
	m.portCooldown = map[int]time.Time{7777: time.Now().Add(-time.Second)} // expired
	if s, err := m.ReservePrivate(keys[0], "", ""); err != nil || s.Port != 7777 {
		t.Fatalf("expired cooldown: %+v %v", s, err)
	}
}

// FDS reports the ports it actually bound, 0 for one it isn't listening on: a reported 0 is a conflict too (the
// container is stopped and the ports cool down). A port the body leaves out isn't checked.
func TestRegisterUnboundPortStops(t *testing.T) {
	decode := func(body string) Registration {
		t.Helper()
		var r Registration
		if err := json.Unmarshal([]byte(body), &r); err != nil {
			t.Fatal(err)
		}
		return r
	}
	if r := decode(`{"port":7777,"beacon_port":7877,"venue":"VenueC","rounds":2}`); r.Port != 7777 || r.BeaconPort != 7877 ||
		!r.portsReported || !r.beaconReported || r.Venue != "VenueC" || r.Rounds != 2 {
		t.Fatalf("decoded %+v", r)
	}

	for _, body := range []string{`{"port":%d,"beacon_port":0}`, `{"port":0,"beacon_port":%d}`} {
		m := newFDSManager(t, 2, 4)
		done := make(chan string, 4)
		m.docker = func(_ context.Context, args ...string) ([]byte, error) {
			done <- strings.Join(args, " ")
			return nil, nil
		}
		s, err := m.ReserveAnyFor([]Key{{Rounds: 1, Measures: 32}}, "", "")
		if err != nil {
			t.Fatal(err)
		}
		port := s.Port
		if strings.HasPrefix(body, `{"port":0`) {
			port += beaconOffset
		}
		if err := m.Register(s.ID, s.regToken, decode(fmt.Sprintf(body, port))); err == nil {
			t.Fatalf("%s: register accepted", body)
		}
		select {
		case call := <-done:
			if call != "stop --time 5 "+s.ID {
				t.Fatalf("%s: docker %q", body, call)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: container not stopped", body)
		}
		if ready, _ := m.State(s.ID); ready || !s.portConflict || m.portCooldown[s.Port].IsZero() ||
			m.portCooldown[s.Port+beaconOffset].IsZero() {
			t.Fatalf("%s: ready %v, conflict %v, cooldown %v", body, ready, s.portConflict, m.portCooldown)
		}
	}

	// Left out: not checked, as before.
	m := newFDSManager(t, 2, 4)
	s, err := m.ReserveAnyFor([]Key{{Rounds: 1, Measures: 32}}, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Register(s.ID, s.regToken, decode(fmt.Sprintf(`{"port":%d}`, s.Port))); err != nil {
		t.Fatalf("register without beacon_port: %v", err)
	}
	if ready, _ := m.State(s.ID); !ready {
		t.Fatal("slot not ready")
	}
}

// A container due for a restart (recycle_pending) takes no new tickets, viewers or joins, but stays alive and
// listed.
func TestRecyclePendingStopsPlacements(t *testing.T) {
	m := newFDSManager(t, 2, 6)
	keys := []Key{{Rounds: 1, Measures: 32}}
	s, _ := m.ReserveAnyFor(keys, "", "")
	m.Register(s.ID, s.regToken, Registration{Port: s.Port})
	if err := m.UpdateStatus(s.ID, s.regToken, Status{Players: 2, DJs: 2, RecyclePending: true}); err != nil {
		t.Fatal(err)
	}
	if m.Available(s.ID) || m.ReserveSlot(s.ID) == nil {
		t.Fatal("recycling server takes joins")
	}
	if _, err := m.ReserveExisting(); !errors.Is(err, ErrCapacity) {
		t.Fatalf("viewer placed on a recycling server: %v", err)
	}
	if got, err := m.ReserveAnyFor(keys, "", ""); err != nil || got.ID == s.ID {
		t.Fatalf("ticket placed on a recycling server: %v", err)
	}
	if ready, _ := m.State(s.ID); !ready || m.unhealthy(s.ID) != "" {
		t.Fatal("recycling server treated as gone")
	}
	if _, ok := m.SiteSnapshot()[s.ID]; !ok {
		t.Fatal("recycling server not listed")
	}
}

// The warm pool keeps warm_servers public servers with no player and no reservation; a ticket placed on one makes
// the pool start another. Warm servers wait for players without the unused-server timeout.
func TestWarmPool(t *testing.T) {
	m := newFDSManager(t, 3, 4)
	if m.topUpWarm() != nil {
		t.Fatal("warm server started with the pool off")
	}
	m.cfg.WarmServers, m.cfg.WarmRounds = 1, 2
	w := m.topUpWarm()
	if w == nil || !w.warm || w.reserved != 0 || w.Key != (Key{Rounds: 2, Measures: 64}) || w.Private || w.MainStage {
		t.Fatalf("warm slot %+v", w)
	}
	if m.topUpWarm() != nil {
		t.Fatal("a second warm server started while the first boots")
	}
	m.Register(w.ID, w.regToken, Registration{Port: w.Port})
	w.readyAt = time.Now().Add(-time.Hour)
	if reason := m.unhealthy(w.ID); reason != "" {
		t.Fatalf("idle warm server unhealthy: %s", reason)
	}
	if got, err := m.ReserveAnyFor([]Key{{Rounds: 2, Measures: 64}}, "", ""); err != nil || got.ID != w.ID {
		t.Fatalf("ticket not placed on the warm server: %+v %v", got, err)
	}
	next := m.topUpWarm()
	if next == nil || next.ID == w.ID {
		t.Fatal("pool not topped up after its server was taken")
	}
	m.pausedUntil = time.Now().Add(time.Minute)
	m.mu.Lock()
	delete(m.slots, next.ID)
	m.mu.Unlock()
	if m.topUpWarm() != nil {
		t.Fatal("warm server started during the launch pause")
	}
}

// The warm pool leaves a slot for ticket-started servers (and one for a Main Stage server that is down); a server
// that finds every slot taken retires an idle warm server, never one with a player or one still starting.
func TestWarmPoolLeavesRoom(t *testing.T) {
	m := newFDSManager(t, 4, 4)
	f := &fakeDocker{configs: map[string]any{}}
	m.docker = f.run
	m.cfg.WarmServers, m.cfg.WarmRounds, m.cfg.WarmKeepMainStage = 4, 1, true
	var warm []*Slot
	for w := m.topUpWarm(); w != nil; w = m.topUpWarm() {
		m.Register(w.ID, w.regToken, Registration{Port: w.Port})
		warm = append(warm, w)
	}
	if len(warm) != 2 {
		t.Fatalf("%d warm servers with 4 slots and the Main Stage down, want 2", len(warm))
	}
	ms, err := m.EnsureMainStage("", "", 50)
	if err != nil {
		t.Fatalf("main stage server: %v", err)
	}
	if m.topUpWarm() != nil {
		t.Fatal("warm server took the last free slot")
	}
	m.UpdateStatus(warm[0].ID, warm[0].regToken, Status{Players: 1}) // in use: never retired
	if _, err := m.ReserveAnyFor([]Key{{Rounds: 3, Measures: 128}}, "", ""); err != nil {
		t.Fatalf("ticket server: %v", err)
	}
	if _, err := m.ReservePrivate(Key{Rounds: 2, Measures: 64}, "", ""); !errors.Is(err, ErrCapacity) {
		t.Fatalf("private server with every slot taken: %v", err)
	}
	m.mu.Lock()
	idle, used, stage := warm[1].draining, warm[0].draining, ms.draining
	m.mu.Unlock()
	if !idle || used || stage {
		t.Fatalf("retired: idle warm %v, warm with a player %v, main stage %v", idle, used, stage)
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		f.mu.Lock()
		stopped := strings.Join(f.stopped, " ")
		f.mu.Unlock()
		if stopped == warm[1].ID {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("stopped %q, want the idle warm server", stopped)
		}
	}
	m.stop(warm[1].ID, nil) // its container exited
	if s, err := m.ReservePrivate(Key{Rounds: 2, Measures: 64}, "", ""); err != nil || s.Port != warm[1].Port {
		t.Fatalf("private server after the warm one exited: %+v %v", s, err)
	}
}

// fakeDocker answers Recover's docker calls from a table of running containers (name -> inspect .Config).
type fakeDocker struct {
	mu      sync.Mutex
	configs map[string]any
	stopped []string
}

func (f *fakeDocker) run(_ context.Context, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch args[0] {
	case "ps":
		var names []string
		for name := range f.configs {
			names = append(names, name)
		}
		return []byte(strings.Join(names, "\n") + "\n"), nil
	case "inspect":
		return json.Marshal(f.configs[args[len(args)-1]])
	case "stop":
		f.stopped = append(f.stopped, args[len(args)-1])
		return nil, nil
	}
	return nil, errors.New("unexpected docker call")
}

// On startup a stray Main Stage server is stopped, a clean session container re-adopted (its heartbeats accepted,
// its token never logged) and the rest left running (they may hold players).
func TestRecoverAdoptsCleanContainers(t *testing.T) {
	var logs bytes.Buffer
	m := newFDSManager(t, 4, 4)
	m.log = slog.New(slog.NewTextHandler(&logs, nil))
	m.cfg.TokenPublicKey = "pubkey"
	token := strings.Repeat("ab", 32)
	env := func(name, access string, port string, extra ...string) map[string]any {
		e := []string{"PATH=/usr/bin", "FDS_PORT=" + port, "FDS_SLOT_ID=" + name, "FDS_REGISTER_URL=http://127.0.0.1:8000",
			"FDS_ROUNDS=2", "FDS_TURN_MEASURES=64", "FDS_SERVER_ACCESS=" + access, "FDS_TOKEN_PUBLIC_KEY=pubkey",
			"FDS_VENUE=VenueC", "FDS_TIME_OF_DAY=night", "FDS_REGISTER_TOKEN=" + token, "FDS_PAK_KEY=secret-pak-key"}
		return map[string]any{"Image": "fds-server:test", "Env": append(e, extra...)}
	}
	const (
		public    = "fuserd-public-100-7777"
		private   = "fuserd-private-101-7977"
		mainStage = "fuserd-mainstage-102-8177"
		otherKey  = "fuserd-public-103-8377"
		offPlan   = "fuserd-public-104-7778"
	)
	f := &fakeDocker{configs: map[string]any{
		public:    env(public, "public", "7777"),
		private:   env(private, "private", "7977"),
		mainStage: env(mainStage, "public", "8177", "FDS_MAIN_STAGE=1"),
		otherKey:  env(otherKey, "public", "8377", "FDS_TOKEN_PUBLIC_KEY="),
		offPlan:   env(offPlan, "public", "7778"),
	}}
	f.configs[otherKey].(map[string]any)["Env"] = []string{"FDS_PORT=8377", "FDS_SLOT_ID=" + otherKey,
		"FDS_REGISTER_URL=http://127.0.0.1:8000", "FDS_SERVER_ACCESS=public", "FDS_TOKEN_PUBLIC_KEY=old",
		"FDS_REGISTER_TOKEN=" + token}
	m.docker = f.run
	m.Recover()

	f.mu.Lock()
	stopped := strings.Join(f.stopped, " ")
	f.mu.Unlock()
	if stopped != mainStage {
		t.Errorf("stopped %q, want only the stray main stage server", stopped)
	}
	if m.slots[otherKey] != nil || m.slots[offPlan] != nil {
		t.Error("container with another key or off the port plan re-adopted")
	}
	pub, priv := m.slots[public], m.slots[private]
	if pub == nil || priv == nil || !pub.adopted || pub.Private || !priv.Private || pub.Port != 7777 ||
		pub.Key != (Key{Rounds: 2, Measures: 64}) || pub.Venue != "VenueC" || pub.TimeOfDay != "night" {
		t.Fatalf("adopted slots: %+v %+v", pub, priv)
	}
	if err := m.UpdateStatus(public, token, Status{Players: 1, DJs: 1, Members: []Member{{UserID: 5, Team: "dj"}}}); err != nil {
		t.Fatalf("adopted container's heartbeat: %v", err)
	}
	if got, err := m.ReserveAnyFor([]Key{{Rounds: 2, Measures: 64}}, "", ""); err != nil || got.ID != public {
		t.Fatalf("ticket not placed on the adopted server: %+v %v", got, err)
	}
	if s, err := m.ReserveAnyFor([]Key{{Rounds: 3, Measures: 128}}, "", ""); err != nil || s.Port == 7777 || s.Port == 7977 {
		t.Fatalf("new server on an adopted port: %+v %v", s, err)
	}
	if strings.Contains(logs.String(), token) || strings.Contains(logs.String(), "secret-pak-key") {
		t.Fatalf("secret logged: %s", logs.String())
	}
}
