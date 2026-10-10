package api

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fuseronline/internal/catalog"
	"mime/multipart"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestControlAssetUploadPublishArchive(t *testing.T) {
	_, s := newTestServerAndAPI(t, nil)
	dir := t.TempDir()
	s.cfg.Store.CatalogueFile = filepath.Join(dir, "catalogue.json")
	s.cfg.Store.FilesDir = filepath.Join(dir, "files")
	s.cfg.Store.MaxPakMB = 1
	live, err := catalog.NewLive(s.cfg.Store.CatalogueFile, s.cfg.Store.FilesDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	s.SetCatalogue(live)
	pak := filepath.Join(dir, "Upload_Song.pak")
	testPak(t, pak, "Upload_Song")
	pakBytes, _ := os.ReadFile(pak)
	sigBytes, _ := os.ReadFile(filepath.Join(dir, "Upload_Song.sig"))
	upload := func(zipped, signature bool) *httptest.ResponseRecorder {
		var body bytes.Buffer
		m := multipart.NewWriter(&body)
		for k, v := range map[string]string{"title": "Uploaded song", "artist": "Creator", "licence": "Permission to share", "type": "Song"} {
			m.WriteField(k, v)
		}
		filename, data := "song.pak", pakBytes
		if zipped {
			var b bytes.Buffer
			z := zip.NewWriter(&b)
			f, _ := z.Create("folder/song.pak")
			f.Write(pakBytes)
			if signature {
				f, _ = z.Create("folder/song.sig")
				f.Write(sigBytes)
			}
			z.Close()
			filename, data = "song.zip", b.Bytes()
		}
		f, _ := m.CreateFormFile("package", filename)
		f.Write(data)
		if !zipped && signature {
			f, _ = m.CreateFormFile("signature", "song.sig")
			f.Write(sigBytes)
		}
		m.Close()
		r := httptest.NewRequest("POST", "/control/api/assets", &body)
		r.Header.Set("Content-Type", m.FormDataContentType())
		w := httptest.NewRecorder()
		s.controlAssetUpload(w, r)
		return w
	}
	if w := upload(false, false); w.Code != 400 {
		t.Fatalf("missing signature: %d %s", w.Code, w.Body)
	}
	unlock, err := catalog.Lock(s.cfg.Store.CatalogueFile)
	if err != nil {
		t.Fatal(err)
	}
	w := upload(false, true)
	unlock()
	if w.Code != 400 {
		t.Fatalf("concurrent writer: %d", w.Code)
	}
	w = upload(true, true)
	if w.Code != 201 {
		t.Fatalf("upload: %d %s", w.Code, w.Body)
	}
	ctx := context.Background()
	assets, err := s.controlAssets(ctx)
	if err != nil || len(assets) != 1 {
		t.Fatalf("library: %+v %v", assets, err)
	}
	asset := assets[0]
	song, ok := s.cat().ByUUID(asset.ID)
	if !ok || song.ElderCredit != nil {
		t.Fatal("upload must be visible immediately without publishing")
	}
	if w := upload(false, true); w.Code != 400 {
		t.Fatalf("duplicate: %d", w.Code)
	}
	items, err := s.controlItems(ctx)
	if err != nil {
		t.Fatal(err)
	}
	items[0].ID = asset.ID
	items[0].AssetID = asset.ID
	items[0].Title = asset.Title
	items[0].Artist = asset.Artist
	items[0].Shortname = asset.Shortname
	items[0].Enabled = true
	save := func() error {
		b, _ := json.Marshal(items)
		r := httptest.NewRequest("PUT", "/control/api/items", bytes.NewReader(b))
		r.SetPathValue("kind", "items")
		version, _ := s.store.ControlDocumentVersion(ctx, "items")
		r.Header.Set("If-Match", version)
		return s.controlSave(httptest.NewRecorder(), r, &controlChange{})
	}
	if err := save(); err != nil {
		t.Fatal(err)
	}
	version, _ := s.store.ControlDocumentVersion(ctx, "assets_hidden")
	if err := s.controlAssetArchive(ctx, asset.ID, version); err == nil {
		t.Fatal("linked asset archived")
	}
	items[0].Enabled = false
	items[0].Shortname = ""
	items[0].AssetID = ""
	if err := save(); err != nil {
		t.Fatal(err)
	}
	if err := s.controlAssetArchive(ctx, asset.ID, version); err != nil {
		t.Fatal(err)
	}
	assets, err = s.controlAssets(ctx)
	if err != nil || len(assets) != 0 {
		t.Fatal("archive remains selectable", err)
	}
	if _, ok := s.cat().ByUUID(asset.ID); !ok {
		t.Fatal("archive removed download catalogue entry")
	}
	loaded, err := catalog.Load(s.cfg.Store.CatalogueFile, s.cfg.Store.FilesDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := loaded.ByUUID(asset.ID); !ok {
		t.Fatal("upload did not persist")
	}
	items[0].Shortname = asset.Shortname
	items[0].AssetID = asset.ID
	items[0].Enabled = true
	if err := save(); err == nil {
		t.Fatal("archived asset published")
	}
}

func TestControlAssetZipRejectsUnsafeAndDuplicateEntries(t *testing.T) {
	for _, names := range [][]string{{"../song.pak"}, {"C:/song.pak"}, {"song.pak", "second.pak"}, {"song.pak", "song.sig", "other.sig"}, {"song.pak", "script.exe"}} {
		dir := t.TempDir()
		filename := filepath.Join(dir, "upload.zip")
		var b bytes.Buffer
		z := zip.NewWriter(&b)
		for _, name := range names {
			f, _ := z.Create(name)
			f.Write([]byte("test"))
		}
		z.Close()
		os.WriteFile(filename, b.Bytes(), 0600)
		if _, _, err := unzipAsset(filename, dir, 1024, false); err == nil {
			t.Fatalf("accepted %v", names)
		}
	}
}
