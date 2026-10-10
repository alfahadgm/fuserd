package api

import (
	"archive/zip"
	"bytes"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"fuseronline/internal/config"
)

// The poll's song report: different files under one custom shortname come back as a conflict to every member;
// official songs never do; an older loader's poll (no "songs") changes nothing.
func TestSongPollConflicts(t *testing.T) {
	srv := newTestServerWith(t, func(c *config.Config) {
		c.P2P.Enabled = true
		c.SongManifest.Songs = []string{"OfficialSong"}
	})
	aAuth, a := login(t, srv, "epc", "account-a")
	bAuth, _ := login(t, srv, "epc", "account-b")
	sid := hostPublic(t, srv, aAuth, a.UserID)
	if status, env := call(t, srv, "POST", "multiplayer/player_sessions/", bAuth, map[string]string{"game_session_id": sid}); status != http.StatusOK {
		t.Fatalf("join: %d %+v", status, env.Error)
	}
	type answer struct {
		Conflicts []struct {
			Song     string `json:"song"`
			Versions int    `json:"versions"`
		} `json:"conflicts"`
	}
	poll := func(auth string, body map[string]any) answer {
		t.Helper()
		status, env := call(t, srv, "POST", "p2p/song/poll/", auth, with(body, "game_session_id", sid))
		if status != http.StatusOK {
			t.Fatalf("poll: %d %+v", status, env.Error)
		}
		var ans answer
		decodeResults(t, env, &ans)
		if ans.Conflicts == nil {
			t.Fatal("conflicts missing from the answer")
		}
		return ans
	}
	songs := func(kv ...string) []map[string]string {
		var out []map[string]string
		for i := 0; i+1 < len(kv); i += 2 {
			out = append(out, map[string]string{"song": kv[i], "sha256": strings.Repeat(kv[i+1], 64)})
		}
		return out
	}
	poll(aAuth, map[string]any{"songs": songs("Song_X", "a", "Song_Same", "c", "OfficialSong", "1")})
	if got := poll(bAuth, map[string]any{"songs": songs("song_x", "b", "Song_Same", "c", "officialsong", "2")}); len(got.Conflicts) != 1 ||
		got.Conflicts[0].Song != "song_x" || got.Conflicts[0].Versions != 2 {
		t.Fatalf("B's answer: %+v", got)
	}
	if got := poll(aAuth, map[string]any{"sending": true}); len(got.Conflicts) != 1 {
		t.Fatalf("A's poll without a report keeps its last one: %+v", got)
	}
	if got := poll(bAuth, map[string]any{"songs": songs("song_x", "a")}); len(got.Conflicts) != 0 {
		t.Fatalf("B now holds A's file: %+v", got)
	}
}

// blocked_songs: nothing while every member runs the loader and holds what it needs; a member's missing song alone;
// with a stock member, every custom song the session knows of. Official songs never.
func TestFDSBlockedSongs(t *testing.T) {
	srv, s := newTestServerAndAPI(t, func(c *config.Config) {
		c.P2P.Enabled = true
		c.SongManifest.Songs = []string{"OfficialSong"}
	})
	aAuth, a := login(t, srv, "epc", "account-a")
	bAuth, b := login(t, srv, "epc", "account-b")
	sid := hostPublic(t, srv, aAuth, a.UserID)
	if status, env := call(t, srv, "POST", "multiplayer/player_sessions/", bAuth, map[string]string{"game_session_id": sid}); status != http.StatusOK {
		t.Fatalf("join: %d %+v", status, env.Error)
	}
	sess, ok := s.mm.Session(sid)
	if !ok {
		t.Fatal("no session")
	}
	s.loaders.record(int64(a.UserID), "1.0", "x", time.Now())
	s.loaders.record(int64(b.UserID), "1.0", "x", time.Now())
	poll := func(auth string, body map[string]any) {
		t.Helper()
		if status, env := call(t, srv, "POST", "p2p/song/poll/", auth, with(body, "game_session_id", sid)); status != http.StatusOK {
			t.Fatalf("poll: %d %+v", status, env.Error)
		}
	}
	hash := strings.Repeat("a", 64)
	poll(aAuth, map[string]any{"songs": []map[string]string{{"song": "Song_X", "sha256": hash}, {"song": "OfficialSong", "sha256": hash}}})
	poll(bAuth, map[string]any{"songs": []map[string]string{{"song": "Song_X", "sha256": hash}}, "missing": []string{"Song_Y", "OfficialSong"}})
	if got := s.blockedSongs("slot-1", sess); !slices.Equal(got, []string{"song_y"}) {
		t.Fatalf("loader members, B lacks Song_Y: %v", got)
	}
	s.loaders = loaderReports{} // both count as stock now
	s.loaders.record(int64(a.UserID), "1.0", "x", time.Now())
	if got := s.blockedSongs("slot-1", sess); !slices.Equal(got, []string{"song_x", "song_y"}) {
		t.Fatalf("a stock member: %v", got)
	}
	poll(bAuth, map[string]any{"songs": []map[string]string{}})
	s.loaders.record(int64(b.UserID), "1.0", "x", time.Now())
	if got := s.blockedSongs("slot-1", sess); got == nil || len(got) != 0 {
		t.Fatalf("all ready: %#v", got)
	}
}

func zipWith(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		io.WriteString(w, body)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func zipFiles(t *testing.T, data []byte) map[string]string {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(rc)
		rc.Close()
		out[f.Name] = string(b)
	}
	return out
}

// The mod pack download carries server.txt (public_url) beside its setup script, replacing any it had; a pack without
// the script, or a loopback public_url, is served unchanged.
func TestModPackServerTxt(t *testing.T) {
	pack := filepath.Join(t.TempDir(), "FUSERNativeLoader.zip")
	publicURL := "https://fuser.example.org"
	srv := newTestServerWith(t, func(c *config.Config) {
		c.PublicURL = publicURL
		c.ModPackFile = pack
	})
	write := func(data []byte, mod time.Time) {
		t.Helper()
		if err := os.WriteFile(pack, data, 0o644); err != nil {
			t.Fatal(err)
		}
		os.Chtimes(pack, mod, mod)
	}
	write(zipWith(t, map[string]string{"Setup-FuserOnline.ps1": "setup", "server.txt": "old.example", "Fuser/Binaries/Win64/winhttp.dll": "dll"}),
		time.Now().Add(-time.Hour))
	resp, body := getPage(t, srv.URL+"/modpack")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	files := zipFiles(t, []byte(body))
	if len(files) != 3 || files["server.txt"] != publicURL+"\r\n" || files["Fuser/Binaries/Win64/winhttp.dll"] != "dll" || files["Setup-FuserOnline.ps1"] != "setup" {
		t.Fatalf("stamped pack: %v", files)
	}
	if resp.Header.Get("ETag") == "" {
		t.Fatal("no ETag on the stamped pack")
	}

	plain := zipWith(t, map[string]string{"Fuser/Binaries/Win64/winhttp.dll": "dll"})
	write(plain, time.Now())
	if _, body := getPage(t, srv.URL+"/modpack"); body != string(plain) {
		t.Fatal("a pack without the setup script changed")
	}

	if stampableURL("http://127.0.0.1:8000") || stampableURL("http://localhost:8000") || !stampableURL(publicURL) {
		t.Fatal("stampableURL")
	}
}
