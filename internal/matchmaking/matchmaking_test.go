package matchmaking

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newTest(opt Options) (*Manager, *clock) {
	c := &clock{time.Unix(1_800_000_000, 0)}
	m := New(opt)
	m.now = c.now
	return m, c
}

var opts = Options{HostPort: 7777, HostTimeout: time.Minute}

// privateManaged makes a private session the only way there is: an FDS container of its own (slotID).
func privateManaged(t *testing.T, m *Manager, playerID int64, slotID string) Session {
	t.Helper()
	ticket, _ := m.CreateManagedPending(Player{ID: playerID, IsDJ: true}, slotID, QueueFreestylePrivate)
	if !m.CompleteManaged(ticket.ID, slotID, "198.51.100.20", 8100, 4) {
		t.Fatal("private placement failed")
	}
	_, s, _ := m.Ticket(ticket.ID, playerID)
	if !s.Private || !s.Managed {
		t.Fatalf("private session %+v", s)
	}
	return s
}

func TestPublicCreateHostsNewSession(t *testing.T) {
	m, _ := newTest(opts)
	tk, s, _, err := m.CreateTicket(QueueFreestylePublic, Player{ID: 1, Venue: "VenueA"}, true, "203.0.113.5")
	if err != nil {
		t.Fatal(err)
	}
	if tk.Status != StatusCompleted || tk.SessionID != s.ID {
		t.Fatalf("ticket %+v", tk)
	}
	if !strings.HasPrefix(tk.PlayerSessionID, HostPrefix) {
		t.Fatalf("creator's player session %q lacks the host prefix", tk.PlayerSessionID)
	}
	if s.HostID != 1 || s.Addr != "203.0.113.5" || s.Port != 7777 || s.Private {
		t.Fatalf("session %+v", s)
	}
}

// Private sessions run only on FDS containers: a player never hosts one.
func TestPrivateQueueIsNeverPlayerHosted(t *testing.T) {
	m, _ := newTest(opts)
	if _, _, _, err := m.CreateTicket(QueueFreestylePrivate, Player{ID: 1}, true, "203.0.113.5"); !errors.Is(err, ErrUnsupportedQueue) {
		t.Fatalf("private CreateTicket: %v", err)
	}
	if list, _ := m.List(); len(list) != 0 {
		t.Fatalf("a player-hosted private session was created: %+v", list)
	}
}

func TestPublicQuickplayJoinsNewestPublic(t *testing.T) {
	m, c := newTest(opts)
	_, older, _, _ := m.CreateTicket(QueueFreestylePublic, Player{ID: 1}, true, "198.51.100.1")
	c.advance(time.Second)
	_, newer, _, _ := m.CreateTicket(QueueFreestylePublic, Player{ID: 2}, true, "198.51.100.2")
	privateManaged(t, m, 3, "slot-p") // never matched into

	tk, s, _, err := m.CreateTicket(QueueFreestylePublic, Player{ID: 4}, false, "198.51.100.4")
	if err != nil {
		t.Fatal(err)
	}
	if s.ID != newer.ID || s.ID == older.ID {
		t.Fatalf("joined %s, want the newest public session %s", s.ID, newer.ID)
	}
	if strings.HasPrefix(tk.PlayerSessionID, HostPrefix) || s.Addr != "198.51.100.2" {
		t.Fatalf("joiner got host role or wrong address: %+v %+v", tk, s)
	}
	if _, ok := s.Member(4); !ok || s.Host().ID != 2 {
		t.Fatalf("members %+v", s.Members)
	}
}

func TestQuickplayWithNoSessionHosts(t *testing.T) {
	m, _ := newTest(opts)
	tk, s, _, _ := m.CreateTicket(QueueFreestylePublic, Player{ID: 1, IsDJ: true}, false, "198.51.100.1")
	if s.HostID != 1 || !strings.HasPrefix(tk.PlayerSessionID, HostPrefix) {
		t.Fatalf("with no public session the DJ should host: %+v", s)
	}
}

