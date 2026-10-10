package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fuseronline/internal/catalog"
	"fuseronline/internal/config"
	"fuseronline/internal/platform"
)

type ecItem struct {
	UUID   string `json:"uuid"`
	Assets []struct {
		AssetName    string `json:"asset_name"`
		AssetType    string `json:"asset_type"`
		AssetTypeVal string `json:"asset_type_val"`
	} `json:"assets"`
	StartDate string `json:"start_date"`
	EndDate   string `json:"end_date"`
	Price     int32  `json:"price"`
	IsOwned   bool   `json:"is_owned"`
	Image     string `json:"featured_image_url"`
}

type ecList struct {
	Featured []ecItem `json:"featured_items"`
	Items    []ecItem `json:"store_items"`
}

// elderCatalogue is testCatalogue with Seinfeld_Theme (art, featured, 100 credits) and Another_Song (5000 credits)
// published, and a third, expired offer.
func elderCatalogue(t *testing.T) *catalog.Catalog {
	t.Helper()
	c := testCatalogue(t)
	dir := t.TempDir()
	art := filepath.Join(dir, "cover.png")
	os.WriteFile(art, []byte("\x89PNG cover"), 0o644)
	p := filepath.Join(dir, "Seinfeld_Theme_P.pak")
	testPak(t, p, "Seinfeld_Theme")
	// Re-adding the same files with album art is an update: version 2, with the art.
	if _, err := c.Add(catalog.AddOptions{PakPath: p, SigPath: strings.TrimSuffix(p, ".pak") + ".sig",
		Licence: "test only", Name: "Seinfeld Theme", Artist: "Jonathan Wolff", ArtPath: art, Update: true}); err != nil {
		t.Fatal(err)
	}
	for _, pub := range []struct {
		sn string
		l  catalog.Listing
	}{
		{"Seinfeld_Theme", catalog.Listing{Price: 100, Featured: true, StartDate: "2026-01-01T00:00:00Z"}},
		{"Another_Song", catalog.Listing{Price: 5000, StartDate: "2026-01-01T00:00:00Z"}},
	} {
		if _, err := c.Publish(pub.sn, pub.l); err != nil {
			t.Fatal(err)
		}
	}
	return c
}

