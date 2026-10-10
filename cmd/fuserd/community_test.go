package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"fuseronline/internal/store"
)

func TestCommunityCommand(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "fuseronline.db")
	cfgPath := filepath.Join(dir, "config.json")
	os.WriteFile(filepath.Join(dir, "dlc_songs.json"), []byte(`["countingstars"]`), 0o644)
	cfg := `{"token_secret": "` + strings.Repeat("k", 32) + `", "database_path": ` + jsonString(db) +
		`, "song_manifest": {"songs_file": "dlc_songs.json"}}`
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	run := func(stdin string, args ...string) (string, error) {
		var out strings.Builder
		err := communityCommand(append([]string{args[0], "-config", cfgPath}, args[1:]...), strings.NewReader(stdin), &out)
		return out.String(), err
	}

	add := []string{"add", "-x", "Song_A", "-kind", "archive", "-archive", "rar4", "-file", "1abcdefghijk,5000,Song A.rar",
		"-pak-entry", "Song_A_P.pak", "-sig-entry", "Song_A_P.sig", "-link", "https://drive.google.com/file/d/1abcdefghijk/view",
		"-creator", "SheepQueen", "-note", "permission given 2026-10-05"}
	if out, err := run("", add...); err != nil || !strings.HasPrefix(out, "added Song_A (archive rar4, 1 file(s)") {
		t.Fatalf("add: %q %v", out, err)
	}
	if _, err := run("", add...); err == nil || !strings.Contains(err.Error(), "already has an entry") {
		t.Fatalf("second add: %v", err)
	}
	if out, err := run("", "update", "-x", "Song_A", "-title", "New Title"); err != nil || !strings.HasPrefix(out, "updated") {
		t.Fatalf("update: %q %v", out, err)
	}
	if _, err := run("", "update", "-x", "Nope", "-title", "x"); err == nil {
		t.Fatal("update of a missing entry succeeded")
	}
	official := append([]string{}, add...)
	official[2] = "countingstars"
	if _, err := run("", official...); err == nil || !strings.Contains(err.Error(), "official") {
		t.Fatalf("official shortname: %v", err)
	}

	batch := strings.Join([]string{
		`# community batch`,
		`{"x":"Song_B","kind":"folder","files":[{"id":"1bbbbbbbbbbb","name":"Song_B_P.pak","size":10},{"id":"1cccccccccccc","name":"Song_B_P.sig","size":10}],"link":"https://drive.google.com/drive/folders/1dddddddddd","creator":"Gorilita"}`,
		`{"x":"Song_C","kind":"folder","files":[],"link":"https://drive.google.com/x","creator":"Gorilita"}`,
		`{"x":"Song_D","kind":"archive","archive":"zip","files":[{"id":"1eeeeeeeeeee","name":"d.zip","size":10}],"pak_entry":"Song_D_P.pak","sig_entry":"Song_D_P.sig","link":"https://drive.google.com/x","creator":"Harmonix/Someone"}`,
		`{"x":"Song_A","kind":"archive","archive":"zip","files":[{"id":"1fffffffffff","name":"a.zip","size":10}],"pak_entry":"Song_A_P.pak","sig_entry":"Song_A_P.sig","link":"https://drive.google.com/x","creator":"SheepQueen"}`,
		`{"x":"Song_E","unknown_field":1}`,
		``,
	}, "\n")
	out, err := run(batch, "import", "-batch", "-")
	if err != nil || !strings.Contains(out, "import: 1 added, 0 updated, 0 unchanged, 1 kept, 3 refused") {
		t.Fatalf("import: %q %v", out, err)
	}
	if out, err := run(batch, "import", "-batch", "-", "-update"); err != nil ||
		!strings.Contains(out, "import: 0 added, 1 updated, 1 unchanged, 0 kept, 3 refused") {
		t.Fatalf("import -update: %q %v", out, err)
	}

	if out, err := run("", "disable", "-x", "Song_B", "-reason", "creator asked"); err != nil || out != "disabled Song_B\n" {
		t.Fatalf("disable: %q %v", out, err)
	}
	if out, err := run("", "list", "-creator", "gori"); err != nil || !strings.Contains(out, "disabled: creator asked") || !strings.Contains(out, "1 entry") {
		t.Fatalf("list: %q %v", out, err)
	}
	if out, err := run("", "show", "-x", "song_a"); err != nil || !strings.Contains(out, `"archive": "zip"`) {
		t.Fatalf("show: %q %v", out, err)
	}
	if out, err := run("", "remove", "-x", "Song_A"); err != nil || out != "removed Song_A\n" {
		t.Fatalf("remove: %q %v", out, err)
	}
	if _, err := run("", "show", "-x", "Song_A"); err == nil {
		t.Fatal("show after remove succeeded")
	}
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if l, _ := st.ListCommunitySongs(context.Background(), ""); len(l) != 1 || l[0].X != "Song_B" || !l[0].Disabled {
		t.Fatalf("left: %+v", l)
	}
}

