package publichost

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// FuserDedicatedServer (FDS) containers call fuserd's /fds/v1 API with the slot's registration token:
// register at start (the slot becomes ready), status on change and every heartbeat, exit when it shuts down.
// The token is random per slot, reaches the container only through its environment (docker run -e NAME with the
// value in the docker CLI's own environment, never on a command line) and is never logged.

func newRegistrationToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("registration token: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// authorized finds the slot a registration call names and checks its token (constant time). Called with m.mu held.
func (m *Manager) authorized(id, token string) (*Slot, error) {
	s := m.slots[id]
	if s == nil || s.stopped {
		return nil, ErrUnknownSlot
	}
	if s.regToken == "" || subtle.ConstantTimeCompare([]byte(token), []byte(s.regToken)) != 1 {
		return nil, ErrUnauthorized
	}
	return s, nil
}

// Register is POST /fds/v1/register: the container is up and accepting players, so the slot becomes ready.
// Registering again (a retry after a lost reply) updates the details and is not an error.
func (m *Manager) Register(id, token string, r Registration) error {
	m.mu.Lock()
	s, err := m.authorized(id, token)
	if err != nil {
		m.mu.Unlock()
		return err
	}
	if r.Port != 0 && r.Port != s.Port {
		m.mu.Unlock()
		return fmt.Errorf("registered port %d, slot port %d", r.Port, s.Port)
	}
	s.registered = r
	if r.Rounds > 0 && r.TurnMeasure > 0 {
		m.setActual(s, Key{Rounds: r.Rounds, Measures: r.TurnMeasure}, "fds register")
	}
	event := m.markReady(s)
	if s.lastStatus.IsZero() {
		s.lastStatus = time.Now()
	}
	m.mu.Unlock()
	m.log.Info("public fds registered", "slot", id, "version", r.Version, "net_version", r.NetVersion, "port", r.Port,
		"venue", r.Venue, "map", r.Map, "time_of_day", r.TimeOfDay, "rounds", r.Rounds, "turn_measures", r.TurnMeasure)
	m.emit(event)
	return nil
}

// UpdateStatus is POST /fds/v1/status, the container's heartbeat: players connected (DJs and viewers), stage.
// Reservations are consumed as players arrive, and an empty server that had players drains.
func (m *Manager) UpdateStatus(id, token string, st Status) error {
	m.mu.Lock()
	s, err := m.authorized(id, token)
	if err != nil {
		m.mu.Unlock()
		return err
	}
	if !s.readyFlag {
		m.mu.Unlock()
		return errors.New("status before register")
	}
	if st.Players < 0 {
		st.Players = 0
	}
	// The DJ count, for the performer-slot check (performers): a rise consumes DJ reservations, as players do
	// reservations in applyStatus.
	if st.DJs < 0 {
		st.DJs = 0
	}
	if rise := st.DJs - s.djs; rise > 0 {
		s.djReservedAt = s.djReservedAt[min(rise, len(s.djReservedAt)):]
	}
	s.djs = st.DJs
	s.live = Live{Members: st.Members, CurrentDJ: st.CurrentDJ, Event: st.Event, At: time.Now()}
	event := m.applyStatus(s, st.Players, st.Stage)
	m.mu.Unlock()
	m.emit(event)
	return nil
}

// ReportExit is POST /fds/v1/exit: the container is shutting down (last player gone, idle, error). The slot stops
// taking players at once; it is removed when the container exits (docker wait).
func (m *Manager) ReportExit(id, token, reason string) error {
	m.mu.Lock()
	s, err := m.authorized(id, token)
	if err != nil {
		m.mu.Unlock()
		return err
	}
	if !s.draining {
		s.draining = true
		s.drainingAt = time.Now()
	}
	m.mu.Unlock()
	m.log.Info("public fds exiting", "slot", id, "reason", reason)
	return nil
}

// Authorize checks an FDS call's slot and token (another /fds/v1 call than register / status / exit) and reports
// whether the slot is the Main Stage server.
func (m *Manager) Authorize(id, token string) (mainStage bool, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, err := m.authorized(id, token)
	if err != nil {
		return false, err
	}
	return s.MainStage, nil
}

// HeartbeatSeconds is the status interval FDS containers are given.
func (m *Manager) HeartbeatSeconds() int { return int(m.cfg.Heartbeat / time.Second) }

var venueValue = regexp.MustCompile(`^[A-Za-z0-9_/]{1,128}$`)

// fdsRunArgs is the docker run command line for an FDS slot. Secrets appear only as bare -e names; their values
// are in the docker CLI's environment (runFDS).
func fdsRunArgs(s *Slot, cfg Config, heartbeat time.Duration) []string {
	access := "public"
	if s.Private {
		access = "private"
	}
	args := []string{"run", "-d", "--rm", "--name", s.ID, "--network", "host",
		"--mount", "type=bind,src=" + cfg.GameRoot + ",dst=/fuser,readonly",
		"-e", "FDS_PORT=" + strconv.Itoa(s.Port),
		"-e", "FDS_SLOT_ID=" + s.ID,
		"-e", "FDS_REGISTER_URL=" + strings.TrimRight(cfg.RegisterURL, "/"),
		"-e", "FDS_HEARTBEAT_SECONDS=" + strconv.Itoa(int(heartbeat/time.Second)),
		"-e", "FDS_ROUNDS=" + strconv.Itoa(s.Key.Rounds),
		"-e", "FDS_TURN_MEASURES=" + strconv.Itoa(s.Key.Measures),
		"-e", "FDS_SERVER_ACCESS=" + access, // as the lobby and the session browser report it
		"-e", "FDS_ADDRESS=" + cfg.Address, // reported in the session browser's beacon reply
	}
	if s.MainStage {
		// The Main Stage: FUSER's Main Stage join carries no session token, so FDS checks fuserd's passes
		// instead (POST /fds/v1/mainstage/admit); it stays up when empty.
		args = append(args, "-e", "FDS_MAIN_STAGE=1", "-e", "FDS_EXIT_WHEN_EMPTY=0",
			"-e", "FDS_MAX_PLAYERS="+strconv.Itoa(s.MaxPlayers))
	} else if cfg.TokenPublicKey != "" {
		args = append(args, "-e", "FDS_TOKEN_PUBLIC_KEY="+cfg.TokenPublicKey) // public: clients must bring a fuserd token
	}
	if venueValue.MatchString(s.Venue) {
		args = append(args, "-e", "FDS_VENUE="+s.Venue)
	}
	if TimeOfDayValid(s.TimeOfDay) {
		args = append(args, "-e", "FDS_TIME_OF_DAY="+s.TimeOfDay)
	}
	return append(args, "-e", "FDS_REGISTER_TOKEN", "-e", "FDS_PAK_KEY", cfg.Image)
}

// runFDS starts an FDS container and supervises it until it exits: ready when it registers (within the startup
// timeout), stopped when its heartbeats stall or it stays unused, removed when it exits.
func (m *Manager) runFDS(s *Slot) {
	defer m.runWG.Done()
	ctx, cancel := context.WithTimeout(context.Background(), m.cfg.StartupTimeout)
	defer cancel()
	run := exec.CommandContext(ctx, "docker", fdsRunArgs(s, m.cfg, m.cfg.Heartbeat)...)
	run.Env = append(os.Environ(), "FDS_REGISTER_TOKEN="+s.regToken, "FDS_PAK_KEY="+m.cfg.PakKey)
	if out, err := run.CombinedOutput(); err != nil {
		m.stop(s.ID, fmt.Errorf("docker run: %w: %s", err, strings.TrimSpace(string(out))))
		return
	}
	m.mu.Lock()
	closed := m.closed
	m.mu.Unlock()
	if closed {
		exec.Command("docker", "stop", s.ID).Run()
		m.stop(s.ID, errors.New("manager closed"))
		return
	}
	m.log.Info("public fds launched", "slot", s.ID, "port", s.Port, "venue", s.Venue, "time_of_day", s.TimeOfDay,
		"rounds", s.Key.Rounds, "measures", s.Key.Measures)
	waitDone := make(chan error, 1)
	go func() {
		out, err := exec.Command("docker", "wait", s.ID).CombinedOutput()
		if err == nil && strings.TrimSpace(string(out)) != "0" {
			err = fmt.Errorf("container exit %s", strings.TrimSpace(string(out)))
		}
		waitDone <- err
	}()
	var err error
	select {
	case <-s.ready:
		cancel() // registered: the startup timeout no longer applies
	case <-ctx.Done():
		m.log.Warn("public fds did not register in time", "slot", s.ID)
		exec.Command("docker", "stop", s.ID).Run()
		m.stop(s.ID, <-waitDone)
		return
	case err = <-waitDone:
		m.stop(s.ID, err)
		return
	case <-m.leftRunning(s):
		return // fuserd is exiting: the container keeps running (Close)
	}
	watch := time.NewTicker(m.cfg.Heartbeat)
	defer watch.Stop()
	for {
		select {
		case err = <-waitDone:
			m.stop(s.ID, err)
			return
		case <-m.leftRunning(s):
			return // fuserd is exiting: the container keeps running (Close)
		case <-watch.C:
			if reason := m.unhealthy(s.ID); reason != "" {
				m.log.Warn("stopping unhealthy public fds", "slot", s.ID, "reason", reason)
				exec.Command("docker", "stop", s.ID).Run()
			}
		}
	}
}
