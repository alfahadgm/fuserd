package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"fuseronline/internal/config"
	"fuseronline/internal/matchmaking"
)

func decodeResults(t *testing.T, env envelope, v any) {
	t.Helper()
	if env.Error != nil {
		t.Fatalf("error response: %+v", env.Error)
	}
	if err := json.Unmarshal(env.Results, v); err != nil {
		t.Fatal(err)
	}
}

// The client flow for a player-hosted session: with no public servers configured, the creator of a public match gets a session it hosts; a second player is invited,
// finds it in the browser and joins with a player session. (Private sessions run only on FDS: TestPrivateNeedsFDS.)
func TestMultiplayerHostAndJoin(t *testing.T) {
	// The test client is on loopback, like a host on the fuserd machine: the session must carry a reachable address.
	srv := newTestServerWith(t, func(c *config.Config) { c.Multiplayer.LoopbackHostAddress = "198.51.100.7" })
	hostAuth, host := login(t, srv, "epc", "host-account")
	joinAuth, joiner := login(t, srv, "epc", "joiner-account")

	var regions struct {
		Regions []struct {
			Region       string `json:"region"`
			EndpointHTTP string `json:"endpoint_http"`
		} `json:"gamelift_regions_http"`
	}
	_, env := call(t, srv, "GET", "multiplayer/game_sessions/regions_http/", hostAuth, nil)
	decodeResults(t, env, &regions)
	if len(regions.Regions) != 1 || regions.Regions[0].Region == "" {
		t.Fatalf("regions %+v", regions)
	}
	// The latency probe is a plain GET the client retries until it completes.
	if resp, err := http.Get(srv.URL + strings.TrimPrefix(regions.Regions[0].EndpointHTTP, "http://127.0.0.1:8000")); err != nil || resp.StatusCode != 200 {
		t.Fatalf("latency endpoint: %v %v", err, resp)
	}

	status, env := call(t, srv, "POST", "multiplayer/tickets/", hostAuth, map[string]any{
		"queue": "freestyle_public", "changelist": 1310155,
		"players": []map[string]any{{"player_id": host.UserID, "platform": "epc", "is_creator": true,
			"force_create_new_match": 1, "venue_select": "VenueA", "venue_time_of_day": "sunset", "is_dj": true,
			"turn_length": []string{"128"}, "num_rounds": []string{"3"},
			"latency_results": []map[string]any{{"region": "fuserd", "latency": 12}}}},
	})
	if status != http.StatusOK {
		t.Fatalf("create ticket: %d %+v", status, env.Error)
	}
	var created ticketResponse
	decodeResults(t, env, &created)

	var tk ticketResponse
	_, env = call(t, srv, "POST", "multiplayer/tickets/get/", hostAuth, map[string]string{"ticket_id": created.TicketID})
	decodeResults(t, env, &tk)
	ci := tk.ConnectionInfo
	if tk.Status != "COMPLETED" || ci.GameSessionID == "" || ci.IPAddress != "198.51.100.7" || ci.Port != "7777" {
		t.Fatalf("ticket %+v", tk)
	}
	if len(ci.MatchedPlayerSessions) != 1 || ci.MatchedPlayerSessions[0].PlayerID != strconv.Itoa(int(host.UserID)) ||
		!strings.HasPrefix(ci.MatchedPlayerSessions[0].PlayerSessionID, matchmaking.HostPrefix) {
		t.Fatalf("matched player sessions %+v", ci.MatchedPlayerSessions)
	}
	// Another player cannot read the host's ticket.
	if status, _ := call(t, srv, "POST", "multiplayer/tickets/get/", joinAuth, map[string]string{"ticket_id": created.TicketID}); status == http.StatusOK {
		t.Fatal("ticket readable by another player")
	}

	var gs gameSessionResponse
	_, env = call(t, srv, "POST", "multiplayer/game_sessions/get/", hostAuth, map[string]string{"game_session_id": ci.GameSessionID})
	decodeResults(t, env, &gs)
	if gs.Status != "ACTIVE" || gs.IPAddress != "198.51.100.7" || gs.Port != "7777" {
		t.Fatalf("game session %+v", gs)
	}
	checkMatchmakerData(t, gs.MatchmakerData, host.UserID, "VenueA", "sunset")

	// A public session is listed for everyone; an invite then labels it as one.
	if list := browse(t, srv, joinAuth); len(list) != 1 || list[0].Source != "public" {
		t.Fatalf("stranger's view of the public session: %+v", list)
	}
	_, env = call(t, srv, "POST", "multiplayer/game_sessions/invite/", hostAuth,
		map[string]any{"game_session_id": ci.GameSessionID, "target_user_id": joiner.UserID})
	decodeResults(t, env, &map[string]string{})
	var feed struct {
		Invites []struct {
			Profile struct {
				UserID       int32  `json:"user_id"`
				HypeUsername string `json:"hype_username"`
			} `json:"profile"`
			InviterType   string `json:"inviter_type"`
			Timestamp     int64  `json:"timestamp"`
			GameSessionID string `json:"game_session_id"`
		} `json:"invites"`
	}
	_, env = call(t, srv, "POST", "feed/pull_notifications/", joinAuth, map[string]int{"page": 1, "pageSize": 10})
	decodeResults(t, env, &feed)
	if len(feed.Invites) != 1 || feed.Invites[0].GameSessionID != ci.GameSessionID ||
		feed.Invites[0].Profile.UserID != host.UserID || feed.Invites[0].Timestamp == 0 {
		t.Fatalf("invite feed %+v", feed)
	}
	list := browse(t, srv, joinAuth)
	if len(list) != 1 || list[0].GameSession.GameSessionID != ci.GameSessionID || list[0].Source != "invite" ||
		list[0].User.UserID != host.UserID || list[0].User.HypeUsername == "" {
		t.Fatalf("session list %+v", list)
	}

	var ps map[string]string
	_, env = call(t, srv, "POST", "multiplayer/player_sessions/", joinAuth, map[string]string{"game_session_id": ci.GameSessionID})
	decodeResults(t, env, &ps)
	_, env = call(t, srv, "POST", "feed/pull_notifications/", joinAuth, nil)
	decodeResults(t, env, &feed)
	if len(feed.Invites) != 0 {
		t.Fatalf("the invite outlived the join: %+v", feed.Invites)
	}
	if ps["game_session_id"] != ci.GameSessionID || ps["player_id"] != strconv.Itoa(int(joiner.UserID)) ||
		ps["ip_address"] != "198.51.100.7" || ps["port"] != "7777" || ps["player_session_id"] == "" ||
		strings.HasPrefix(ps["player_session_id"], matchmaking.HostPrefix) {
		t.Fatalf("player session %+v", ps)
	}
	_, env = call(t, srv, "POST", "multiplayer/game_sessions/get/", joinAuth, map[string]string{"game_session_id": ci.GameSessionID})
	decodeResults(t, env, &gs)
	checkMatchmakerData(t, gs.MatchmakerData, host.UserID, "VenueA", "sunset")

	// Unsupported queues fail cleanly (FailedStartMatchmaking on the client).
	if status, _ := call(t, srv, "POST", "multiplayer/tickets/", joinAuth, map[string]any{"queue": "battle_ranked"}); status == http.StatusOK {
		t.Fatal("battle queue accepted")
	}
	// Matchmaking needs a session token.
	if status, _ := call(t, srv, "POST", "multiplayer/tickets/", "", map[string]any{"queue": "freestyle_private"}); status != http.StatusUnauthorized {
		t.Fatalf("anonymous ticket: %d", status)
	}

	// The host cancelling ends the session.
	_, env = call(t, srv, "POST", "multiplayer/tickets/cancel/", hostAuth, map[string]string{"ticket_id": created.TicketID})
	decodeResults(t, env, &map[string]string{})
	if status, _ := call(t, srv, "POST", "multiplayer/game_sessions/get/", joinAuth, map[string]string{"game_session_id": ci.GameSessionID}); status == http.StatusOK {
		t.Fatal("session outlived its host's cancel")
	}
}

