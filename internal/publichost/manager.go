// Package publichost supervises the FuserDedicatedServer (FDS) containers that serve public freestyle matchmaking,
// private sessions and the Main Stage. Each container registers with fuserd over /fds/v1 with a per-container token.
// Docker is only invoked when the feature is enabled in fuserd's configuration.
package publichost

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	mrand "math/rand/v2"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

var ErrCapacity = errors.New("no public server available")

// Errors of the FDS registration API (Register / UpdateStatus / ReportExit).
var (
	ErrUnknownSlot  = errors.New("unknown or stopped slot")
	ErrUnauthorized = errors.New("registration token does not match the slot")
)

type Config struct {
	Image, GameRoot, Address                   string
	MaxServers, BasePort, PortStep, MaxPlayers int
	// MaxDJs is a lobby's performer slots: a DJ ticket only joins a server with a free one (0 = no check).
	MaxDJs         int
	StartupTimeout time.Duration
	// GameRoot is a FUSER install mounted read-only at /fuser, RegisterURL fuserd's base URL as the container
	// reaches it, PakKey FUSER's pak key (passed through the container's environment only) and Heartbeat the status
	// interval the container is given.
	RegisterURL string
	PakKey      string
	Heartbeat   time.Duration
	// TokenPublicKey is the public key of fuserd's session-token signer (fdstoken); an FDS container that has it
	// admits only clients with a valid token (FDS_TOKEN_PUBLIC_KEY). Empty: containers admit anyone.
	TokenPublicKey string
}

type Key struct{ Rounds, Measures int }

type Slot struct {
	ID  string
	Key Key
	// TimeOfDay is the venue_time_of_day the container loads: the creating ticket's, else a random one. Every
	// player sees the same sky.
	TimeOfDay string
	// Venue is the venue the slot was started for (the creating ticket's venue_select); an FDS container loads it.
	Venue string
	// Private is an FDS container started for one private session (ReservePrivate): never offered to public tickets
	// or viewers; its players arrive by the creator's ticket and by invite (ReserveSlot).
	Private bool
	// MainStage is the persistent Main Stage server (EnsureMainStage): never offered to tickets
	// or viewers, never drained when empty; clients reach it through main_stage/servers/. MaxPlayers is its capacity.
	MainStage  bool
	MaxPlayers int
	Address    string
	Port       int
	ready      chan struct{}
	err        error
	readyFlag  bool
	stopped    bool
	draining   bool
	drainingAt time.Time
	remote     int
	reserved   int
	reservedAt []time.Time
	// djs is the DJ count the container reports; djReservedAt are the DJ tickets placed here and not yet seen in it
	// (a subset of reservedAt, pruned and consumed the same way).
	djs          int
	djReservedAt []time.Time
	seenRemote   bool
	lastStatus   time.Time
	stage        int
	readyAt      time.Time
	// actual is the set length the container reported at registration. Zero until reported.
	actual Key
	// regToken is the container's registration token: random, per slot, handed to the container through its
	// environment and never logged.
	regToken string
	// registered is what the FDS container reported at registration.
	registered Registration
	// live is its newest player list (UpdateStatus).
	live Live
}

// Registration is an FDS container's POST /fds/v1/register body.
type Registration struct {
	Version     string `json:"version"`
	NetVersion  int    `json:"net_version"`
	Port        int    `json:"port"`
	BeaconPort  int    `json:"beacon_port"`
	Venue       string `json:"venue"`
	Map         string `json:"map"`
	TimeOfDay   string `json:"time_of_day"`
	Rounds      int    `json:"rounds"`
	TurnMeasure int    `json:"turn_measures"`
}

// Status is an FDS container's POST /fds/v1/status body (its heartbeat).
type Status struct {
	Players int `json:"players"`
	DJs     int `json:"djs"`
	Viewers int `json:"viewers"`
	Stage   int `json:"stage"`
	Round   int `json:"round"`
	// Who is connected, by hype user id in join order, the turn's DJ (0 = none) and the Main
	// Stage event being played ("" = none).
	Members   []Member `json:"members"`
	CurrentDJ int64    `json:"current_dj"`
	Event     string   `json:"event"`
}

// Member is one connected player: team is "dj", "spectator" or "unknown".
type Member struct {
	UserID int64  `json:"user_id"`
	Team   string `json:"team"`
}

// Live is the newest player list an FDS container reported.
type Live struct {
	Members   []Member
	CurrentDJ int64
	Event     string
	At        time.Time
}

