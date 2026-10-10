package api

import (
	"net/http"
	"testing"

	"fuseronline/internal/config"
)

func TestBlocks(t *testing.T) {
	srv := newTestServerWith(t, func(c *config.Config) { c.Multiplayer.LoopbackHostAddress = "198.51.100.7" })
	aAuth, a := login(t, srv, "epc", "block-a")
	bAuth, b := login(t, srv, "epc", "block-b")
	cAuth, c := login(t, srv, "epc", "block-c")

	// Block list endpoints.
	var blocking struct {
		Blocking int32 `json:"blocking"`
	}
	for i := 0; i < 2; i++ { // blocking twice succeeds both times
		status, env := call(t, srv, "POST", "players/block/", aAuth, map[string]any{"user_id": b.UserID})
		if status != http.StatusOK {
			t.Fatalf("block: %d %+v", status, env.Error)
		}
		decodeResults(t, env, &blocking)
		if blocking.Blocking != b.UserID {
			t.Fatalf("block answer %+v", blocking)
		}
	}
	if status, _ := call(t, srv, "POST", "players/block/", aAuth, map[string]any{"user_id": a.UserID}); status == http.StatusOK {
		t.Fatal("self block accepted")
	}
	if status, _ := call(t, srv, "POST", "players/block/", aAuth, map[string]any{"user_id": 99999}); status == http.StatusOK {
		t.Fatal("unknown user blocked")
	}
	var list struct {
		Users []struct {
			UserID    int32 `json:"user_id"`
			IsBlocked bool  `json:"is_blocked"`
		} `json:"blocked_mini_users"`
		Total int `json:"total_entries"`
	}
	_, env := call(t, srv, "POST", "players/blocked_users/", aAuth, map[string]int{"page": 1, "pageSize": 10})
	decodeResults(t, env, &list)
	if list.Total != 1 || len(list.Users) != 1 || list.Users[0].UserID != b.UserID || !list.Users[0].IsBlocked {
		t.Fatalf("A's block list %+v", list)
	}
	var prof struct {
		IsBlocked  bool `json:"is_blocked"`
		BlockedYou bool `json:"blocked_you"`
	}
	_, env = call(t, srv, "POST", "players/get/", bAuth, map[string]any{"user_id": a.UserID})
	decodeResults(t, env, &prof)
	if prof.IsBlocked || !prof.BlockedYou {
		t.Fatalf("B's view of A %+v", prof)
	}

	// A's public session: hidden from B (blocked by A) and closed to B's join; C sees and may join it.
	session := hostPublic(t, srv, aAuth, a.UserID)
	if l := browse(t, srv, bAuth); len(l) != 0 {
		t.Fatalf("B sees a session of the player who blocked it: %+v", l)
	}
	if l := browse(t, srv, cAuth); len(l) != 1 || l[0].GameSession.GameSessionID != session {
		t.Fatalf("C's browser %+v", l)
	}
	if status, _ := call(t, srv, "POST", "multiplayer/player_sessions/", bAuth, map[string]string{"game_session_id": session}); status == http.StatusOK {
		t.Fatal("B joined A's session")
	}
	// Quickplay never puts B in A's session: as a DJ it hosts its own; C (no blocks) is placed in the newest (B's).
	quick := func(auth string, id int32) string {
		t.Helper()
		var tk ticketResponse
		_, env := call(t, srv, "POST", "multiplayer/tickets/", auth, map[string]any{"queue": "freestyle_public",
			"players": []map[string]any{{"player_id": id, "is_dj": true, "venue_select": "VenueA"}}})
		decodeResults(t, env, &tk)
		return tk.ConnectionInfo.GameSessionID
	}
	bSession := quick(bAuth, b.UserID)
	if bSession == "" || bSession == session {
		t.Fatalf("B's quickplay session %q (A's is %q)", bSession, session)
	}
	if got := quick(cAuth, c.UserID); got != bSession {
		t.Fatalf("C's quickplay session %q, want B's %q", got, bSession)
	}

	// B invites A to B's session: A blocked B, so the invite is dropped (B is told nothing).
	status, env := call(t, srv, "POST", "multiplayer/game_sessions/invite/", bAuth,
		map[string]any{"game_session_id": bSession, "target_user_id": a.UserID})
	if status != http.StatusOK {
		t.Fatalf("invite to a blocker: %d %+v", status, env.Error)
	}
	var feed struct {
		Invites []map[string]any `json:"invites"`
	}
	_, env = call(t, srv, "POST", "feed/pull_notifications/", aAuth, map[string]any{})
	decodeResults(t, env, &feed)
	if len(feed.Invites) != 0 {
		t.Fatalf("A got an invite from a player it blocked: %+v", feed.Invites)
	}

	// A invites B to A's session: B blocked nobody, so it gets the invite, and the invite opens the session to it.
	_, env = call(t, srv, "POST", "multiplayer/game_sessions/invite/", aAuth,
		map[string]any{"game_session_id": session, "target_user_id": b.UserID})
	decodeResults(t, env, &map[string]string{})
	found := false
	for _, e := range browse(t, srv, bAuth) { // B also lists its own session
		found = found || (e.GameSession.GameSessionID == session && e.Source == "invite")
	}
	if !found {
		t.Fatal("A's session is not in B's browser after A's invite")
	}
	if status, env := call(t, srv, "POST", "multiplayer/player_sessions/", bAuth, map[string]string{"game_session_id": session}); status != http.StatusOK {
		t.Fatalf("B's invited join: %d %+v", status, env.Error)
	}

	// Unblock: the list is empty again.
	var unblocked struct {
		Unblocked int32 `json:"unblocked"`
	}
	_, env = call(t, srv, "POST", "players/unblock/", aAuth, map[string]any{"user_id": b.UserID})
	decodeResults(t, env, &unblocked)
	_, env = call(t, srv, "POST", "players/blocked_users/", aAuth, map[string]int{"page": 1, "pageSize": 10})
	decodeResults(t, env, &list)
	if unblocked.Unblocked != b.UserID || list.Total != 0 {
		t.Fatalf("after unblock: %+v, list %+v", unblocked, list)
	}
}