// checkMatchmakerData mirrors what the client's parser (FUN_14070ff00) needs: JSON in a string, teams[].players[]
// with a numeric-string playerId and {"valueAttribute": v} attributes, and one is_creator player carrying the venue.
func checkMatchmakerData(t *testing.T, raw string, creator int32, venue, tod string) {
	t.Helper()
	var md struct {
		Teams []struct {
			Name    string `json:"name"`
			Players []struct {
				PlayerID   string `json:"playerId"`
				Attributes map[string]struct {
					Value any `json:"valueAttribute"`
				} `json:"attributes"`
			} `json:"players"`
		} `json:"teams"`
	}
	if err := json.Unmarshal([]byte(raw), &md); err != nil {
		t.Fatalf("matchmaker_data is not JSON: %v", err)
	}
	creators := 0
	for _, team := range md.Teams {
		if team.Name != "djs" && team.Name != "spectators" {
			t.Fatalf("team name %q", team.Name)
		}
		for _, p := range team.Players {
			if _, err := strconv.Atoi(p.PlayerID); err != nil {
				t.Fatalf("playerId %q is not numeric", p.PlayerID)
			}
			if _, ok := p.Attributes["is_creator"].Value.(float64); !ok {
				t.Fatalf("is_creator must be a number: %+v", p.Attributes)
			}
			if p.Attributes["is_creator"].Value.(float64) != 1 {
				continue
			}
			creators++
			if p.PlayerID != strconv.Itoa(int(creator)) || p.Attributes["venue_select"].Value != venue ||
				p.Attributes["venue_time_of_day"].Value != tod || p.Attributes["hype_user_id"].Value != p.PlayerID {
				t.Fatalf("creator entry %+v", p)
			}
		}
	}
	if creators != 1 {
		t.Fatalf("%d creators in %s", creators, raw)
	}
}

