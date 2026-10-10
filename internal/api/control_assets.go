package api

import (
	"archive/zip"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"fuseronline/internal/catalog"
	"fuseronline/internal/control"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

type controlAsset struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Artist    string `json:"artist"`
	Shortname string `json:"shortname"`
	Type      string `json:"type"`
	Image     string `json:"image"`
	Filename  string `json:"filename"`
	Size      int64  `json:"size"`
	Signature string `json:"signature"`
	Licence   string `json:"licence"`
	Source    string `json:"source"`
	Version   int32  `json:"version"`
}

func (s *Server) controlHiddenAssets(ctx context.Context) (map[string]bool, error) {
	var ids []string
	_, err := s.store.ControlDocument(ctx, "assets_hidden", &ids)
	out := map[string]bool{}
	for _, id := range ids {
		out[id] = true
	}
	return out, err
}
func (s *Server) controlAssets(ctx context.Context) ([]controlAsset, error) {
	hidden, err := s.controlHiddenAssets(ctx)
	if err != nil {
		return nil, err
	}
	out := []controlAsset{}
	if c := s.cat(); c != nil {
		for _, it := range c.Items() {
			if hidden[it.UUID] {
				continue
			}
			out = append(out, controlAsset{it.UUID, it.Name, it.Artist.Name, it.ShortName, "Song", s.artURL(it), it.PakName(), it.PakSize, it.SigName(), it.Licence, it.Source, it.Version})
		}
	}
	return out, nil
}
func (s *Server) controlAssetArchive(ctx context.Context, id, version string) error {
	c := s.cat()
	if c == nil {
		return errors.New("catalogue unavailable")
	}
	if _, ok := c.ByUUID(id); !ok {
		return errors.New("asset not found")
	}
	items, err := s.controlItems(ctx)
	if err != nil {
		return err
	}
	for _, it := range items {
		if it.ID == id || it.AssetID == id {
			return errors.New("clear or change all linked offers before removing this asset")
		}
	}
	var ids []string
	_, err = s.store.ControlDocument(ctx, "assets_hidden", &ids)
	if err != nil {
		return err
	}
	for _, old := range ids {
		if old == id {
			return nil
		}
	}
	ids = append(ids, id)
	return s.store.ControlSaveDocument(ctx, "assets_hidden", ids, version)
}
func copyAssetUpload(file *multipart.FileHeader, dest string, limit int64) error {
	if file.Size > limit {
		return errors.New("uploaded file exceeds its size limit")
	}
	in, err := file.Open()
	if err != nil {
		return err
	}
	defer in.Close()
	return copyAssetReader(in, dest, limit)
}
func copyAssetReader(in io.Reader, dest string, limit int64) error {
	out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	n, err := io.Copy(out, io.LimitReader(in, limit+1))
	closeErr := out.Close()
	if err != nil {
		return err
	}
	if n > limit {
		return errors.New("asset exceeds its size limit")
	}
	return closeErr
}

// ZIP members are copied to fixed temporary names, never to paths supplied by the archive.
func unzipAsset(file string, dir string, maxPak int64, externalSig bool) (string, string, error) {
	z, err := zip.OpenReader(file)
	if err != nil {
		return "", "", errors.New("invalid ZIP package")
	}
	defer z.Close()
	if len(z.File) > 32 {
		return "", "", errors.New("ZIP has too many entries")
	}
	pak, sig := "", ""
	for _, entry := range z.File {
		n := strings.ReplaceAll(entry.Name, "\\", "/")
		if path.IsAbs(n) || strings.Contains(n, ":") {
			return "", "", errors.New("unsafe ZIP entry path")
		}
		for _, part := range strings.Split(n, "/") {
			if part == ".." {
				return "", "", errors.New("unsafe ZIP entry path")
			}
		}
		if entry.FileInfo().IsDir() {
			continue
		}
		ext := strings.ToLower(path.Ext(n))
		var dest string
		limit := int64(8 << 20)
		switch ext {
		case ".pak":
			if pak != "" {
				return "", "", errors.New("ZIP must contain exactly one PAK")
			}
			pak = filepath.Join(dir, "asset.pak")
			dest = pak
			limit = maxPak
		case ".sig":
			if sig != "" || externalSig {
				return "", "", errors.New("provide exactly one signature")
			}
			sig = filepath.Join(dir, "asset.sig")
			dest = sig
		default:
			return "", "", errors.New("ZIP may contain only a PAK and its signature")
		}
		if entry.UncompressedSize64 > uint64(limit) {
			return "", "", errors.New("ZIP entry exceeds its size limit")
		}
		in, err := entry.Open()
		if err != nil {
			return "", "", err
		}
		err = copyAssetReader(in, dest, limit)
		in.Close()
		if err != nil {
			return "", "", err
		}
	}
	if pak == "" {
		return "", "", errors.New("ZIP contains no PAK")
	}
	return pak, sig, nil
}