// "Watch a Public Show" with nothing running: no session is created and the ticket reads back TIMED_OUT.
func TestViewerWithNoPublicSessionTimesOut(t *testing.T) {
	m, _ := newTest(opts)
	privateManaged(t, m, 3, "slot-p") // private: not watchable
	tk, s, _, err := m.CreateTicket(QueueFreestylePublic, Player{ID: 1}, false, "198.51.100.1")
	if !errors.Is(err, ErrNoPublicSession) || tk.Status != StatusTimedOut || s.ID != "" || tk.SessionID != "" {
		t.Fatalf("viewer ticket %+v session %+v err %v", tk, s, err)
	}
	if got, sess, ok := m.Ticket(tk.ID, 1); !ok || got.Status != StatusTimedOut || sess.ID != "" {
		t.Fatalf("polled viewer ticket %+v %+v %v", got, sess, ok)
	}
	if list, _ := m.List(); len(list) != 1 || !list[0].Private {
		t.Fatalf("viewer created a session: %+v", list)
	}
	// Once a DJ hosts a public show, the viewer joins it.
	_, show, _, _ := m.CreateTicket(QueueFreestylePublic, Player{ID: 2, IsDJ: true}, true, "198.51.100.2")
	if _, joined, _, err := m.CreateTicket(QueueFreestylePublic, Player{ID: 1}, false, "198.51.100.1"); err != nil || joined.ID != show.ID {
		t.Fatalf("viewer join: %+v %v", joined, err)
	}
	unmatched, _ := m.CreateUnmatchedTicket(QueueFreestylePublic, Player{ID: 1})
	if unmatched.Status != StatusTimedOut {
		t.Fatalf("unmatched ticket %+v", unmatched)
	}
	if sess, _ := m.Session(show.ID); len(sess.Members) != 1 {
		t.Fatalf("a new ticket should take the viewer out of the show: %+v", sess.Members)
	}
}

func TestManagedPublicPlacementAndFirstPlayerDeparture(t *testing.T) {
	m, _ := newTest(opts)
	a, _ := m.CreateManagedPending(Player{ID: 1, Venue: "VenueA", TimeOfDay: "night", IsDJ: true}, "slot-a", QueueFreestylePublic)
	if got, sess, ok := m.Ticket(a.ID, 1); !ok || got.Status != StatusPlacing || sess.ID != "" {
		t.Fatalf("pending ticket: %+v %+v %v", got, sess, ok)
	}
	if !m.CompleteManaged(a.ID, "slot-a", "198.51.100.10", 8000, 2) {
		t.Fatal("first placement failed")
	}
	b, _ := m.CreateManagedPending(Player{ID: 2}, "slot-a", QueueFreestylePublic)
	if !m.CompleteManaged(b.ID, "slot-a", "198.51.100.10", 8000, 2) {
		t.Fatal("second placement failed")
	}
	first, sess, _ := m.Ticket(a.ID, 1)
	second, _, _ := m.Ticket(b.ID, 2)
	if first.SessionID != second.SessionID || strings.HasPrefix(first.PlayerSessionID, HostPrefix) ||
		strings.HasPrefix(second.PlayerSessionID, HostPrefix) || !sess.Managed || sess.Port != 8000 {
		t.Fatalf("managed tickets %+v %+v session %+v", first, second, sess)
	}
	if _, _, _, err := m.Join(sess.ID, Player{ID: 3}); !errors.Is(err, ErrCapacity) {
		t.Fatalf("full join: %v", err)
	}
	m.leave(1) // the first human does not own the container
	remaining, ok := m.Session(sess.ID)
	if !ok || remaining.HostID != 2 || remaining.Venue != "VenueA" || remaining.TimeOfDay != "night" {
		t.Fatalf("first player leaving ended/changed session: %+v %v", remaining, ok)
	}
	m.FailManaged("slot-a")
	if _, ok := m.Session(sess.ID); ok {
		t.Fatal("stopped server remained in browser")
	}
}

func TestManagedCancellationDuringStartup(t *testing.T) {
	m, _ := newTest(opts)
	ticket, _ := m.CreateManagedPending(Player{ID: 1}, "slot-a", QueueFreestylePublic)
	if _, err := m.CancelTicket(ticket.ID, 1); err != nil {
		t.Fatal(err)
	}
	if m.CompleteManaged(ticket.ID, "slot-a", "198.51.100.10", 8000, 11) {
		t.Fatal("cancelled ticket completed")
	}
}