// The client sends turn_length / num_rounds as TArray<FString>; numbers are accepted too.
func TestFlexStrings(t *testing.T) {
	var f struct {
		A flexStrings `json:"a"`
	}
	if err := json.Unmarshal([]byte(`{"a":["128",64,3.5]}`), &f); err != nil {
		t.Fatal(err)
	}
	if strings.Join(f.A, ",") != "128,64,3.5" {
		t.Fatalf("got %q", f.A)
	}
	if err := json.Unmarshal([]byte(`{"a":[true]}`), &f); err == nil {
		t.Fatal("a bool element should fail")
	}
}

// "Watch a Public Show" with no show running: the ticket is TIMED_OUT at once and when polled, and nothing is
// listed. "Join a Public Show" (a DJ) still hosts.
func TestWatchPublicShowWithNoSession(t *testing.T) {
	srv := newTestServerWith(t, func(c *config.Config) { c.Multiplayer.LoopbackHostAddress = "198.51.100.7" })
	viewerAuth, viewer := login(t, srv, "epc", "viewer-account")
	djAuth, dj := login(t, srv, "epc", "dj-account")
	watch := func() ticketResponse {
		var tk ticketResponse
		_, env := call(t, srv, "POST", "multiplayer/tickets/", viewerAuth, map[string]any{"queue": "freestyle_public",
			"players": []map[string]any{{"player_id": viewer.UserID, "is_dj": false, "turn_length": []string{"128"}, "num_rounds": []string{"3"}}}})
		decodeResults(t, env, &tk)
		return tk
	}
	tk := watch()
	if tk.Status != "TIMED_OUT" || tk.ConnectionInfo.GameSessionID != "" {
		t.Fatalf("watch with no show: %+v", tk)
	}
	var polled ticketResponse
	_, env := call(t, srv, "POST", "multiplayer/tickets/get/", viewerAuth, map[string]string{"ticket_id": tk.TicketID})
	decodeResults(t, env, &polled)
	if polled.Status != "TIMED_OUT" {
		t.Fatalf("polled watch ticket %+v", polled)
	}
	if list := browse(t, srv, viewerAuth); len(list) != 0 {
		t.Fatalf("a viewer created a session: %+v", list)
	}
	var hosted ticketResponse
	_, env = call(t, srv, "POST", "multiplayer/tickets/", djAuth, map[string]any{"queue": "freestyle_public",
		"players": []map[string]any{{"player_id": dj.UserID, "is_dj": true, "venue_select": "VenueA"}}})
	decodeResults(t, env, &hosted)
	if hosted.Status != "COMPLETED" || hosted.ConnectionInfo.GameSessionID == "" {
		t.Fatalf("DJ join with no show should host: %+v", hosted)
	}
	if joined := watch(); joined.Status != "COMPLETED" || joined.ConnectionInfo.GameSessionID != hosted.ConnectionInfo.GameSessionID {
		t.Fatalf("viewer should join the running show: %+v", joined)
	}
	if !isViewer(mmPlayer{}, false) || isViewer(mmPlayer{IsDJ: true}, false) || isViewer(mmPlayer{}, true) {
		t.Fatal("isViewer")
	}
}

