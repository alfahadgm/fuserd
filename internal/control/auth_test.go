package control

import (
	"bytes"
	"context"
	"encoding/json"
	"fuseronline/internal/config"
	"fuseronline/internal/logx"
	"fuseronline/internal/store"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOriginAndAuthenticationIsolation(t *testing.T) {
	for _, origin := range []string{"", "http://example.org", "https://good.org/path", "https://good.org?foo=x", "https://user:pass@good.org"} {
		if ValidateOrigin(origin) == nil {
			t.Fatalf("accepted %q", origin)
		}
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "auth.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	auth, err := New(config.ControlConfig{Origin: "https://good.org"}, st, testLogs(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	auth.Register(mux)
	mux.Handle("GET /control/private", auth.Protect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { JSON(w, 200, true) })))
	do := func(method, path, origin, body string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Origin", origin)
		r.Header.Set("Authorization", "JWT game-token")
		for _, c := range cookies {
			r.AddCookie(c)
		}
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w
	}
	if w := do("GET", "/control/private", "", ""); w.Code != 401 {
		t.Fatal("player token granted admin access")
	}
	if w := do("POST", "/control/auth/login/begin", "https://evil.org", "{}"); w.Code != 403 {
		t.Fatal("cross-origin login accepted")
	}
	token, _, err := st.ControlToken(context.Background(), "operator", time.Minute, store.ControlCLI)
	if err != nil {
		t.Fatal(err)
	}
	w := do("POST", "/control/auth/enroll/begin", "https://good.org", `{"token":"`+token+`"}`)
	if w.Code != 200 {
		t.Fatalf("enroll begin: %d %s", w.Code, w.Body.String())
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != "__Host-fuser_ceremony" || cookies[0].Path != "/" || cookies[0].Domain != "" || !cookies[0].HttpOnly || !cookies[0].Secure || cookies[0].SameSite != http.SameSiteStrictMode {
		t.Fatal("insecure ceremony cookie")
	}
	w = do("POST", "/control/auth/enroll/finish", "https://good.org", "{}", cookies...)
	if w.Code != 401 {
		t.Fatalf("invalid enrollment: %d", w.Code)
	}
	w = do("POST", "/control/auth/enroll/finish", "https://good.org", "{}", cookies...)
	if w.Code != 400 {
		t.Fatal("ceremony replay accepted")
	}
	if _, e := st.ControlTokenLookup(context.Background(), store.ControlHash(token)); e != nil {
		t.Fatal("invalid assertion consumed enrollment token")
	}
}

func testLogs(t *testing.T, w io.Writer) *logx.Logs {
	t.Helper()
	if w == nil {
		w = io.Discard
	}
	// Only AUTH is selected: CONTROL must be output regardless.
	l, err := logx.New(slog.NewTextHandler(w, nil), []string{"AUTH"}, []byte("test key"))
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// signedIn enrolls an administrator directly in the store and returns a session cookie for them.
func signedIn(t *testing.T, st *store.Store, name string) (store.ControlAdmin, *http.Cookie) {
	t.Helper()
	ctx := context.Background()
	token, _, err := st.ControlToken(ctx, name, time.Minute, store.ControlCLI)
	if err != nil {
		t.Fatal(err)
	}
	a := store.ControlAdmin{ID: name + "-id", Name: name, CredentialID: name + "-cred", Credential: []byte(`{}`)}
	if err := st.ControlEnroll(ctx, store.ControlHash(token), a); err != nil {
		t.Fatal(err)
	}
	if err := st.ControlSession(ctx, store.ControlHash(name+"-session"), a.ID, time.Now().Add(time.Hour), ""); err != nil {
		t.Fatal(err)
	}
	return a, &http.Cookie{Name: "__Host-fuser_control", Value: name + "-session"}
}

func TestAdminManagementAndAudit(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "admins.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var logs bytes.Buffer
	auth, err := New(config.ControlConfig{Origin: "https://good.org"}, st, testLogs(t, &logs))
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	auth.Register(mux)
	mux.Handle("POST /control/test/audit", auth.Protect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth.Audit(r, "users.save", "12345", "banned=true")
		JSON(w, 200, true)
	})))
	do := func(method, path, origin, body string, c *http.Cookie) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		r.AddCookie(c)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w
	}
	const good = "https://good.org"
	ctx := context.Background()
	alice, aliceCookie := signedIn(t, st, "alice")
	bob, bobCookie := signedIn(t, st, "bob")

	if w := do("POST", "/control/api/admins/tokens", "https://evil.org", `{"name":"carol","minutes":15}`, aliceCookie); w.Code != 403 {
		t.Fatalf("cross-origin token creation: %d", w.Code)
	}
	if w := do("POST", "/control/api/admins/tokens", good, `{"name":"carol","minutes":15}`, &http.Cookie{Name: "__Host-fuser_control", Value: "forged"}); w.Code != 401 {
		t.Fatalf("token creation without a session: %d", w.Code)
	}
	for _, body := range []string{`{"name":"carol","minutes":0}`, `{"name":"carol","minutes":61}`, `{"name":" ","minutes":5}`, `{"name":"carol","minutes":5,"admin":true}`} {
		if w := do("POST", "/control/api/admins/tokens", good, body, aliceCookie); w.Code != 400 {
			t.Fatalf("%s accepted: %d", body, w.Code)
		}
	}
	w := do("POST", "/control/api/admins/tokens", good, `{"name":"carol","minutes":15}`, aliceCookie)
	var created struct{ ID, Token string }
	if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &created) != nil || len(created.Token) != 16 || len(created.ID) != 16 {
		t.Fatalf("token creation: %d %s", w.Code, w.Body.String())
	}
	if pt, err := st.ControlTokenLookup(ctx, store.ControlHash(created.Token)); err != nil || pt.CreatedBy != alice.ID || pt.Expires.Sub(time.Now()) > 15*time.Minute {
		t.Fatalf("stored token %+v %v", pt, err)
	}
	w = do("GET", "/control/api/admins", "", "", aliceCookie)
	var list struct {
		Self   string
		Admins []struct{ ID, Name string }
		Tokens []struct {
			ID, Name  string
			CreatedBy string `json:"created_by"`
		}
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &list) != nil || list.Self != alice.ID || len(list.Admins) != 2 || len(list.Tokens) != 1 || list.Tokens[0].CreatedBy != "alice" {
		t.Fatalf("admin list: %d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), created.Token) {
		t.Fatal("admin list exposed an enrollment token")
	}
	if w := do("DELETE", "/control/api/admins/tokens/"+created.ID, good, "", aliceCookie); w.Code != 200 {
		t.Fatalf("cancel token: %d", w.Code)
	}
	if w := do("DELETE", "/control/api/admins/tokens/"+created.ID, good, "", aliceCookie); w.Code != 404 {
		t.Fatalf("cancel token twice: %d", w.Code)
	}
	if _, err := st.ControlTokenLookup(ctx, store.ControlHash(created.Token)); err == nil {
		t.Fatal("cancelled token still valid")
	}
	if w := do("DELETE", "/control/api/admins/"+alice.ID, good, "", aliceCookie); w.Code != 400 {
		t.Fatalf("self removal: %d", w.Code)
	}

	// Removing bob ends his session and cancels the token he issued.
	w = do("POST", "/control/api/admins/tokens", good, `{"name":"dave","minutes":5}`, bobCookie)
	if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &created) != nil {
		t.Fatalf("bob's token: %d", w.Code)
	}
	if w := do("DELETE", "/control/api/admins/"+bob.ID, "https://evil.org", "", aliceCookie); w.Code != 403 {
		t.Fatalf("cross-origin removal: %d", w.Code)
	}
	if w := do("DELETE", "/control/api/admins/"+bob.ID, good, "", aliceCookie); w.Code != 200 {
		t.Fatalf("remove bob: %d %s", w.Code, w.Body.String())
	}
	if w := do("GET", "/control/api/admins", "", "", bobCookie); w.Code != 401 {
		t.Fatalf("removed admin's session: %d", w.Code)
	}
	if _, err := st.ControlTokenLookup(ctx, store.ControlHash(created.Token)); err == nil {
		t.Fatal("removed admin's token still valid")
	}
	if w := do("DELETE", "/control/api/admins/"+bob.ID, good, "", aliceCookie); w.Code != 404 {
		t.Fatalf("remove bob twice: %d", w.Code)
	}

	if w := do("POST", "/control/test/audit", good, "", aliceCookie); w.Code != 200 {
		t.Fatal(w.Code)
	}
	entries, err := st.ControlAudit(ctx, 50)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]store.ControlAuditEntry{}
	for _, e := range entries {
		got[e.Action] = e
	}
	for _, action := range []string{"admins.token_create", "admins.token_cancel", "admins.revoke", "users.save"} {
		if e, ok := got[action]; !ok || e.AdminID != alice.ID || e.IP == "" {
			t.Fatalf("audit lacks %s by alice: %+v", action, entries)
		}
	}
	if got["users.save"].Target != "12345" || got["admins.revoke"].Detail != "bob" {
		t.Fatalf("audit targets %+v", entries)
	}
	out := logs.String()
	for _, want := range []string{"cat=CONTROL", "action=admins.revoke", "action=users.save"} {
		if !strings.Contains(out, want) {
			t.Fatalf("log lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "12345") || strings.Contains(out, "192.0.2.1") {
		t.Fatalf("log carries a player ID or client address verbatim:\n%s", out)
	}
}
