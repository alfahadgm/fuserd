package control

import (
	"fmt"
	"fuseronline/internal/config"
	"fuseronline/internal/store"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func newAuth(t *testing.T, cfg config.ControlConfig) *Auth {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "limits.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	a, err := New(cfg, st, testLogs(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func from(method, target, peer string) *http.Request {
	r := httptest.NewRequest(method, target, strings.NewReader("{}"))
	r.RemoteAddr = peer
	r.Header.Set("Origin", "https://good.org")
	return r
}

func TestClientKey(t *testing.T) {
	for _, c := range []struct{ a, b string }{
		{"[2001:db8:1:2:aaaa::1]:5000", "[2001:db8:1:2:ffff::9]:6000"}, // one /64
		{"[::ffff:203.0.113.5]:1", "203.0.113.5:2"},                    // IPv4-mapped is IPv4
	} {
		if ka, kb := clientKey(from("GET", "/", c.a)), clientKey(from("GET", "/", c.b)); ka != kb {
			t.Errorf("%s and %s keyed apart: %s, %s", c.a, c.b, ka, kb)
		}
	}
	if clientKey(from("GET", "/", "[2001:db8:1:2::1]:1")) == clientKey(from("GET", "/", "[2001:db8:1:3::1]:1")) {
		t.Error("different /64s share a key")
	}
	if clientKey(from("GET", "/", "203.0.113.5:1")) == clientKey(from("GET", "/", "203.0.113.6:1")) {
		t.Error("different IPv4 addresses share a key")
	}
}

func TestRateLimitTableFullDoesNotLockOut(t *testing.T) {
	a := newAuth(t, config.ControlConfig{Origin: "https://good.org"})
	for i := 0; i < maxClients; i++ {
		if !a.Allow(from("POST", "/", fmt.Sprintf("[2001:db8:%x:%x::1]:1", i/65536, i%65536))) {
			t.Fatal("fresh client refused while filling")
		}
	}
	admin := from("POST", "/", "198.51.100.7:1")
	for i := 0; i < authAttempts; i++ {
		if !a.Allow(admin) {
			t.Fatalf("new client refused at attempt %d with a full table", i+1)
		}
	}
	if a.Allow(admin) {
		t.Fatal("client exceeded its own limit")
	}
	if len(a.limits) > maxClients {
		t.Fatalf("table grew to %d", len(a.limits))
	}
	// Every address in one /64 shares the limit.
	for i := 0; i < authAttempts; i++ {
		a.Allow(from("POST", "/", fmt.Sprintf("[2001:db8:ffff:1::%x]:1", i+1)))
	}
	if a.Allow(from("POST", "/", "[2001:db8:ffff:1::abcd]:1")) {
		t.Fatal("rotating addresses within a /64 escaped the limit")
	}
}

func TestPendingCeremoniesEvictOldest(t *testing.T) {
	a := newAuth(t, config.ControlConfig{Origin: "https://good.org"})
	begin := func(peer string) *http.Cookie {
		t.Helper()
		w := httptest.NewRecorder()
		a.beginLogin(w, from("POST", "/control/auth/login/begin", peer))
		if w.Code != 200 || len(w.Result().Cookies()) != 1 {
			t.Fatalf("begin from %s: %d %s", peer, w.Code, w.Body.String())
		}
		return w.Result().Cookies()[0]
	}
	pending := func(c *http.Cookie) bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		_, ok := a.pending[store.ControlHash(c.Value)]
		return ok
	}
	// A client keeps only its newest pendingPerClient ceremonies.
	first := begin("192.0.2.1:1")
	var rest []*http.Cookie
	for i := 0; i < pendingPerClient; i++ {
		rest = append(rest, begin("192.0.2.1:1"))
	}
	if pending(first) {
		t.Fatal("client's oldest ceremony kept beyond its share")
	}
	for _, c := range rest {
		if !pending(c) {
			t.Fatal("client's newer ceremony dropped")
		}
	}
	// A full table drops its oldest ceremony instead of refusing a new client.
	for i := len(a.pending); i < maxPending; i++ {
		a.mu.Lock()
		a.pending[fmt.Sprint("filler", i)] = ceremony{client: fmt.Sprint("filler", i), expires: rest[0].Expires}
		a.mu.Unlock()
	}
	admin := begin("198.51.100.7:1")
	if !pending(admin) || len(a.pending) != maxPending {
		t.Fatalf("new ceremony refused or table grew: %d", len(a.pending))
	}
}