// A session-browser join sends the host's own dev game session id (from the beacon metadata). With one visible
// session fuserd places the player there and returns its own id; with two it cannot tell and refuses.
func TestBrowserJoinWithHostSessionID(t *testing.T) {
	srv := newTestServerWith(t, func(c *config.Config) { c.Multiplayer.LoopbackHostAddress = "198.51.100.7" })
	hostAuth, host := login(t, srv, "epc", "host-account")
	joinAuth, _ := login(t, srv, "epc", "joiner-account")
	secondAuth, second := login(t, srv, "epc", "second-host-account")
	hostPublic := func(auth string, id int32) string {
		var tk ticketResponse
		_, env := call(t, srv, "POST", "multiplayer/tickets/", auth, map[string]any{"queue": "freestyle_public",
			"players": []map[string]any{{"player_id": id, "is_creator": true, "is_dj": true, "venue_select": "VenueA"}}})
		decodeResults(t, env, &tk)
		return tk.ConnectionInfo.GameSessionID
	}
	sessionID := hostPublic(hostAuth, host.UserID)
	var ps map[string]string
	status, env := call(t, srv, "POST", "multiplayer/player_sessions/", joinAuth,
		map[string]string{"game_session_id": "local-dev-gamesession-DF4AB634"})
	if status != http.StatusOK {
		t.Fatalf("browser join: %d %+v", status, env.Error)
	}
	decodeResults(t, env, &ps)
	if ps["game_session_id"] != sessionID || ps["ip_address"] != "198.51.100.7" || ps["player_session_id"] == "" {
		t.Fatalf("player session %+v, want session %s", ps, sessionID)
	}
	var gs gameSessionResponse
	_, env = call(t, srv, "POST", "multiplayer/game_sessions/get/", joinAuth, map[string]string{"game_session_id": ps["game_session_id"]})
	decodeResults(t, env, &gs)
	if gs.GameSessionID != sessionID || gs.MatchmakerData == "" {
		t.Fatalf("game session %+v", gs)
	}
	hostPublic(secondAuth, second.UserID)
	if status, _ := call(t, srv, "POST", "multiplayer/player_sessions/", joinAuth,
		map[string]string{"game_session_id": "local-dev-gamesession-DF4AB634"}); status == http.StatusOK {
		t.Fatal("ambiguous host session id was resolved")
	}
}

func TestPublicSetOptionsPreserveRoundsAndPreferLongerMeasures(t *testing.T) {
	keys, _, _ := publicSetKeys(mmPlayer{NumRounds: []string{"1", "2", "3"}, TurnLength: []string{"32", "64"}})
	if keys[0].Rounds != 1 || keys[0].Measures != 64 {
		t.Fatalf("new server would use %+v; want 1/64", keys[0])
	}
	if got := acceptedInts([]string{"64"}, 128); len(got) != 1 || got[0] != 64 {
		t.Fatalf("single explicit measure option changed: %v", got)
	}
}

