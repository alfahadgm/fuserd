package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"fuseronline/internal/catalog"
	"fuseronline/internal/config"
	"fuseronline/internal/platform"
	"fuseronline/internal/store"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestControlProtectionAndPublishedMedia(t *testing.T) {
	_, s := newTestServerAndAPI(t, nil)
	s.cfg.Control = config.ControlConfig{Enabled: true, Origin: "https://control.example.org"}
	h, err := s.ControlHandler()
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "/control/api/state", nil)
	r.Header.Set("Authorization", "JWT player-token")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatalf("unauth state %d", w.Code)
	}
	r = httptest.NewRequest("POST", "/control/api/assets", strings.NewReader("package"))
	r.Header.Set("Origin", s.cfg.Control.Origin)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatalf("unauth asset upload %d", w.Code)
	}
	r = httptest.NewRequest("GET", "/control/", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if !strings.Contains(w.Body.String(), "Enroll WebAuthN") || strings.Contains(w.Body.String(), "store-grid") || w.Header().Get("Cache-Control") != "no-store" || !strings.Contains(w.Header().Get("Content-Security-Policy"), "script-src 'self'") {
		t.Fatal("login page or security headers wrong")
	}
	var imageBytes bytes.Buffer
	im := image.NewRGBA(image.Rect(0, 0, 2, 2))
	im.Set(0, 0, color.RGBA{255, 0, 0, 255})
	png.Encode(&imageBytes, im)
	url, err := s.controlImage(context.Background(), "data:image/png;base64,"+base64.StdEncoding.EncodeToString(imageBytes.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	path := url[strings.Index(url, "/control/"):]
	r = httptest.NewRequest("GET", path, nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 || w.Header().Get("Content-Type") != "image/png" {
		t.Fatal("game artwork cannot download")
	}
	if _, _, e := image.Decode(bytes.NewReader(w.Body.Bytes())); e != nil {
		t.Fatal(e)
	}
	r = httptest.NewRequest("GET", path, nil)
	w = httptest.NewRecorder()
	s.cfg.Control.Enabled = false
	s.Handler().ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatal("published artwork disappeared with control disabled")
	}

	if _, err := s.controlImage(context.Background(), "data:image/svg+xml;base64,"+base64.StdEncoding.EncodeToString([]byte(`<svg></svg>`))); err == nil {
		t.Fatal("active image format accepted")
	}
}
func TestControlStorePurchasePromotionAndBan(t *testing.T) {
	srv, s := newTestServerAndAPI(t, func(c *config.Config) { c.Store.ElderCreditBalance = 1000 })
	cat := elderCatalogue(t)
	s.SetCatalogue(catalog.Static(cat))
	ctx := context.Background()
	items, err := s.controlItems(ctx)
	if err != nil {
		t.Fatal(err)
	}
	items[0].Price = 321
	v, _ := s.store.ControlDocumentVersion(ctx, "items")
	if err := s.store.ControlSaveDocument(ctx, "items", items, v); err != nil {
		t.Fatal(err)
	}
	_, env := call(t, srv, "GET", "store/elder_credit_store/get_list/", "", nil)
	var list ecList
	decodeResults(t, env, &list)
	if len(list.Featured) != 1 || list.Featured[0].Price != 321 {
		t.Fatalf("edited store %+v", list)
	}
	auth, loginResult := login(t, srv, platform.Epic, "controlled-buyer")
	status, env := call(t, srv, "POST", "store/elder_credit_store/purchase/", auth, map[string]any{"uuid": items[0].ID, "expected_price": 100})
	if status != 400 || env.Error.Code != "price_changed" {
		t.Fatal("old price accepted")
	}
	status, _ = call(t, srv, "POST", "store/elder_credit_store/purchase/", auth, map[string]any{"uuid": items[0].ID, "expected_price": 321})
	if status != 200 {
		t.Fatal("new price rejected")
	}
	p := store.ControlPromotion{ID: 7, Type: "hmx_promotion", Name: "Announcement", Message: "Carousel", Extra: "Long message", Enabled: true, Start: store.ControlStored(time.Now().Add(-time.Hour)), End: store.ControlStored(time.Now().Add(time.Hour)), Image: "https://control.example.org/image.png"}
	v, _ = s.store.ControlDocumentVersion(ctx, "promotion")
	if err := s.store.ControlSaveDocument(ctx, "promotion", p, v); err != nil {
		t.Fatal(err)
	}
	_, env = call(t, srv, "GET", "main_menu/promotions/get_active/", "", nil)
	var response struct {
		Promotions []map[string]any `json:"promotions"`
	}
	decodeResults(t, env, &response)
	if len(response.Promotions) != 1 || response.Promotions[0]["image_medium_download_url"] != p.Image || response.Promotions[0]["extra_text"] != p.Extra {
		t.Fatalf("promotion %+v", response)
	}
	id := int64(loginResult.UserID)
	player, _ := s.store.Player(ctx, id)
	if err := s.store.ControlUserSave(ctx, id, player.HypeUsername, true, "ban test"); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"main_stage/purchase_slot/", "players/ping/"} {
		status, _ := call(t, srv, "POST", path, auth, map[string]any{})
		if status != http.StatusUnauthorized {
			t.Fatalf("banned session accepted on %s: %d", path, status)
		}
	}
	raw, _ := json.Marshal(items)
	if bytes.Contains(raw, []byte("token")) {
		t.Fatal("token leaked")
	}
}

func TestControlChangesAreAudited(t *testing.T) {
	srv, s := newTestServerAndAPI(t, nil)
	s.cfg.Control = config.ControlConfig{Enabled: true, Origin: "https://control.example.org"}
	h, err := s.ControlHandler()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	token, _, err := s.store.ControlToken(ctx, "operator", time.Minute, store.ControlCLI)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.store.ControlEnroll(ctx, store.ControlHash(token), store.ControlAdmin{ID: "op", Name: "operator", CredentialID: "c", Credential: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if err := s.store.ControlSession(ctx, store.ControlHash("session"), "op", time.Now().Add(time.Hour), ""); err != nil {
		t.Fatal(err)
	}
	_, player := login(t, srv, platform.Epic, "audited-player")
	body := `{"id":` + fmt.Sprint(player.UserID) + `,"name":"Renamed","platform":"","key":"","online":false,"banned":true,"lastSeen":"","notes":"cheating"}`
	r := httptest.NewRequest("PUT", "/control/api/users", strings.NewReader(body))
	r.Header.Set("Origin", s.cfg.Control.Origin)
	r.AddCookie(&http.Cookie{Name: "__Host-fuser_control", Value: "session"})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("user save: %d %s", w.Code, w.Body.String())
	}
	entries, err := s.store.ControlAudit(ctx, 5)
	if err != nil || len(entries) == 0 {
		t.Fatalf("audit %+v %v", entries, err)
	}
	if e := entries[0]; e.Action != "users.save" || e.AdminID != "op" || e.Target != fmt.Sprint(player.UserID) || e.Detail != "banned=true" {
		t.Fatalf("audit entry %+v", e)
	}
}

func TestControlTimesFollowSessionZone(t *testing.T) {
	_, s := newTestServerAndAPI(t, nil)
	s.SetCatalogue(catalog.Static(elderCatalogue(t)))
	s.cfg.Control = config.ControlConfig{Enabled: true, Origin: "https://control.example.org", Timezone: "Australia/Perth"}
	h, err := s.ControlHandler()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	token, _, _ := s.store.ControlToken(ctx, "operator", time.Minute, store.ControlCLI)
	if err := s.store.ControlEnroll(ctx, store.ControlHash(token), store.ControlAdmin{ID: "op", Name: "operator", CredentialID: "c", Credential: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	// A promotion saved before the timezone setting: Perth wall times without an offset.
	legacy := store.ControlPromotion{ID: 1, Type: "hmx_promotion", Name: "n", Message: "m", Extra: "e", Start: "2026-10-09T08:00:00", End: "2026-10-10T08:00:00"}
	v, _ := s.store.ControlDocumentVersion(ctx, "promotion")
	if err := s.store.ControlSaveDocument(ctx, "promotion", legacy, v); err != nil {
		t.Fatal(err)
	}
	do := func(session, method, path, body, ifMatch string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Origin", s.cfg.Control.Origin)
		if ifMatch != "" {
			r.Header.Set("If-Match", ifMatch)
		}
		r.AddCookie(&http.Cookie{Name: "__Host-fuser_control", Value: session})
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	state := func(session string) (out struct {
		Timezone   string
		Promotions []store.ControlPromotion
		Versions   map[string]string
	}) {
		w := do(session, "GET", "/control/api/state", "", "")
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil {
			t.Fatalf("state: %d %s", w.Code, w.Body.String())
		}
		return
	}
	s.store.ControlSession(ctx, store.ControlHash("london"), "op", time.Now().Add(time.Hour), "Europe/London")
	s.store.ControlSession(ctx, store.ControlHash("default"), "op", time.Now().Add(time.Hour), "")
	// 08:00 Perth is 00:00 UTC, 01:00 in London (BST).
	// The legacy single-promotion document reads as a one-item list.
	if got := state("london"); got.Timezone != "Europe/London" || len(got.Promotions) != 1 || got.Promotions[0].Start != "2026-10-09T01:00:00" {
		t.Fatalf("London session: %+v", got)
	}
	got := state("default")
	if got.Timezone != "Australia/Perth" || len(got.Promotions) != 1 || got.Promotions[0].Start != "2026-10-09T08:00:00" {
		t.Fatalf("default session: %+v", got)
	}
	// Saving from London stores an absolute time that the game reads correctly.
	p := got.Promotions[0]
	p.Start, p.End = "2026-10-09T02:00:00", "2026-10-09T03:00"
	body, _ := json.Marshal([]store.ControlPromotion{p})
	if w := do("london", "PUT", "/control/api/promotions", string(body), got.Versions["promotions"]); w.Code != 200 {
		t.Fatalf("save: %d %s", w.Code, w.Body.String())
	}
	var stored store.ControlPromotions
	s.store.ControlDocument(ctx, "promotions", &stored)
	if len(stored.Promotions) != 1 || stored.Promotions[0].Start != "2026-10-09T01:00:00Z" || stored.Promotions[0].End != "2026-10-09T02:00:00Z" ||
		stored.Promotions[0].ID != 1 {
		t.Fatalf("stored %+v", stored)
	}
	if got := state("default"); got.Promotions[0].Start != "2026-10-09T09:00:00" {
		t.Fatalf("Perth view of the London save: %s", got.Promotions[0].Start)
	}
}
