package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"fuseronline/internal/config"
	"fuseronline/internal/mainstage"
	"fuseronline/internal/platform"
)

// reportLoader makes a logged-in test player a loader user, the way the loader reports itself.
func reportLoader(t *testing.T, srv *httptest.Server, auth string) {
	t.Helper()
	status, env := call(t, srv, "POST", "multiplayer/limits/", auth, map[string]string{"loader": "0.13.7", "build": "EGS v1.4.2"})
	if status != http.StatusOK || env.Error != nil {
		t.Fatalf("loader report: %d %+v", status, env.Error)
	}
}

// The stock / modded split: loader users and stock players never share a public session or
// see each other's in the browser; an invite overrides it, as for blocks.
func TestStockModdedSplit(t *testing.T) {
	srv, s := newTestServerAndAPI(t, func(c *config.Config) { c.Multiplayer.LoopbackHostAddress = "198.51.100.7" })
	aAuth, a := login(t, srv, "epc", "split-a") // loader
	bAuth, b := login(t, srv, "epc", "split-b") // stock
	cAuth, c := login(t, srv, "epc", "split-c") // loader
	reportLoader(t, srv, aAuth)
	reportLoader(t, srv, cAuth)
	if !s.modded(int64(a.UserID)) || s.modded(int64(b.UserID)) {
		t.Fatal("loader reports not recorded")
	}
	// A report without a login, or without a loader version, records nothing.
	call(t, srv, "POST", "multiplayer/limits/", "", map[string]string{"loader": "0.13.7"})
	call(t, srv, "POST", "multiplayer/limits/", bAuth, map[string]string{"build": "EGS v1.4.2"})
	if s.modded(int64(b.UserID)) {
		t.Fatal("a report without a loader version made B a loader user")
	}

	session := hostPublic(t, srv, aAuth, a.UserID)
	if l := browse(t, srv, bAuth); len(l) != 0 {
		t.Fatalf("stock B sees the loader session: %+v", l)
	}
	if l := browse(t, srv, cAuth); len(l) != 1 || l[0].GameSession.GameSessionID != session {
		t.Fatalf("loader C's browser %+v", l)
	}
	if status, _ := call(t, srv, "POST", "multiplayer/player_sessions/", bAuth, map[string]string{"game_session_id": session}); status == http.StatusOK {
		t.Fatal("stock B joined the loader session")
	}
	quick := func(auth string, id int32) string {
		t.Helper()
		var tk ticketResponse
		_, env := call(t, srv, "POST", "multiplayer/tickets/", auth, map[string]any{"queue": "freestyle_public",
			"players": []map[string]any{{"player_id": id, "is_dj": true, "venue_select": "VenueA"}}})
		decodeResults(t, env, &tk)
		return tk.ConnectionInfo.GameSessionID
	}
	// Quickplay: stock B hosts its own session; loader C goes to A's, not the newer stock one.
	bSession := quick(bAuth, b.UserID)
	if bSession == "" || bSession == session {
		t.Fatalf("B's quickplay session %q (A's is %q)", bSession, session)
	}
	if got := quick(cAuth, c.UserID); got != session {
		t.Fatalf("C's quickplay session %q, want A's %q", got, session)
	}
	// An invite opens the loader session to stock B.
	_, env := call(t, srv, "POST", "multiplayer/game_sessions/invite/", aAuth,
		map[string]any{"game_session_id": session, "target_user_id": b.UserID})
	decodeResults(t, env, &map[string]string{})
	if status, env := call(t, srv, "POST", "multiplayer/player_sessions/", bAuth, map[string]string{"game_session_id": session}); status != http.StatusOK {
		t.Fatalf("B's invited join: %d %+v", status, env.Error)
	}
}

// separate_stock_players off: everyone counts as a loader user, so stock and loader players mix.
func TestStockModdedSplitOff(t *testing.T) {
	srv := newTestServerWith(t, func(c *config.Config) {
		c.Multiplayer.LoopbackHostAddress = "198.51.100.7"
		c.Multiplayer.SeparateStockPlayers = false
	})
	aAuth, a := login(t, srv, "epc", "mix-a")
	bAuth, _ := login(t, srv, "epc", "mix-b")
	reportLoader(t, srv, aAuth)
	session := hostPublic(t, srv, aAuth, a.UserID)
	if l := browse(t, srv, bAuth); len(l) != 1 || l[0].GameSession.GameSessionID != session {
		t.Fatalf("with the split off, stock B's browser %+v", l)
	}
}

// The Main Stage: stock players may book only events with allow_stock, and join only while the current (else the
// next) event allows them.
func TestMainStageStockPlayers(t *testing.T) {
	t0 := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	for _, allow := range []bool{false, true} {
		srv, s := newTestServerAndAPI(t, func(c *config.Config) {
			c.MainStage.Enabled = true
			c.MainStage.Rotation.Anchor = t0.Format(time.RFC3339)
			c.MainStage.Rotation.IntervalMinutes = 120
			c.MainStage.Rotation.AllowStock = allow
		})
		s.now = func() time.Time { return t0.Add(-30 * time.Minute) }
		auth, _ := login(t, srv, platform.Steam, "76561190000000400")
		status, env := call(t, srv, "POST", "main_stage/purchase_slot/", auth, map[string]string{"event": mainstage.RotationUUID(t0)})
		if allow {
			var r map[string]any
			if env.Error != nil || json.Unmarshal(env.Results, &r) != nil || r["event_purchased"] != true {
				t.Fatalf("stock purchase of an allow_stock event: %d %+v", status, env.Error)
			}
		} else if env.Error == nil || env.Error.Code != codeNotAvailable {
			t.Fatalf("stock purchase of a loader-only event: %d %+v", status, env.Error)
		}
	}

	at := func(c *mainstage.Classified) mainstage.Schedule { return mainstage.Schedule{Current: c} }
	yes := &mainstage.Classified{Event: mainstage.Event{AllowStock: true}}
	no := &mainstage.Classified{Event: mainstage.Event{}}
	switch {
	case !stockAllowed(at(yes)), stockAllowed(at(no)):
		t.Fatal("the current event decides")
	case !stockAllowed(mainstage.Schedule{Next: yes}), stockAllowed(mainstage.Schedule{Next: no}):
		t.Fatal("between events, the next one decides")
	case stockAllowed(mainstage.Schedule{Current: no, Next: yes}):
		t.Fatal("the next event overrode the current one")
	case stockAllowed(mainstage.Schedule{}):
		t.Fatal("no event allowed stock players")
	}
}