// controlAssetUpload imports an uploaded asset and returns its UUID and shortname, or empty strings after writing an
// error response.
func (s *Server) controlAssetUpload(w http.ResponseWriter, r *http.Request) (id, shortname string) {
	fail := func(err error) { control.Fail(w, 400, err.Error()) }
	if s.catalog == nil || s.cfg.Store.CatalogueFile == "" || s.cfg.Store.FilesDir == "" {
		fail(errors.New("store catalogue and files directory must be configured"))
		return "", ""
	}
	maxPak := int64(s.cfg.Store.MaxPakMB) << 20
	if maxPak <= 0 || maxPak > 2<<30 {
		fail(errors.New("asset upload limit must be between 1 MB and 2 GB"))
		return "", ""
	}
	// Authenticated package transfers need longer than the game's 30-second request timeout.
	deadline := time.Now().Add(10 * time.Minute)
	response := http.NewResponseController(w)
	_ = response.SetReadDeadline(deadline)
	_ = response.SetWriteDeadline(deadline)
	r.Body = http.MaxBytesReader(w, r.Body, maxPak+(16<<20))
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		if r.MultipartForm != nil {
			r.MultipartForm.RemoveAll()
		}
		fail(errors.New("invalid or oversized upload"))
		return "", ""
	}
	defer r.MultipartForm.RemoveAll()
	for name, files := range r.MultipartForm.File {
		if name != "package" && name != "signature" && name != "artwork" || len(files) != 1 {
			fail(errors.New("unexpected or duplicate upload file"))
			return "", ""
		}
	}
	for name, values := range r.MultipartForm.Value {
		if name != "title" && name != "artist" && name != "shortname" && name != "licence" && name != "source" && name != "type" || len(values) != 1 || len(values[0]) > 2000 {
			fail(errors.New("invalid upload metadata"))
			return "", ""
		}
	}
	if r.FormValue("type") != "" && r.FormValue("type") != "Song" {
		fail(errors.New("only community song assets are supported"))
		return "", ""
	}
	title, artist := strings.TrimSpace(r.FormValue("title")), strings.TrimSpace(r.FormValue("artist"))
	if title == "" || artist == "" || len(title) > 80 || len(artist) > 80 || strings.TrimSpace(r.FormValue("licence")) == "" {
		fail(errors.New("name, artist and licence are required (name and artist: 80 characters maximum)"))
		return "", ""
	}
	packages := r.MultipartForm.File["package"]
	if len(packages) != 1 {
		fail(errors.New("asset package is required"))
		return "", ""
	}
	dir, err := os.MkdirTemp("", "fuser-control-asset-")
	if err != nil {
		control.Fail(w, 500, "could not stage upload")
		return "", ""
	}
	defer os.RemoveAll(dir)
	pak, sig, artPath := filepath.Join(dir, "asset.pak"), "", ""
	externalSig := len(r.MultipartForm.File["signature"]) == 1
	switch strings.ToLower(filepath.Ext(packages[0].Filename)) {
	case ".pak":
		err = copyAssetUpload(packages[0], pak, maxPak)
	case ".zip":
		z := filepath.Join(dir, "upload.zip")
		err = copyAssetUpload(packages[0], z, maxPak+(8<<20))
		if err == nil {
			pak, sig, err = unzipAsset(z, dir, maxPak, externalSig)
		}
	default:
		err = errors.New("choose a PAK or ZIP package")
	}
	if err != nil {
		fail(err)
		return "", ""
	}
	if externalSig {
		file := r.MultipartForm.File["signature"][0]
		if !strings.EqualFold(filepath.Ext(file.Filename), ".sig") {
			fail(errors.New("signature must be a .sig file"))
			return "", ""
		}
		sig = filepath.Join(dir, "asset.sig")
		if err = copyAssetUpload(file, sig, 8<<20); err != nil {
			fail(err)
			return "", ""
		}
	}
	if sig == "" {
		fail(errors.New("a signature is required, separately or inside the ZIP"))
		return "", ""
	}
	inspection, err := catalog.InspectPak(pak)
	if err != nil {
		fail(err)
		return "", ""
	}
	if expected := strings.TrimSpace(r.FormValue("shortname")); expected != "" && !strings.EqualFold(expected, inspection.ShortName) {
		fail(fmt.Errorf("package shortname is %s, not %s", inspection.ShortName, expected))
		return "", ""
	}
	if files := r.MultipartForm.File["artwork"]; len(files) == 1 {
		file := files[0]
		if file.Size > 3<<20 {
			fail(errors.New("artwork must be under 3 MB"))
			return "", ""
		}
		in, err := file.Open()
		if err != nil {
			fail(err)
			return "", ""
		}
		b, err := io.ReadAll(io.LimitReader(in, (3<<20)+1))
		in.Close()
		if err != nil {
			fail(err)
			return "", ""
		} // Reuse the existing validated image decoder and canonical PNG output.
		imageURL, err := s.controlImage(r.Context(), "data:image/png;base64,"+base64.StdEncoding.EncodeToString(b))
		if err != nil {
			fail(err)
			return "", ""
		}
		hash := path.Base(imageURL)
		pngBytes, err := s.store.ControlMedia(r.Context(), hash)
		if err != nil || len(pngBytes) > 4<<20 {
			fail(errors.New("normalized artwork exceeds 4 MB"))
			return "", ""
		}
		artPath = filepath.Join(dir, "art.png")
		if err = os.WriteFile(artPath, pngBytes, 0600); err != nil {
			fail(err)
			return "", ""
		}
	}
	reserved := map[string]bool{}
	for _, sn := range append(append([]string{}, s.cfg.SongManifest.Songs...), s.cfg.Store.Reserved...) {
		reserved[strings.ToLower(sn)] = true
	}
	it, err := s.catalog.Import(catalog.AddOptions{PakPath: pak, SigPath: sig, Name: title, SongName: title, Artist: artist, Licence: strings.TrimSpace(r.FormValue("licence")), Source: r.FormValue("source"), Reserved: reserved, MaxPakBytes: maxPak, ArtPath: artPath})
	if err != nil {
		if errors.Is(err, catalog.ErrUnchanged) {
			err = errors.New("this asset is already in the catalogue")
		}
		fail(err)
		return "", ""
	}
	control.JSON(w, 201, map[string]any{"id": it.UUID, "shortname": it.ShortName})
	return it.UUID, it.ShortName
}