type browserEntry struct {
	GameSession gameSessionResponse `json:"game_session"`
	User        struct {
		UserID       int32  `json:"user_id"`
		HypeUsername string `json:"hype_username"`
	} `json:"user"`
	Source string `json:"source"`
}

func browse(t *testing.T, srv *httptest.Server, auth string) []browserEntry {
	t.Helper()
	var list struct {
		Entries []browserEntry `json:"session_browser_entries"`
	}
	_, env := call(t, srv, "POST", "multiplayer/game_sessions/get_list/", auth, map[string]int{"changelist": 1310155})
	decodeResults(t, env, &list)
	return list.Entries
}

// hostPublic has the player create a public match it hosts (no public servers configured): a player-hosted
// session, as the P2P and song-transfer tests need.
func hostPublic(t *testing.T, srv *httptest.Server, auth string, id int32) string {
	t.Helper()
	var tk ticketResponse
	_, env := call(t, srv, "POST", "multiplayer/tickets/", auth, map[string]any{"queue": "freestyle_public",
		"players": []map[string]any{{"player_id": id, "is_creator": true, "venue_select": "VenueA"}}})
	decodeResults(t, env, &tk)
	return tk.ConnectionInfo.GameSessionID
}

// privateOnFDS gives the player a private session the way fuserd does: a managed session on an FDS container of
// its own (here placed directly, without Docker). Returns the session id.
func privateOnFDS(t *testing.T, api *Server, id int32) string {
	t.Helper()
	slot := "fuserd-private-test-" + strconv.Itoa(int(id))
	ticket, _ := api.mm.CreateManagedPending(matchmaking.Player{ID: int64(id), Venue: "VenueA", IsDJ: true}, slot,
		matchmaking.QueueFreestylePrivate)
	if !api.mm.CompleteManaged(ticket.ID, slot, "198.51.100.30", 7777, 12) {
		t.Fatal("private placement failed")
	}
	_, sess, _ := api.mm.Ticket(ticket.ID, int64(id))
	if !sess.Private {
		t.Fatalf("session %+v is not private", sess)
	}
	return sess.ID
}

// Private sessions run only on FDS containers: without them a private ticket is refused, never player-hosted.
func TestPrivateNeedsFDS(t *testing.T) {
	srv := newTestServer(t)
	auth, user := login(t, srv, "epc", "account-a")
	status, env := call(t, srv, "POST", "multiplayer/tickets/", auth, map[string]any{"queue": "freestyle_private",
		"players": []map[string]any{{"player_id": user.UserID, "is_creator": true, "venue_select": "VenueA"}}})
	if status != http.StatusServiceUnavailable || env.Error == nil {
		t.Fatalf("private ticket without FDS: %d %+v", status, env.Error)
	}
	if list := browse(t, srv, auth); len(list) != 0 {
		t.Fatalf("a session was created: %+v", list)
	}
}

