package api

// Profile pictures. The game renders the player's profile picture from its avatar as three JPEGs (FProfilePictureData:
// RawData_Large / _Med / _Small, kept in the save game) whenever it captures a new one (FUN_1405bc910 ->
// UPlayerProfileMgr FUN_140740db0). It then asks players/profile_pic/ (a GET, BackendGetProfilePicAssetUploadURL)
// for FHypeProfilePicUploadAssetResponse {asset_upload_url, asset_medium_upload_url, asset_small_upload_url} and,
// for each non-empty URL, sends a BackendUploadDataJob (FUN_14071ad70): verb PUT (job slot 1 = 2; GET jobs 0, POST
// 1), the absolute URL as given (no API base), the raw JPEG as the body, no backend login (job slot 2 = 0). Its
// answer is not parsed. So an upload URL is a one-time capability: a random token, valid profile_pics.upload_minutes.
//
// Other players' games take the picture from FHypeUser / FMiniHypeUser.profile_pic (FHypeProfilePicMetadata
// {pic_id, asset_download_url, asset_medium_download_url, asset_small_download_url}) - player cards
// (UProfileCircle -> UFetchProfilePic: players/get/, then an image download of the size it shows), lists and
// profiles - and cache it per player (FCachedOtherProfilePicData). Every upload gets a new pic_id, and the download
// URLs carry it, so a new picture is fetched afresh. Before the first upload a player has no profile_pic: the
// default image.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg" // image.DecodeConfig formats: the game's JPEG, and PNG
	_ "image/png"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"fuseronline/internal/logx"
	"fuseronline/internal/store"
)

// The picture sizes, in the upload response's order: RawData_Large, _Med, _Small.
var picSizes = []string{"large", "medium", "small"}

// maxPicDimension bounds an uploaded image's width and height (the game's pictures are far smaller).
const maxPicDimension = 2048

type picUpload struct {
	player  int64
	picID   int32
	size    string
	expires time.Time
}

// picUploads holds the upload URLs handed out and not yet used, and the next picture id.
type picUploads struct {
	mu      sync.Mutex
	pending map[string]picUpload // token -> upload
	nextID  int32                // 0: not yet read from the store
}

// profilePicUploadURLs is players/profile_pic/: three one-time upload URLs for a new picture of the logged-in player.
// With profile_pics off, or no login, an empty answer: the game then skips the upload.
func (s *Server) profilePicUploadURLs(q *request) (any, error) {
	id := q.playerID()
	if !s.cfg.ProfilePics.Enabled || id == 0 {
		return nil, nil
	}
	picID, tokens, err := s.newPicUpload(q.Context(), id)
	if err != nil {
		return nil, err
	}
	base := s.cfg.PublicURL + "/pic/upload/"
	s.logs.For(logx.Players).Info("profile picture upload URLs", "player", s.logs.TagID(id), "pic_id", picID)
	return map[string]string{"asset_upload_url": base + tokens[0], "asset_medium_upload_url": base + tokens[1],
		"asset_small_upload_url": base + tokens[2]}, nil
}

func (s *Server) newPicUpload(ctx context.Context, player int64) (int32, []string, error) {
	u := &s.pics
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.nextID == 0 {
		max, err := s.store.MaxProfilePicID(ctx)
		if err != nil {
			return 0, nil, err
		}
		u.nextID = max + 1
	}
	if u.pending == nil {
		u.pending = map[string]picUpload{}
	}
	now := time.Now()
	for t, p := range u.pending {
		if now.After(p.expires) {
			delete(u.pending, t)
		}
	}
	picID := u.nextID
	u.nextID++
	expires := now.Add(time.Duration(s.cfg.ProfilePics.UploadMinutes) * time.Minute)
	tokens := make([]string, len(picSizes))
	for i, size := range picSizes {
		b := make([]byte, 16)
		if _, err := rand.Read(b); err != nil {
			return 0, nil, err
		}
		tokens[i] = hex.EncodeToString(b)
		u.pending[tokens[i]] = picUpload{player: player, picID: picID, size: size, expires: expires}
	}
	return picID, tokens, nil
}

// takePicUpload removes and returns a pending, unexpired upload.
func (s *Server) takePicUpload(token string) (picUpload, bool) {
	u := &s.pics
	u.mu.Lock()
	defer u.mu.Unlock()
	p, ok := u.pending[token]
	delete(u.pending, token)
	return p, ok && time.Now().Before(p.expires)
}

