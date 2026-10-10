package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"fuseronline/internal/config"
	"fuseronline/internal/store"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// FUSER turns a 16-bit image into a PF_FloatRGBA texture whose integers read as half floats (an overbright bloom), so
// every control image must be served as 8-bit: a JPEG (YCbCr) or 16-bit PNG upload included.
func TestControlImagesAre8Bit(t *testing.T) {
	_, s := newTestServerAndAPI(t, nil)
	src := image.NewNRGBA64(image.Rect(0, 0, 4, 3))
	for x := 0; x < 4; x++ {
		src.Set(x, 1, color.NRGBA64{0xffff, 0x8000, 0, 0xffff})
	}
	var png16, jpg bytes.Buffer
	if err := png.Encode(&png16, src); err != nil {
		t.Fatal(err)
	}
	if pngBitDepth(png16.Bytes()) != 16 {
		t.Fatal("test image is not 16-bit")
	}
	if err := jpeg.Encode(&jpg, src, nil); err != nil {
		t.Fatal(err)
	}
	fetch := func(url string) []byte {
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, httptest.NewRequest("GET", url[strings.Index(url, "/control/"):], nil))
		if w.Code != 200 {
			t.Fatalf("%s: %d", url, w.Code)
		}
		return w.Body.Bytes()
	}
	for name, b := range map[string][]byte{"png16": png16.Bytes(), "jpeg": jpg.Bytes()} {
		url, err := s.controlImage(context.Background(), "data:image/x;base64,"+base64.StdEncoding.EncodeToString(b))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		got := fetch(url)
		if d := pngBitDepth(got); d != 8 {
			t.Errorf("%s: served bit depth %d, want 8", name, d)
		}
		if im, err := png.Decode(bytes.NewReader(got)); err != nil || im.Bounds().Dx() != 4 || im.Bounds().Dy() != 3 {
			t.Errorf("%s: served image %v %v", name, err, im)
		}
	}
	// Media stored as 16-bit is converted when served.
	hash, err := s.store.ControlMediaSave(context.Background(), png16.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	got := fetch(s.cfg.PublicURL + "/control/media/" + hash)
	if d := pngBitDepth(got); d != 8 {
		t.Fatalf("stored 16-bit media served at bit depth %d", d)
	}
	im, err := png.Decode(bytes.NewReader(got))
	if err != nil {
		t.Fatal(err)
	}
	if r, g, b, a := im.At(2, 1).RGBA(); r>>8 != 0xff || g>>8 != 0x80 || b != 0 || a>>8 != 0xff {
		t.Errorf("converted pixel %x %x %x %x", r, g, b, a)
	}
}

