package api

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"fuseronline/internal/config"
	"fuseronline/internal/turnrelay"
)

type p2pPeerJSON struct {
	PeerID     uint32 `json:"peer_id"`
	Key        string `json:"key"`
	Candidates []struct {
		Type string `json:"type"`
		IP   string `json:"ip"`
		Port int    `json:"port"`
	} `json:"candidates"`
	Mapping string `json:"mapping"`
}

type p2pAnswer struct {
	Role   string        `json:"role"`
	Peers  []p2pPeerJSON `json:"peers"`
	PeerID uint32        `json:"peer_id"`
	Key    string        `json:"key"`
	Host   *struct {
		Candidates []struct {
			Port int `json:"port"`
		} `json:"candidates"`
		Mapping string `json:"mapping"`
	} `json:"host"`
	PollMs int `json:"poll_ms"`
}

func candidates(port int) map[string]any {
	return map[string]any{
		"candidates": []map[string]any{{"type": "host", "ip": "192.168.1.137", "port": port},
			{"type": "srflx", "ip": "61.245.133.92", "port": port}},
		"mapping": "endpoint-independent",
	}
}

func with(body map[string]any, kv ...any) map[string]any {
	out := map[string]any{}
	for k, v := range body {
		out[k] = v
	}
	for i := 0; i+1 < len(kv); i += 2 {
		out[kv[i].(string)] = kv[i+1]
	}
	return out
}

