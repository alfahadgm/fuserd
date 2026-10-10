package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"fuseronline/internal/config"
	"fuseronline/internal/logx"
	"fuseronline/internal/platform"
	"fuseronline/internal/store"
	"fuseronline/internal/token"
)

// fakeVerifier accepts the ticket "valid-<userID>", standing in for a platform's signature check.
type fakeVerifier struct {
	platform string
	dlc      []uint32
}

func (f fakeVerifier) Verify(_ context.Context, userID, ticket string) (platform.Identity, error) {
	if ticket != "valid-"+userID {
		return platform.Identity{}, platform.ErrInvalid
	}
	return platform.Identity{Platform: f.platform, UserID: userID, OwnedDLC: f.dlc}, nil
}

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	return newTestServerWith(t, nil)
}

// newTestServerWith lets a test adjust the configuration first.
func newTestServerWith(t *testing.T, adjust func(*config.Config)) *httptest.Server {
	t.Helper()
	srv, _ := newTestServerAndAPI(t, adjust)
	return srv
}

// newTestServerAndAPI also returns the API server, for tests that attach parts (a relay, a catalogue).
func newTestServerAndAPI(t *testing.T, adjust func(*config.Config)) (*httptest.Server, *Server) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := config.Default()
	cfg.SongManifest.Songs = []string{"countingstars", "adoreyou"}
	cfg.MainStage.ImageDir = t.TempDir()
	if adjust != nil {
		adjust(&cfg)
	}
	logs, err := logx.New(slog.NewTextHandler(io.Discard, nil), nil, []byte("test key"))
	if err != nil {
		t.Fatal(err)
	}
	s := New(cfg, st, token.NewIssuer(strings.Repeat("k", 32), time.Hour), map[string]platform.Verifier{
		platform.Steam: fakeVerifier{platform.Steam, []uint32{1500001}},
		platform.Epic:  fakeVerifier{platform: platform.Epic},
	}, logs)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return srv, s
}

type envelope struct {
	Results json.RawMessage `json:"results"`
	Error   *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func call(t *testing.T, srv *httptest.Server, method, path, auth string, body any) (int, envelope) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, srv.URL+Prefix+path, rd)
	req.Header.Set("Content-Type", "application/json")
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var env envelope
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("%s: response is not a JSON object: %v", path, err)
	}
	if (env.Results == nil) == (env.Error == nil) {
		t.Fatalf("%s: response must carry exactly one of results/error", path)
	}
	if env.Error != nil && strings.Contains(env.Error.Code, "maintenance") {
		t.Fatalf("%s: returned %s, which disables the client's online UI", path, env.Error.Code)
	}
	return resp.StatusCode, env
}

func login(t *testing.T, srv *httptest.Server, plat, user string) (string, authResponse) {
	t.Helper()
	status, env := call(t, srv, "POST", "auth/jwt/", "", map[string]string{
		"platform": plat, "platform_user_id": user, "platform_username": "Tester One",
		"Token": "valid-" + user, "environment": "Live", "client_id": "x",
	})
	if status != http.StatusOK {
		t.Fatalf("login: %d %+v", status, env.Error)
	}
	var ar authResponse
	if err := json.Unmarshal(env.Results, &ar); err != nil {
		t.Fatal(err)
	}
	return ar.Prefix + " " + ar.Token, ar
}

func TestRouteTableCoversEveryEndpoint(t *testing.T) {
	s := &Server{}
	seen := map[string]bool{}
	for _, r := range s.routes() {
		if seen[r.path] {
			t.Fatalf("duplicate route %s", r.path)
		}
		seen[r.path] = true
	}
	// 91 endpoint literals from the exe + nop/.
	if len(seen) != 92 {
		t.Fatalf("route table has %d paths, want 92", len(seen))
	}
}

func TestEveryRouteHasALogCategory(t *testing.T) {
	for _, r := range (&Server{}).routes() {
		if c := categoryOf(r.path); !slices.Contains(logx.All, c) {
			t.Errorf("%s logs as %q, which is not in logx.All", r.path, c)
		}
	}
}

// With log_bodies on, no identifying value of a login reaches the log verbatim.
func TestLogsCarryTagsNotIdentities(t *testing.T) {
	var buf bytes.Buffer
	logs, err := logx.New(slog.NewTextHandler(&buf, nil), nil, []byte("test key"))
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := config.Default()
	cfg.LogBodies = true
	s := New(cfg, st, token.NewIssuer(strings.Repeat("k", 32), time.Hour),
		map[string]platform.Verifier{platform.Steam: fakeVerifier{platform: platform.Steam}}, logs)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	authz, ar := login(t, srv, platform.Steam, "76561198000000001")
	call(t, srv, "POST", "players/crossplay/", authz, map[string]bool{"enable_crossplay": true})
	call(t, srv, "POST", "auth/jwt/", "", map[string]string{"platform": "stm", "platform_user_id": "76561198000000009",
		"token": "forged-ticket"})

	out := buf.String()
	_, sessionToken, _ := strings.Cut(authz, " ")
	for _, secret := range []string{"76561198000000001", "76561198000000009", "valid-76561198000000001",
		"forged-ticket", sessionToken, "Tester", "TesterOne#"} {
		if strings.Contains(out, secret) {
			t.Errorf("log contains %q:\n%s", secret, out)
		}
	}
	for _, want := range []string{"player=" + logs.TagID(int64(ar.UserID)), "platform_user=" + logs.Tag("76561198000000009")} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %q:\n%s", want, out)
		}
	}
}

