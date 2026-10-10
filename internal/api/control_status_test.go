package api

import (
	"context"
	"testing"

	"fuseronline/internal/matchmaking"
)

// The Status tab lists the front page's public lobbies plus the private ones, with each lobby's members by name and
// role; the front page still shows public lobbies only.
func TestControlStatus(t *testing.T) {
	srv, s := newTestServerAndAPI(t, nil)
	aAuth, a := login(t, srv, "epc", "status-a")
	bAuth, b := login(t, srv, "epc", "status-b")
	cAuth, c := login(t, srv, "epc", "status-c")
	for _, auth := range []string{aAuth, bAuth, cAuth} {
		call(t, srv, "GET", "players/ping/", auth, nil) // seen online
	}
	slot := "fuserd-public-test"
	ticket, _ := s.mm.CreateManagedPending(matchmaking.Player{ID: int64(a.UserID), Venue: "VenueB", TimeOfDay: "night", IsDJ: true}, slot,
		matchmaking.QueueFreestylePublic)
	if !s.mm.CompleteManaged(ticket.ID, slot, "198.51.100.30", 7777, 11) {
		t.Fatal("public placement failed")
	}
	_, pub, _ := s.mm.Ticket(ticket.ID, int64(a.UserID))
	if _, _, _, err := s.mm.Join(pub.ID, matchmaking.Player{ID: int64(b.UserID)}); err != nil {
		t.Fatal(err)
	}
	privateOnFDS(t, s, c.UserID)

	st, err := s.controlStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Online != 3 || st.InPublic != 2 || st.InPrivate != 1 || st.Elsewhere != 0 || st.Performers != 2 || st.Viewers != 1 || len(st.Lobbies) != 2 {
		t.Fatalf("status %+v", st)
	}
	byTitle := map[string]controlStatusLobby{}
	for _, l := range st.Lobbies {
		byTitle[l.Title] = l
	}
	p, ok := byTitle["Public lobby 1"]
	if !ok || p.Private || p.Venue != "Cherry Dip'd" || p.TimeOfDay != "Night" || p.Players != 2 || p.Performers != 1 || p.Viewers != 1 ||
		len(p.Members) != 2 || p.Members[0].ID != int64(a.UserID) || !p.Members[0].Creator || p.Members[0].Role != "performer" ||
		p.Members[1].Role != "viewer" || p.Members[1].Creator || p.Members[0].Name == "" || p.State != "Server not ready" {
		t.Fatalf("public lobby %+v", p)
	}
	q, ok := byTitle["Private lobby 1"]
	if !ok || !q.Private || len(q.Members) != 1 || q.Members[0].ID != int64(c.UserID) || !q.Members[0].Creator {
		t.Fatalf("private lobby %+v", q)
	}
	if fp := s.frontpageStatus(); len(fp.Lobbies) != 0 { // no public FDS server reporting: the front page lists none
		t.Fatalf("front page %+v", fp)
	}
	if _, sessions, _ := s.mm.SiteSnapshot(); len(sessions) != 1 || sessions[0].Private {
		t.Fatalf("front page sessions %+v", sessions)
	}
}
