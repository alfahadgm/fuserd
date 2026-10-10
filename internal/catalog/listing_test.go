package catalog

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPublishArtAndUUID(t *testing.T) {
	dir := t.TempDir()
	pak, sig, art := filepath.Join(dir, "in_P.pak"), filepath.Join(dir, "in_P.sig"), filepath.Join(dir, "cover.PNG")
	writePak(t, pak, CommunityMountPoint, songFiles("Test_Song"))
	writeSig(t, sig)
	os.WriteFile(art, []byte("\x89PNG fake"), 0o644)
	catPath, files := filepath.Join(dir, "catalogue.json"), filepath.Join(dir, "files")
	c, err := Load(catPath, files)
	if err != nil {
		t.Fatal(err)
	}
	opts := AddOptions{PakPath: pak, SigPath: sig, Licence: "test only", ArtPath: art}
	it, err := c.Add(opts)
	if err != nil {
		t.Fatal(err)
	}
	if it.Art != "Test_Song_art.png" {
		t.Fatalf("art %q", it.Art)
	}
	if _, err := os.Stat(filepath.Join(c.Dir(it), it.Art)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Add(AddOptions{PakPath: pak, SigPath: sig, Licence: "x", ArtPath: sig, Update: true}); err == nil {
		t.Fatal("a .sig accepted as album art")
	}

	if _, err := c.Publish("Nope", Listing{Price: 1}); err == nil {
		t.Fatal("published a song that isn't in the catalogue")
	}
	if _, err := c.Publish("test_song", Listing{Price: -5}); err == nil {
		t.Fatal("negative price accepted")
	}
	pub, err := c.Publish("test_song", Listing{Price: 250, Featured: true, StartDate: "2026-10-01T00:00:00Z"})
	if err != nil {
		t.Fatal(err)
	}
	if pub.ElderCredit.EndDate != DefaultListingEnd {
		t.Fatalf("default end date %q", pub.ElderCredit.EndDate)
	}
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}

	loaded, err := Load(catPath, files)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := loaded.ByUUID(it.UUID)
	if !ok || got.ElderCredit == nil || got.ElderCredit.Price != 250 || !got.ElderCredit.Featured {
		t.Fatalf("ByUUID after reload: %+v %v", got, ok)
	}

	// An update without new art carries the art and the listing over to the new version.
	m := songFiles("Test_Song")
	m["Audio/Songs/Test_Song/extra.uasset"] = []byte("v2")
	writePak(t, pak, CommunityMountPoint, m)
	v2, err := loaded.Add(AddOptions{PakPath: pak, SigPath: sig, Licence: "test only", Update: true})
	if err != nil {
		t.Fatal(err)
	}
	if v2.Version != 2 || v2.Art != it.Art || v2.ElderCredit == nil || v2.ElderCredit.Price != 250 {
		t.Fatalf("update dropped art or listing: %+v", v2)
	}
	if _, err := os.Stat(filepath.Join(loaded.Dir(v2), v2.Art)); err != nil {
		t.Fatal(err)
	}

	un, err := loaded.Unpublish("TEST_SONG")
	if err != nil || un.ElderCredit != nil {
		t.Fatalf("unpublish: %+v %v", un, err)
	}

	// A catalogue whose art file is missing doesn't load.
	if err := loaded.Save(); err != nil {
		t.Fatal(err)
	}
	os.Remove(filepath.Join(loaded.Dir(v2), v2.Art))
	if _, err := Load(catPath, files); err == nil {
		t.Fatal("loaded with the album art missing")
	}
}

func TestListingActive(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	var none *Listing
	if none.Active(now) {
		t.Error("nil listing is active")
	}
	for _, tc := range []struct {
		l    Listing
		want bool
	}{
		{Listing{}, true},
		{Listing{EndDate: DefaultListingEnd}, true},
		{Listing{StartDate: "2026-10-02T00:00:00Z"}, false},
		{Listing{StartDate: "2026-09-01", EndDate: "2026-10-01T11:59:59Z"}, false},
		{Listing{StartDate: "2026-09-01", EndDate: "2026-10-02"}, true},
	} {
		if got := tc.l.Active(now); got != tc.want {
			t.Errorf("%+v: active %v, want %v", tc.l, got, tc.want)
		}
	}
}