func TestAllowedNetworks(t *testing.T) {
	for _, bad := range [][]string{{"203.0.113.7"}, {"not a network"}, {"203.0.113.0/33"}, {""}} {
		cfg := config.ControlConfig{Origin: "https://good.org", AllowedNetworks: bad}
		if _, err := cfg.Networks(); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	a := newAuth(t, config.ControlConfig{Origin: "https://good.org", AllowedNetworks: []string{"203.0.113.0/24", " 2001:db8:5::/48 "}})
	reached := false
	h := a.Restrict(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached = true }))
	for _, c := range []struct {
		method, path, peer string
		ok                 bool
	}{
		{"GET", "/control/", "203.0.113.200:1", true},
		{"POST", "/control/auth/login/begin", "[::ffff:203.0.113.9]:1", true},
		{"GET", "/control/", "[2001:db8:5:ffff::1]:1", true},
		{"GET", "/control/", "198.51.100.1:1", false},
		{"POST", "/control/auth/login/begin", "198.51.100.1:1", false},
		{"GET", "/control/api/state", "[2001:db8:6::1]:1", false},
		{"GET", "/control/media/" + strings.Repeat("a", 64), "198.51.100.1:1", true}, // the game's artwork stays public
		{"POST", "/control/media/x", "198.51.100.1:1", false},
		{"GET", "/control/media/../api/state", "198.51.100.1:1", false},
		{"GET", "/control/", "garbage", false},
	} {
		reached = false
		w := httptest.NewRecorder()
		h.ServeHTTP(w, from(c.method, c.path, c.peer))
		if reached != c.ok || !c.ok && w.Code != 403 {
			t.Errorf("%s %s from %s: reached=%v code=%d", c.method, c.path, c.peer, reached, w.Code)
		}
	}
	open := newAuth(t, config.ControlConfig{Origin: "https://good.org"})
	reached = false
	open.Restrict(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached = true })).ServeHTTP(httptest.NewRecorder(), from("GET", "/control/", "198.51.100.1:1"))
	if !reached {
		t.Fatal("empty allowed_networks refused an address")
	}
}

func TestCookieNamesAndHSTS(t *testing.T) {
	secure := newAuth(t, config.ControlConfig{Origin: "https://good.org"})
	dev := newAuth(t, config.ControlConfig{Origin: "http://localhost:8000"})
	if secure.sessionCookie != "__Host-fuser_control" || secure.ceremonyCookie != "__Host-fuser_ceremony" {
		t.Fatal("HTTPS origin without __Host- cookies")
	}
	if dev.sessionCookie != "fuser_control" {
		t.Fatal("HTTP localhost cookies cannot use __Host-, which needs Secure")
	}
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	for _, c := range []struct {
		a      *Auth
		target string
		want   bool
	}{
		{secure, "https://good.org/control/", true},
		{secure, "http://good.org/control/", false}, // not over TLS
		{dev, "https://localhost:8000/control/", false},
	} {
		w := httptest.NewRecorder()
		c.a.Headers(ok).ServeHTTP(w, httptest.NewRequest("GET", c.target, nil))
		got := w.Header().Get("Strict-Transport-Security")
		if (got != "") != c.want || c.want && (got != hstsHeader || strings.Contains(got, "includeSubDomains")) {
			t.Errorf("%s: HSTS %q", c.target, got)
		}
		if w.Header().Get("X-Frame-Options") != "DENY" {
			t.Errorf("%s: security headers missing", c.target)
		}
	}
}
