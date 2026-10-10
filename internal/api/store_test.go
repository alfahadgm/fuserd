package api

import (
	"bytes"
	"crypto/sha1"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fuseronline/internal/catalog"
	"fuseronline/internal/config"
	"fuseronline/internal/logx"
	"fuseronline/internal/platform"
	"fuseronline/internal/store"
	"fuseronline/internal/token"
)

// testPak writes a version 8 community song pak (fuser_pak_create.py's layout) for song sn.
func testPak(t *testing.T, path, sn string) {
	t.Helper()
	files := map[string][]byte{
		"DLC/Songs/" + sn + "/Meta_" + sn + ".uasset":       []byte("song meta " + sn),
		"Audio/Songs/" + sn + "/" + sn + "bs/Meta_x.uasset": bytes.Repeat([]byte(sn), 300),
	}
	var out, index bytes.Buffer
	fstring := func(b *bytes.Buffer, s string) {
		binary.Write(b, binary.LittleEndian, int32(len(s)+1))
		b.WriteString(s)
		b.WriteByte(0)
	}
	type rec struct {
		name string
		off  int64
		data []byte
	}
	var recs []rec
	for n, data := range files {
		h := sha1.Sum(data)
		off := int64(out.Len())
		binary.Write(&out, binary.LittleEndian, [3]int64{0, int64(len(data)), int64(len(data))})
		binary.Write(&out, binary.LittleEndian, uint32(0))
		out.Write(h[:])
		out.WriteByte(0)
		binary.Write(&out, binary.LittleEndian, uint32(0))
		out.Write(data)
		recs = append(recs, rec{n, off, data})
	}
	fstring(&index, catalog.CommunityMountPoint)
	binary.Write(&index, binary.LittleEndian, int32(len(recs)))
	for _, r := range recs {
		h := sha1.Sum(r.data)
		fstring(&index, r.name)
		binary.Write(&index, binary.LittleEndian, [3]int64{r.off, int64(len(r.data)), int64(len(r.data))})
		binary.Write(&index, binary.LittleEndian, uint32(0))
		index.Write(h[:])
		index.WriteByte(0)
		binary.Write(&index, binary.LittleEndian, uint32(0))
	}
	indexOffset := int64(out.Len())
	out.Write(index.Bytes())
	out.Write(make([]byte, 17))
	binary.Write(&out, binary.LittleEndian, [2]uint32{0x5A6F12E1, 8})
	binary.Write(&out, binary.LittleEndian, [2]int64{indexOffset, int64(index.Len())})
	ih := sha1.Sum(index.Bytes())
	out.Write(ih[:])
	out.Write(make([]byte, 160))
	if err := os.WriteFile(path, out.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	sig := make([]byte, 40)
	binary.LittleEndian.PutUint32(sig, 0x73832DAA)
	copy(sig[8:], sn)
	if err := os.WriteFile(strings.TrimSuffix(path, ".pak")+".sig", sig, 0o644); err != nil {
		t.Fatal(err)
	}
}

// testCatalogue builds a catalogue with two songs.
func testCatalogue(t *testing.T) *catalog.Catalog {
	t.Helper()
	dir := t.TempDir()
	c, err := catalog.Load(filepath.Join(dir, "catalogue.json"), filepath.Join(dir, "files"))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []struct {
		sn, name, artist string
		genre, year      int32
		date             string
	}{
		{"Seinfeld_Theme", "Seinfeld Theme", "Jonathan Wolff", 6, 1989, "2026-10-01"},
		{"Another_Song", "Another Song", "Zed", 8, 2001, "2026-09-01"},
	} {
		p := filepath.Join(dir, s.sn+"_P.pak")
		testPak(t, p, s.sn)
		if _, err := c.Add(catalog.AddOptions{PakPath: p, SigPath: strings.TrimSuffix(p, ".pak") + ".sig",
			Licence: "test only", Source: "unit test", Name: s.name, Artist: s.artist, GenreID: s.genre,
			Year: s.year, ReleaseDate: s.date, Now: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	return c
}

func newStoreServer(t *testing.T, c *catalog.Catalog, adjust func(*config.Config)) *httptest.Server {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := config.Default()
	cfg.SongManifest.Songs = []string{"countingstars"}
	if adjust != nil {
		adjust(&cfg)
	}
	logs, _ := logx.New(slog.NewTextHandler(io.Discard, nil), nil, []byte("test key"))
	s := New(cfg, st, token.NewIssuer(strings.Repeat("k", 32), time.Hour), map[string]platform.Verifier{
		platform.Epic: fakeVerifier{platform: platform.Epic},
	}, logs)
	s.SetCatalogue(catalog.Static(c))
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	// The download URLs the server hands out must point at the test server.
	s.cfg.PublicURL = srv.URL
	return srv
}

func get(t *testing.T, url string, header map[string]string) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

func TestStoreGetSong(t *testing.T) {
	c := testCatalogue(t)
	srv := newStoreServer(t, c, nil)
	status, env := call(t, srv, "POST", "store/songs/get_song/", "", map[string]string{"shortName": "seinfeld_theme"})
	if status != http.StatusOK {
		t.Fatalf("get_song: %d %+v", status, env.Error)
	}
	var item map[string]any
	decodeResults(t, env, &item)
	if item["shortName"] != "Seinfeld_Theme" || item["type"] != "song" || item["name"] != "Seinfeld Theme" {
		t.Fatalf("item %v", item)
	}
	if u, _ := item["pc_asset_download_url"].(string); u != srv.URL+"/dl/Seinfeld_Theme/1/Seinfeld_Theme.bin" {
		t.Fatalf("pc_asset_download_url %q", u)
	}
	for _, k := range []string{"steam_offer", "epic_offer", "xbox_offer", "psn_offer", "nintendo_offer", "unlockables",
		"contained_items", "artist", "genre"} {
		if _, ok := item[k]; !ok {
			t.Errorf("FStoreItemResponse field %s missing", k)
		}
	}
	raw, _ := json.Marshal(item)
	for _, secret := range []string{"licence", "sha256", "meta_path", "mount_point", "source"} {
		if strings.Contains(string(raw), secret) {
			t.Errorf("server-only field %s sent to the game", secret)
		}
	}
	if status, _ := call(t, srv, "POST", "store/songs/get_song/", "", map[string]string{"shortName": "nope"}); status != http.StatusNotFound {
		t.Fatalf("missing song: %d", status)
	}
}

func TestStoreListSwitchSortAndPage(t *testing.T) {
	c := testCatalogue(t)
	type page struct {
		CurrentPage  int              `json:"current_page"`
		NumPages     int              `json:"num_pages"`
		TotalEntries int              `json:"total_entries"`
		Items        []map[string]any `json:"items"`
		Additional   []any            `json:"additional_items"`
	}
	list := func(srv *httptest.Server, req map[string]any) page {
		t.Helper()
		_, env := call(t, srv, "POST", "store/get_list/", "", req)
		var p page
		decodeResults(t, env, &p)
		return p
	}
	off := newStoreServer(t, c, nil)
	if p := list(off, map[string]any{"page": 1, "pageSize": 24}); p.TotalEntries != 0 || p.NumPages != 1 || p.Items == nil {
		t.Fatalf("list_in_store off: %+v", p)
	}
	on := newStoreServer(t, c, func(cfg *config.Config) { cfg.Store.ListInStore = true })
	names := func(p page) string {
		var n []string
		for _, it := range p.Items {
			n = append(n, it["shortName"].(string))
		}
		return strings.Join(n, ",")
	}
	cases := []struct {
		method, dir int
		want        string
	}{
		{0, 1, "Seinfeld_Theme,Another_Song"}, // newest first
		{0, 0, "Another_Song,Seinfeld_Theme"},
		{1, 0, "Another_Song,Seinfeld_Theme"},
		{4, 0, "Seinfeld_Theme,Another_Song"}, // Jonathan Wolff < Zed
		{5, 0, "Seinfeld_Theme,Another_Song"}, // Pop(6) < Rock(8)
		{6, 1, "Another_Song,Seinfeld_Theme"}, // 2001 > 1989
	}
	for _, cs := range cases {
		if got := names(list(on, map[string]any{"page": 1, "pageSize": 24, "sort_method": cs.method, "sort_dir": cs.dir})); got != cs.want {
			t.Errorf("sort %d/%d = %s, want %s", cs.method, cs.dir, got, cs.want)
		}
	}
	if got := names(list(on, map[string]any{"filter": "wolff"})); got != "Seinfeld_Theme" {
		t.Errorf("filter by artist = %s", got)
	}
	p := list(on, map[string]any{"page": 2, "pageSize": 1, "sort_method": 1})
	if p.NumPages != 2 || p.CurrentPage != 2 || names(p) != "Seinfeld_Theme" || p.Additional == nil {
		t.Errorf("page 2 of 2: %+v", p)
	}
	if p := list(on, map[string]any{"page": 9, "pageSize": 1}); len(p.Items) != 0 || p.TotalEntries != 2 {
		t.Errorf("past the last page: %+v", p)
	}
}

func TestStoreManifestAndElderCredit(t *testing.T) {
	c := testCatalogue(t)
	songs := func(srv *httptest.Server) []string {
		_, env := call(t, srv, "GET", "store/song_manifest/get/", "", nil)
		var m struct {
			Songs   []string `json:"songs"`
			Version int32    `json:"version"`
		}
		decodeResults(t, env, &m)
		if m.Version != 738126010 {
			t.Fatalf("manifest version changed: %d", m.Version)
		}
		return m.Songs
	}
	if s := songs(newStoreServer(t, c, nil)); strings.Join(s, ",") != "countingstars" {
		t.Errorf("catalogue songs added to the manifest by default: %v", s)
	}
	srv := newStoreServer(t, c, func(cfg *config.Config) { cfg.Store.AddToSongManifest = true })
	if s := songs(srv); len(s) != 3 {
		t.Errorf("add_to_song_manifest: %v", s)
	}
	var ec struct {
		Featured []any `json:"featured_items"`
		Items    []any `json:"store_items"`
	}
	_, env := call(t, srv, "GET", "store/elder_credit_store/get_list/", "", nil)
	decodeResults(t, env, &ec)
	if ec.Featured == nil || ec.Items == nil {
		t.Errorf("elder credit store: %s", env.Results)
	}
	var claimed struct {
		Assets  []any `json:"assets"`
		Credits []any `json:"credits"`
	}
	_, env = call(t, srv, "GET", "store/elder_credit_store/get_claimed_assets/", "", nil)
	decodeResults(t, env, &claimed)
	if claimed.Assets == nil || claimed.Credits == nil {
		t.Errorf("claimed assets: %s", env.Results)
	}
	if status, _ := call(t, srv, "POST", "store/elder_credit_store/purchase/", "", map[string]any{"uUId": "x"}); status != http.StatusBadRequest {
		t.Errorf("purchase: %d", status)
	}
}

type resolveResult struct {
	Songs []struct {
		Query string `json:"query"`
		Kind  string `json:"kind"`
		Found bool   `json:"found"`
		Song  *struct {
			ShortName string `json:"shortName"`
			Version   int32  `json:"version"`
			MetaPath  string `json:"meta_path"`
			PakURL    string `json:"pak_url"`
			SigURL    string `json:"sig_url"`
			PakSHA256 string `json:"pak_sha256"`
			PakSize   int64  `json:"pak_size"`
			SigSHA256 string `json:"sig_sha256"`
			Licence   string `json:"licence"`
		} `json:"song"`
	} `json:"songs"`
}

func TestStoreResolve(t *testing.T) {
	c := testCatalogue(t)
	srv := newStoreServer(t, c, nil)
	body := map[string]any{"paths": []string{
		"/Game/DLC/Songs/Seinfeld_Theme/Meta_Seinfeld_Theme.Meta_Seinfeld_Theme",
		"/Game/DLC/Songs/Another_Song/Meta_Another_Song",
		"/Game/DLC/Songs/Unknown/Meta_Unknown.Meta_Unknown",
	}, "shortNames": []string{"SEINFELD_THEME"}}
	if status, _ := call(t, srv, "POST", "community/resolve/", "", body); status != http.StatusUnauthorized {
		t.Fatalf("resolve without a login: %d", status)
	}
	auth, _ := login(t, srv, platform.Epic, "resolver")
	status, env := call(t, srv, "POST", "community/resolve/", auth, body)
	if status != http.StatusOK {
		t.Fatalf("resolve: %d %+v", status, env.Error)
	}
	var r resolveResult
	decodeResults(t, env, &r)
	if len(r.Songs) != 4 || !r.Songs[0].Found || !r.Songs[1].Found || r.Songs[2].Found || !r.Songs[3].Found ||
		r.Songs[3].Kind != "shortName" {
		t.Fatalf("resolve answers: %s", env.Results)
	}
	s := r.Songs[0].Song
	it, _ := c.ByShortName("Seinfeld_Theme")
	if s.PakURL != srv.URL+"/dl/Seinfeld_Theme/1/Seinfeld_Theme_P.pak" || s.SigURL != srv.URL+"/dl/Seinfeld_Theme/1/Seinfeld_Theme_P.sig" ||
		s.PakSHA256 != it.PakSHA256 || s.Licence != "test only" || s.MetaPath != it.MetaPath {
		t.Fatalf("resolved song %+v", s)
	}
	many := make([]string, maxResolve+1)
	if status, _ := call(t, srv, "POST", "community/resolve/", auth, map[string]any{"paths": many}); status != http.StatusBadRequest {
		t.Fatalf("oversized resolve: %d", status)
	}
	open := newStoreServer(t, c, func(cfg *config.Config) { cfg.Store.ResolveRequiresLogin = false })
	if status, _ := call(t, open, "POST", "community/resolve/", "", body); status != http.StatusOK {
		t.Fatalf("resolve with resolve_requires_login off: %d", status)
	}
}

func TestStoreDownloads(t *testing.T) {
	c := testCatalogue(t)
	srv := newStoreServer(t, c, nil)
	it, _ := c.ByShortName("Seinfeld_Theme")
	pak, _ := os.ReadFile(filepath.Join(c.Dir(it), it.PakName()))
	sig, _ := os.ReadFile(filepath.Join(c.Dir(it), it.SigName()))
	base := srv.URL + "/dl/Seinfeld_Theme/1/"

	resp, b := get(t, base+it.PakName(), nil)
	if resp.StatusCode != http.StatusOK || !bytes.Equal(b, pak) || resp.Header.Get("ETag") != `"`+it.PakSHA256+`"` ||
		resp.Header.Get("Content-Type") != "application/octet-stream" {
		t.Fatalf("pak: %d etag %s, %d bytes", resp.StatusCode, resp.Header.Get("ETag"), len(b))
	}
	resp, b = get(t, base+it.PakName(), map[string]string{"Range": "bytes=10-19"})
	if resp.StatusCode != http.StatusPartialContent || !bytes.Equal(b, pak[10:20]) {
		t.Fatalf("pak range: %d %q", resp.StatusCode, b)
	}
	resp, _ = get(t, base+it.PakName(), map[string]string{"If-None-Match": `"` + it.PakSHA256 + `"`})
	if resp.StatusCode != http.StatusNotModified {
		t.Fatalf("pak If-None-Match: %d", resp.StatusCode)
	}
	if resp, b = get(t, base+it.SigName(), nil); resp.StatusCode != http.StatusOK || !bytes.Equal(b, sig) {
		t.Fatalf("sig: %d", resp.StatusCode)
	}

	// The game's container: int32 sigLen, .sig, .pak - also served in ranges that cross the boundaries.
	want := binary.LittleEndian.AppendUint32(nil, uint32(len(sig)))
	want = append(append(want, sig...), pak...)
	resp, b = get(t, base+it.BinName(), nil)
	if resp.StatusCode != http.StatusOK || !bytes.Equal(b, want) {
		t.Fatalf("bin: %d, %d bytes (want %d)", resp.StatusCode, len(b), len(want))
	}
	lo, hi := 2, len(sig)+10
	resp, b = get(t, base+it.BinName(), map[string]string{"Range": fmt.Sprintf("bytes=%d-%d", lo, hi)})
	if resp.StatusCode != http.StatusPartialContent || !bytes.Equal(b, want[lo:hi+1]) {
		t.Fatalf("bin range across the header and .sig: %d", resp.StatusCode)
	}

	for _, bad := range []string{
		srv.URL + "/dl/Seinfeld_Theme/2/" + it.PakName(), // not the current version
		srv.URL + "/dl/Seinfeld_Theme/1/other.pak",       // not one of the entry's files
		srv.URL + "/dl/Unknown/1/Unknown_P.pak",          // not in the catalogue
		srv.URL + "/dl/seinfeld_theme/1/" + it.PakName(), // shortnames in URLs are exact
	} {
		if resp, _ := get(t, bad, nil); resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s: %d, want 404", bad, resp.StatusCode)
		}
	}
}

func TestStoreDownloadRateLimit(t *testing.T) {
	c := testCatalogue(t)
	srv := newStoreServer(t, c, func(cfg *config.Config) { cfg.Store.RequestsPerMinute = 2 })
	it, _ := c.ByShortName("Another_Song")
	url := srv.URL + "/dl/Another_Song/1/" + it.SigName()
	codes := []int{}
	for i := 0; i < 3; i++ {
		resp, _ := get(t, url, nil)
		codes = append(codes, resp.StatusCode)
	}
	if codes[0] != 200 || codes[1] != 200 || codes[2] != http.StatusTooManyRequests {
		t.Fatalf("rate limit: %v", codes)
	}
}
