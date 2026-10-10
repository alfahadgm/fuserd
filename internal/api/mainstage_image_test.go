package api

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/jpeg"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"fuseronline/internal/config"
	"fuseronline/internal/mainstage"
	"fuseronline/internal/platform"
	"fuseronline/internal/store"
)

func TestMainStageEventCard(t *testing.T) {
	t0 := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	pics := t.TempDir()
	srv, s := newTestServerAndAPI(t, func(c *config.Config) {
		c.MainStage.Enabled = true
		c.MainStage.Rotation.Anchor = t0.Format(time.RFC3339)
		c.ProfilePics.Dir = pics
	})
	s.cfg.PublicURL = srv.URL
	now := t0.Add(-30 * time.Minute)
	s.now = func() time.Time { return now }
	ctx := context.Background()
	next := mainstage.RotationUUID(t0)

	imageOf := func(endpoint string) map[string]any {
		t.Helper()
		imgs := rawGet(t, srv.URL+Prefix+endpoint, "")["images"].([]any)
		if len(imgs) != 1 {
			t.Fatalf("%s images: %v", endpoint, imgs)
		}
		return imgs[0].(map[string]any)
	}
	image0 := func() map[string]any { t.Helper(); return imageOf("mainstage/next_event/") }
	fetch := func(u string, w, h int) image.Image {
		t.Helper()
		status, body, hdr := rawRequest(t, "GET", u, nil)
		if status != http.StatusOK || hdr.Get("Content-Type") != "image/jpeg" {
			t.Fatalf("GET %s: %d %s", u, status, hdr.Get("Content-Type"))
		}
		img, err := jpeg.Decode(bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if b := img.Bounds(); b.Dx() != w || b.Dy() != h {
			t.Fatalf("%s is %v, want %dx%d", u, b, w, h)
		}
		return img
	}
	// Pixels inside tiles on the large card (240 px tiles 40 px apart, from y 310), left of each tile's centre so clear
	// of an open slot's number. With all 4 slots drawn, slot i starts at x 100 + (i-1)*280; a lone tile at x 520.
	px := func(img image.Image, x int) (r, g, b uint32) {
		r, g, b, _ = img.At(x, 370).RGBA()
		return r >> 8, g >> 8, b >> 8
	}
	const slot1Of4, slot2Of4, lone = 160, 440, 580
	isRed := func(img image.Image, x int) bool { r, g, b := px(img, x); return r >= 0xc0 && g <= 0x50 && b <= 0x50 }
	isOpen := func(img image.Image, x int) bool { _, g, _ := px(img, x); return g >= 0x18 && g <= 0x2c } // cardOpen, not background

	// Purchase open, nobody booked: four open slots.
	empty := image0()
	large := empty["image_download_url"].(string)
	if !mainstage.IsGUID(empty["uuid"].(string)) || !strings.HasPrefix(large, srv.URL+Prefix+"mainstage/image/") ||
		!strings.HasSuffix(large, "/large.jpg") {
		t.Fatalf("empty card: %v", empty)
	}
	if img := fetch(large, 1280, 720); !isOpen(img, slot1Of4) || !isOpen(img, slot2Of4) {
		t.Fatal("open slots not drawn while purchase is open")
	}
	fetch(empty["image_medium_download_url"].(string), 640, 360)
	fetch(empty["image_small_download_url"].(string), 320, 180)

	// A headliner with a red profile picture claims slot 1: a new card (key, uuid, URLs) showing it.
	_, me := login(t, srv, platform.Steam, "76561190000000031")
	id := int64(me.UserID)
	red := image.NewRGBA(image.Rect(0, 0, 96, 96))
	for i := 0; i < len(red.Pix); i += 4 {
		red.Pix[i], red.Pix[i+3] = 0xff, 0xff
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, red, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(pics, strconv.FormatInt(id, 10)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pics, strconv.FormatInt(id, 10), "7_large.jpg"), buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.AddProfilePicSize(ctx, id, 7, "large"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.AddMainStageHeadliner(ctx, next, 1, id, 0, store.HeadlinerPurchase); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Second)
	// Like a live screenshot slot: the same uuid, with new URLs and a new modified_time (the client re-downloads a known
	// uuid when its modified_time changes).
	claimed := image0()
	if claimed["uuid"] != empty["uuid"] || claimed["image_download_url"] == large ||
		claimed["modified_time"] == empty["modified_time"] {
		t.Fatalf("the card did not change as a screenshot slot: %v after %v", claimed, empty)
	}
	if img := fetch(claimed["image_download_url"].(string), 1280, 720); !isRed(img, slot1Of4) || !isOpen(img, slot2Of4) {
		t.Fatal("want the red headliner in slot 1 and slots 2-4 open")
	}
	// Unchanged headliners: the same card, not rendered again.
	now = now.Add(2 * time.Second)
	if again := image0(); again["uuid"] != claimed["uuid"] || again["modified_time"] != claimed["modified_time"] ||
		again["image_download_url"] != claimed["image_download_url"] {
		t.Fatalf("re-rendered: %v then %v", claimed, again)
	}

	// Purchase closed (purchase_close_minutes before the start): the headliner alone, the open slots gone.
	now = t0.Add(-4 * time.Minute)
	closed := image0()
	if closed["uuid"] != claimed["uuid"] || closed["image_download_url"] == claimed["image_download_url"] {
		t.Fatalf("closing the purchase did not re-render: %v", closed)
	}
	if img := fetch(closed["image_download_url"].(string), 1280, 720); !isRed(img, lone) || isOpen(img, slot2Of4) {
		t.Fatal("closed: want the lone headliner and no open slots")
	}
	// In progress: the current event shows its headliners as when closed (the same card).
	now = t0.Add(time.Minute)
	if cur := imageOf("mainstage/current_event/"); cur["uuid"] != closed["uuid"] ||
		cur["image_download_url"] != closed["image_download_url"] {
		t.Fatalf("current event card %v, want %v", cur, closed)
	}

	// Names lose their #NNNN; a static image_url replaces the card.
	if got := displayName("Zed#1234"); got != "Zed" {
		t.Fatalf("displayName %q", got)
	}
	s.cfg.MainStage.Rotation.ImageURL = "https://img.example/ms.jpg"
	now = now.Add(2 * time.Second)
	if static := imageOf("mainstage/current_event/"); static["image_download_url"] != "https://img.example/ms.jpg" {
		t.Fatalf("static image: %v", static)
	}

	for _, bad := range []string{"mainstage/image/zz/large.jpg", "mainstage/image/0123456789abcdef01234567/huge.jpg",
		"mainstage/image/0123456789abcdef01234567/large.jpg"} {
		if status, _, _ := rawRequest(t, "GET", srv.URL+Prefix+bad, nil); status != http.StatusNotFound {
			t.Fatalf("%s: %d", bad, status)
		}
	}
}

func TestRenderCardLayouts(t *testing.T) {
	// Every slot count the config allows, long and non-Latin names, missing pictures.
	for n := 1; n <= 8; n++ {
		spec := cardSpec{Event: "e", Title: strings.Repeat("Very Long Event Name ", 5)}
		for i := 1; i <= n; i++ {
			name := ""
			if i%2 == 1 {
				name = strings.Repeat("Ünïcødé_DJ_", i)
			}
			spec.Slots = append(spec.Slots, cardSlot{Slot: i, Player: int64(i), Name: name, Pic: "/does/not/exist.jpg"})
		}
		imgs, err := renderCard(spec)
		if err != nil {
			t.Fatal(err)
		}
		if len(imgs) != 3 || imgs[2].Bounds().Dx() != 320 {
			t.Fatalf("%d slots: %d images", n, len(imgs))
		}
		if c := color.RGBAModel.Convert(imgs[0].At(5, 5)).(color.RGBA); c.R > 0x40 || c.B < 0x30 {
			t.Fatalf("background %v", c)
		}
	}
}
