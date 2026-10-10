package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"fuseronline/internal/catalog"
)

func TestGenreID(t *testing.T) {
	for in, want := range map[string]int32{"0": 0, "6": 6, "pop": 6, "Rock": 8, "hiphop": 4} {
		if got, err := genreID(in); err != nil || got != want {
			t.Errorf("genreID(%q) = %d, %v", in, got, err)
		}
	}
	for _, bad := range []string{"9", "-1", "polka"} {
		if _, err := genreID(bad); err == nil {
			t.Errorf("genreID(%q) accepted", bad)
		}
	}
}

// TestCatalogueAddRealPak runs "fuserd catalogue add" end to end on a real community pak when FUSER_TEST_PAK points
// at one (the test song is not in the repo).
func TestCatalogueAddRealPak(t *testing.T) {
	pak := os.Getenv("FUSER_TEST_PAK")
	if pak == "" {
		t.Skip("FUSER_TEST_PAK not set")
	}
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	os.WriteFile(filepath.Join(dir, "dlc_songs.json"), []byte(`["countingstars"]`), 0o644)
	os.WriteFile(cfgPath, []byte(`{"token_secret": "`+strings.Repeat("k", 32)+`",
		"song_manifest": {"songs_file": "dlc_songs.json"},
		"store": {"catalogue_file": "cat.json", "files_dir": "files"}}`), 0o644)
	args := []string{"add", "-config", cfgPath, "-pak", pak, "-licence", "local test only", "-name", "Seinfeld Theme",
		"-artist", "Jonathan Wolff", "-genre", "pop", "-year", "1989"}
	if err := catalogueCommand(args); err != nil {
		t.Fatal(err)
	}
	if err := catalogueCommand(args); err != nil { // identical files: reported, not an error
		t.Fatal(err)
	}
	c, err := catalog.Load(filepath.Join(dir, "cat.json"), filepath.Join(dir, "files"))
	if err != nil {
		t.Fatal(err)
	}
	items := c.Items()
	if len(items) != 1 || items[0].Genre.Name != "Pop" || items[0].Artist.Name != "Jonathan Wolff" {
		t.Fatalf("catalogue %+v", items)
	}
	t.Logf("%s v%d %s, pak %d bytes sha256 %s", items[0].ShortName, items[0].Version, items[0].MetaPath,
		items[0].PakSize, items[0].PakSHA256)
	if err := catalogueCommand([]string{"list", "-config", cfgPath}); err != nil {
		t.Fatal(err)
	}
}
