package publichost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Containers a previous fuserd left behind. Close leaves the session containers running so a restart doesn't end
// anyone's set, and a crash leaves every container running. On startup (Recover) fuserd takes back the ones it can
// prove it started with today's settings (re-adopted: their heartbeats are accepted again). The others may still
// have players (an image, port plan or token_secret change, or another fuserd on this Docker host), so they are left
// running: their ports stay off (runningPorts), and an FDS whose heartbeats are refused exits once it is empty. Only a
// stray Main Stage server is stopped, since fuserd starts its own.

// runDocker runs a docker CLI command and returns its standard output. The error never carries the output: an
// inspect's output holds the container's secrets.
func runDocker(ctx context.Context, args ...string) ([]byte, error) {
	out, err := exec.CommandContext(ctx, "docker", args...).Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return nil, fmt.Errorf("docker %s: exit %d", args[0], exit.ExitCode())
		}
		return nil, fmt.Errorf("docker %s: %w", args[0], err)
	}
	return out, nil
}

// stopContainer stops a container (5 s to exit cleanly) and waits for docker, at most 15 s.
func (m *Manager) stopContainer(id string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, err := m.docker(ctx, "stop", "--time", "5", id)
	return err
}

// udpPortsFree reports whether this host can bind a game port and its beacon port over UDP: false when another
// program holds either (the containers use host networking, so they would have to take other ports).
func udpPortsFree(port int) bool {
	for _, p := range []int{port, port + beaconOffset} {
		c, err := net.ListenUDP("udp", &net.UDPAddr{Port: p})
		if err != nil {
			return false
		}
		c.Close()
	}
	return true
}

// slotName is a slot id: fuserd-<kind>-<nanoseconds>-<port>.
var slotName = regexp.MustCompile(`^fuserd-(public|private|mainstage)-[0-9]+-([0-9]{1,5})$`)

// parseSlotName reads a container name as a slot id.
func parseSlotName(name string) (kind string, port int, ok bool) {
	match := slotName.FindStringSubmatch(name)
	if match == nil {
		return "", 0, false
	}
	port, err := strconv.Atoi(match[2])
	if err != nil || port < 1 || port > 65535 {
		return "", 0, false
	}
	return match[1], port, true
}

// Recover deals with the fuserd-* containers already running as fuserd starts, before the Main Stage server and
// the warm pool are started: stray Main Stage servers are stopped, session containers re-adopted when clean
// (adoptable) and left running otherwise. Their registration tokens are read from docker inspect and never logged.
func (m *Manager) Recover() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	out, err := m.docker(ctx, "ps", "--filter", "name=^fuserd-", "--format", "{{.Names}}")
	cancel()
	if err != nil {
		m.log.Warn("could not list containers left running", "err", err)
		return
	}
	for _, name := range strings.Fields(string(out)) {
		kind, port, ok := parseSlotName(name)
		if !ok {
			continue
		}
		if kind == "mainstage" {
			m.stopStray(name, "stray main stage server")
			continue
		}
		s, why := m.adoptable(name, kind, port)
		if s == nil {
			m.leaveStray(name, why)
			continue
		}
		m.mu.Lock()
		taken := m.closed || len(m.slots) >= m.cfg.MaxServers
		for _, other := range m.slots {
			taken = taken || other.Port == s.Port
		}
		if !taken {
			m.slots[s.ID] = s
			m.runWG.Add(1)
		}
		m.mu.Unlock()
		if taken {
			m.leaveStray(name, "no slot free for it")
			continue
		}
		m.log.Info("public fds re-adopted", "slot", s.ID, "port", s.Port, "private", s.Private, "venue", s.Venue,
			"time_of_day", s.TimeOfDay, "rounds", s.Key.Rounds, "measures", s.Key.Measures)
		go m.supervise(s)
	}
}

// leaveStray leaves a session container fuserd won't re-adopt running: it may hold players. Its port is not
// reused while it runs (runningPorts).
func (m *Manager) leaveStray(name, why string) {
	m.log.Info("container left running not re-adopted: leaving it to exit by itself", "slot", name, "reason", why)
}