// A batch in community_sync.py's output format imports cleanly: its line format is the entry format. The batch is
// synthetic (made-up ids, names and links) with the kinds a real sync writes: rar5 and zip archives, the pak at the
// top or in a folder of the archive, and loose folders.
func TestCommunityImportSyncBatch(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	cfg := `{"token_secret": "` + strings.Repeat("k", 32) + `", "database_path": ` + jsonString(filepath.Join(dir, "c.db")) +
		`, "song_manifest": {"songs_file": ""}}`
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	batch := filepath.Join(dir, "community_batch.jsonl")
	if err := os.WriteFile(batch, []byte(syntheticSyncBatch()), 0o644); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	err := communityCommand([]string{"import", "-config", cfgPath, "-batch", batch}, strings.NewReader(""), &out)
	if err != nil || !strings.Contains(out.String(), "import: 12 added, 0 updated, 0 unchanged, 0 kept, 0 refused") {
		t.Fatalf("import: %q %v", out.String(), err)
	}
}

// syntheticSyncBatch is a community_sync.py-style JSON Lines batch of 12 made-up entries (a comment line first, as
// the script writes): 1 rar5 archive, 5 zip archives (every other one with its files in a folder) and 6 folders.
func syntheticSyncBatch() string {
	var b strings.Builder
	b.WriteString("# community_sync.py output (synthetic test data): 12 rows (rar5, zip, folders)\n")
	line := func(v map[string]any) {
		raw, err := json.Marshal(v)
		if err != nil {
			panic(err)
		}
		b.Write(raw)
		b.WriteByte('\n')
	}
	for i := 1; i <= 12; i++ {
		x := fmt.Sprintf("Test_Song%02d_Fake", i)
		id := func(n int) string { return fmt.Sprintf("FakeDriveId%02d%dabcdefghij", i, n) }
		entry := map[string]any{"x": x, "artist": fmt.Sprintf("Test Artist %d", i), "title": fmt.Sprintf("Test Title %d", i),
			"creator": "Test Creator", "submitted": "", "sheet": fmt.Sprintf("1000:%d", i+8), "note": "",
			"link": "https://example.invalid/community/" + id(0)}
		switch {
		case i == 1:
			entry["kind"], entry["archive"] = "archive", "rar5"
			entry["files"] = []map[string]any{{"id": id(0), "name": x + ".rar", "size": 8_000_000 + i}}
			entry["pak_entry"], entry["sig_entry"] = x+"_P.pak", x+"_P.sig"
		case i%2 == 1:
			folder := ""
			if i%4 == 3 {
				folder = fmt.Sprintf("Test Artist %d - Test Title %d/", i, i)
			}
			entry["kind"], entry["archive"] = "archive", "zip"
			entry["files"] = []map[string]any{{"id": id(0), "name": fmt.Sprintf("Test Title %d.zip", i), "size": 9_000_000 + i}}
			entry["pak_entry"], entry["sig_entry"] = folder+x+"_P.pak", folder+x+"_P.sig"
		default:
			entry["kind"] = "folder"
			entry["files"] = []map[string]any{{"id": id(1), "name": x + "_P.pak", "size": 7_000_000 + i},
				{"id": id(2), "name": x + "_P.sig", "size": 1080}}
		}
		line(entry)
	}
	return b.String()
}
