// Package matchmaking keeps the multiplayer game sessions, tickets and player sessions behind the client's
// multiplayer/* endpoints. State is in memory only: a session lives as long as the player hosting it.
//
// FUSER's sessions ran on Harmonix's GameLift fleet. Here a player's own game hosts (a listen server),
// so a session is created by the player who asked for a new match, at that player's address, and
// everyone else is sent there. The client learns it is the host from its player session id (HostPrefix): its
// loader hosts instead of connecting to itself.
package matchmaking

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sort"
	"sync"
	"time"
)

// Queue strings, as the client sends them (EMatchmakingQueueType, FUN_140717e90).
const (
	QueueFreestylePrivate = "freestyle_private"
	QueueFreestylePublic  = "freestyle_public"
	QueueBattleCasual     = "battle_casual"
	QueueBattleRanked     = "battle_ranked"
	QueueMainStage        = "main_stage"
)

// Ticket statuses: the GameLift set the client parses (FUN_140708610). The client's ticket poll (FUN_14071d110)
// reports FAILED as FailedGetMatchmaking and TIMED_OUT as MatchmakingTimedOut ("Timed out waiting for
// matchmaking.").
const (
	StatusCompleted = "COMPLETED"
	StatusCancelled = "CANCELLED"
	StatusPlacing   = "PLACING"
	StatusFailed    = "FAILED"
	StatusTimedOut  = "TIMED_OUT"
)

// HostPrefix starts the player session id of a session's host. The host's loader checks for it.
const HostPrefix = "psess-host-"

var (
	ErrUnsupportedQueue = errors.New("queue not supported")
	ErrNoSession        = errors.New("no such game session")
	ErrNoTicket         = errors.New("no such ticket")
	ErrNotMember        = errors.New("inviter is not in the game session")
	ErrCapacity         = errors.New("game session is full")
	// ErrNoPublicSession: a viewer asked to watch a public show and none is running. The ticket is TIMED_OUT.
	ErrNoPublicSession = errors.New("no public session to watch")
)

// inviteLifetime bounds how long an unanswered invite is offered.
const inviteLifetime = time.Hour

// Player is what a ticket says about the player asking (FMatchmakingPlayer). Venue and TimeOfDay are echoed back
// in matchmaker_data, where the client resolves the creator's venue.
type Player struct {
	ID         int64
	Platform   string
	Venue      string
	TimeOfDay  string
	IsDJ       bool
	TurnLength []string
	NumRounds  []string
}

// Member is a player holding a player session in a game session.
type Member struct {
	Player
	PlayerSessionID string
	Joined          time.Time
}

// Session is one player-hosted game session. Values handed out are copies.
type Session struct {
	ID         string
	Queue      string
	Private    bool
	HostID     int64
	Addr       string // the host's IP address as this server saw it
	Port       int
	Created    time.Time
	Members    []Member // the host first
	Managed    bool
	SlotID     string
	MaxPlayers int
	Venue      string
	TimeOfDay  string
}

// Host returns the hosting member.
func (s Session) Host() Member { return s.Members[0] }

// Member returns the member with the given player id.
func (s Session) Member(playerID int64) (Member, bool) {
	for _, m := range s.Members {
		if m.ID == playerID {
			return m, true
		}
	}
	return Member{}, false
}

// Ticket is a matchmaking ticket. It completes when it is created.
type Ticket struct {
	ID              string
	PlayerID        int64
	Queue           string
	Status          string
	SessionID       string
	PlayerSessionID string
	Created         time.Time
	SlotID          string
}

// Options configures a Manager.
type Options struct {
	HostPort int
	// HostTimeout ends a session when its host has not been seen (Seen) for this long.
	HostTimeout time.Duration
	// ManagedPlayerSession issues the player session id of a player placed in a managed (container) session: for
	// FuserDedicatedServer, a signed session token the container checks at login.
	// Nil, or an empty result: a plain "psess-" id.
	ManagedPlayerSession func(s Session, playerID int64) string
}

// managedPlayerSession is the player session id for a player joining a managed session. Called with m.mu held.
func (m *Manager) managedPlayerSession(s *session, playerID int64) string {
	if m.opt.ManagedPlayerSession != nil {
		if id := m.opt.ManagedPlayerSession(s.Session, playerID); id != "" {
			return id
		}
	}
	return "psess-" + randomID()
}

// Invite is an offer to join a game session, delivered with the invitee's notifications.
type Invite struct {
	SessionID string
	InviterID int64
	TargetID  int64
	Created   time.Time
	Delivered bool // handed to the invitee at least once (for logging)
}

