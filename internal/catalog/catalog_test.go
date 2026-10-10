package catalog

import (
	"bytes"
	"crypto/sha1"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writePak builds a version 8 pak the way fuser_pak_create.py does: per-file entry headers and data, the index,
// then FPakInfo (key GUID, encrypted flag, magic, version, index offset/size/hash) and the compression-name table.
func writePak(t *testing.T, path, mount string, files map[string][]byte) {
	t.Helper()
	var out bytes.Buffer
	var index bytes.Buffer
	fstring := func(b *bytes.Buffer, s string) {
		binary.Write(b, binary.LittleEndian, int32(len(s)+1))
		b.WriteString(s)
		b.WriteByte(0)
	}
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	type rec struct {
		name   string
		offset int64
		data   []byte
	}
	var recs []rec
	for _, n := range names {
		data := files[n]
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
	fstring(&index, mount)
	binary.Write(&index, binary.LittleEndian, int32(len(recs)))
	for _, r := range recs {
		h := sha1.Sum(r.data)
		fstring(&index, r.name)
		binary.Write(&index, binary.LittleEndian, [3]int64{r.offset, int64(len(r.data)), int64(len(r.data))})
		binary.Write(&index, binary.LittleEndian, uint32(0))
		index.Write(h[:])
		index.WriteByte(0)
		binary.Write(&index, binary.LittleEndian, uint32(0))
	}
	indexOffset := int64(out.Len())
	out.Write(index.Bytes())
	out.Write(make([]byte, 17))
	binary.Write(&out, binary.LittleEndian, [2]uint32{pakMagic, 8})
	binary.Write(&out, binary.LittleEndian, [2]int64{indexOffset, int64(index.Len())})
	ih := sha1.Sum(index.Bytes())
	out.Write(ih[:])
	out.Write(make([]byte, 160))
	if err := os.WriteFile(path, out.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeSig(t *testing.T, path string) {
	t.Helper()
	b := make([]byte, 32)
	binary.LittleEndian.PutUint32(b, sigMagic)
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func songFiles(sn string) map[string][]byte {
	base := "Audio/Songs/" + sn + "/"
	return map[string][]byte{
		"DLC/Songs/" + sn + "/Meta_" + sn + ".uasset":     []byte("song meta"),
		"DLC/Songs/" + sn + "/Meta_" + sn + ".uexp":       []byte("song meta exports"),
		"UI/AlbumArt/T_" + sn + ".uasset":                 []byte("art"),
		"UI/AlbumArt/T_" + sn + "_small.uexp":             []byte("art"),
		base + sn + "bs/Meta_" + sn + "bs.uasset":         []byte("stem meta"),
		base + sn + "bs/" + sn + "bs_midisong_maj.uasset": []byte("midi"),
	}
}

func TestOwnedBySongAlbumArt(t *testing.T) {
	for name, want := range map[string]bool{
		"ui/albumart/t_song.uasset":        true,
		"ui/albumart/t_song.uexp":          true,
		"ui/albumart/t_song.ubulk":         true,
		"ui/albumart/t_song_small.uasset":  true,
		"ui/albumart/t_song_small.ubulk":   true,
		"ui/albumart/t_songbird.uasset":    false, // another song's art
		"ui/albumart/t_song_big.uexp":      false,
		"ui/albumart/t_song":               false,
		"ui/albumart/t_song.uexp.uasset":   false,
		"ui/albumart/t_song_small.png":     false,
		"ui/albumart/t_song/evil.uasset":   false,
		"audio/songs/song/songbs/x.uasset": true,
		"audio/songs/songbird/x.uasset":    false,
	} {
		if got := ownedBySong(name, "song"); got != want {
			t.Errorf("ownedBySong(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestReadPakIndexAndInspect(t *testing.T) {
	dir := t.TempDir()
	pak := filepath.Join(dir, "Test_Song_P.pak")
	writePak(t, pak, CommunityMountPoint, songFiles("Test_Song"))
	f, _ := os.Open(pak)
	defer f.Close()
	st, _ := f.Stat()
	idx, err := ReadPakIndex(f, st.Size())
	if err != nil {
		t.Fatal(err)
	}
	if idx.Version != 8 || idx.MountPoint != CommunityMountPoint || len(idx.Entries) != 6 {
		t.Fatalf("index %+v", idx)
	}
	in, err := InspectPak(pak)
	if err != nil {
		t.Fatal(err)
	}
	if in.ShortName != "Test_Song" || in.MetaPath != "/Game/DLC/Songs/Test_Song/Meta_Test_Song.Meta_Test_Song" {
		t.Fatalf("inspection %+v", in)
	}
}

func TestInspectRejects(t *testing.T) {
	dir := t.TempDir()
	cases := map[string]struct {
		mount string
		files map[string][]byte
		want  string
	}{
		"mount": {"../../../Fuser/Content/DLC/", songFiles("A"), "mount point"},
		"no meta": {CommunityMountPoint, map[string][]byte{"Audio/Songs/A/A_midisong.uasset": []byte("x")},
			"found 0"},
		"two songs": {CommunityMountPoint, func() map[string][]byte {
			m := songFiles("A")
			for k, v := range songFiles("B") {
				m[k] = v
			}
			return m
		}(), "found 2"},
		"override": {CommunityMountPoint, func() map[string][]byte {
			m := songFiles("A")
			m["Gameplay/Hud/WBP_Hud.uasset"] = []byte("evil")
			m["UI/AlbumArt/T_Other.uasset"] = []byte("someone else's art")
			return m
		}(), "outside"},
	}
	for name, c := range cases {
		p := filepath.Join(dir, name+".pak")
		writePak(t, p, c.mount, c.files)
		if _, err := InspectPak(p); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err %v, want %q", name, err, c.want)
		}
	}
	notPak := filepath.Join(dir, "junk.pak")
	os.WriteFile(notPak, bytes.Repeat([]byte{1}, 400), 0o644)
	if _, err := InspectPak(notPak); err == nil {
		t.Error("junk accepted as a pak")
	}
}

func TestAddLoadAndLookup(t *testing.T) {
	dir := t.TempDir()
	pak, sig := filepath.Join(dir, "in_P.pak"), filepath.Join(dir, "in_P.sig")
	writePak(t, pak, CommunityMountPoint, songFiles("Test_Song"))
	writeSig(t, sig)
	catPath, files := filepath.Join(dir, "catalogue.json"), filepath.Join(dir, "files")
	c, err := Load(catPath, files)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	opts := AddOptions{PakPath: pak, SigPath: sig, Licence: "test only", Name: "Test Song", Artist: "Tester",
		GenreID: 6, Year: 2026, Now: now, Reserved: map[string]bool{"countingstars": true}}
	if _, err := c.Add(AddOptions{PakPath: pak, SigPath: sig}); err == nil {
		t.Fatal("added without a licence")
	}
	it, err := c.Add(opts)
	if err != nil {
		t.Fatal(err)
	}
	if it.Version != 1 || it.Genre.Name != "Pop" || it.ReleaseDate != "2026-10-01" || !uuidPattern.MatchString(it.UUID) {
		t.Fatalf("item %+v", it)
	}
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(files, "Test_Song", "1", "Test_Song_P.pak")); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Add(opts); !errors.Is(err, ErrUnchanged) {
		t.Fatalf("re-adding identical files: %v", err)
	}

	loaded, err := Load(catPath, files)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"/Game/DLC/Songs/Test_Song/Meta_Test_Song.Meta_Test_Song",
		"/game/dlc/songs/test_song/meta_test_song", "/Game/DLC/Songs/Test_Song/Meta_Test_Song.Meta_Test_Song:X"} {
		if got, ok := loaded.ByMetaPath(q); !ok || got.UUID != it.UUID {
			t.Errorf("ByMetaPath(%q) = %v", q, ok)
		}
	}
	if _, ok := loaded.ByShortName("TEST_SONG"); !ok {
		t.Error("ByShortName is case-sensitive")
	}

	// A changed pak needs Update, and then bumps the version and keeps the UUID.
	m := songFiles("Test_Song")
	m["Audio/Songs/Test_Song/extra.uasset"] = []byte("v2")
	writePak(t, pak, CommunityMountPoint, m)
	if _, err := loaded.Add(opts); err == nil {
		t.Fatal("replaced without update")
	}
	opts.Update = true
	v2, err := loaded.Add(opts)
	if err != nil || v2.Version != 2 || v2.UUID != it.UUID {
		t.Fatalf("update: %+v %v", v2, err)
	}

	// Reserved shortnames and size limits.
	writePak(t, pak, CommunityMountPoint, songFiles("countingstars"))
	if _, err := loaded.Add(AddOptions{PakPath: pak, SigPath: sig, Licence: "x", Reserved: opts.Reserved}); err == nil ||
		!strings.Contains(err.Error(), "reserved") {
		t.Fatalf("reserved shortname: %v", err)
	}
	writePak(t, pak, CommunityMountPoint, songFiles("Other"))
	if _, err := loaded.Add(AddOptions{PakPath: pak, SigPath: sig, Licence: "x", MaxPakBytes: 10}); err == nil {
		t.Fatal("over-size pak accepted")
	}
	os.WriteFile(sig, []byte("not a signature file"), 0o644)
	if _, err := loaded.Add(AddOptions{PakPath: pak, SigPath: sig, Licence: "x"}); err == nil {
		t.Fatal("bad .sig accepted")
	}
}

func TestLoadDetectsTamperedFiles(t *testing.T) {
	dir := t.TempDir()
	pak, sig := filepath.Join(dir, "in_P.pak"), filepath.Join(dir, "in_P.sig")
	writePak(t, pak, CommunityMountPoint, songFiles("S"))
	writeSig(t, sig)
	catPath, files := filepath.Join(dir, "catalogue.json"), filepath.Join(dir, "files")
	c, _ := Load(catPath, files)
	it, err := c.Add(AddOptions{PakPath: pak, SigPath: sig, Licence: "x"})
	if err != nil {
		t.Fatal(err)
	}
	c.Save()
	os.WriteFile(filepath.Join(c.Dir(it), it.PakName()), []byte("short"), 0o644)
	if _, err := Load(catPath, files); err == nil {
		t.Fatal("a pak with the wrong size was accepted")
	}
}

// TestRealSongPak inspects a real community pak when FUSER_TEST_PAK points at one (it is not in the repo).
func TestRealSongPak(t *testing.T) {
	p := os.Getenv("FUSER_TEST_PAK")
	if p == "" {
		t.Skip("FUSER_TEST_PAK not set")
	}
	in, err := InspectPak(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%s: %d entries, shortname %s, meta %s", filepath.Base(p), in.Entries, in.ShortName, in.MetaPath)
}