// picUpload is PUT (or POST) /pic/upload/<token>: one size of a new picture. The body must be a JPEG or PNG within
// profile_pics.max_kb and maxPicDimension. It is stored as <dir>/<player>/<pic id>_<size>.jpg (or .png); a newer
// picture replaces the player's previous one, whose files are deleted.
func (s *Server) picUpload(w http.ResponseWriter, r *http.Request) {
	lg := s.logs.For(logx.Players)
	if !s.cfg.ProfilePics.Enabled {
		http.NotFound(w, r)
		return
	}
	up, ok := s.takePicUpload(r.PathValue("token"))
	if !ok {
		lg.Warn("profile picture upload refused: unknown or expired URL", "client", s.logs.Tag(s.remoteHost(r)))
		http.Error(w, "unknown or expired upload URL", http.StatusNotFound)
		return
	}
	max := int64(s.cfg.ProfilePics.MaxKB) * 1024
	body, err := io.ReadAll(io.LimitReader(r.Body, max+1))
	if err != nil {
		http.Error(w, "read failed", http.StatusBadRequest)
		return
	}
	if int64(len(body)) > max {
		lg.Warn("profile picture upload refused: too large", "player", s.logs.TagID(up.player), "size", up.size, "max_kb", s.cfg.ProfilePics.MaxKB)
		http.Error(w, "image too large", http.StatusRequestEntityTooLarge)
		return
	}
	ext, err := picExtension(body)
	if err != nil {
		lg.Warn("profile picture upload refused", "player", s.logs.TagID(up.player), "size", up.size, "bytes", len(body), "err", err)
		http.Error(w, err.Error(), http.StatusUnsupportedMediaType)
		return
	}
	dir := filepath.Join(s.cfg.ProfilePics.Dir, strconv.FormatInt(up.player, 10))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		lg.Error("profile picture not stored", "player", s.logs.TagID(up.player), "err", err)
		http.Error(w, "storage failed", http.StatusInternalServerError)
		return
	}
	name := fmt.Sprintf("%d_%s%s", up.picID, up.size, ext)
	if err := writeFileAtomic(filepath.Join(dir, name), body); err != nil {
		lg.Error("profile picture not stored", "player", s.logs.TagID(up.player), "err", err)
		http.Error(w, "storage failed", http.StatusInternalServerError)
		return
	}
	replaced, err := s.store.AddProfilePicSize(r.Context(), up.player, up.picID, up.size)
	if errors.Is(err, store.ErrStalePicture) {
		os.Remove(filepath.Join(dir, name))
		lg.Info("profile picture upload dropped: a newer picture exists", "player", s.logs.TagID(up.player), "pic_id", up.picID)
		http.Error(w, "a newer picture exists", http.StatusConflict)
		return
	}
	if err != nil {
		os.Remove(filepath.Join(dir, name))
		lg.Error("profile picture not stored", "player", s.logs.TagID(up.player), "err", err)
		http.Error(w, "storage failed", http.StatusInternalServerError)
		return
	}
	if replaced != 0 {
		removePicFiles(dir, replaced)
	}
	lg.Info("profile picture stored", "player", s.logs.TagID(up.player), "pic_id", up.picID, "size", up.size, "bytes", len(body))
	w.WriteHeader(http.StatusOK)
}

// picExtension checks that b is a JPEG or PNG of sane dimensions and returns the file extension to store it under.
func picExtension(b []byte) (string, error) {
	cfg, format, err := image.DecodeConfig(bytes.NewReader(b))
	if err != nil {
		return "", errors.New("not a JPEG or PNG image")
	}
	if cfg.Width < 1 || cfg.Height < 1 || cfg.Width > maxPicDimension || cfg.Height > maxPicDimension {
		return "", fmt.Errorf("image is %dx%d; at most %d on each side", cfg.Width, cfg.Height, maxPicDimension)
	}
	switch format {
	case "jpeg":
		return ".jpg", nil
	case "png":
		return ".png", nil
	}
	return "", errors.New("not a JPEG or PNG image")
}

func writeFileAtomic(path string, b []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// removePicFiles deletes every size of picture picID in a player's directory.
func removePicFiles(dir string, picID int32) {
	prefix := strconv.Itoa(int(picID)) + "_"
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), prefix) {
			os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

// profilePicMeta is the FHypeProfilePicMetadata of a player's current picture, or nil (none, or pictures off).
func (s *Server) profilePicMeta(ctx context.Context, player int64) (map[string]any, error) {
	if !s.cfg.ProfilePics.Enabled {
		return nil, nil
	}
	pic, err := s.store.ProfilePic(ctx, player)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	url := func(size string) string {
		return fmt.Sprintf("%s/pic/%d/%d/%s.jpg", s.cfg.PublicURL, player, pic.PicID, size)
	}
	return map[string]any{"pic_id": pic.PicID, "asset_download_url": url("large"),
		"asset_medium_download_url": url("medium"), "asset_small_download_url": url("small")}, nil
}

// picFallback is the order sizes are tried in for a requested size: the nearest one first.
var picFallback = map[string][]string{
	"large":  {"large", "medium", "small"},
	"medium": {"medium", "large", "small"},
	"small":  {"small", "medium", "large"},
}

// servePic is GET /pic/<player>/<pic id>/<size>.jpg: the player's current picture in that size. A size not (yet)
// uploaded is served from the nearest one of the same picture (picFallback). An old picture id is gone.
func (s *Server) servePic(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.ProfilePics.Enabled {
		http.NotFound(w, r)
		return
	}
	if !s.limiter.allow(s.remoteHost(r)) {
		http.Error(w, "too many requests", http.StatusTooManyRequests)
		return
	}
	player, err1 := strconv.ParseInt(r.PathValue("player"), 10, 64)
	picID, err2 := strconv.ParseInt(r.PathValue("pic"), 10, 32)
	size := strings.TrimSuffix(r.PathValue("file"), ".jpg")
	if err1 != nil || err2 != nil || !slices.Contains(picSizes, size) {
		http.NotFound(w, r)
		return
	}
	pic, err := s.store.ProfilePic(r.Context(), player)
	if err != nil || int64(pic.PicID) != picID {
		http.NotFound(w, r)
		return
	}
	dir := filepath.Join(s.cfg.ProfilePics.Dir, strconv.FormatInt(player, 10))
	for _, sz := range picFallback[size] {
		if !slices.Contains(pic.Sizes, sz) {
			continue
		}
		for ext, ctype := range map[string]string{".jpg": "image/jpeg", ".png": "image/png"} {
			f, err := os.Open(filepath.Join(dir, fmt.Sprintf("%d_%s%s", picID, sz, ext)))
			if err != nil {
				continue
			}
			defer f.Close()
			h := w.Header()
			h.Set("Content-Type", ctype)
			h.Set("Cache-Control", "public, max-age=31536000, immutable")
			http.ServeContent(w, r, "", time.Time{}, f)
			return
		}
	}
	http.NotFound(w, r)
}
