package api

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"fuseronline/internal/config"
	"fuseronline/internal/platform"
)

func testJPEG(t *testing.T, side int, shade uint8) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, side, side))
	for i := range img.Pix {
		img.Pix[i] = shade
	}
	img.Set(0, 0, color.RGBA{255, 0, 0, 255})
	var b bytes.Buffer
	if err := jpeg.Encode(&b, img, nil); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// onServer turns one of fuserd's absolute URLs (public_url based) into the test server's.
func onServer(t *testing.T, srv *httptest.Server, abs string) string {
	t.Helper()
	u, err := url.Parse(abs)
	if err != nil {
		t.Fatal(err)
	}
	return srv.URL + u.Path
}

func rawRequest(t *testing.T, method, u string, body []byte) (int, []byte, http.Header) {
	t.Helper()
	req, _ := http.NewRequest(method, u, bytes.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b, resp.Header
}

func TestProfilePictures(t *testing.T) {
	defer func(w time.Duration) { ownStatsWait = w }(ownStatsWait)
	ownStatsWait = 0 // A's own profile below has no players/xp/
	dir := t.TempDir()
	srv := newStoreServer(t, testCatalogue(t), func(cfg *config.Config) {
		cfg.ProfilePics.Dir = dir
		cfg.ProfilePics.MaxKB = 64
	})
	aAuth, a := login(t, srv, platform.Epic, "picA")
	bAuth, _ := login(t, srv, platform.Epic, "picB")

	type urls struct {
		Large  string `json:"asset_upload_url"`
		Medium string `json:"asset_medium_upload_url"`
		Small  string `json:"asset_small_upload_url"`
	}
	uploadURLs := func() urls {
		t.Helper()
		status, env := call(t, srv, "GET", "players/profile_pic/", aAuth, nil)
		if status != http.StatusOK {
			t.Fatalf("players/profile_pic/: %d %+v", status, env.Error)
		}
		var u urls
		decodeResults(t, env, &u)
		if u.Large == "" || u.Medium == "" || u.Small == "" || u.Large == u.Medium {
			t.Fatalf("upload URLs %+v", u)
		}
		return u
	}
	type picMeta struct {
		PicID  int32  `json:"pic_id"`
		Large  string `json:"asset_download_url"`
		Medium string `json:"asset_medium_download_url"`
		Small  string `json:"asset_small_download_url"`
	}
	picOf := func(auth string) *picMeta {
		t.Helper()
		_, env := call(t, srv, "POST", "players/get/", auth, map[string]any{"user_id": a.UserID})
		var u struct {
			ProfilePic *picMeta `json:"profile_pic"`
		}
		decodeResults(t, env, &u)
		return u.ProfilePic
	}

	if picOf(bAuth) != nil {
		t.Fatal("a picture before any upload")
	}

	// The first picture: large and medium arrive, small doesn't.
	first := uploadURLs()
	large, medium := testJPEG(t, 128, 40), testJPEG(t, 64, 90)
	if status, body, _ := rawRequest(t, "PUT", onServer(t, srv, first.Large), large); status != http.StatusOK {
		t.Fatalf("PUT large: %d %s", status, body)
	}
	if status, _, _ := rawRequest(t, "PUT", onServer(t, srv, first.Large), large); status != http.StatusNotFound {
		t.Fatalf("an upload URL is used twice: %d", status)
	}
	if status, _, _ := rawRequest(t, "POST", onServer(t, srv, first.Medium), medium); status != http.StatusOK {
		t.Fatalf("POST medium: %d", status)
	}
	pic := picOf(bAuth)
	if pic == nil || pic.PicID < 1 {
		t.Fatalf("B sees no picture of A: %+v", pic)
	}
	if own := picOf(aAuth); own == nil || own.PicID != pic.PicID {
		t.Fatalf("A's own profile: %+v", own)
	}
	status, got, h := rawRequest(t, "GET", onServer(t, srv, pic.Large), nil)
	if status != http.StatusOK || !bytes.Equal(got, large) || h.Get("Content-Type") != "image/jpeg" {
		t.Fatalf("GET large: %d %s %d bytes", status, h.Get("Content-Type"), len(got))
	}
	if status, got, _ = rawRequest(t, "GET", onServer(t, srv, pic.Small), nil); status != http.StatusOK || !bytes.Equal(got, medium) {
		t.Fatalf("GET small (not uploaded: medium expected): %d %d bytes", status, len(got))
	}

	// Refusals: not an image, too large.
	bad := uploadURLs()
	if status, _, _ := rawRequest(t, "PUT", onServer(t, srv, bad.Large), []byte("not an image")); status != http.StatusUnsupportedMediaType {
		t.Fatalf("not an image: %d", status)
	}
	if status, _, _ := rawRequest(t, "PUT", onServer(t, srv, bad.Medium), make([]byte, 65*1024)); status != http.StatusRequestEntityTooLarge {
		t.Fatalf("too large: %d", status)
	}

	// A newer picture replaces the first: its files go, its URLs stop working, and the first's late upload is dropped.
	second := uploadURLs()
	newLarge := testJPEG(t, 128, 200)
	if status, _, _ := rawRequest(t, "PUT", onServer(t, srv, second.Large), newLarge); status != http.StatusOK {
		t.Fatalf("PUT second large: %d", status)
	}
	now := picOf(bAuth)
	if now == nil || now.PicID <= pic.PicID || now.Large == pic.Large {
		t.Fatalf("second picture %+v after %+v", now, pic)
	}
	if status, got, _ = rawRequest(t, "GET", onServer(t, srv, now.Medium), nil); status != http.StatusOK || !bytes.Equal(got, newLarge) {
		t.Fatalf("GET second medium (large expected): %d", status)
	}
	if status, _, _ := rawRequest(t, "GET", onServer(t, srv, pic.Large), nil); status != http.StatusNotFound {
		t.Fatalf("the first picture is still served: %d", status)
	}
	if status, _, _ := rawRequest(t, "PUT", onServer(t, srv, first.Small), testJPEG(t, 32, 10)); status != http.StatusConflict {
		t.Fatalf("a late upload of the first picture: %d", status)
	}
	entries, _ := os.ReadDir(filepath.Join(dir, strconv.Itoa(int(a.UserID))))
	if len(entries) != 1 {
		t.Fatalf("player directory holds %d files, want the second picture's large only", len(entries))
	}

	// Pictures off: no upload URLs, no picture in profiles.
	off := newStoreServer(t, testCatalogue(t), func(cfg *config.Config) { cfg.ProfilePics.Enabled = false })
	offAuth, _ := login(t, off, platform.Epic, "picOff")
	if status, env := call(t, off, "GET", "players/profile_pic/", offAuth, nil); status != http.StatusOK || string(env.Results) != "{}" {
		t.Fatalf("players/profile_pic/ with pictures off: %d %s", status, env.Results)
	}
}