func TestElderCreditStore(t *testing.T) {
	c := elderCatalogue(t)
	srv := newStoreServer(t, c, func(cfg *config.Config) { cfg.Store.ElderCreditBalance = 1000 })
	seinfeld, _ := c.ByShortName("Seinfeld_Theme")
	another, _ := c.ByShortName("Another_Song")

	list := func(auth string) ecList {
		t.Helper()
		_, env := call(t, srv, "GET", "store/elder_credit_store/get_list/", auth, nil)
		var l ecList
		decodeResults(t, env, &l)
		return l
	}
	l := list("")
	if len(l.Featured) != 1 || len(l.Items) != 1 {
		t.Fatalf("list: %+v", l)
	}
	f := l.Featured[0]
	if f.UUID != seinfeld.UUID || f.Price != 100 || f.IsOwned || f.EndDate != catalog.DefaultListingEnd ||
		len(f.Assets) != 1 || f.Assets[0].AssetName != "Seinfeld_Theme" || f.Assets[0].AssetType != "Song" ||
		f.Assets[0].AssetTypeVal != "Song" || f.Image != srv.URL+"/dl/Seinfeld_Theme/2/Seinfeld_Theme_art.png" {
		t.Fatalf("featured item %+v", f)
	}
	if l.Items[0].UUID != another.UUID || l.Items[0].Image != "" {
		t.Fatalf("standard item %+v", l.Items[0])
	}

	purchase := func(auth, uuid string, price int32) (int, envelope) {
		t.Helper()
		return call(t, srv, "POST", "store/elder_credit_store/purchase/", auth, map[string]any{"uuid": uuid, "expected_price": price})
	}
	refused := func(status int, env envelope, code string) {
		t.Helper()
		if status != http.StatusBadRequest || env.Error == nil || env.Error.Code != code {
			t.Fatalf("want %s, got %d %+v", code, status, env.Error)
		}
	}
	status, env := purchase("", seinfeld.UUID, 100)
	refused(status, env, "not_logged_in")
	auth, _ := login(t, srv, platform.Epic, "buyer")
	status, env = purchase(auth, "00000000-0000-4000-8000-000000000000", 100)
	refused(status, env, "does_not_exist")
	status, env = purchase(auth, seinfeld.UUID, 99)
	refused(status, env, "price_changed")
	status, env = purchase(auth, another.UUID, 5000)
	refused(status, env, "not_enough_credits")

	status, env = purchase(auth, strings.ToUpper(seinfeld.UUID), 100)
	if status != http.StatusOK {
		t.Fatalf("purchase: %d %+v", status, env.Error)
	}
	var bought struct {
		Remaining int `json:"num_elder_credits_remaining"`
	}
	decodeResults(t, env, &bought)
	if bought.Remaining != 1000 {
		t.Fatalf("balance changed by a purchase: %d", bought.Remaining)
	}
	status, env = purchase(auth, seinfeld.UUID, 100)
	refused(status, env, "already_owned")

	if l := list(auth); !l.Featured[0].IsOwned || l.Items[0].IsOwned {
		t.Fatalf("ownership in the list: %+v", l)
	}
	if l := list(""); l.Featured[0].IsOwned {
		t.Fatal("owned without a login")
	}

	var claimed struct {
		Assets []struct {
			AssetName string `json:"asset_name"`
			AssetType string `json:"asset_type"`
		} `json:"assets"`
		Credits []any `json:"credits"`
	}
	_, env = call(t, srv, "GET", "store/elder_credit_store/get_claimed_assets/", auth, nil)
	decodeResults(t, env, &claimed)
	if len(claimed.Assets) != 1 || claimed.Assets[0].AssetName != "Seinfeld_Theme" || claimed.Assets[0].AssetType != "Song" ||
		claimed.Credits == nil {
		t.Fatalf("claimed assets: %s", env.Results)
	}

	// The album art is served from /dl/.
	resp, body := get(t, f.Image, nil)
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "image/png" || string(body) != "\x89PNG cover" {
		t.Fatalf("art: %d %s %q", resp.StatusCode, resp.Header.Get("Content-Type"), body)
	}
	// ... and named in the loader's resolve answer.
	_, env = call(t, srv, "POST", "community/resolve/", auth, map[string]any{"shortNames": []string{"Seinfeld_Theme"}})
	var r struct {
		Songs []struct {
			Song struct {
				ArtURL   string `json:"art_url"`
				SongName string `json:"song_name"`
			} `json:"song"`
		} `json:"songs"`
	}
	decodeResults(t, env, &r)
	if r.Songs[0].Song.ArtURL != f.Image || r.Songs[0].Song.SongName != "Seinfeld Theme" {
		t.Fatalf("resolve: %s", env.Results)
	}

	// An offer that has ended is neither listed nor sold.
	if _, err := c.Publish("Another_Song", catalog.Listing{Price: 1, StartDate: "2020-01-01", EndDate: "2021-01-01"}); err != nil {
		t.Fatal(err)
	}
	if l := list(auth); len(l.Items) != 0 {
		t.Fatalf("expired offer listed: %+v", l.Items)
	}
	status, env = purchase(auth, another.UUID, 1)
	refused(status, env, "no_longer_available")
}

func TestPlayersXPBalance(t *testing.T) {
	c := testCatalogue(t)
	body := map[string]any{"current_level": 12, "hype_profile": map[string]any{"xp": 4321, "cached_elder_credits": 7}}

	off := newStoreServer(t, c, nil)
	status, env := call(t, off, "POST", "players/xp/", "", body)
	if status != http.StatusOK || string(env.Results) != "{}" {
		t.Fatalf("players/xp/ without a balance: %d %s", status, env.Results)
	}

	srv := newStoreServer(t, c, func(cfg *config.Config) { cfg.Store.ElderCreditBalance = 2500 })
	auth, _ := login(t, srv, platform.Epic, "xp")
	status, env = call(t, srv, "POST", "players/xp/", auth, body)
	if status != http.StatusOK {
		t.Fatalf("players/xp/: %d %+v", status, env.Error)
	}
	var r struct {
		Profile struct {
			UserID       int64 `json:"user_id"`
			ElderCredits struct {
				Amount   int     `json:"amount"`
				Progress float64 `json:"progress_to_next_credit"`
			} `json:"elder_credits"`
			HypeProfile json.RawMessage `json:"hype_profile"`
		} `json:"profile"`
		Earned int `json:"elder_credits_earned"`
		XPPer  int `json:"xp_per_elder_credit"`
	}
	decodeResults(t, env, &r)
	if r.Profile.ElderCredits.Amount != 2500 || r.Earned != 0 || r.XPPer != 1000 || r.Profile.UserID == 0 ||
		!strings.Contains(string(r.Profile.HypeProfile), `"xp":4321`) {
		t.Fatalf("players/xp/: %s", env.Results)
	}
}

