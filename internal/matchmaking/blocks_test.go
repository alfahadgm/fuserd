package matchmaking

import (
	"errors"
	"slices"
	"testing"
	"time"
)

// A ticket avoiding some players never lands in a session holding one of them: a DJ hosts instead, a viewer finds
// no show. Members gone quiet for longer than presenceWindow no longer count, nor do they keep anyone out.
func TestCreateTicketAvoiding(t *testing.T) {
	m, c := newTest(Options{HostPort: 7777, HostTimeout: time.Hour})
	_, show, _, _ := m.CreateTicket(QueueFreestylePublic, Player{ID: 1, IsDJ: true}, true, "a")
	avoid1 := func(id int64) bool { return id == 1 }

	_, other, _, err := m.CreateTicketAvoiding(QueueFreestylePublic, Player{ID: 2, IsDJ: true}, false, "b", avoid1)
	if err != nil || other.ID == show.ID || other.HostID != 2 {
		t.Fatalf("a DJ avoiding the only show's host should host its own: %+v %v", other, err)
	}
	avoid12 := func(id int64) bool { return id == 1 || id == 2 }
	if _, _, _, err := m.CreateTicketAvoiding(QueueFreestylePublic, Player{ID: 3}, false, "c", avoid12); !errors.Is(err, ErrNoPublicSession) {
		t.Fatalf("a viewer avoiding every show: %v", err)
	}
	if _, joined, _, err := m.CreateTicketAvoiding(QueueFreestylePublic, Player{ID: 4}, false, "d", avoid1); err != nil || joined.ID != other.ID {
		t.Fatalf("a viewer avoiding 1 should join 2's show: %+v %v", joined, err)
	}
	if occ := m.Occupants(other.ID); !slices.Contains(occ, 2) || !slices.Contains(occ, 4) {
		t.Fatalf("occupants %v", occ)
	}

	// Player 1 goes quiet: after presenceWindow it is no occupant of its own show.
	c.t = c.t.Add(presenceWindow + time.Second)
	m.Seen(2)
	if occ := m.Occupants(show.ID); slices.Contains(occ, 1) {
		t.Fatalf("a quiet member still counts: %v", occ)
	}
	if occ := m.Occupants(other.ID); !slices.Contains(occ, 2) || slices.Contains(occ, 4) {
		t.Fatalf("occupants after the window %v", occ)
	}
}

// A managed slot's occupants include the players still being placed into it.
func TestSlotOccupantsIncludePending(t *testing.T) {
	m, _ := newTest(opts)
	m.CreateManagedPending(Player{ID: 7, IsDJ: true}, "slot-1", QueueFreestylePublic)
	if occ := m.SlotOccupants("slot-1"); !slices.Equal(occ, []int64{7}) {
		t.Fatalf("pending occupants %v", occ)
	}
}

// Blocking withdraws the blocked player's open invites to the blocker, and only those.
func TestDropInvitesFrom(t *testing.T) {
	m, _ := newTest(Options{HostPort: 7777, HostTimeout: time.Hour})
	_, s1, _, _ := m.CreateTicket(QueueFreestylePublic, Player{ID: 1, IsDJ: true}, true, "a")
	_, s2, _, _ := m.CreateTicket(QueueFreestylePublic, Player{ID: 2, IsDJ: true}, true, "b")
	if err := m.Invite(s1.ID, 1, 9); err != nil {
		t.Fatal(err)
	}
	if err := m.Invite(s2.ID, 2, 9); err != nil {
		t.Fatal(err)
	}
	m.DropInvitesFrom(1, 9)
	if m.Invited(9, s1.ID) || !m.Invited(9, s2.ID) {
		t.Fatal("wrong invites dropped")
	}
}