// Several promotions: kept in carousel order; a promotion keeps its id while its image is unchanged; a new one or a
// changed image takes a never-used id (a running FUSER keeps images by promotion_id); at most maxPromotions.
func TestPromotionsList(t *testing.T) {
	srv, s := newTestServerAndAPI(t, nil)
	s.cfg.Control = config.ControlConfig{Enabled: true, Origin: "https://control.example.org", Timezone: "UTC"}
	h, err := s.ControlHandler()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	token, _, _ := s.store.ControlToken(ctx, "operator", time.Minute, store.ControlCLI)
	if err := s.store.ControlEnroll(ctx, store.ControlHash(token), store.ControlAdmin{ID: "op", Name: "operator", CredentialID: "c", Credential: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	s.store.ControlSession(ctx, store.ControlHash("session"), "op", time.Now().Add(time.Hour), "")
	wall := func(d time.Duration) string { return time.Now().UTC().Add(d).Format("2006-01-02T15:04:05") }
	promo := func(id int, name, image string) store.ControlPromotion {
		return store.ControlPromotion{ID: id, Name: name, Message: "m", Extra: "e", Enabled: true, Start: wall(-time.Hour), End: wall(time.Hour), Image: image}
	}
	put := func(list []store.ControlPromotion) (int, []store.ControlPromotion) {
		t.Helper()
		v, _ := s.store.ControlDocumentVersion(ctx, "promotions")
		body, _ := json.Marshal(list)
		r := httptest.NewRequest("PUT", "/control/api/promotions", strings.NewReader(string(body)))
		r.Header.Set("Origin", s.cfg.Control.Origin)
		r.Header.Set("If-Match", v)
		r.AddCookie(&http.Cookie{Name: "__Host-fuser_control", Value: "session"})
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		var stored store.ControlPromotions
		s.store.ControlDocument(ctx, "promotions", &stored)
		return w.Code, stored.Promotions
	}
	ids := func(list []store.ControlPromotion) []int {
		out := []int{}
		for _, p := range list {
			out = append(out, p.ID)
		}
		return out
	}
	active := func() []string {
		t.Helper()
		var res struct {
			Promotions []struct {
				ID   int    `json:"promotion_id"`
				Name string `json:"name"`
				Type string `json:"promotion_type"`
			} `json:"promotions"`
		}
		_, env := call(t, srv, "GET", "main_menu/promotions/get_active/", "", nil)
		decodeResults(t, env, &res)
		out := []string{}
		for _, p := range res.Promotions {
			if p.Type != "hmx_promotion" {
				t.Fatalf("type %q", p.Type)
			}
			out = append(out, fmt.Sprintf("%d:%s", p.ID, p.Name))
		}
		return out
	}

	_, got := put([]store.ControlPromotion{promo(0, "A", "https://cdn.example.org/a.png"), promo(0, "B", "https://cdn.example.org/b.png")})
	if fmt.Sprint(ids(got)) != "[1 2]" || fmt.Sprint(active()) != "[1:A 2:B]" {
		t.Fatalf("first save %v, active %v", ids(got), active())
	}
	// Reordered, unchanged images: same ids, new order.
	_, got = put([]store.ControlPromotion{got[1], got[0]})
	if fmt.Sprint(ids(got)) != "[2 1]" || fmt.Sprint(active()) != "[2:B 1:A]" {
		t.Fatalf("reordered %v, active %v", ids(got), active())
	}
	// B's image changes: a new id; A keeps its own.
	got[0].Image = "https://cdn.example.org/b2.png"
	_, got = put(got)
	if fmt.Sprint(ids(got)) != "[3 1]" {
		t.Fatalf("after B's new image %v", ids(got))
	}
	// A removed and a new one added: ids are never reused.
	_, got = put([]store.ControlPromotion{got[0], promo(0, "C", "")})
	if fmt.Sprint(ids(got)) != "[3 4]" {
		t.Fatalf("after removing A %v", ids(got))
	}
	// A repeated id (a copied entry) takes a new one.
	_, got = put([]store.ControlPromotion{got[0], got[1], got[1]})
	if fmt.Sprint(ids(got)) != "[3 4 5]" {
		t.Fatalf("a repeated id %v", ids(got))
	}
	// Disabled, ended and future promotions are not sent.
	off, ended, future := got[1], got[2], promo(0, "F", "")
	off.Enabled = false
	ended.End = wall(-time.Minute)
	future.Start, future.End = wall(time.Hour), wall(2*time.Hour)
	_, got = put([]store.ControlPromotion{got[0], off, ended, future})
	if fmt.Sprint(active()) != "[3:B]" {
		t.Fatalf("active %v", active())
	}
	too := make([]store.ControlPromotion, maxPromotions+1)
	for i := range too {
		too[i] = promo(0, fmt.Sprint("P", i), "")
	}
	if code, _ := put(too); code == 200 {
		t.Fatalf("%d promotions accepted", len(too))
	}
	// An enabled promotion needs its title and both texts.
	empty := promo(0, " ", "")
	if code, _ := put([]store.ControlPromotion{empty}); code == 200 {
		t.Fatal("an enabled promotion without a title accepted")
	}
}