// players/get/ for one's own id: the name, the stats players/xp/ last reported (a -1 keeps the stored value), and the
// Elder Credit balance - the client's own with the Diamond store off, the store's fixed one with it on.
func TestOwnProfileStats(t *testing.T) {
	c := testCatalogue(t)
	type own struct {
		HypeUsername    string `json:"hype_username"`
		IsBanned        bool   `json:"is_banned"`
		PlatformProfile struct {
			PlatformUsername string `json:"platform_username"`
		} `json:"platform_profile"`
		HypeProfile struct {
			NumDiscs          int32 `json:"num_discs"`
			MatchmakingRating int32 `json:"matchmaking_rating"`
			TotalBattleWins   int32 `json:"total_battle_wins"`
			TotalBattleLosses int32 `json:"total_battle_losses"`
		} `json:"hype_profile"`
		ElderCredits struct {
			Amount int32 `json:"amount"`
		} `json:"elder_credits"`
	}
	get := func(srv *httptest.Server, auth string, id int32) own {
		t.Helper()
		status, env := call(t, srv, "POST", "players/get/", auth, map[string]any{"user_id": id})
		if status != http.StatusOK {
			t.Fatalf("players/get/: %d %+v", status, env.Error)
		}
		var o own
		decodeResults(t, env, &o)
		return o
	}
	full := map[string]any{"current_level": 12, "hype_profile": map[string]any{"xp": 4321, "num_discs": 40,
		"matchmaking_rating": 1200, "total_battle_wins": 3, "total_battle_losses": 2, "cached_elder_credits": 7}}
	partial := map[string]any{"current_level": -1, "hype_profile": map[string]any{"xp": -1, "num_discs": -1,
		"matchmaking_rating": -1, "total_battle_wins": 5, "total_battle_losses": -1, "cached_elder_credits": -1}}

	off := newStoreServer(t, c, nil)
	auth, ar := login(t, off, platform.Epic, "own")
	for _, body := range []map[string]any{full, partial} {
		if status, env := call(t, off, "POST", "players/xp/", auth, body); status != http.StatusOK || string(env.Results) != "{}" {
			t.Fatalf("players/xp/ without a balance: %d %s", status, env.Results)
		}
	}
	o := get(off, auth, ar.UserID)
	if o.HypeUsername == "" || o.PlatformProfile.PlatformUsername != strings.SplitN(o.HypeUsername, "#", 2)[0] || !strings.Contains(o.HypeUsername, "#") || o.IsBanned ||
		o.HypeProfile.NumDiscs != 40 || o.HypeProfile.MatchmakingRating != 1200 || o.HypeProfile.TotalBattleWins != 5 ||
		o.HypeProfile.TotalBattleLosses != 2 || o.ElderCredits.Amount != 7 {
		t.Fatalf("own profile, store off: %+v", o)
	}

	// With the store on, and players/get/ arriving before players/xp/: it waits for the stats.
	on := newStoreServer(t, c, func(cfg *config.Config) { cfg.Store.ElderCreditBalance = 2500 })
	auth, ar = login(t, on, platform.Epic, "own2")
	done := make(chan own, 1)
	go func() { done <- get(on, auth, ar.UserID) }()
	time.Sleep(300 * time.Millisecond)
	if status, env := call(t, on, "POST", "players/xp/", auth, full); status != http.StatusOK {
		t.Fatalf("players/xp/: %d %+v", status, env.Error)
	}
	o = <-done
	if o.HypeProfile.NumDiscs != 40 || o.HypeProfile.TotalBattleWins != 3 || o.ElderCredits.Amount != 2500 {
		t.Fatalf("own profile, store on: %+v", o)
	}
}