func TestP2PSignaling(t *testing.T) {
	srv := newTestServerWith(t, func(c *config.Config) {
		c.P2P.Enabled = true
		c.Multiplayer.LoopbackHostAddress = "198.51.100.7"
	})
	aAuth, a := login(t, srv, "epc", "account-a")
	bAuth, _ := login(t, srv, "epc", "account-b")
	cAuth, userC := login(t, srv, "epc", "account-c")
	sid := hostPublic(t, srv, aAuth, a.UserID)

	post := func(path, auth string, body map[string]any) (int, p2pAnswer, string) {
		t.Helper()
		status, env := call(t, srv, "POST", path, auth, body)
		var ans p2pAnswer
		if env.Error != nil {
			return status, ans, env.Error.Message
		}
		decodeResults(t, env, &ans)
		return status, ans, ""
	}
	bad := func(path, auth string, body map[string]any, why string) {
		t.Helper()
		if status, _, msg := post(path, auth, body); status != http.StatusBadRequest {
			t.Fatalf("%s (%s): %d %s", path, why, status, msg)
		}
	}

	// Membership decides roles: b is not in the session yet.
	bad("p2p/join/", bAuth, with(candidates(50001), "game_session_id", sid), "not a member")
	bad("p2p/join/", aAuth, with(candidates(50000), "game_session_id", sid), "the host joining")
	bad("p2p/host/", aAuth, with(candidates(50000), "game_session_id", "gsess-nope"), "unknown session")
	bad("p2p/host/", aAuth, map[string]any{"game_session_id": sid, "candidates": []any{}}, "no candidates")
	bad("p2p/poll/", aAuth, map[string]any{"game_session_id": sid}, "host poll before registering")
	if status, _ := call(t, srv, "POST", "p2p/host/", "", with(candidates(50000), "game_session_id", sid)); status != http.StatusUnauthorized {
		t.Fatalf("anonymous host registration: %d", status)
	}

	for _, auth := range []string{bAuth, cAuth} {
		if status, env := call(t, srv, "POST", "multiplayer/player_sessions/", auth, map[string]string{"game_session_id": sid}); status != http.StatusOK {
			t.Fatalf("join: %d %+v", status, env.Error)
		}
	}
	bad("p2p/host/", bAuth, with(candidates(50001), "game_session_id", sid), "a joiner as host")

	// b registers before the host: no host endpoint yet.
	_, jb, _ := post("p2p/join/", bAuth, with(candidates(50001), "game_session_id", sid))
	if jb.Role != "joiner" || jb.PeerID == 0 || len(jb.Key) != 64 || jb.Host != nil || jb.PollMs != p2pPollMs {
		t.Fatalf("b join: %+v", jb)
	}
	_, ha, _ := post("p2p/host/", aAuth, with(candidates(50000), "game_session_id", sid))
	if ha.Role != "host" || len(ha.Peers) != 1 || ha.Peers[0].PeerID != jb.PeerID || ha.Peers[0].Key != jb.Key ||
		ha.Peers[0].Candidates[1].IP != "61.245.133.92" || ha.Peers[0].Mapping != "endpoint-independent" {
		t.Fatalf("host register: %+v", ha)
	}
	_, pb, _ := post("p2p/poll/", bAuth, map[string]any{"game_session_id": sid})
	if pb.Role != "joiner" || pb.PeerID != jb.PeerID || pb.Host == nil || pb.Host.Candidates[0].Port != 50000 {
		t.Fatalf("b poll: %+v", pb)
	}
	_, jc, _ := post("p2p/join/", cAuth, with(candidates(50002), "game_session_id", sid))
	if jc.PeerID == jb.PeerID || jc.Host == nil {
		t.Fatalf("c join: %+v", jc)
	}
	_, pa, _ := post("p2p/poll/", aAuth, map[string]any{"game_session_id": sid})
	if pa.Role != "host" || len(pa.Peers) != 2 {
		t.Fatalf("host poll: %+v", pa)
	}

	// Reports: each side about its own pair; the host about any.
	ok := func(auth string, peer uint32) {
		t.Helper()
		if status, _, msg := post("p2p/report/", auth, map[string]any{"game_session_id": sid, "peer_id": peer,
			"state": "connected", "path": "direct", "rtt_ms": 23}); status != http.StatusOK {
			t.Fatalf("report: %d %s", status, msg)
		}
	}
	ok(bAuth, jb.PeerID)
	ok(aAuth, jc.PeerID)
	bad("p2p/report/", bAuth, map[string]any{"game_session_id": sid, "peer_id": jc.PeerID, "state": "connected"}, "b about c")

	// A player who leaves the session drops out of the host's list. Hosting a new match leaves the old session.
	hostPublic(t, srv, cAuth, userC.UserID)
	_, pa, _ = post("p2p/poll/", aAuth, map[string]any{"game_session_id": sid})
	if len(pa.Peers) != 1 || pa.Peers[0].PeerID != jb.PeerID {
		t.Fatalf("after c left: %+v", pa)
	}
	bad("p2p/poll/", cAuth, map[string]any{"game_session_id": sid}, "c after leaving")
}

func TestP2PSignalingDisabled(t *testing.T) {
	srv := newTestServer(t)
	aAuth, a := login(t, srv, "epc", "account-a")
	sid := hostPublic(t, srv, aAuth, a.UserID)
	if status, _ := call(t, srv, "POST", "p2p/host/", aAuth, with(candidates(50000), "game_session_id", sid)); status != http.StatusNotFound {
		t.Fatalf("p2p disabled: %d", status)
	}
}