func (m *Manager) stopStray(name, why string) {
	m.log.Info("stopping container left running", "slot", name, "reason", why)
	if err := m.stopContainer(name); err != nil {
		m.log.Warn("could not stop container left running", "slot", name, "err", err)
	}
}

// adoptable inspects a running session container and rebuilds its slot when it is clean: it runs this fuserd's
// image, on a port of this fuserd's port plan, registers with this fuserd's URL, checks this fuserd's session tokens
// and holds a well-formed registration token. Otherwise it says why not.
func (m *Manager) adoptable(name, kind string, port int) (*Slot, string) {
	if port < m.cfg.BasePort || (port-m.cfg.BasePort)%m.cfg.PortStep != 0 ||
		(port-m.cfg.BasePort)/m.cfg.PortStep >= m.cfg.MaxServers {
		return nil, "port outside the port plan"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	out, err := m.docker(ctx, "inspect", "--format", "{{json .Config}}", name)
	cancel()
	if err != nil {
		return nil, "not inspectable"
	}
	var cfg struct {
		Image string   `json:"Image"`
		Env   []string `json:"Env"`
	}
	if json.Unmarshal(out, &cfg) != nil {
		return nil, "unreadable inspect output"
	}
	env := map[string]string{}
	for _, kv := range cfg.Env {
		if k, v, ok := strings.Cut(kv, "="); ok {
			env[k] = v
		}
	}
	token := env["FDS_REGISTER_TOKEN"]
	switch {
	case cfg.Image != m.cfg.Image:
		return nil, "another image"
	case env["FDS_SLOT_ID"] != name || env["FDS_PORT"] != strconv.Itoa(port) || env["FDS_SERVER_ACCESS"] != kind ||
		env["FDS_MAIN_STAGE"] != "":
		return nil, "environment does not match its name"
	case env["FDS_REGISTER_URL"] != strings.TrimRight(m.cfg.RegisterURL, "/"):
		return nil, "another registration URL"
	case env["FDS_TOKEN_PUBLIC_KEY"] != m.cfg.TokenPublicKey:
		return nil, "another session token key"
	case len(token) != 64 || strings.Trim(token, "0123456789abcdef") != "":
		return nil, "no registration token"
	}
	rounds, _ := strconv.Atoi(env["FDS_ROUNDS"])
	measures, _ := strconv.Atoi(env["FDS_TURN_MEASURES"])
	now := time.Now()
	s := &Slot{ID: name, Key: Key{Rounds: max(rounds, 0), Measures: max(measures, 0)}, Private: kind == "private",
		MaxPlayers: m.cfg.MaxPlayers, Address: m.cfg.Address, Port: port, ready: make(chan struct{}), regToken: token,
		readyFlag: true, readyAt: now, lastStatus: now, adopted: true}
	close(s.ready) // it registered with the previous fuserd and now only sends heartbeats
	if venueValue.MatchString(env["FDS_VENUE"]) {
		s.Venue = env["FDS_VENUE"]
	}
	if TimeOfDayValid(env["FDS_TIME_OF_DAY"]) {
		s.TimeOfDay = env["FDS_TIME_OF_DAY"]
	}
	return s, ""
}

// superviseAdopted watches a re-adopted container as runFDS does a started one after registration.
func (m *Manager) superviseAdopted(s *Slot) {
	defer m.runWG.Done()
	m.watch(s, waitContainer(s.ID))
}

// waitContainer reports a container's exit (docker wait): nil for exit status 0.
func waitContainer(id string) <-chan error {
	done := make(chan error, 1)
	go func() {
		out, err := exec.Command("docker", "wait", id).CombinedOutput()
		if err == nil && strings.TrimSpace(string(out)) != "0" {
			err = fmt.Errorf("container exit %s", strings.TrimSpace(string(out)))
		}
		done <- err
	}()
	return done
}