func TestLoginFlow(t *testing.T) {
	srv := newTestServer(t)

	// Ping works before auth (AttemptToGoOnline precedes CheckAuthentication).
	if status, _ := call(t, srv, "GET", "players/ping/", "", nil); status != http.StatusOK {
		t.Fatalf("anonymous ping: %d", status)
	}

	authz, ar := login(t, srv, platform.Steam, "76561198000000001")
	if ar.Prefix != "JWT" || ar.UserID == 0 || ar.TokenDuration != 3600 || ar.EncodedUserID == "" {
		t.Fatalf("auth response: %+v", ar)
	}
	// Logging in again is the same account.
	if _, again := login(t, srv, platform.Steam, "76561198000000001"); again.UserID != ar.UserID {
		t.Fatalf("second login got user %d, want %d", again.UserID, ar.UserID)
	}
	// Same platform user ID on another platform is a different account.
	if _, epic := login(t, srv, platform.Epic, "76561198000000001"); epic.UserID == ar.UserID {
		t.Fatal("epic and steam accounts collided")
	}

	for _, path := range []string{"players/ping/", "multiplayer/vivox/config/", "multiplayer/vivox/gen_token/",
		"store/song_manifest/get/", "players/active_battle_season/", "main_menu/promotions/get_active/",
		"main_stage/refresh_urls/", "bi/events/start_session/"} {
		if status, env := call(t, srv, "POST", path, authz, map[string]any{}); status != http.StatusOK {
			t.Fatalf("%s: %d %+v", path, status, env.Error)
		}
	}

	if status, _ := call(t, srv, "POST", "players/crossplay/", authz, map[string]bool{"enable_crossplay": false}); status != http.StatusOK {
		t.Fatalf("crossplay: %d", status)
	}

	_, env := call(t, srv, "GET", "auth/steam/owned_dlc/", authz, nil)
	var dlc struct {
		OwnedDLC []int32 `json:"owned_dlc"`
	}
	json.Unmarshal(env.Results, &dlc)
	if len(dlc.OwnedDLC) != 1 || dlc.OwnedDLC[0] != 1500001 {
		t.Fatalf("owned_dlc = %v", dlc.OwnedDLC)
	}

	_, env = call(t, srv, "GET", "store/song_manifest/get/", "", nil)
	var manifest struct {
		Songs   []string `json:"songs"`
		Version int32    `json:"version"`
	}
	json.Unmarshal(env.Results, &manifest)
	if len(manifest.Songs) != 2 || manifest.Version != 738126010 {
		t.Fatalf("manifest = %+v", manifest)
	}

	// Battles are off by default: the client's own "no season" name.
	_, env = call(t, srv, "GET", "players/active_battle_season/", "", nil)
	var season struct {
		SeasonName string `json:"season_name"`
	}
	json.Unmarshal(env.Results, &season)
	if season.SeasonName != "noseason" {
		t.Fatalf("default season = %q", season.SeasonName)
	}
	on := newTestServerWith(t, func(c *config.Config) { c.BattleSeason.Enabled = true })
	_, env = call(t, on, "GET", "players/active_battle_season/", "", nil)
	season.SeasonName = ""
	json.Unmarshal(env.Results, &season)
	if season.SeasonName != "season2" {
		t.Fatalf("enabled season = %q", season.SeasonName)
	}
}

func TestAuthFailures(t *testing.T) {
	srv := newTestServer(t)
	cases := []struct {
		name   string
		body   map[string]string
		status int
	}{
		{"bad ticket", map[string]string{"platform": "stm", "platform_user_id": "1", "token": "forged"}, 401},
		{"ticket for another user", map[string]string{"platform": "stm", "platform_user_id": "1", "token": "valid-2"}, 401},
		{"unsupported platform", map[string]string{"platform": "xbl", "platform_user_id": "1", "token": "valid-1"}, 400},
		{"missing fields", map[string]string{"platform": "stm"}, 400},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			status, env := call(t, srv, "POST", "auth/jwt/", "", c.body)
			if status != c.status || env.Error == nil {
				t.Fatalf("got %d %+v, want %d error", status, env.Error, c.status)
			}
		})
	}

	// Endpoints that need a player reject missing and forged session tokens.
	for _, authz := range []string{"", "JWT not-a-token", "Bearer x"} {
		if status, _ := call(t, srv, "POST", "players/crossplay/", authz, map[string]bool{}); status != 401 {
			t.Fatalf("crossplay with %q: %d, want 401", authz, status)
		}
	}
}

func TestStubsAndUnknownPaths(t *testing.T) {
	srv := newTestServer(t)
	status, env := call(t, srv, "POST", "recordings/get_popular/", "", map[string]any{})
	if status != 200 || string(env.Results) != "{}" {
		t.Fatalf("stub: %d %s", status, env.Results)
	}
	if status, _ := call(t, srv, "GET", "no/such/endpoint/", "", nil); status != 404 {
		t.Fatalf("unknown path: %d", status)
	}
	// {$} anchoring: a known path with a suffix is not the known endpoint.
	if status, _ := call(t, srv, "GET", "auth/jwt/extra/", "", nil); status != 404 {
		t.Fatalf("suffixed path: %d", status)
	}
}
