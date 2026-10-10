package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fuseronline/internal/catalog"
	"fuseronline/internal/community"
	"fuseronline/internal/config"
	"fuseronline/internal/logx"
	"fuseronline/internal/platform"
	"fuseronline/internal/store"
	"fuseronline/internal/token"
)

// newCommunityServer is a store server with the test catalogue and community entries for the given shortnames.
func newCommunityServer(t *testing.T, xs ...string) (*httptest.Server, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	for _, x := range xs {
		s := community.Song{X: x, Kind: community.KindArchive, Archive: "zip",
			Files:    []community.File{{ID: "1abcdefghij_" + x, Name: x + ".zip", Size: 4321}},
			PakEntry: "dir/" + x + "_P.pak", SigEntry: "dir/" + x + "_P.sig",
			Link: "https://drive.google.com/file/d/1abcdefghij/view", Artist: "Artist", Title: "Title",
			Creator: "SheepQueen", Sheet: "1851575492:9"}
		if _, err := st.PutCommunitySong(context.Background(), s, false); err != nil {
			t.Fatal(err)
		}
	}
	cfg := config.Default()
	cfg.SongManifest.Songs = []string{"countingstars"}
	logs, _ := logx.New(slog.NewTextHandler(io.Discard, nil), nil, []byte("test key"))
	s := New(cfg, st, token.NewIssuer(strings.Repeat("k", 32), time.Hour), map[string]platform.Verifier{
		platform.Epic: fakeVerifier{platform: platform.Epic},
	}, logs)
	s.SetCatalogue(catalog.Static(testCatalogue(t)))
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	s.cfg.PublicURL = srv.URL
	return srv, st
}

type communityResolveResult struct {
	Songs []struct {
		Query     string         `json:"query"`
		Found     bool           `json:"found"`
		Song      *struct{}      `json:"song"`
		Community *communityLink `json:"community"`
	} `json:"songs"`
}

func TestCommunityResolve(t *testing.T) {
	// Seinfeld_Theme is in the catalogue too: the catalogue answers first. countingstars is official.
	srv, st := newCommunityServer(t, "Bey_DrunkInLove_DSR", "Seinfeld_Theme", "countingstars", "Disabled_Song")
	if err := st.SetCommunityDisabled(context.Background(), "Disabled_Song", true, "test"); err != nil {
		t.Fatal(err)
	}
	auth, _ := login(t, srv, platform.Epic, "resolver")
	status, env := call(t, srv, "POST", "community/resolve/", auth, map[string]any{
		"paths": []string{
			"/Game/DLC/Songs/Bey_DrunkInLove_DSR/Meta_Bey_DrunkInLove_DSR.Meta_Bey_DrunkInLove_DSR",
			"/Game/DLC/Songs/Seinfeld_Theme/Meta_Seinfeld_Theme.Meta_Seinfeld_Theme",
			"/Game/DLC/Songs/countingstars/Meta_countingstars.Meta_countingstars",
			"/Game/DLC/Songs/Disabled_Song/Meta_Disabled_Song",
			"/Game/DLC/Songs/Nobody/Meta_Nobody",
		},
		"shortNames": []string{"bey_drunkinlove_dsr"},
	})
	if status != http.StatusOK {
		t.Fatalf("resolve: %d %+v", status, env.Error)
	}
	var r communityResolveResult
	decodeResults(t, env, &r)
	if len(r.Songs) != 6 {
		t.Fatalf("answers: %s", env.Results)
	}
	c := r.Songs[0].Community
	if r.Songs[0].Found || r.Songs[0].Song != nil || c == nil || c.X != "Bey_DrunkInLove_DSR" || c.Kind != "archive" ||
		c.Archive != "zip" || len(c.Files) != 1 || c.Files[0].Size != 4321 || c.PakEntry != "dir/Bey_DrunkInLove_DSR_P.pak" ||
		c.Files[0].URL != community.DownloadURL("1abcdefghij_Bey_DrunkInLove_DSR") || c.SeenSHA256 == nil {
		t.Fatalf("community answer: %s", env.Results)
	}
	if !r.Songs[1].Found || r.Songs[1].Community != nil {
		t.Fatalf("catalogue song: %s", env.Results)
	}
	for i := 2; i <= 4; i++ { // official, disabled, unknown
		if r.Songs[i].Found || r.Songs[i].Community != nil {
			t.Fatalf("answer %d: %s", i, env.Results)
		}
	}
	if r.Songs[5].Community == nil {
		t.Fatalf("shortName query: %s", env.Results)
	}
}

func TestCommunityReport(t *testing.T) {
	srv, st := newCommunityServer(t, "Song_X")
	h := strings.Repeat("0f", 32)
	if status, _ := call(t, srv, "POST", "community/report/", "", map[string]any{"x": "Song_X", "ok": true, "sha256": h}); status != http.StatusUnauthorized {
		t.Fatalf("report without a login: %d", status)
	}
	a1, _ := login(t, srv, platform.Epic, "one")
	a2, _ := login(t, srv, platform.Epic, "two")
	for _, bad := range []map[string]any{
		{"x": "Song_X", "ok": false},                    // no reason
		{"x": "Song_X", "ok": false, "reason": "bored"}, // unknown reason
		{"x": "Song_X", "ok": true},                     // a good install without its hash
		{"x": "Song_X", "ok": true, "sha256": "abc"},
	} {
		if status, _ := call(t, srv, "POST", "community/report/", a1, bad); status != http.StatusBadRequest {
			t.Fatalf("bad report %v: %d", bad, status)
		}
	}
	if status, _ := call(t, srv, "POST", "community/report/", a1, map[string]any{"x": "Nope", "ok": false, "reason": "http"}); status != http.StatusNotFound {
		t.Fatalf("report for an unknown song: %d", status)
	}
	if status, env := call(t, srv, "POST", "community/report/", a1, map[string]any{"x": "song_x", "ok": true, "sha256": h, "size": 9}); status != http.StatusOK {
		t.Fatalf("good report: %d %+v", status, env.Error)
	}
	call(t, srv, "POST", "community/report/", a1, map[string]any{"x": "Song_X", "ok": false, "reason": "layout"})
	status, env := call(t, srv, "POST", "community/report/", a2, map[string]any{"x": "Song_X", "ok": false, "reason": "layout"})
	var res struct {
		Disabled bool `json:"disabled"`
	}
	decodeResults(t, env, &res)
	if status != http.StatusOK || !res.Disabled {
		t.Fatalf("second bad-file report: %d %s", status, env.Results)
	}
	if e, _ := st.CommunitySong(context.Background(), "Song_X"); !e.Disabled {
		t.Fatal("entry not disabled")
	}
}