func TestP2PRelayCredentials(t *testing.T) {
	relay, err := turnrelay.Listen(turnrelay.Options{Bind: "127.0.0.1", RelayIP: net.IPv4(127, 0, 0, 1),
		RelayBind: "127.0.0.1", MinPort: 42000, MaxPort: 42099, Secret: "relay secret"})
	if err != nil {
		t.Fatal(err)
	}
	defer relay.Close()
	var srv *httptest.Server
	var api *Server
	srv, api = newTestServerAndAPI(t, func(c *config.Config) {
		c.P2P.Enabled = true
		c.P2P.TurnPort = 3481
		c.PublicURL = "http://203.0.113.9"
		c.Multiplayer.LoopbackHostAddress = "198.51.100.7"
	})
	api.SetRelay(relay)
	aAuth, a := login(t, srv, "epc", "account-a")
	bAuth, _ := login(t, srv, "epc", "account-b")
	sid := hostPublic(t, srv, aAuth, a.UserID)
	call(t, srv, "POST", "multiplayer/player_sessions/", bAuth, map[string]string{"game_session_id": sid})

	var cfg struct {
		Relay bool `json:"relay"`
	}
	_, env := call(t, srv, "GET", "p2p/config/", "", nil)
	decodeResults(t, env, &cfg)
	if !cfg.Relay {
		t.Fatal("p2p/config/ doesn't announce the relay")
	}

	body := with(candidates(50001), "game_session_id", sid)
	_, env = call(t, srv, "POST", "p2p/join/", bAuth, body)
	var ans struct {
		Relay *struct {
			Host     string `json:"host"`
			Port     int    `json:"port"`
			Realm    string `json:"realm"`
			Username string `json:"username"`
			Password string `json:"password"`
			Expires  int64  `json:"expires"`
		} `json:"relay"`
	}
	decodeResults(t, env, &ans)
	r := ans.Relay
	if r == nil || r.Host != "203.0.113.9" || r.Port != 3481 || r.Realm != relay.Realm() ||
		r.Password != turnrelay.Password("relay secret", r.Username) || r.Expires < time.Now().Add(time.Hour).Unix() {
		t.Fatalf("relay credentials: %s", env.Results)
	}

	// The joiner registers its relay address as a "relay" candidate; the host sees it.
	withRelay := map[string]any{"game_session_id": sid, "mapping": "endpoint-dependent", "candidates": []map[string]any{
		{"type": "srflx", "ip": "1.146.221.207", "port": 1966}, {"type": "relay", "ip": "203.0.113.9", "port": 49170}}}
	if status, env := call(t, srv, "POST", "p2p/join/", bAuth, withRelay); status != http.StatusOK {
		t.Fatalf("relay candidate refused: %d %+v", status, env.Error)
	}
	_, env = call(t, srv, "POST", "p2p/host/", aAuth, with(candidates(50000), "game_session_id", sid))
	if !strings.Contains(string(env.Results), `"type":"relay"`) {
		t.Fatalf("host doesn't see the relay candidate: %s", env.Results)
	}
}

// p2p.requests_per_minute caps each player's signaling calls (and each address's p2p/config/ calls); another
// player keeps its own budget.
func TestP2PRateLimit(t *testing.T) {
	srv := newTestServerWith(t, func(c *config.Config) {
		c.P2P.Enabled = true
		c.P2P.RequestsPerMinute = 3
		c.Multiplayer.LoopbackHostAddress = "198.51.100.7"
	})
	aAuth, a := login(t, srv, "epc", "account-a")
	bAuth, _ := login(t, srv, "epc", "account-b")
	sid := hostPublic(t, srv, aAuth, a.UserID)
	body := with(candidates(40000), "game_session_id", sid)

	for i := 0; i < 3; i++ {
		if status, env := call(t, srv, "POST", "p2p/host/", aAuth, body); status != http.StatusOK {
			t.Fatalf("host call %d: %d %+v", i, status, env.Error)
		}
	}
	if status, _ := call(t, srv, "POST", "p2p/poll/", aAuth, map[string]any{"game_session_id": sid}); status != http.StatusTooManyRequests {
		t.Fatalf("4th call in the minute: want 429, got %d", status)
	}
	// B is limited separately: its first call is judged on its merits (it is not a member, so 400, not 429).
	if status, _ := call(t, srv, "POST", "p2p/join/", bAuth, body); status == http.StatusTooManyRequests {
		t.Fatal("another player must not share A's budget")
	}
	for i := 0; i < 3; i++ {
		call(t, srv, "GET", "p2p/config/", "", nil)
	}
	if status, _ := call(t, srv, "GET", "p2p/config/", "", nil); status != http.StatusTooManyRequests {
		t.Fatalf("p2p/config/ over the limit: want 429, got %d", status)
	}
}