// Follows, username search, other profiles and platform friends, and how they open a private session.
func TestSocial(t *testing.T) {
	srv, api := newTestServerAndAPI(t, nil)
	aAuth, a := login(t, srv, "epc", "account-a")
	bAuth, b := login(t, srv, "epc", "account-b")
	cAuth, _ := login(t, srv, "stm", "76561198000000003")

	// Search finds B by part of its hype username, but not the searcher itself.
	var found struct {
		Results []struct {
			UserID       int32  `json:"user_id"`
			HypeUsername string `json:"hype_username"`
		} `json:"search_results"`
		Total int `json:"total_entries"`
	}
	_, env := call(t, srv, "POST", "search/username/", aAuth, map[string]any{"page": 1, "pageSize": 10, "search": "tester"})
	decodeResults(t, env, &found)
	if found.Total != 2 || len(found.Results) != 2 {
		t.Fatalf("search %+v", found) // B and C (all test logins are "Tester One")
	}
	for _, r := range found.Results {
		if r.UserID == a.UserID {
			t.Fatal("search returned the searcher")
		}
	}

	sid := privateOnFDS(t, api, a.UserID)
	if len(browse(t, srv, bAuth)) != 0 {
		t.Fatal("B sees A's private session without any relation")
	}

	// B follows A: A's private session shows up for B as followship; A's following/followers lists agree.
	_, env = call(t, srv, "POST", "followship/follow/", bAuth, map[string]any{"user_id": a.UserID})
	decodeResults(t, env, &map[string]string{})
	if list := browse(t, srv, bAuth); len(list) != 1 || list[0].GameSession.GameSessionID != sid || list[0].Source != "followship" {
		t.Fatalf("follow does not open the session: %+v", list)
	}
	var fl struct {
		Count     int `json:"count"`
		MiniUsers []struct {
			UserID     int32 `json:"user_id"`
			IsFollowed bool  `json:"is_followed"`
			FollowsYou bool  `json:"follows_you"`
		} `json:"mini_users"`
	}
	_, env = call(t, srv, "POST", "followship/followers/", aAuth, map[string]int{"page": 1, "pageSize": 10})
	decodeResults(t, env, &fl)
	if fl.Count != 1 || fl.MiniUsers[0].UserID != b.UserID || !fl.MiniUsers[0].FollowsYou || fl.MiniUsers[0].IsFollowed {
		t.Fatalf("A's followers %+v", fl)
	}
	_, env = call(t, srv, "POST", "followship/following/", aAuth, map[string]any{"user_id": b.UserID, "page": 1, "pageSize": 10})
	decodeResults(t, env, &fl)
	if fl.Count != 1 || fl.MiniUsers[0].UserID != a.UserID {
		t.Fatalf("B's following, read by A: %+v", fl)
	}

	// Another player's profile, with the relation flags; one's own carries the name (no players/xp/ yet: zero stats).
	var prof map[string]any
	_, env = call(t, srv, "POST", "players/get/", aAuth, map[string]any{"user_id": b.UserID})
	decodeResults(t, env, &prof)
	if prof["hype_username"] == "" || prof["follows_you"] != true || prof["platform_profile"].(map[string]any)["platform_user_id"] != "" ||
		prof["platform_profile"].(map[string]any)["platform_username"] != displayName(prof["hype_username"].(string)) ||
		strings.Contains(prof["platform_profile"].(map[string]any)["platform_username"].(string), "#") {
		t.Fatalf("profile %+v", prof)
	}
	defer func(w time.Duration) { ownStatsWait = w }(ownStatsWait)
	ownStatsWait = 0
	_, env = call(t, srv, "POST", "players/get/", aAuth, map[string]any{"user_id": a.UserID})
	own := map[string]any{}
	decodeResults(t, env, &own)
	if own["hype_username"] == "" || own["platform_profile"].(map[string]any)["platform_username"] != displayName(own["hype_username"].(string)) ||
		own["is_banned"] != false || own["hype_profile"].(map[string]any)["num_discs"] != float64(0) {
		t.Fatalf("own profile %+v", own)
	}

	_, env = call(t, srv, "POST", "followship/unfollow/", bAuth, map[string]any{"user_id": a.UserID})
	decodeResults(t, env, &map[string]string{})
	if len(browse(t, srv, bAuth)) != 0 {
		t.Fatal("unfollow left the session visible")
	}

	// Platform friends: B uploads A's Epic account ID; A appears in B's platform friends and the session opens.
	_, env = call(t, srv, "POST", "players/platform_blocked_users_and_friends/", bAuth,
		map[string]any{"friends_list": []string{"account-a", "76561198000000003"}, "blocked_list": []string{}})
	decodeResults(t, env, &map[string]string{})
	var pf struct {
		Friends []struct {
			UserID int32 `json:"user_id"`
		} `json:"platform_friends"`
	}
	_, env = call(t, srv, "POST", "players/platform_friends/", bAuth, map[string]int{"page": 1, "pageSize": 10})
	decodeResults(t, env, &pf)
	if len(pf.Friends) != 1 || pf.Friends[0].UserID != a.UserID {
		t.Fatalf("platform friends %+v (the Steam ID must not match across platforms)", pf)
	}
	// The reverse direction: A uploaded nothing, yet B is A's platform friend. A's invite screen lists
	// platform_friends plus followers_without_platform_friends, so B (who follows A) must be in exactly one of them.
	_, env = call(t, srv, "POST", "followship/follow/", bAuth, map[string]any{"user_id": a.UserID})
	decodeResults(t, env, &map[string]string{})
	pf.Friends = nil
	_, env = call(t, srv, "POST", "players/platform_friends/", aAuth, map[string]int{"page": 1, "pageSize": 10})
	decodeResults(t, env, &pf)
	var nf struct {
		Followers []struct {
			UserID int32 `json:"user_id"`
		} `json:"followers"`
	}
	_, env = call(t, srv, "POST", "players/followers_without_platform_friends/", aAuth, map[string]int{"page": 1, "pageSize": 10})
	decodeResults(t, env, &nf)
	if len(pf.Friends) != 1 || pf.Friends[0].UserID != b.UserID || len(nf.Followers) != 0 {
		t.Fatalf("A's invite lists: platform friends %+v, followers without friends %+v", pf, nf)
	}
	_, env = call(t, srv, "POST", "followship/unfollow/", bAuth, map[string]any{"user_id": a.UserID})
	decodeResults(t, env, &map[string]string{})

	if list := browse(t, srv, bAuth); len(list) != 1 || list[0].Source != "platform_friend" {
		t.Fatalf("platform friend does not see the session: %+v", list)
	}
	if len(browse(t, srv, cAuth)) != 0 {
		t.Fatal("an unrelated Steam player sees the private session")
	}

	// Only members may invite.
	if status, _ := call(t, srv, "POST", "multiplayer/game_sessions/invite/", cAuth,
		map[string]any{"game_session_id": sid, "target_user_id": b.UserID}); status == http.StatusOK {
		t.Fatal("a non-member invited")
	}
}

