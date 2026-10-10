package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fuseronline/internal/community"
)

func testCommunitySong(x string) community.Song {
	return community.Song{X: x, Kind: community.KindArchive, Archive: "rar5",
		Files:    []community.File{{ID: "1abcdefghijklmnop", Name: x + ".rar", Size: 1234}},
		PakEntry: x + "_P.pak", SigEntry: x + "_P.sig", Link: "https://drive.google.com/file/d/1abcdefghijklmnop/view",
		Artist: "A", Title: "T", Creator: "SheepQueen", Sheet: "1851575492:42"}
}

func TestCommunitySongs(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	s := testCommunitySong("Song_One")
	if res, err := st.PutCommunitySong(ctx, s, false); err != nil || res != CommunityAdded {
		t.Fatalf("add: %q %v", res, err)
	}
	if _, err := st.PutCommunitySong(ctx, s, false); !errors.Is(err, ErrCommunityExists) {
		t.Fatalf("second add: %v", err)
	}
	if res, err := st.PutCommunitySong(ctx, s, true); err != nil || res != CommunityUnchanged {
		t.Fatalf("identical update: %q %v", res, err)
	}
	bad := s
	bad.Creator = "Harmonix"
	if _, err := st.PutCommunitySong(ctx, bad, true); !errors.Is(err, community.ErrHarmonix) {
		t.Fatalf("harmonix update: %v", err)
	}
	if _, err := st.PutCommunitySong(ctx, testCommunitySong("Song_Two"), false); err != nil {
		t.Fatal(err)
	}

	// Lookups are case-insensitive, as FNames are.
	m, err := st.CommunitySongs(ctx, []string{"song_one", "SONG_TWO", "missing"})
	if err != nil || len(m) != 2 || m["song_one"] == nil || m["song_one"].X != "Song_One" || m["song_one"].Files[0].Size != 1234 {
		t.Fatalf("lookup: %v %+v", err, m)
	}

	// Reports: a good install records its hash; two players reporting a bad file disable the entry.
	h := strings.Repeat("ab", 32)
	if d, err := st.AddCommunityReport(ctx, "song_one", CommunityReport{PlayerID: 1, OK: true, SHA256: h}); err != nil || d {
		t.Fatalf("good report: %v %v", d, err)
	}
	if d, err := st.AddCommunityReport(ctx, "Song_One", CommunityReport{PlayerID: 2, Reason: "quota"}); err != nil || d {
		t.Fatalf("quota report: %v %v", d, err)
	}
	if d, err := st.AddCommunityReport(ctx, "Song_One", CommunityReport{PlayerID: 2, Reason: "layout"}); err != nil || d {
		t.Fatalf("first layout report: %v %v", d, err)
	}
	if d, err := st.AddCommunityReport(ctx, "Song_One", CommunityReport{PlayerID: 2, Reason: "layout"}); err != nil || d {
		t.Fatalf("same player again: %v %v", d, err)
	}
	if d, err := st.AddCommunityReport(ctx, "Song_One", CommunityReport{PlayerID: 3, Reason: "sig"}); err != nil || !d {
		t.Fatalf("second player's bad file: %v %v", d, err)
	}
	e, err := st.CommunitySong(ctx, "Song_One")
	if err != nil || !e.Disabled || len(e.SeenSHA256) != 1 || e.SeenSHA256[0] != h {
		t.Fatalf("after reports: %v %+v", err, e)
	}
	if _, err := st.AddCommunityReport(ctx, "missing", CommunityReport{PlayerID: 1, Reason: "http"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("report for a missing entry: %v", err)
	}

	// Enabling clears the failure reports; a changed update clears them all and re-enables.
	if err := st.SetCommunityDisabled(ctx, "Song_One", false, ""); err != nil {
		t.Fatal(err)
	}
	if r, _ := st.CommunityReports(ctx, "Song_One"); len(r) != 1 || !r[0].OK {
		t.Fatalf("reports after enable: %+v", r)
	}
	st.SetCommunityDisabled(ctx, "Song_One", true, "test")
	changed := s
	changed.Files = []community.File{{ID: "1zyxwvutsrqponmlk", Name: "new.rar", Size: 99}}
	if res, err := st.PutCommunitySong(ctx, changed, true); err != nil || res != CommunityUpdated {
		t.Fatalf("changed update: %q %v", res, err)
	}
	if e, _ := st.CommunitySong(ctx, "Song_One"); e.Disabled || len(e.SeenSHA256) != 0 || e.Files[0].ID != "1zyxwvutsrqponmlk" {
		t.Fatalf("after a changed update: %+v", e)
	}

	if l, err := st.ListCommunitySongs(ctx, "sheep"); err != nil || len(l) != 2 {
		t.Fatalf("list by creator: %v %d", err, len(l))
	}
	if l, _ := st.ListCommunitySongs(ctx, "nobody"); len(l) != 0 {
		t.Fatalf("list by another creator: %d", len(l))
	}
	if err := st.RemoveCommunitySong(ctx, "song_two"); err != nil {
		t.Fatal(err)
	}
	if err := st.RemoveCommunitySong(ctx, "song_two"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second remove: %v", err)
	}
}

// CommunityPage filters, sorts and pages over a database the size of the live one, with stats over everything.
func TestCommunityPage(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	const n = 12500
	songs := make([]community.Song, n)
	for i := range songs {
		s := testCommunitySong(fmt.Sprintf("Song_%05d", i))
		s.Artist, s.Title, s.Creator = fmt.Sprintf("Artist %03d", (n-i)%500), "Title", fmt.Sprintf("Creator%d", i%40)
		if i%5 == 0 {
			s.Kind, s.Archive, s.PakEntry, s.SigEntry = community.KindFolder, "", "", ""
			s.Files = []community.File{{ID: "1abcdefghijklmnop", Name: s.X + "_P.pak", Size: 9}, {ID: "1abcdefghijklmnoq", Name: s.X + "_P.sig", Size: 9}}
		}
		songs[i] = s
	}
	start := time.Now()
	res, err := st.PutCommunitySongs(ctx, songs, false)
	if err != nil || len(res) != n || res[0] != CommunityAdded {
		t.Fatalf("batch put: %d results, %v", len(res), err)
	}
	t.Logf("put %d entries in one transaction: %v", n, time.Since(start))
	if err := st.SetCommunityDisabled(ctx, "Song_00007", true, "test"); err != nil {
		t.Fatal(err)
	}
	for _, r := range []CommunityReport{{PlayerID: 1, OK: true}, {PlayerID: 2, OK: false, Reason: "download"}} {
		if _, err := st.AddCommunityReport(ctx, "Song_00003", r); err != nil {
			t.Fatal(err)
		}
	}

	start = time.Now()
	page, err := st.CommunityPage(ctx, CommunityQuery{Page: 1, Size: 50})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("first page of %d: %v", n, time.Since(start))
	want := CommunityStats{Songs: n, Enabled: n - 1, Disabled: 1, Archives: n - n/5, Folders: n / 5, Failing: 1}
	if page.Stats != want || page.Matched != n || page.Pages != 250 || len(page.Items) != 50 {
		t.Fatalf("first page: stats %+v matched %d pages %d items %d", page.Stats, page.Matched, page.Pages, len(page.Items))
	}

	start = time.Now()
	page, err = st.CommunityPage(ctx, CommunityQuery{Search: "creator7", Sort: "artist", Page: 2, Size: 25})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("search and artist sort: %v", time.Since(start))
	// Creator7 and Creator70..? don't exist (i%40), so the match is exactly the 312 or 313 entries with i%40 == 7.
	if page.Matched != 313 || page.Page != 2 || len(page.Items) != 25 {
		t.Fatalf("search: matched %d page %d items %d", page.Matched, page.Page, len(page.Items))
	}
	for i := 1; i < len(page.Items); i++ {
		if page.Items[i-1].Artist > page.Items[i].Artist {
			t.Fatalf("not sorted by artist: %q then %q", page.Items[i-1].Artist, page.Items[i].Artist)
		}
	}

	page, err = st.CommunityPage(ctx, CommunityQuery{State: "failing", Page: 1, Size: 50})
	if err != nil || page.Matched != 1 || page.Items[0].X != "Song_00003" || page.Items[0].ReportsOK != 1 || page.Items[0].ReportsFailed != 1 {
		t.Fatalf("failing: %+v %v", page, err)
	}
	page, err = st.CommunityPage(ctx, CommunityQuery{State: "disabled", Kind: community.KindArchive, Page: 1, Size: 50})
	if err != nil || page.Matched != 1 || !page.Items[0].Disabled || page.Items[0].DisabledReason != "test" {
		t.Fatalf("disabled archives: %+v %v", page, err)
	}
	page, err = st.CommunityPage(ctx, CommunityQuery{Kind: community.KindFolder, Sort: "x", Page: 999, Size: 100})
	if err != nil || page.Matched != n/5 || page.Page != 25 || page.Items[0].X != "Song_12000" {
		t.Fatalf("clamped last folder page: matched %d page %d first %s %v", page.Matched, page.Page, page.Items[0].X, err)
	}
	for _, q := range []CommunityQuery{{State: "nope", Size: 1}, {Kind: "zip", Size: 1}, {Sort: "random", Size: 1}, {Size: 0}} {
		if _, err := st.CommunityPage(ctx, q); err == nil {
			t.Errorf("%+v accepted", q)
		}
	}

	all, err := st.CommunityEntriesJSON(ctx)
	if err != nil || len(all) != n || !strings.Contains(all["song_00001"], `"x":"Song_00001"`) {
		t.Fatalf("entries JSON: %d %v", len(all), err)
	}
	// A failing batch writes nothing.
	bad := songs[1]
	bad.Link = "not a link"
	if _, err := st.PutCommunitySongs(ctx, []community.Song{testCommunitySong("Fresh_One"), bad}, true); err == nil {
		t.Fatal("bad batch accepted")
	}
	if c, _ := st.CommunityCount(ctx); c != n {
		t.Fatalf("count after failed batch: %d", c)
	}
}