// roundsOf is the round count a slot serves: the reported one, else the requested one.
func (s *Slot) roundsOf() int {
	if s.actual.Rounds > 0 {
		return s.actual.Rounds
	}
	return s.Key.Rounds
}

func (s *Slot) Ready() <-chan struct{} { return s.ready }

type Event struct {
	SlotID        string
	Ready         bool
	Stopped       bool
	RemotePlayers int
	Stage         int
	Err           error
}

type Manager struct {
	mu     sync.Mutex
	runWG  sync.WaitGroup
	cfg    Config
	log    *slog.Logger
	slots  map[string]*Slot
	events chan Event
	closed bool
	// exiting is closed when fuserd shuts down: the supervisors of the containers left running return (Close).
	exiting chan struct{}
	// start supervises a new slot's container (runFDS) and calls runWG.Done when it ends; tests replace it.
	start func(*Slot)
	// runningPorts lists the game ports of running fuserd-* containers (dockerPorts); tests replace it.
	runningPorts func() map[int]bool
}

func New(cfg Config, log *slog.Logger) *Manager {
	if cfg.StartupTimeout <= 0 {
		cfg.StartupTimeout = 120 * time.Second
	}
	if cfg.PortStep <= 100 {
		cfg.PortStep = 200
	}
	if cfg.MaxPlayers <= 0 {
		cfg.MaxPlayers = 11
	}
	if cfg.Heartbeat <= 0 {
		cfg.Heartbeat = 10 * time.Second
	}
	m := &Manager{cfg: cfg, log: log, slots: make(map[string]*Slot), events: make(chan Event, 128), exiting: make(chan struct{})}
	m.start = m.runFDS
	m.runningPorts = dockerPorts
	return m
}

// timesOfDay are the client's venue_time_of_day strings, in EVenueTimeOfDay order.
var timesOfDay = []string{"morning", "day", "sunset", "night"}

// TimeOfDayValid reports whether tod is one of timesOfDay. Only these reach a container's environment.
func TimeOfDayValid(tod string) bool {
	for _, t := range timesOfDay {
		if tod == t {
			return true
		}
	}
	return false
}

func randomTimeOfDay() string { return timesOfDay[mrand.IntN(len(timesOfDay))] }

func (m *Manager) Events() <-chan Event { return m.events }

func pruneReservations(s *Slot) {
	cutoff := time.Now().Add(-2 * time.Minute)
	kept := s.reservedAt[:0]
	for _, at := range s.reservedAt {
		if at.After(cutoff) {
			kept = append(kept, at)
		}
	}
	s.reservedAt = kept
	s.reserved = len(kept)
	djKept := s.djReservedAt[:0]
	for _, at := range s.djReservedAt {
		if at.After(cutoff) {
			djKept = append(djKept, at)
		}
	}
	s.djReservedAt = djKept
}

// performers is a slot's DJs for the performer-slot check: the DJs the container reported plus the DJ tickets
// still being placed there.
func (s *Slot) performers() int { return s.djs + len(s.djReservedAt) }

// reserve counts one more ticket placed in the slot (a DJ's also as a performer). Called with m.mu held.
func (s *Slot) reserve(dj bool) {
	now := time.Now()
	s.reservedAt = append(s.reservedAt, now)
	s.reserved++
	if dj {
		s.djReservedAt = append(s.djReservedAt, now)
	}
}

// Reserve chooses a compatible READY/STARTING server or starts a new one. It
// counts in-flight assignments as well as players reported by the game.
func (m *Manager) Reserve(k Key) (*Slot, error) {
	return m.ReserveAny([]Key{k})
}

// ReserveAny prefers a compatible existing server before creating one for
// the first acceptable setting pair.
// A server it starts gets a random time of day; that is not part of the match.
func (m *Manager) ReserveAny(keys []Key) (*Slot, error) {
	return m.ReserveAnyFor(keys, "", "")
}

// ReserveAnyFor is ReserveAny for a creating ticket's venue and time of day. They only matter when it starts a
// new server: the container loads that venue and keeps that time of day (a valid one, else a random one), so
// the session's matchmaker_data and the server agree.
func (m *Manager) ReserveAnyFor(keys []Key, venue, timeOfDay string) (*Slot, error) {
	return m.ReserveAnyForExcept(keys, venue, timeOfDay, nil, false)
}