// Manager holds all matchmaking state. It is safe for concurrent use.
type Manager struct {
	opt Options
	now func() time.Time

	mu       sync.Mutex
	sessions map[string]*session
	tickets  map[string]*Ticket
	seen     map[int64]time.Time
	invites  map[int64][]*Invite // by invitee
	pending  map[string]Player
}

type session struct {
	Session
	members map[int64]*Member
}

func New(opt Options) *Manager {
	return &Manager{opt: opt, now: time.Now, sessions: map[string]*session{}, tickets: map[string]*Ticket{},
		seen: map[int64]time.Time{}, invites: map[int64][]*Invite{}, pending: map[string]Player{}}
}

// Seen records that a player is still online (the client pings every ~10 s while online).
func (m *Manager) Seen(playerID int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seen[playerID] = m.now()
}

// Ended is a session that expire removed, for logging.
type Ended struct {
	Session Session
	Reason  string
}

// expire drops sessions whose host has gone quiet and tickets older than an hour. Callers hold mu.
func (m *Manager) expire() []Ended {
	now := m.now()
	var ended []Ended
	for id, s := range m.sessions {
		if s.Managed {
			continue
		}
		last, ok := m.seen[s.HostID]
		if !ok || last.Before(s.Created) {
			last = s.Created
		}
		if m.opt.HostTimeout > 0 && now.Sub(last) > m.opt.HostTimeout {
			ended = append(ended, Ended{m.snapshot(s), "host timed out"})
			delete(m.sessions, id)
		}
	}
	for id, t := range m.tickets {
		if now.Sub(t.Created) > time.Hour {
			delete(m.tickets, id)
			delete(m.pending, id)
		}
	}
	for target, list := range m.invites {
		kept := list[:0]
		for _, inv := range list {
			if _, ok := m.sessions[inv.SessionID]; ok && now.Sub(inv.Created) <= inviteLifetime {
				kept = append(kept, inv)
			}
		}
		if len(kept) == 0 {
			delete(m.invites, target)
		} else {
			m.invites[target] = kept
		}
	}
	return ended
}

// Expire runs the expiry pass on its own and reports what ended.
func (m *Manager) Expire() []Ended {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.expire()
}

func (m *Manager) snapshot(s *session) Session {
	out := s.Session
	out.Members = make([]Member, 0, len(s.members))
	out.Members = append(out.Members, *s.members[s.HostID])
	var rest []Member
	for id, mem := range s.members {
		if id != s.HostID {
			rest = append(rest, *mem)
		}
	}
	sort.Slice(rest, func(i, j int) bool { return rest[i].Joined.Before(rest[j].Joined) })
	out.Members = append(out.Members, rest...)
	return out
}

// leave removes the player from every session it is in. A session it hosts ends. Callers hold mu.
func (m *Manager) leave(playerID int64) []Ended {
	var ended []Ended
	for id, s := range m.sessions {
		if s.Managed {
			if _, ok := s.members[playerID]; !ok {
				continue
			}
			before := m.snapshot(s)
			delete(s.members, playerID)
			if len(s.members) == 0 {
				ended = append(ended, Ended{before, "last player left"})
				delete(m.sessions, id)
			} else if s.HostID == playerID {
				for next := range s.members {
					s.HostID = next
					break
				}
			}
			continue
		}
		if s.HostID == playerID {
			ended = append(ended, Ended{m.snapshot(s), "host left"})
			delete(m.sessions, id)
		} else {
			delete(s.members, playerID)
		}
	}
	return ended
}

// CreateManagedPending records a ticket while its Docker server starts: public, or (an FDS container of its own)
// private.
func (m *Manager) CreateManagedPending(p Player, slotID, queue string) (Ticket, []Ended) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ended := m.expire()
	ended = append(ended, m.leave(p.ID)...)
	if queue != QueueFreestylePrivate {
		queue = QueueFreestylePublic
	}
	t := &Ticket{ID: "ticket-" + randomID(), PlayerID: p.ID, Queue: queue,
		Status: StatusPlacing, SlotID: slotID, Created: m.now()}
	m.tickets[t.ID] = t
	m.pending[t.ID] = p
	return *t, ended
}