// open_private_sessions lists every private session for everyone, as an invite.
func TestOpenPrivateSessions(t *testing.T) {
	srv, api := newTestServerAndAPI(t, func(c *config.Config) { c.Multiplayer.OpenPrivateSessions = true })
	_, a := login(t, srv, "epc", "account-a")
	bAuth, _ := login(t, srv, "epc", "account-b")
	sid := privateOnFDS(t, api, a.UserID)
	if list := browse(t, srv, bAuth); len(list) != 1 || list[0].GameSession.GameSessionID != sid || list[0].Source != "invite" {
		t.Fatalf("open private sessions: %+v", list)
	}
}

// multiplayer/limits/ hands the loader multiplayer.max_crate_songs, without a login (the game's crate check runs
// before it logs in), with GET or POST.
func TestMultiplayerLimits(t *testing.T) {
	var got struct {
		MaxCrateSongs *int `json:"max_crate_songs"`
	}
	_, env := call(t, newTestServer(t), "GET", "multiplayer/limits/", "", nil)
	decodeResults(t, env, &got)
	if got.MaxCrateSongs == nil || *got.MaxCrateSongs != 0 {
		t.Fatalf("default limit %v, want 0", got.MaxCrateSongs)
	}
	srv := newTestServerWith(t, func(c *config.Config) { c.Multiplayer.MaxCrateSongs = 48 })
	for _, method := range []string{"GET", "POST"} {
		got.MaxCrateSongs = nil
		status, env := call(t, srv, method, "multiplayer/limits/", "", nil)
		decodeResults(t, env, &got)
		if status != http.StatusOK || got.MaxCrateSongs == nil || *got.MaxCrateSongs != 48 {
			t.Errorf("%s: status %d, limit %v, want 48", method, status, got.MaxCrateSongs)
		}
	}
}