// ReserveAnyForExcept is ReserveAnyFor that passes over the existing slots skip reports true for (a slot holding a
// player the ticket's player blocked or was blocked by). For a DJ ticket ("Join a public session") it also passes
// over servers whose performer slots are full (performers >= MaxDJs). It starts a new server when no other slot
// fits.
func (m *Manager) ReserveAnyForExcept(keys []Key, venue, timeOfDay string, skip func(slotID string) bool, dj bool) (*Slot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || len(keys) == 0 {
		return nil, ErrCapacity
	}
	for _, s := range m.slots {
		pruneReservations(s)
		// Only the round count is compared. A public ticket's turn_length is not the player's choice: the client
		// builds it from the round choice too (FUN_1407596c0 reads _publicCoopRoundNumIndex), so 1 round + 64 bars
		// arrives as 1/32. The turn length is set in the server by the first player's
		// ServerSetCoopRoundNumTurnMeasures, which carries the real choice.
		compatible := false
		for _, k := range keys {
			if s.roundsOf() == k.Rounds {
				compatible = true
				break
			}
		}
		if compatible && !s.Private && !s.MainStage && !s.stopped && !s.draining && s.remote+s.reserved < m.cfg.MaxPlayers &&
			(!dj || m.cfg.MaxDJs <= 0 || s.performers() < m.cfg.MaxDJs) && (skip == nil || !skip(s.ID)) {
			s.reserve(dj)
			return s, nil
		}
	}
	return m.newSlot(keys[0], venue, timeOfDay, false, false, dj)
}

// ReservePrivate starts a container for one private session (the creator's ticket), never shared with public
// matchmaking.
func (m *Manager) ReservePrivate(k Key, venue, timeOfDay string) (*Slot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, ErrCapacity
	}
	return m.newSlot(k, venue, timeOfDay, true, false, true)
}

// EnsureMainStage starts the persistent Main Stage server unless one is running or starting, and returns it.
// fuserd calls it periodically, so a Main Stage server that stops is started again.
func (m *Manager) EnsureMainStage(venue, timeOfDay string, maxPlayers int) (*Slot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, ErrCapacity
	}
	for _, s := range m.slots {
		if s.MainStage && !s.stopped {
			return s, nil
		}
	}
	s, err := m.newSlotWith(Key{}, venue, timeOfDay, false, true, false, maxPlayers)
	if err == nil {
		m.log.Info("main stage server starting", "slot", s.ID, "port", s.Port, "venue", venue, "time_of_day", s.TimeOfDay,
			"max_players", maxPlayers)
	}
	return s, err
}

// MainStageLive is the Main Stage server's newest player list, if it reported one within the last three heartbeats.
func (m *Manager) MainStageLive() (Live, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range m.slots {
		if s.MainStage && !s.stopped && s.readyFlag && !s.live.At.IsZero() &&
			time.Since(s.live.At) <= max(3*m.cfg.Heartbeat, 30*time.Second) {
			return s.live, true
		}
	}
	return Live{}, false
}

// IsMainStage reports whether id is the running Main Stage slot.
func (m *Manager) IsMainStage(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.slots[id]
	return s != nil && s.MainStage && !s.stopped
}

// MainStageServer is the Main Stage server's address and port, and whether it is registered and taking players.
func (m *Manager) MainStageServer() (address string, port int, ready bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range m.slots {
		if s.MainStage && !s.stopped {
			return s.Address, s.Port, s.readyFlag
		}
	}
	return "", 0, false
}

// newSlot starts a server on the first free port. Called with m.mu held.
func (m *Manager) newSlot(k Key, venue, timeOfDay string, private, mainStage, dj bool) (*Slot, error) {
	return m.newSlotWith(k, venue, timeOfDay, private, mainStage, dj, m.cfg.MaxPlayers)
}

func (m *Manager) newSlotWith(k Key, venue, timeOfDay string, private, mainStage, dj bool, maxPlayers int) (*Slot, error) {
	if len(m.slots) >= m.cfg.MaxServers {
		return nil, ErrCapacity
	}
	// Containers a previous fuserd left running (Close) still hold their ports.
	used := m.runningPorts()
	for _, s := range m.slots {
		used[s.Port] = true
	}
	port := 0
	for i := 0; i < m.cfg.MaxServers; i++ {
		candidate := m.cfg.BasePort + i*m.cfg.PortStep
		if !used[candidate] {
			port = candidate
			break
		}
	}
	if port == 0 {
		return nil, ErrCapacity
	}
	kind := "public"
	if private {
		kind = "private"
	}
	if mainStage {
		kind = "mainstage"
	}
	id := fmt.Sprintf("fuserd-%s-%d-%d", kind, time.Now().UnixNano(), port)
	token, err := newRegistrationToken()
	if err != nil {
		return nil, err
	}
	s := &Slot{ID: id, Key: k, TimeOfDay: randomTimeOfDay(), Venue: venue, Private: private, MainStage: mainStage,
		MaxPlayers: maxPlayers, Address: m.cfg.Address, Port: port, ready: make(chan struct{}), regToken: token}
	if TimeOfDayValid(timeOfDay) {
		s.TimeOfDay = timeOfDay
	}
	if !mainStage {
		s.reserve(dj)
	}
	m.slots[id] = s
	m.runWG.Add(1)
	go m.start(s)
	return s, nil
}