// CompleteManaged places a pending player after the server has registered.
// It is safe if a client cancelled the ticket while Docker was starting.
func (m *Manager) CompleteManaged(ticketID, slotID, addr string, port, maxPlayers int) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	t := m.tickets[ticketID]
	p, ok := m.pending[ticketID]
	if t == nil || !ok || t.Status != StatusPlacing || t.SlotID != slotID {
		return false
	}
	var s *session
	for _, candidate := range m.sessions {
		if candidate.Managed && candidate.SlotID == slotID {
			s = candidate
			break
		}
	}
	if s == nil {
		s = &session{Session: Session{ID: "gsess-" + randomID(), Queue: t.Queue, Private: t.Queue == QueueFreestylePrivate,
			HostID: p.ID, Addr: addr, Port: port, Created: m.now(), Managed: true,
			SlotID: slotID, MaxPlayers: maxPlayers, Venue: p.Venue, TimeOfDay: p.TimeOfDay}, members: map[int64]*Member{}}
		m.sessions[s.ID] = s
	}
	if len(s.members) >= s.MaxPlayers {
		t.Status = StatusFailed
		delete(m.pending, ticketID)
		return false
	}
	mem := &Member{Player: p, PlayerSessionID: m.managedPlayerSession(s, p.ID), Joined: m.now()}
	s.members[p.ID] = mem
	t.Status, t.SessionID, t.PlayerSessionID = StatusCompleted, s.ID, mem.PlayerSessionID
	delete(m.pending, ticketID)
	return true
}

// FailManaged ends pending tickets and the browser entry for a stopped server.
func (m *Manager) FailManaged(slotID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, t := range m.tickets {
		if t.SlotID == slotID && t.Status == StatusPlacing {
			t.Status = StatusFailed
			delete(m.pending, id)
		}
	}
	for id, s := range m.sessions {
		if s.Managed && s.SlotID == slotID {
			delete(m.sessions, id)
		}
	}
}

// CreateUnmatchedTicket records a ticket that no session can be found for, as TIMED_OUT, so the client stops
// matchmaking with an error instead of waiting. Like any new ticket it takes the player out of other sessions.
func (m *Manager) CreateUnmatchedTicket(queue string, p Player) (Ticket, []Ended) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ended := m.expire()
	ended = append(ended, m.leave(p.ID)...)
	return m.unmatched(queue, p.ID), ended
}

// unmatched stores a TIMED_OUT ticket. Callers hold mu.
func (m *Manager) unmatched(queue string, playerID int64) Ticket {
	t := &Ticket{ID: "ticket-" + randomID(), PlayerID: playerID, Queue: queue, Status: StatusTimedOut, Created: m.now()}
	m.tickets[t.ID] = t
	return *t
}

// CreateTicket is multiplayer/tickets/ for a public ticket with no managed servers: the player either creates a
// session it will host (when createNew is set or a DJ finds no public session) or is placed in the newest public
// session. A viewer ("Watch a Public Show": public, not a DJ, not creating) never hosts: with no public session it
// gets a TIMED_OUT ticket and ErrNoPublicSession. addr is the player's address, which becomes the session address
// if it hosts. Private sessions are never player-hosted: they run on their own FDS container (CreateManagedPending
// with QueueFreestylePrivate), so a private queue here is ErrUnsupportedQueue.
func (m *Manager) CreateTicket(queue string, p Player, createNew bool, addr string) (Ticket, Session, []Ended,
	error) {
	return m.CreateTicketAvoiding(queue, p, createNew, addr, nil)
}