func TestUnsupportedQueue(t *testing.T) {
	m, _ := newTest(opts)
	for _, q := range []string{QueueBattleCasual, QueueBattleRanked, QueueMainStage, QueueFreestylePrivate, ""} {
		if _, _, _, err := m.CreateTicket(q, Player{ID: 1}, true, "x"); !errors.Is(err, ErrUnsupportedQueue) {
			t.Fatalf("%q: %v", q, err)
		}
	}
}

func TestJoinAndList(t *testing.T) {
	m, c := newTest(opts)
	priv := privateManaged(t, m, 1, "slot-p")
	c.advance(time.Second)
	_, pub, _, _ := m.CreateTicket(QueueFreestylePublic, Player{ID: 2}, true, "b")

	if list, _ := m.List(); len(list) != 2 || list[0].ID != pub.ID {
		t.Fatalf("List returns every session, newest first: %+v", list)
	}

	mem, s, _, err := m.Join(priv.ID, Player{ID: 9})
	if err != nil || strings.HasPrefix(mem.PlayerSessionID, HostPrefix) || len(s.Members) != 2 {
		t.Fatalf("join: %v %+v %+v", err, mem, s)
	}
	// Joining another session leaves the first.
	m.Join(pub.ID, Player{ID: 9})
	if s, _ := m.Session(priv.ID); len(s.Members) != 1 {
		t.Fatalf("player still in the old session: %+v", s.Members)
	}
	if _, _, _, err := m.Join("gsess-nope", Player{ID: 9}); !errors.Is(err, ErrNoSession) {
		t.Fatal(err)
	}
	// The host "joining" its own session keeps the host player session.
	mem, _, _, _ = m.Join(pub.ID, Player{ID: 2})
	if !strings.HasPrefix(mem.PlayerSessionID, HostPrefix) {
		t.Fatalf("host rejoin lost the host prefix: %q", mem.PlayerSessionID)
	}
}

func TestInvites(t *testing.T) {
	m, c := newTest(opts)
	_, s, _, _ := m.CreateTicket(QueueFreestylePublic, Player{ID: 1}, true, "a")
	if err := m.Invite(s.ID, 5, 2); !errors.Is(err, ErrNotMember) {
		t.Fatalf("a non-member invited: %v", err)
	}
	if err := m.Invite("gsess-nope", 1, 2); !errors.Is(err, ErrNoSession) {
		t.Fatal(err)
	}
	if err := m.Invite(s.ID, 1, 2); err != nil {
		t.Fatal(err)
	}
	m.Invite(s.ID, 1, 2) // a refresh, not a duplicate
	all, fresh := m.Invites(2)
	if len(all) != 1 || len(fresh) != 1 || all[0].InviterID != 1 || !m.Invited(2, s.ID) {
		t.Fatalf("invites %+v fresh %+v", all, fresh)
	}
	if _, fresh := m.Invites(2); len(fresh) != 0 {
		t.Fatal("an invite was reported fresh twice")
	}
	// Joining consumes the invite.
	m.Join(s.ID, Player{ID: 2})
	if all, _ := m.Invites(2); len(all) != 0 {
		t.Fatalf("invite survived the join: %+v", all)
	}
	// Cancel, and expiry with the session.
	m.Invite(s.ID, 1, 3)
	m.CancelInvite(s.ID, 3)
	if m.Invited(3, s.ID) {
		t.Fatal("cancelled invite still open")
	}
	m.Invite(s.ID, 1, 4)
	c.advance(2 * time.Minute) // the host stopped pinging
	if all, _ := m.Invites(4); len(all) != 0 {
		t.Fatalf("invite outlived its session: %+v", all)
	}
}

func TestHostTimeoutAndPings(t *testing.T) {
	m, c := newTest(opts)
	_, s, _, _ := m.CreateTicket(QueueFreestylePublic, Player{ID: 1}, true, "a")
	c.advance(50 * time.Second)
	m.Seen(1)
	c.advance(50 * time.Second)
	if _, ok := m.Session(s.ID); !ok {
		t.Fatal("session ended although the host pinged")
	}
	c.advance(11 * time.Second)
	ended := m.Expire()
	if len(ended) != 1 || ended[0].Session.ID != s.ID {
		t.Fatalf("expected the session to time out, got %+v", ended)
	}
	if _, ok := m.Session(s.ID); ok {
		t.Fatal("session survived its host's timeout")
	}
}