// ReserveExisting reserves a place in a running public show for a viewer, whatever its set length, and never
// starts a server: READY servers first, then STARTING ones. ErrCapacity when there is none, or all are full.
func (m *Manager) ReserveExisting() (*Slot, error) {
	return m.ReserveExistingExcept(nil)
}

// ReserveExistingExcept is ReserveExisting that passes over the slots skip reports true for.
func (m *Manager) ReserveExistingExcept(skip func(slotID string) bool) (*Slot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var best *Slot
	for _, s := range m.slots {
		pruneReservations(s)
		if m.closed || s.Private || s.MainStage || s.stopped || s.draining || s.remote+s.reserved >= m.cfg.MaxPlayers ||
			(skip != nil && skip(s.ID)) {
			continue
		}
		if best == nil || (s.readyFlag && !best.readyFlag) {
			best = s
		}
	}
	if best == nil {
		return nil, ErrCapacity
	}
	best.reserve(false)
	return best, nil
}

func (m *Manager) ReserveSlot(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.slots[id]
	if s != nil {
		pruneReservations(s)
	}
	if s == nil || s.MainStage || !s.readyFlag || s.stopped || s.draining || s.remote+s.reserved >= m.cfg.MaxPlayers {
		return ErrCapacity
	}
	s.reserve(false)
	return nil
}

func (m *Manager) Release(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s := m.slots[id]; s != nil && s.reserved > 0 {
		s.reservedAt = s.reservedAt[1:]
		s.reserved--
	}
}

// Drain prevents new assignments while the last connected player leaves.
func (m *Manager) Drain(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s := m.slots[id]; s != nil && !s.draining {
		s.draining = true
		s.drainingAt = time.Now()
	}
}

func (m *Manager) Available(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.slots[id]
	if s != nil {
		pruneReservations(s)
	}
	return s != nil && s.readyFlag && !s.stopped && !s.draining && s.remote+s.reserved < m.cfg.MaxPlayers
}

// Close runs as fuserd exits. Only the Main Stage server is stopped (fuserd starts a new one when it comes back):
// public and private session containers keep serving their players and exit by themselves once empty. Their
// supervisors return without waiting for them, so a fuserd restart doesn't end anyone's set.
func (m *Manager) Close() {
	m.mu.Lock()
	m.closed = true
	stop, left := m.closeSplit()
	m.mu.Unlock()
	for _, id := range left {
		m.log.Info("public server left running as fuserd exits", "slot", id)
	}
	close(m.exiting)
	for _, id := range stop {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		if err := exec.CommandContext(ctx, "docker", "stop", "--time", "5", id).Run(); err != nil {
			m.log.Warn("could not stop public server", "slot", id, "err", err)
		}
		cancel()
	}
	done := make(chan struct{})
	go func() { m.runWG.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		m.log.Warn("timed out waiting for public server supervisors")
	}
}

// dockerPorts is the game ports of the running fuserd-* containers: a slot id ends in its port. Empty when docker
// can't be asked.
func dockerPorts() map[int]bool {
	ports := map[int]bool{}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "ps", "--filter", "name=^fuserd-", "--format", "{{.Names}}").Output()
	if err != nil {
		return ports
	}
	return slotPorts(string(out))
}

// slotPorts reads the ports out of a list of slot ids (fuserd-<kind>-<nanoseconds>-<port>), one per line.
func slotPorts(names string) map[int]bool {
	ports := map[int]bool{}
	for _, name := range strings.Fields(names) {
		if i := strings.LastIndexByte(name, '-'); i > 0 && strings.HasPrefix(name, "fuserd-") {
			if port, err := strconv.Atoi(name[i+1:]); err == nil && port > 0 {
				ports[port] = true
			}
		}
	}
	return ports
}