// CreateTicketAvoiding is CreateTicket that never places the player in a session where avoid reports one of its
// occupants (Occupants) true: the players it blocked or was blocked by. With no such session left it hosts (a DJ)
// or gets ErrNoPublicSession (a viewer), as when there is none at all.
func (m *Manager) CreateTicketAvoiding(queue string, p Player, createNew bool, addr string, avoid func(int64) bool) (Ticket,
	Session, []Ended, error) {
	if queue != QueueFreestylePublic {
		return Ticket{}, Session{}, nil, ErrUnsupportedQueue
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	ended := m.expire()
	ended = append(ended, m.leave(p.ID)...)
	now := m.now()

	var s *session
	var member *Member
	if !createNew {
		s = m.newestPublic(avoid)
		if s == nil && !p.IsDJ {
			return m.unmatched(queue, p.ID), Session{}, ended, ErrNoPublicSession
		}
	}
	if s == nil {
		s = &session{Session: Session{ID: "gsess-" + randomID(), Queue: queue, HostID: p.ID, Addr: addr,
			Port: m.opt.HostPort, Created: now}, members: map[int64]*Member{}}
		member = &Member{Player: p, PlayerSessionID: HostPrefix + randomID(), Joined: now}
		m.sessions[s.ID] = s
		m.seen[p.ID] = now
	} else {
		member = &Member{Player: p, PlayerSessionID: "psess-" + randomID(), Joined: now}
	}
	s.members[p.ID] = member

	t := &Ticket{ID: "ticket-" + randomID(), PlayerID: p.ID, Queue: queue, Status: StatusCompleted,
		SessionID: s.ID, PlayerSessionID: member.PlayerSessionID, Created: now}
	m.tickets[t.ID] = t
	return *t, m.snapshot(s), ended, nil
}

func (m *Manager) newestPublic(avoid func(int64) bool) *session {
	var best *session
	for _, s := range m.sessions {
		if !s.Private && (best == nil || s.Created.After(best.Created)) && !m.anyOccupant(s, avoid) {
			best = s
		}
	}
	return best
}

// Ticket is multiplayer/tickets/get/. Only the ticket's own player may read it. The session is the zero value
// when it has ended since.
func (m *Manager) Ticket(id string, playerID int64) (Ticket, Session, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.expire()
	t, ok := m.tickets[id]
	if !ok || t.PlayerID != playerID {
		return Ticket{}, Session{}, false
	}
	if s, ok := m.sessions[t.SessionID]; ok {
		return *t, m.snapshot(s), true
	}
	return *t, Session{}, true
}

// CancelTicket is multiplayer/tickets/cancel/: the player gives up on the match, leaving its session (ending it
// if it was the host).
func (m *Manager) CancelTicket(id string, playerID int64) ([]Ended, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tickets[id]
	if !ok || t.PlayerID != playerID {
		return nil, ErrNoTicket
	}
	t.Status = StatusCancelled
	delete(m.pending, id)
	return m.leave(playerID), nil
}

// Session is multiplayer/game_sessions/get/.
func (m *Manager) Session(id string) (Session, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.expire()
	s, ok := m.sessions[id]
	if !ok {
		return Session{}, false
	}
	return m.snapshot(s), true
}

// Join is multiplayer/player_sessions/: a player joining a session picked from the session browser. Joining a
// session the player hosts gives back its host player session.
func (m *Manager) Join(sessionID string, p Player) (Member, Session, []Ended, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ended := m.expire()
	s, ok := m.sessions[sessionID]
	if !ok {
		return Member{}, Session{}, ended, ErrNoSession
	}
	if s.HostID == p.ID {
		return *s.members[p.ID], m.snapshot(s), ended, nil
	}
	if s.Managed && len(s.members) >= s.MaxPlayers {
		return Member{}, Session{}, ended, ErrCapacity
	}
	ended = append(ended, m.leave(p.ID)...)
	psid := "psess-" + randomID()
	if s.Managed {
		psid = m.managedPlayerSession(s, p.ID)
	}
	mem := &Member{Player: p, PlayerSessionID: psid, Joined: m.now()}
	s.members[p.ID] = mem
	m.dropInvite(sessionID, p.ID)
	return *mem, m.snapshot(s), ended, nil
}

// List is multiplayer/game_sessions/get_list/: every live session, newest first. Which ones a viewer may see
// depends on the social graph, so the caller decides. The client checks each listed session with a beacon query
// to the host before showing it.
// ManagedSession is the session placed in a container slot, if any player is in it yet.
func (m *Manager) ManagedSession(slotID string) (Session, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range m.sessions {
		if s.Managed && s.SlotID == slotID {
			return m.snapshot(s), true
		}
	}
	return Session{}, false
}

func (m *Manager) List() ([]Session, []Ended) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ended := m.expire()
	var out []Session
	for _, s := range m.sessions {
		out = append(out, m.snapshot(s))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.After(out[j].Created) })
	return out, ended
}

// Invite is multiplayer/game_sessions/invite/: a member of a session invites another player. Inviting the same
// player to the same session again refreshes the invite.
func (m *Manager) Invite(sessionID string, inviter, target int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.expire()
	s, ok := m.sessions[sessionID]
	if !ok {
		return ErrNoSession
	}
	if _, ok := s.members[inviter]; !ok {
		return ErrNotMember
	}
	m.dropInvite(sessionID, target)
	m.invites[target] = append(m.invites[target], &Invite{SessionID: sessionID, InviterID: inviter, TargetID: target,
		Created: m.now()})
	return nil
}

// CancelInvite is multiplayer/game_sessions/cancel_invite/.
func (m *Manager) CancelInvite(sessionID string, target int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.dropInvite(sessionID, target)
}

func (m *Manager) dropInvite(sessionID string, target int64) {
	list := m.invites[target]
	kept := list[:0]
	for _, inv := range list {
		if inv.SessionID != sessionID {
			kept = append(kept, inv)
		}
	}
	if len(kept) == 0 {
		delete(m.invites, target)
	} else {
		m.invites[target] = kept
	}
}