func TestNewTicketEndsHostedSessionAndCancel(t *testing.T) {
	m, _ := newTest(opts)
	tk1, s1, _, _ := m.CreateTicket(QueueFreestylePublic, Player{ID: 1}, true, "a")
	_, s2, ended, _ := m.CreateTicket(QueueFreestylePublic, Player{ID: 1}, true, "a")
	if len(ended) != 1 || ended[0].Session.ID != s1.ID || s2.ID == s1.ID {
		t.Fatalf("a host's new session should end its previous one: %+v", ended)
	}
	if _, sess, ok := m.Ticket(tk1.ID, 1); !ok || sess.ID != "" {
		t.Fatalf("old ticket should report its session gone: %v %+v", ok, sess)
	}
	if _, _, ok := m.Ticket(tk1.ID, 2); ok {
		t.Fatal("another player read the ticket")
	}

	tk3, _, _, _ := m.CreateTicket(QueueFreestylePublic, Player{ID: 3}, true, "c")
	if _, err := m.CancelTicket(tk3.ID, 1); !errors.Is(err, ErrNoTicket) {
		t.Fatal("cancelled someone else's ticket")
	}
	ended, err := m.CancelTicket(tk3.ID, 3)
	if err != nil || len(ended) != 1 {
		t.Fatalf("cancel: %v %+v", err, ended)
	}
}

func TestSiteSnapshotOnlyManagedPublicAndRecentPlayers(t *testing.T) {
	m, c := newTest(opts)
	m.CreateTicket(QueueFreestylePublic, Player{ID: 1, IsDJ: true}, true, "private-address")
	privateManaged(t, m, 2, "slot-private") // managed but private: not on the site
	for _, player := range []Player{{ID: 3, IsDJ: true}, {ID: 4}} {
		ticket, _ := m.CreateManagedPending(player, "slot", QueueFreestylePublic)
		if !m.CompleteManaged(ticket.ID, "slot", "private-address", 8000, 2) {
			t.Fatal("placement failed")
		}
		m.Seen(player.ID)
	}
	m.Seen(1)
	m.Seen(2)
	online, sessions, _ := m.SiteSnapshot()
	if online != 4 || len(sessions) != 1 || len(sessions[0].Members) != 2 {
		t.Fatalf("snapshot: %d %+v", online, sessions)
	}
	sessions[0].Members[0].IsDJ = false
	c.advance(time.Minute + time.Second)
	m.Seen(4)
	online, sessions, _ = m.SiteSnapshot()
	if online != 1 || len(sessions) != 1 || len(sessions[0].Members) != 1 || sessions[0].Members[0].ID != 4 {
		t.Fatalf("stale members: %d %+v", online, sessions)
	}
}

// Managed sessions take their player session ids from Options.ManagedPlayerSession (FDS session tokens), both for
// tickets completing on the slot and for browser / invite joins; a private managed ticket makes a private session.
func TestManagedPlayerSessionAndPrivateQueue(t *testing.T) {
	var issued []string
	m := New(Options{HostPort: 7777, ManagedPlayerSession: func(s Session, playerID int64) string {
		id := fmt.Sprintf("psess-fds1.%s.%s.%d", s.SlotID, s.ID, playerID)
		issued = append(issued, id)
		return id
	}})
	ticket, _ := m.CreateManagedPending(Player{ID: 1, Venue: "VenueB"}, "slot-p", QueueFreestylePrivate)
	if ticket.Queue != QueueFreestylePrivate {
		t.Fatalf("ticket queue %q", ticket.Queue)
	}
	if !m.CompleteManaged(ticket.ID, "slot-p", "198.51.100.10", 7777, 4) {
		t.Fatal("complete")
	}
	got, sess, ok := m.Ticket(ticket.ID, 1)
	if !ok || !sess.Private || sess.Queue != QueueFreestylePrivate || got.PlayerSessionID != issued[0] {
		t.Fatalf("ticket %+v session %+v issued %v", got, sess, issued)
	}
	mem, _, _, err := m.Join(sess.ID, Player{ID: 2})
	if err != nil || mem.PlayerSessionID != issued[1] || !strings.Contains(mem.PlayerSessionID, ".slot-p.") {
		t.Fatalf("invite join: %+v %v (issued %v)", mem, err, issued)
	}
}
