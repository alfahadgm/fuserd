package api

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fuseronline/internal/matchmaking"
	"image/jpeg"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"fuseronline/internal/config"
)

func getPage(t *testing.T, url string) (*http.Response, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, string(b)
}

func TestLandingPage(t *testing.T) {
	pack := filepath.Join(t.TempDir(), "FUSERNativeLoader.zip")
	srv := newTestServerWith(t, func(c *config.Config) {
		c.PublicURL = "https://fuser.example.org"
		c.ModPackFile = pack
	})

	resp, body := getPage(t, srv.URL+"/")
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("status %d, type %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	u := `&#34;https://fuser.example.org/api/&#34;` // html/template escapes the quotes
	want := "[/Script/Hype.BackendServerMgrSettings]\nEnvironmentSettings=((Live, (ClientURL=" + u + ",UsWest1URL=" + u +
		",Northeast2URL=" + u + ",Euwest3URL=" + u + ",UsEast1URL=" + u + ")))"
	if !strings.Contains(body, want) {
		t.Fatalf("page lacks the Game.ini section:\n%s", body)
	}
	if !strings.Contains(body, `src="/logo.png"`) || !strings.Contains(body, "available yet") {
		t.Fatalf("page lacks the logo or the no-mod-pack notice:\n%s", body)
	}
	if resp, _ := getPage(t, srv.URL+"/modpack"); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("missing mod pack: status %d", resp.StatusCode)
	}

	if err := os.WriteFile(pack, []byte("PK\x03\x04 test zip"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, body := getPage(t, srv.URL+"/"); !strings.Contains(body, `href="/modpack"`) || !strings.Contains(body, "FUSERNativeLoader.zip") {
		t.Fatalf("page lacks the mod pack link:\n%s", body)
	}
	resp, body = getPage(t, srv.URL+"/modpack")
	if resp.StatusCode != http.StatusOK || body != "PK\x03\x04 test zip" {
		t.Fatalf("mod pack: status %d, body %q", resp.StatusCode, body)
	}
	if cd := resp.Header.Get("Content-Disposition"); cd != `attachment; filename="FUSERNativeLoader.zip"` {
		t.Fatalf("Content-Disposition %q", cd)
	}

	resp, body = getPage(t, srv.URL+"/logo.jpg")
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "image/jpeg" || len(body) != len(siteLogo) {
		t.Fatalf("logo: status %d, type %q, %d bytes", resp.StatusCode, resp.Header.Get("Content-Type"), len(body))
	}
	for path, want := range map[string][]byte{"/logo.png": siteMenuLogo, "/logo-mask.png": siteLogoMask} {
		resp, body := getPage(t, srv.URL+path)
		if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "image/png" || body != string(want) {
			t.Fatalf("%s: status %d, type %q", path, resp.StatusCode, resp.Header.Get("Content-Type"))
		}
	}
	resp, body = getPage(t, srv.URL+"/favicon.ico")
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "image/x-icon" || body != string(siteFavicon) {
		t.Fatalf("favicon: status %d, type %q, %d bytes", resp.StatusCode, resp.Header.Get("Content-Type"), len(body))
	}
	if _, body := getPage(t, srv.URL+"/"); !strings.Contains(body, `href="/favicon.ico"`) || !strings.Contains(body, "IsReadOnly") {
		t.Fatal("page lacks the favicon link or the read-only step")
	}
	if resp, body := getPage(t, srv.URL+"/robots.txt"); resp.StatusCode != http.StatusOK || body != "User-agent: *\nDisallow: /\n" {
		t.Fatalf("robots.txt: status %d, body %q", resp.StatusCode, body)
	}
	// Other paths still get the JSON 404.
	if resp, _ := getPage(t, srv.URL+"/nothing-here"); resp.StatusCode != http.StatusNotFound ||
		resp.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("unknown path: status %d, type %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
}

func TestStatusTemplateRendersSnapshot(t *testing.T) {
	var body bytes.Buffer
	data := siteHome{
		Status: siteStatus{Online: 5, Performers: 2, Viewers: 1, InPublic: 3, Elsewhere: 2,
			Lobbies: []siteLobby{{VenueName: "Cherry Dip'd", TimeOfDay: "Sunset", Image: "/venues/A-sunset.jpg", Number: 1, Performers: 2, Viewers: 1, Players: 3, Capacity: 3, Fill: 100, Rounds: 4, Bars: 32, State: "In gameplay", StateClass: "gameplay"}}},
	}
	if err := siteTemplate.Execute(&body, data); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"/venues/A-sunset.jpg", "Cherry Dip&#39;d 01", "Sunset", "Server Status", "Performers", "Viewers", "32 bars", "In gameplay", "Full", "3 in public lobbies", "2 elsewhere online"} {
		if !strings.Contains(body.String(), want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(body.String(), "ZgotmplZ") {
		t.Fatal("template rejected CSS value")
	}
}

// The mod pack tab and its call to action describe the server's own settings: the stock / modded split, the Main
// Stage and the crate song limit each show only when enabled.
func TestModPackTabFollowsServerSettings(t *testing.T) {
	render := func(d siteHome) string {
		t.Helper()
		var b bytes.Buffer
		if err := siteTemplate.Execute(&b, d); err != nil {
			t.Fatal(err)
		}
		return b.String()
	}
	always := []string{`id="modpack-tab"`, `id="modpack-panel"`, `data-tab="modpack-tab"`, "Custom songs aren't shared",
		"Wider BPM range", "customSongsUnlocked_P.pak", "up to 144 songs and 120 snapshots", "isn't available yet"}
	on := []string{"only with other players who don't have it", "Lobbies with other mod pack players",
		"<strong>Diamond Stage</strong>", "up to 48 songs in a multiplayer crate"}
	off := "including with mod pack players"

	page := render(siteHome{SeparateStock: true, MainStage: true, CrateLimit: 48})
	for _, want := range append(always, on...) {
		if !strings.Contains(page, want) {
			t.Errorf("settings on: missing %q", want)
		}
	}
	if strings.Contains(page, off) {
		t.Error("settings on: shows the mixed-lobby note")
	}

	page = render(siteHome{})
	for _, want := range append(always, off) {
		if !strings.Contains(page, want) {
			t.Errorf("settings off: missing %q", want)
		}
	}
	for _, unwanted := range on {
		if strings.Contains(page, unwanted) {
			t.Errorf("settings off: shows %q", unwanted)
		}
	}
}

func TestHomeExcludesPlayerHostedSessions(t *testing.T) {
	srv, api := newTestServerAndAPI(t, nil)
	api.mm.CreateTicket(matchmaking.QueueFreestylePublic, matchmaking.Player{ID: 42, IsDJ: true}, true, "198.51.100.1")
	api.mm.Seen(42)
	_, body := getPage(t, srv.URL+"/")
	if !strings.Contains(body, "No active public lobbies") || !strings.Contains(body, "1 elsewhere online") || strings.Contains(body, "PUBLIC SERVER 01") {
		t.Fatal("player-hosted session displayed")
	}
}

func TestSiteGameStates(t *testing.T) {
	for stage, want := range map[int]string{0: "Loading", 1: "In lobby", 4: "Loading", 6: "In gameplay", 10: "Results", -1: "State unavailable", 99: "State unavailable"} {
		got, _ := siteGameState(stage)
		if got != want {
			t.Errorf("stage %d: %s", stage, got)
		}
	}
}

func TestVenueImagesAndNames(t *testing.T) {
	srv := newTestServerWith(t, nil)
	venues := []struct{ id, name, texture string }{
		{"VenueA", "Rhythmic Guardian", "C"}, {"VenueB", "Cherry Dip'd", "A"},
		{"VenueC", "Sub Astral", "F"}, {"VenueD", "Source Decay", "D"},
		{"VenueE", "Ravaged Reef", "E"}, {"VenueF", "Starglacier", "B"},
	}
	for _, v := range venues {
		for _, tod := range []string{"morning", "day", "sunset", "night"} {
			name, display, image := siteVenue(v.id, tod)
			if name != v.name || display == "" || image != "/venues/"+v.texture+"-"+tod+".jpg" {
				t.Fatalf("venue %s/%s: %s, %s, %s", v.id, tod, name, display, image)
			}
			resp, body := getPage(t, srv.URL+image)
			if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "image/jpeg" {
				t.Fatalf("image %s: %d", image, resp.StatusCode)
			}
			if _, err := jpeg.Decode(strings.NewReader(body)); err != nil {
				t.Fatalf("image %s: %v", image, err)
			}
		}
	}
	if _, _, image := siteVenue("unknown", "night"); image != "" {
		t.Fatal("unknown venue has image")
	}
	if _, _, image := siteVenue("VenueB", "unknown"); image != "" {
		t.Fatal("unknown time has image")
	}
	if resp, _ := getPage(t, srv.URL+"/venues/not-found.jpg"); resp.StatusCode != http.StatusNotFound {
		t.Fatal("missing image not 404")
	}
	_, body := getPage(t, srv.URL+"/")
	if !strings.Contains(body, "No active event") || strings.Contains(body, "DESIGN PREVIEW") || strings.Index(body, `id="lobbies-tab"`) > strings.Index(body, `id="diamond-tab"`) {
		t.Fatal("default page state or tab order incorrect")
	}
}

// writeTestPack writes a mod pack zip with the given root files.
func writeTestPack(t *testing.T, name string, files map[string]string) string {
	t.Helper()
	pack := filepath.Join(t.TempDir(), name)
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, n := range []string{"VERSION", "FUSEROnline-Setup.exe", "Fuser/Binaries/Win64/winhttp.dll", "server.txt"} {
		body, ok := files[n]
		if !ok {
			continue
		}
		w, err := zw.Create(n)
		if err != nil {
			t.Fatal(err)
		}
		io.WriteString(w, body)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pack, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return pack
}

func TestModPackVersionAndSetup(t *testing.T) {
	exe := "MZ\x90\x00fake installer PE image"
	pack := writeTestPack(t, "FUSERNativeLoader.zip", map[string]string{"VERSION": "\xef\xbb\xbf0.13.8\r\n",
		"FUSEROnline-Setup.exe": exe, "Fuser/Binaries/Win64/winhttp.dll": "dll", "server.txt": "old.example\r\n"})
	srv := newTestServerWith(t, func(c *config.Config) {
		c.PublicURL = "https://fuser.example.org:8443"
		c.ModPackFile = pack
	})

	resp, body := getPage(t, srv.URL+"/modpack/version")
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("version: status %d, type %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	var v struct {
		Version string `json:"version"`
		Size    int64  `json:"size"`
		SHA256  string `json:"sha256"`
		URL     string `json:"url"`
	}
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		t.Fatal(err)
	}
	resp, zipBody := getPage(t, srv.URL+v.URL)
	sum := sha256.Sum256([]byte(zipBody))
	if v.Version != "0.13.8" || v.URL != "/modpack" || v.Size != int64(len(zipBody)) || v.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("version %+v doesn't describe the %d served bytes", v, len(zipBody))
	}
	// The served zip is the stamped one: server.txt beside the installer holds public_url.
	zr, err := zip.NewReader(strings.NewReader(zipBody), int64(len(zipBody)))
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := readZipRoot(zr, "server.txt", 1024); string(b) != "https://fuser.example.org:8443\r\n" {
		t.Fatalf("server.txt %q", b)
	}
	if resp.Header.Get("ETag") == "" {
		t.Fatal("mod pack has no ETag")
	}

	resp, setup := getPage(t, srv.URL+"/setup")
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "application/octet-stream" ||
		resp.Header.Get("Content-Disposition") != `attachment; filename="FUSEROnline-Setup.exe"` {
		t.Fatalf("setup: status %d, type %q, disposition %q", resp.StatusCode, resp.Header.Get("Content-Type"),
			resp.Header.Get("Content-Disposition"))
	}
	if !strings.HasPrefix(setup, exe) || !strings.HasSuffix(setup, "FUSERCFG") {
		t.Fatalf("setup %q", setup)
	}
	trailer := setup[len(exe) : len(setup)-8]
	n := binary.LittleEndian.Uint32([]byte(trailer[len(trailer)-4:]))
	if payload := trailer[:len(trailer)-4]; int(n) != len(payload) || payload != "server=https://fuser.example.org:8443" {
		t.Fatalf("setup stamp %q, length %d", payload, n)
	}

	_, page := getPage(t, srv.URL+"/")
	if !strings.Contains(page, `href="/setup"`) || !strings.Contains(page, `href="/modpack"`) || !strings.Contains(page, "Steam") {
		t.Fatal("page lacks the setup download, the zip alternative or the instructions")
	}
}

func TestModPackVersionAndSetupMissing(t *testing.T) {
	// No mod pack configured, and one configured but missing.
	for _, file := range []string{"", filepath.Join(t.TempDir(), "missing.zip")} {
		srv := newTestServerWith(t, func(c *config.Config) {
			c.PublicURL = "https://fuser.example.org"
			c.ModPackFile = file
		})
		for _, p := range []string{"/modpack/version", "/setup"} {
			if resp, _ := getPage(t, srv.URL+p); resp.StatusCode != http.StatusNotFound {
				t.Fatalf("%q %s: status %d", file, p, resp.StatusCode)
			}
		}
	}

	// A pack without an installer or VERSION: the zip is still served, the rest is 404.
	pack := writeTestPack(t, "FUSERNativeLoader.zip", map[string]string{"Fuser/Binaries/Win64/winhttp.dll": "dll"})
	srv := newTestServerWith(t, func(c *config.Config) {
		c.PublicURL = "https://fuser.example.org"
		c.ModPackFile = pack
	})
	for _, p := range []string{"/modpack/version", "/setup"} {
		if resp, _ := getPage(t, srv.URL+p); resp.StatusCode != http.StatusNotFound {
			t.Fatalf("no installer %s: status %d", p, resp.StatusCode)
		}
	}
	raw, _ := os.ReadFile(pack)
	if resp, body := getPage(t, srv.URL+"/modpack"); resp.StatusCode != http.StatusOK || body != string(raw) {
		t.Fatalf("pack without setup: status %d, not served as is", resp.StatusCode)
	}
	if _, page := getPage(t, srv.URL+"/"); strings.Contains(page, `href="/setup"`) {
		t.Fatal("page offers a setup the pack doesn't have")
	}
}

// A loopback public_url isn't stamped anywhere; the version still describes the bytes /modpack serves.
func TestModPackVersionUnstamped(t *testing.T) {
	pack := writeTestPack(t, "FUSERNativeLoader.zip", map[string]string{"VERSION": "1.2.3", "FUSEROnline-Setup.exe": "MZexe"})
	srv := newTestServerWith(t, func(c *config.Config) {
		c.PublicURL = "http://127.0.0.1:8000"
		c.ModPackFile = pack
	})
	raw, _ := os.ReadFile(pack)
	sum := sha256.Sum256(raw)
	_, body := getPage(t, srv.URL+"/modpack/version")
	want := `{"version":"1.2.3","size":` + strconv.Itoa(len(raw)) + `,"sha256":"` + hex.EncodeToString(sum[:]) + `","url":"/modpack"}`
	if strings.TrimSpace(body) != want {
		t.Fatalf("version %s, want %s", body, want)
	}
	if _, setup := getPage(t, srv.URL+"/setup"); setup != "MZexe" {
		t.Fatalf("setup %q", setup)
	}
}

// A mod pack over maxStampBytes is served from disk as it is, but /setup still carries the server stamp.
func TestModPackTooLargeToStamp(t *testing.T) {
	old := maxStampBytes
	maxStampBytes = 16
	t.Cleanup(func() { maxStampBytes = old })
	pack := writeTestPack(t, "FUSERNativeLoader.zip", map[string]string{"VERSION": "1.2.3", "FUSEROnline-Setup.exe": "MZexe"})
	srv := newTestServerWith(t, func(c *config.Config) {
		c.PublicURL = "https://fuser.example.org"
		c.ModPackFile = pack
	})
	raw, _ := os.ReadFile(pack)
	sum := sha256.Sum256(raw)
	_, body := getPage(t, srv.URL+"/modpack/version")
	want := `{"version":"1.2.3","size":` + strconv.Itoa(len(raw)) + `,"sha256":"` + hex.EncodeToString(sum[:]) + `","url":"/modpack"}`
	if strings.TrimSpace(body) != want {
		t.Fatalf("version %s, want %s", body, want)
	}
	if _, zipBody := getPage(t, srv.URL+"/modpack"); zipBody != string(raw) {
		t.Fatal("an oversized mod pack isn't served as it is")
	}
	if _, setup := getPage(t, srv.URL+"/setup"); !strings.HasPrefix(setup, "MZexe") || !strings.HasSuffix(setup, "FUSERCFG") {
		t.Fatalf("setup %q", setup)
	}
}