// closeSplit sorts the slots for Close: the Main Stage server is stopped, every other container is left running.
// Called with m.mu held.
func (m *Manager) closeSplit() (stop, left []string) {
	for id, s := range m.slots {
		if s.MainStage {
			stop = append(stop, id)
		} else if !s.stopped {
			left = append(left, id)
		}
	}
	return stop, left
}

// leftRunning is the channel a slot's supervisor watches to return at fuserd's exit without stopping its container:
// nil (never ready) for the Main Stage server, which Close stops and whose exit the supervisor waits for.
func (m *Manager) leftRunning(s *Slot) <-chan struct{} {
	if s.MainStage {
		return nil
	}
	return m.exiting
}

func (m *Manager) State(id string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.slots[id]
	if s == nil {
		return false, errors.New("server stopped")
	}
	return s.readyFlag && !s.stopped && !s.draining, s.err
}

// The slot state changes made by the registration API (Register, UpdateStatus). Called with m.mu held.

func (m *Manager) setActual(s *Slot, got Key, source string) {
	if got != s.actual {
		s.actual = got
		m.log.Info("public server set length", "slot", s.ID, "rounds", got.Rounds, "measures", got.Measures,
			"requested_rounds", s.Key.Rounds, "requested_measures", s.Key.Measures, "source", source)
	}
}

func (m *Manager) markReady(s *Slot) *Event {
	if s.readyFlag {
		return nil
	}
	s.readyFlag = true
	s.readyAt = time.Now()
	close(s.ready)
	return &Event{SlotID: s.ID, Ready: true}
}

func (m *Manager) applyStatus(s *Slot, count, stage int) *Event {
	if count > 0 {
		s.seenRemote = true
	}
	if count == 0 && s.seenRemote && !s.draining && !s.MainStage {
		s.draining = true
		s.drainingAt = time.Now()
	}
	// A rising game count consumes reservations made during connection setup.
	if count > s.remote {
		used := count - s.remote
		if used > s.reserved {
			used = s.reserved
		}
		s.reserved -= used
		s.reservedAt = s.reservedAt[used:]
	}
	changed := count != s.remote || stage != s.stage
	s.remote, s.lastStatus, s.stage = count, time.Now(), stage
	if !changed {
		return nil // a heartbeat with nothing new: keeps the slot healthy without an event
	}
	return &Event{SlotID: s.ID, RemotePlayers: count, Stage: stage}
}

func (m *Manager) emit(event *Event) {
	if event == nil {
		return
	}
	select {
	case m.events <- *event:
	default:
		m.log.Warn("public server event queue full", "slot", event.SlotID)
	}
}

func (m *Manager) unhealthy(id string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.slots[id]
	if s == nil || !s.readyFlag {
		return ""
	}
	pruneReservations(s)
	if s.draining && !s.drainingAt.IsZero() && time.Since(s.drainingAt) > 45*time.Second {
		return "draining server did not exit"
	}
	last := s.lastStatus
	if last.IsZero() {
		last = s.readyAt
	}
	if time.Since(last) > max(3*m.cfg.Heartbeat, 30*time.Second) { // three missed heartbeats
		return "status stalled"
	}
	if !s.MainStage && !s.seenRemote && s.reserved == 0 && time.Since(s.readyAt) > 5*time.Minute {
		return "no player connected after startup"
	}
	return ""
}

func (m *Manager) stop(id string, err error) {
	m.mu.Lock()
	s := m.slots[id]
	if s == nil {
		m.mu.Unlock()
		return
	}
	s.stopped, s.err = true, err
	if !s.readyFlag {
		close(s.ready)
	}
	delete(m.slots, id)
	m.mu.Unlock()
	select {
	case m.events <- Event{SlotID: id, Stopped: true, Err: err}:
	default:
		m.log.Warn("public server stop event dropped", "slot", id)
	}
}

// SiteStatus exposes display metadata, including full servers, without exposing addresses.
type SiteStatus struct {
	Venue, TimeOfDay string
	Stage            int
	Rounds, Bars     int
}

func (m *Manager) SiteSnapshot() map[string]SiteStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]SiteStatus)
	for id, s := range m.slots {
		if !s.readyFlag || s.stopped || s.draining || s.MainStage {
			continue
		}
		stage := -1
		if !s.lastStatus.IsZero() && time.Since(s.lastStatus) <= time.Minute {
			stage = s.stage
		}
		bars := s.actual.Measures
		if bars <= 0 {
			bars = s.Key.Measures
		}
		out[id] = SiteStatus{Stage: stage, Rounds: s.roundsOf(), Bars: bars, Venue: s.Venue, TimeOfDay: s.TimeOfDay}
	}
	return out
}
