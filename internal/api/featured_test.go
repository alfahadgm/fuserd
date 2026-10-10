package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"image"
	"image/color"
	"image/png"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"fuseronline/internal/catalog"
)

func TestFeaturedAspect(t *testing.T) {
	for _, c := range []struct {
		w, h int
		ok   bool
	}{{396, 741, true}, {8, 15, true}, {800, 1500, true}, {792, 1482, true}, {500, 500, false}, {741, 396, false}, {398, 741, true}, {402, 741, false}} {
		if got := isFeaturedAspect(c.w, c.h); got != c.ok {
			t.Errorf("%dx%d: %t, want %t", c.w, c.h, got, c.ok)
		}
	}
	// A square image loses its sides; the kept middle is the source's centre.
	sq := image.NewNRGBA(image.Rect(0, 0, 300, 300))
	for y := 0; y < 300; y++ {
		for x := 0; x < 300; x++ {
			sq.Set(x, y, color.NRGBA{uint8(x), 0, 0, 255})
		}
	}
	got := cropFeatured(sq)
	if b := got.Bounds(); b.Dx() != 160 || b.Dy() != 300 {
		t.Fatalf("square crop %v", b)
	}
	if r, _, _, _ := got.At(0, 0).RGBA(); r>>8 != 70 { // (300-160)/2
		t.Errorf("crop starts at x=%d, want 70", r>>8)
	}
	// A wide-and-tall image is trimmed top and bottom when too tall, then scaled to featuredMaxHeight.
	if b := cropFeatured(image.NewNRGBA(image.Rect(0, 0, 100, 400))).Bounds(); b.Dx() != 100 || b.Dy() != 187 {
		t.Errorf("tall crop %v", b)
	}
	if b := cropFeatured(image.NewNRGBA(image.Rect(0, 0, 3000, 3000))).Bounds(); b.Dy() != featuredMaxHeight || b.Dx() != featuredMaxHeight*8/15 {
		t.Errorf("large crop %v", b)
	}
}

// Featured items get 8:15 art: square catalogue art and a non-8:15 upload are centre-cropped and published as 8-bit
// control media; an 8:15 upload and standard items are left alone.
func TestFeaturedArtCropped(t *testing.T) {
	_, s := newTestServerAndAPI(t, nil)
	c := elderCatalogue(t)
	s.SetCatalogue(catalog.Static(c))
	ctx := context.Background()
	seinfeld, _ := c.ByShortName("Seinfeld_Theme")
	square := func(n int) []byte {
		var b bytes.Buffer
		png.Encode(&b, image.NewNRGBA(image.Rect(0, 0, n, n)))
		return b.Bytes()
	}
	// The catalogue art (fake bytes in elderCatalogue) becomes a real square image.
	if err := os.WriteFile(filepath.Join(c.Dir(seinfeld), seinfeld.Art), square(300), 0o644); err != nil {
		t.Fatal(err)
	}
	served := func(url string) image.Image {
		t.Helper()
		if !strings.HasPrefix(url, s.cfg.PublicURL+"/control/media/") {
			t.Fatalf("not cropped media: %q", url)
		}
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, httptest.NewRequest("GET", url[len(s.cfg.PublicURL):], nil))
		if pngBitDepth(w.Body.Bytes()) != 8 {
			t.Fatalf("%s: not an 8-bit PNG", url)
		}
		im, err := png.Decode(bytes.NewReader(w.Body.Bytes()))
		if err != nil {
			t.Fatal(err)
		}
		return im
	}
	if im := served(s.featuredArt(ctx, seinfeld, s.artURL(seinfeld))); !isFeaturedAspect(im.Bounds().Dx(), im.Bounds().Dy()) {
		t.Fatalf("catalogue art crop %v", im.Bounds())
	}
	upload := func(b []byte) string {
		url, err := s.controlImage(ctx, "data:image/png;base64,"+base64.StdEncoding.EncodeToString(b))
		if err != nil {
			t.Fatal(err)
		}
		return url
	}
	if im := served(s.featuredArt(ctx, seinfeld, upload(square(64)))); im.Bounds().Dx() != 34 || im.Bounds().Dy() != 64 {
		t.Fatalf("upload crop %v", im.Bounds())
	}
	var tall bytes.Buffer
	png.Encode(&tall, image.NewNRGBA(image.Rect(0, 0, 396, 741)))
	if u := upload(tall.Bytes()); s.featuredArt(ctx, seinfeld, u) != u {
		t.Fatal("an 8:15 upload was replaced")
	}
	if u := "https://cdn.example.org/art.png"; s.featuredArt(ctx, seinfeld, u) != u {
		t.Fatal("an external URL was replaced")
	}
}