// Invites returns the open invites of a player, oldest first, and marks them delivered. fresh lists the ones not
// delivered before.
func (m *Manager) Invites(target int64) (all, fresh []Invite) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.expire()
	for _, inv := range m.invites[target] {
		if !inv.Delivered {
			fresh = append(fresh, *inv)
			inv.Delivered = true
		}
		all = append(all, *inv)
	}
	return all, fresh
}

// presenceWindow is how recently a member must have been seen (or joined) to count as an occupant. A managed
// session learns of no departures (its server reports counts only), so a member who left it lingers until its
// next ticket; one who went offline stops counting after this.
const presenceWindow = 2 * time.Minute

// occupants is everyone in a session for block checks: its members still around, and, for a managed session, the
// players whose tickets are being placed into its slot. Callers hold mu.
func (m *Manager) occupants(s *session) []int64 {
	cutoff := m.now().Add(-presenceWindow)
	var out []int64
	for id, mem := range s.members {
		if seen, ok := m.seen[id]; mem.Joined.After(cutoff) || (ok && seen.After(cutoff)) {
			out = append(out, id)
		}
	}
	if s.Managed {
		out = append(out, m.slotPending(s.SlotID)...)
	}
	return out
}

// slotPending is the players whose tickets are being placed into a slot. Callers hold mu.
func (m *Manager) slotPending(slotID string) []int64 {
	var out []int64
	for id, t := range m.tickets {
		if t.SlotID == slotID && t.Status == StatusPlacing {
			if p, ok := m.pending[id]; ok {
				out = append(out, p.ID)
			}
		}
	}
	return out
}

// anyOccupant reports whether avoid is true for one of the session's occupants (false for a nil avoid).
// Callers hold mu.
func (m *Manager) anyOccupant(s *session, avoid func(int64) bool) bool {
	if avoid == nil {
		return false
	}
	for _, id := range m.occupants(s) {
		if avoid(id) {
			return true
		}
	}
	return false
}

// Occupants is everyone in a session for block checks (see occupants), or nil for an unknown session.
func (m *Manager) Occupants(sessionID string) []int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.sessions[sessionID]; ok {
		return m.occupants(s)
	}
	return nil
}

// SlotOccupants is everyone in a managed slot for block checks: its session's occupants, or, before the session
// exists, the players being placed into it.
func (m *Manager) SlotOccupants(slotID string) []int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range m.sessions {
		if s.Managed && s.SlotID == slotID {
			return m.occupants(s)
		}
	}
	return m.slotPending(slotID)
}

// DropInvitesFrom withdraws every open invite inviter sent to target (target blocked inviter).
func (m *Manager) DropInvitesFrom(inviter, target int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	list := m.invites[target]
	kept := list[:0]
	for _, inv := range list {
		if inv.InviterID != inviter {
			kept = append(kept, inv)
		}
	}
	if len(kept) == 0 {
		delete(m.invites, target)
	} else {
		m.invites[target] = kept
	}
}

// Invited reports whether the player holds an open invite to the session.
func (m *Manager) Invited(target int64, sessionID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, inv := range m.invites[target] {
		if inv.SessionID == sessionID {
			return true
		}
	}
	return false
}

func randomID() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// SiteSnapshot returns managed public sessions and recently pinging players under one lock.
// Members without a recent heartbeat are omitted from the website's role counts.
func (m *Manager) SiteSnapshot() (int, []Session, []Ended) { return m.managedSnapshot(false) }

// StatusSnapshot is SiteSnapshot with the managed private sessions too (the control panel's Status tab).
func (m *Manager) StatusSnapshot() (int, []Session, []Ended) { return m.managedSnapshot(true) }

func (m *Manager) managedSnapshot(withPrivate bool) (int, []Session, []Ended) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ended := m.expire()
	timeout := m.opt.HostTimeout
	if timeout <= 0 {
		timeout = time.Minute
	}
	cutoff := m.now().Add(-timeout)
	online := 0
	for _, seen := range m.seen {
		if !seen.Before(cutoff) {
			online++
		}
	}
	var sessions []Session
	for _, raw := range m.sessions {
		if raw.Private && !withPrivate || !raw.Managed {
			continue
		}
		s := m.snapshot(raw)
		members := make([]Member, 0, len(s.Members))
		for _, member := range s.Members {
			if seen, ok := m.seen[member.ID]; ok && !seen.Before(cutoff) {
				members = append(members, member)
			}
		}
		s.Members = members
		sessions = append(sessions, s)
	}
	sort.Slice(sessions, func(i, j int) bool {
		if sessions[i].Created.Equal(sessions[j].Created) {
			return sessions[i].ID < sessions[j].ID
		}
		return sessions[i].Created.After(sessions[j].Created)
	})
	return online, sessions, ended
}
