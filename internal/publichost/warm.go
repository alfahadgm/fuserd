package publichost

import (
	"errors"
	"time"
)

// The warm pool (multiplayer.public_servers.warm_servers): public servers started ahead of any ticket, so a player
// who asks for a public session is placed on a server that is already up instead of waiting for one to boot. It only
// shortens the wait; tickets start servers as before when none fits. The pool never takes the last free slot (nor
// the Main Stage server's while that one is down), and a server a ticket, private session or the Main Stage needs
// retires an idle warm server when every slot is taken (retireWarm).

// warmInterval is how often the pool is topped up. At most one server is started per pass.
const warmInterval = 15 * time.Second

// warmMeasures is the turn length a warm server starts with for its round count: the length a public ticket pairs
// with that round choice (the client builds both from the round choice). It is only the fallback: the first player's
// ServerSetCoopRoundNumTurnMeasures sets the turn length (the server keeps its round count).
func warmMeasures(rounds int) int {
	switch rounds {
	case 2:
		return 64
	case 3:
		return 128
	}
	return 32
}

// KeepWarm starts the warm pool's upkeep when servers is positive: every warmInterval, until fuserd exits, it starts
// a public server for rounds rounds (1 if not positive) while fewer than servers are waiting (warmCount). mainStage
// keeps a slot for the Main Stage server while it is not running. Call it once.
func (m *Manager) KeepWarm(servers, rounds int, mainStage bool) {
	if servers <= 0 {
		return
	}
	if rounds <= 0 {
		rounds = 1
	}
	m.mu.Lock()
	m.cfg.WarmServers, m.cfg.WarmRounds, m.cfg.WarmKeepMainStage = servers, rounds, mainStage
	m.mu.Unlock()
	m.log.Info("public server warm pool", "warm_servers", servers, "rounds", rounds)
	go func() {
		tick := time.NewTicker(warmInterval)
		defer tick.Stop()
		for {
			m.topUpWarm()
			select {
			case <-tick.C:
			case <-m.exiting:
				return
			}
		}
	}()
}

// warmCount is the public servers waiting for players: up or starting, taking players, with no player and no
// reservation. Called with m.mu held.
func (m *Manager) warmCount() int {
	n := 0
	for _, s := range m.slots {
		if s.Private || s.MainStage || !s.placeable() {
			continue
		}
		pruneReservations(s)
		if s.remote == 0 && s.reserved == 0 {
			n++
		}
	}
	return n
}

// idleWarm reports whether s is a warm server no one uses yet: no player ever, no reservation, taking players.
// Called with m.mu held.
func idleWarm(s *Slot) bool {
	return s.warm && !s.Private && !s.MainStage && !s.seenRemote && s.remote == 0 && s.reserved == 0 && s.placeable()
}

// warmFreeSlots is how many slots the pool leaves free: one for a ticket's server, and one more for the Main Stage
// server while it is not running (KeepWarm's mainStage). Called with m.mu held.
func (m *Manager) warmFreeSlots() int {
	free := 1
	if m.cfg.WarmKeepMainStage {
		free++
		for _, s := range m.slots {
			if s.MainStage && !s.stopped {
				free--
				break
			}
		}
	}
	return free
}

// retireWarm stops one idle, registered warm server (idleWarm) so a server that is needed can start once its
// container has exited: it takes no player meanwhile. It reports whether one was retired. Called with m.mu held.
func (m *Manager) retireWarm() bool {
	for _, s := range m.slots {
		pruneReservations(s)
		if !s.readyFlag || !idleWarm(s) {
			continue // one still starting is left alone: docker may not have created its container yet
		}
		s.draining, s.drainingAt = true, time.Now()
		m.log.Info("retiring idle warm public server: its slot is needed", "slot", s.ID, "port", s.Port)
		go func(id string) {
			if err := m.stopContainer(id); err != nil {
				m.log.Warn("could not stop warm public server", "slot", id, "err", err)
			}
		}(s.ID)
		return true
	}
	return false
}

// topUpWarm starts one warm server when the pool is short of WarmServers. It reports the slot started, if any.
func (m *Manager) topUpWarm() *Slot {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || m.cfg.WarmServers <= 0 || m.warmCount() >= m.cfg.WarmServers ||
		len(m.slots)+1+m.warmFreeSlots() > m.cfg.MaxServers {
		return nil
	}
	k := Key{Rounds: m.cfg.WarmRounds, Measures: warmMeasures(m.cfg.WarmRounds)}
	s, err := m.newSlotWith(slotSpec{key: k, warm: true, maxPlayers: m.cfg.MaxPlayers})
	if err != nil {
		if !errors.Is(err, ErrCapacity) && !errors.Is(err, ErrPaused) {
			m.log.Warn("warm public server not started", "err", err)
		}
		return nil
	}
	m.log.Info("warm public server starting", "slot", s.ID, "port", s.Port, "rounds", k.Rounds, "time_of_day", s.TimeOfDay)
	return s
}
