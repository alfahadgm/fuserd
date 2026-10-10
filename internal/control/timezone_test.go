package control

import (
	"context"
	"encoding/json"
	"fuseronline/internal/config"
	"fuseronline/internal/store"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestTimezoneSetting(t *testing.T) {
	for _, c := range []struct {
		name, want string
		ok         bool
	}{{"", "UTC", true}, {"Australia/Perth", "Australia/Perth", true}, {"Local", "", false}, {"Mars/Olympus", "", false}} {
		loc, err := config.ControlConfig{Timezone: c.name}.Location()
		if (err == nil) != c.ok || c.ok && loc.String() != c.want {
			t.Errorf("%q: %v %v", c.name, loc, err)
		}
	}
	if _, err := New(config.ControlConfig{Origin: "https://good.org", Timezone: "Nowhere/Land"}, nil, testLogs(t, nil)); err == nil {
		t.Fatal("invalid control.timezone accepted")
	}
}

func TestSessionTimezoneFromBrowser(t *testing.T) {
	a := newAuth(t, config.ControlConfig{Origin: "https://good.org", Timezone: "Australia/Perth"})
	mux := http.NewServeMux()
	a.Register(mux)
	ctx := context.Background()
	admin, _ := signedIn(t, a.st, "zoned")
	for _, c := range []struct{ header, want string }{
		{"Europe/London", "Europe/London"},
		{"", "Australia/Perth"},          // no indicator: the server default
		{"Not/AZone", "Australia/Perth"}, // unknown to the server
		{"Local", "Australia/Perth"},     // the server's own zone, not the browser's
		{"../../etc/passwd", "Australia/Perth"},
	} {
		r := httptest.NewRequest("POST", "/control/auth/login/finish", nil)
		r.Header.Set("X-Timezone", c.header)
		w := httptest.NewRecorder()
		if err := a.session(w, r, admin.ID); err != nil {
			t.Fatal(err)
		}
		cookie := w.Result().Cookies()[0]
		req := httptest.NewRequest("GET", "/control/auth/session", nil)
		req.AddCookie(cookie)
		w = httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		var got struct{ Timezone string }
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &got) != nil || got.Timezone != c.want {
			t.Errorf("X-Timezone %q: session zone %q (%d)", c.header, got.Timezone, w.Code)
		}
		stored := "" // an unusable indicator stores nothing, so the session follows control.timezone
		if c.want == c.header {
			stored = c.header
		}
		if _, zone, err := a.st.ControlSessionLookup(ctx, store.ControlHash(cookie.Value)); err != nil || zone != stored {
			t.Errorf("X-Timezone %q stored as %q: %v", c.header, zone, err)
		}
	}
	if loc := Location(ctx); loc != time.UTC {
		t.Fatalf("no session: %v", loc)
	}
}
