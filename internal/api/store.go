package api

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"fuseronline/internal/catalog"
	"fuseronline/internal/community"
	"fuseronline/internal/logx"
	"fuseronline/internal/store"
)

// The community song catalogue. A player's loader resolves a crate path it can't load to a
// catalogue entry (api/community/resolve/), downloads <sn>_P.pak + .sig from /dl/, checks their sha256 and mounts
// them. The game's own store endpoints describe the same entries; their pc_asset_download_url is the game's DLC
// container (int32 sigLen, .sig bytes, .pak bytes - _OnDownloadFinished FUN_1405e6be0 / FUN_14060e250), so the
// game's crate-download path can use them too.

// SetCatalogue attaches the catalogue the store endpoints answer from (nil = none).
func (s *Server) SetCatalogue(l *catalog.Live) { s.catalog = l }

func (s *Server) cat() *catalog.Catalog {
	if s.catalog == nil {
		return nil
	}
	return s.catalog.Get()
}

// artURL is an entry's album art URL, or "" without one.
func (s *Server) artURL(it catalog.Item) string {
	if it.Art == "" {
		return ""
	}
	return s.downloadBase(it) + it.Art
}

// downloadBase is an entry's /dl/ directory URL.
func (s *Server) downloadBase(it catalog.Item) string {
	return s.cfg.PublicURL + "/dl/" + it.ShortName + "/" + strconv.Itoa(int(it.Version)) + "/"
}

// storeItem is FStoreItemResponse for a catalogue song. Server-only fields are not sent.
func (s *Server) storeItem(it catalog.Item) map[string]any {
	art := s.artURL(it)
	var slots []store.ControlItem
	if ok, err := s.store.ControlDocument(context.Background(), "items", &slots); ok && err == nil {
		for _, slot := range slots {
			if strings.EqualFold(slot.ID, it.UUID) {
				it.Name = slot.Title
				it.SongName = slot.Title
				it.Artist.Name = slot.Artist
				art = slot.Image
				break
			}
		}
	}

	offer := func(idKey string) map[string]string {
		return map[string]string{"release_date": it.ReleaseDate, idKey: ""}
	}
	return map[string]any{
		"uuid": it.UUID, "type": "song", "shortName": it.ShortName, "name": it.Name, "song_name": it.SongName,
		"album_name": it.AlbumName, "artist": it.Artist, "genre": it.Genre, "year": it.Year, "version": it.Version,
		"unlockables": []any{}, "contained_items": []string{},
		"pc_asset_download_url":         s.downloadBase(it) + it.BinName(),
		"pc_image_download_url":         art,
		"pc_preview_asset_download_url": "",
		"ps_asset_download_url":         "", "xbox_asset_download_url": "", "switch_asset_download_url": "",
		"ps_image_download_url": "", "xbox_image_download_url": "", "switch_image_download_url": "",
		"ps_preview_asset_download_url": "", "xbox_preview_asset_download_url": "",
		"switch_preview_asset_download_url": "",
		"steam_offer":                       offer("app_id"),
		"epic_offer":                        offer("offer_id"),
		"xbox_offer":                        map[string]string{"release_date": "", "product_id": ""},
		"psn_offer":                         []any{},
		"nintendo_offer":                    []any{},
	}
}

// getSong is store/songs/get_song/ (FGetDownloadableSongByShortnameRequest {shortName}): the client's per-shortname
// lookup when a crate song is a DLC stub (FUN_1405dd570). Every request is logged, hit or miss.
func (s *Server) getSong(q *request) (any, error) {
	var req struct {
		ShortName string `json:"shortName"`
	}
	if err := q.decode(&req); err != nil {
		return nil, err
	}
	lg := s.logs.For(logx.Store)
	c := s.cat()
	if c != nil {
		if it, ok := c.ByShortName(req.ShortName); ok {
			lg.Info("get_song", "shortName", req.ShortName, "found", true, "version", it.Version)
			return s.storeItem(it), nil
		}
	}
	lg.Info("get_song", "shortName", req.ShortName, "found", false)
	return nil, &apiError{http.StatusNotFound, codeUnknown, "no such song"}
}

// listStoreItems is store/get_list/ (FListStoreItemsRequest): filter, EDLCSortMethod sort, 1-based paging. The
// catalogue is only listed with store.list_in_store; otherwise the store is an empty page.
func (s *Server) listStoreItems(q *request) (any, error) {
	var req struct {
		Filter     string `json:"filter"`
		Page       int    `json:"page"`
		PageSize   int    `json:"pageSize"`
		SortMethod int    `json:"sort_method"`
		SortDir    int    `json:"sort_dir"`
	}
	if err := q.decode(&req); err != nil {
		return nil, err
	}
	var items []catalog.Item
	if c := s.cat(); c != nil && s.cfg.Store.ListInStore {
		items = c.Items()
	}
	if f := strings.ToLower(strings.TrimSpace(req.Filter)); f != "" {
		kept := items[:0]
		for _, it := range items {
			if strings.Contains(strings.ToLower(it.Name), f) || strings.Contains(strings.ToLower(it.SongName), f) ||
				strings.Contains(strings.ToLower(it.Artist.Name), f) {
				kept = append(kept, it)
			}
		}
		items = kept
	}
	sortItems(items, req.SortMethod, req.SortDir == 1)
	size := req.PageSize
	if size <= 0 {
		size = 24
	}
	size = min(size, 100)
	page := max(req.Page, 1)
	numPages := max((len(items)+size-1)/size, 1)
	start := min((page-1)*size, len(items))
	end := min(start+size, len(items))
	out := make([]any, 0, end-start)
	for _, it := range items[start:end] {
		out = append(out, s.storeItem(it))
	}
	return map[string]any{"current_page": page, "num_pages": numPages, "total_entries": len(items),
		"items": out, "additional_items": []any{}}, nil
}

// sortItems orders by EDLCSortMethod: 0 release date, 1 name, 2 type (all songs, so name), 3 song title, 4 artist,
// 5 genre, 6 year. Ties fall back to the name, so the order is stable.
func sortItems(items []catalog.Item, method int, descending bool) {
	key := func(it catalog.Item) string {
		switch method {
		case 0:
			return it.ReleaseDate
		case 3:
			return strings.ToLower(it.SongName)
		case 4:
			return strings.ToLower(it.Artist.Name)
		case 5:
			return fmt.Sprintf("%04d", it.Genre.ID)
		case 6:
			return fmt.Sprintf("%06d", it.Year)
		default:
			return strings.ToLower(it.Name)
		}
	}
	sort.SliceStable(items, func(i, j int) bool {
		a, b := key(items[i]), key(items[j])
		if a == b {
			a, b = strings.ToLower(items[i].Name), strings.ToLower(items[j].Name)
		}
		if descending {
			return a > b
		}
		return a < b
	})
}

// ---- api/community/resolve/ ----------------------------------------------------------------------------------

// maxResolve bounds one resolve request (a crate holds at most a few dozen songs).
const maxResolve = 64

type resolvedSong struct {
	ShortName string `json:"shortName"`
	Version   int32  `json:"version"`
	Name      string `json:"name"`
	Artist    string `json:"artist"`
	MetaPath  string `json:"meta_path"`
	PakURL    string `json:"pak_url"`
	SigURL    string `json:"sig_url"`
	PakSHA256 string `json:"pak_sha256"`
	PakSize   int64  `json:"pak_size"`
	SigSHA256 string `json:"sig_sha256"`
	SigSize   int64  `json:"sig_size"`
	Licence   string `json:"licence"`
	Source    string `json:"source"`
	SongName  string `json:"song_name"`
	ArtURL    string `json:"art_url,omitempty"` // album art for a store tile, before the song is installed
}

type resolveAnswer struct {
	Query     string         `json:"query"`
	Kind      string         `json:"kind"` // "path" or "shortName"
	Found     bool           `json:"found"`
	Song      *resolvedSong  `json:"song,omitempty"`
	Community *communityLink `json:"community,omitempty"` // only when found is false
}

// communityLink is a community song's public files. It goes with found=false, so a loader that doesn't know it
// falls back to player-to-player transfer.
type communityLink struct {
	X          string          `json:"x"`
	Kind       string          `json:"kind"`              // "archive" or "folder"
	Archive    string          `json:"archive,omitempty"` // zip, rar4, rar5
	Files      []communityFile `json:"files"`
	PakEntry   string          `json:"pak_entry,omitempty"`
	SigEntry   string          `json:"sig_entry,omitempty"`
	SeenSHA256 []string        `json:"seen_sha256"` // pak hashes other loaders installed; informational
	Artist     string          `json:"artist"`
	Title      string          `json:"title"`
	Creator    string          `json:"creator"`
}

type communityFile struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Size int64  `json:"size"`
	URL  string `json:"url"`
}

func newCommunityLink(e *store.CommunityEntry) *communityLink {
	l := &communityLink{X: e.X, Kind: e.Kind, Archive: e.Archive, PakEntry: e.PakEntry, SigEntry: e.SigEntry,
		SeenSHA256: append([]string{}, e.SeenSHA256...), Artist: e.Artist, Title: e.Title, Creator: e.Creator}
	for _, f := range e.Files {
		l.Files = append(l.Files, communityFile{ID: f.ID, Name: f.Name, Size: f.Size, URL: community.DownloadURL(f.ID)})
	}
	return l
}

// reservedShortName reports whether x is an official song (the DLC manifest's songs or store.reserved_shortnames_file),
// which is never answered from the community table.
func (s *Server) reservedShortName(x string) bool {
	for _, list := range [][]string{s.cfg.SongManifest.Songs, s.cfg.Store.Reserved} {
		for _, sn := range list {
			if strings.EqualFold(sn, x) {
				return true
			}
		}
	}
	return false
}

// addCommunityLinks fills in the community entries for the answers the catalogue missed.
func (s *Server) addCommunityLinks(q *request, answers []resolveAnswer) {
	var xs []string
	for _, a := range answers {
		if a.Found {
			continue
		}
		if x := communityShortName(a); x != "" {
			xs = append(xs, x)
		}
	}
	if len(xs) == 0 || s.store == nil {
		return
	}
	entries, err := s.store.CommunitySongs(q.Context(), xs)
	if err != nil {
		s.logs.For(logx.Store).Error("community lookup", "err", err)
		return
	}
	for i := range answers {
		a := &answers[i]
		x := communityShortName(*a)
		e := entries[strings.ToLower(x)]
		if a.Found || e == nil {
			continue
		}
		if e.Disabled || s.reservedShortName(e.X) || strings.Contains(strings.ToLower(e.Creator), "harmonix") {
			s.logs.For(logx.Store).Info("community resolve skipped", "x", e.X, "disabled", e.Disabled, "reason", e.DisabledReason)
			continue
		}
		a.Community = newCommunityLink(e)
		s.logs.For(logx.Store).Info("community resolve", "x", e.X, "kind", e.Kind, "archive", e.Archive,
			"creator", e.Creator, "sheet", e.Sheet)
	}
}

func communityShortName(a resolveAnswer) string {
	if a.Kind == "shortName" {
		return a.Query
	}
	return community.ShortNameOfPath(a.Query)
}

// resolveSongs is the loader's lookup: crate soft-object paths it could not load, and/or shortnames, to
// catalogue entries with their download URLs and hashes. A miss is an explicit found=false.
func (s *Server) resolveSongs(q *request) (any, error) {
	var req struct {
		Paths      []string `json:"paths"`
		ShortNames []string `json:"shortNames"`
	}
	if err := q.decode(&req); err != nil {
		return nil, err
	}
	if !s.limiter.allow(s.remoteHost(q.Request)) {
		return nil, &apiError{http.StatusTooManyRequests, codeUnknown, "too many requests"}
	}
	if len(req.Paths)+len(req.ShortNames) > maxResolve {
		return nil, errBadRequest(fmt.Sprintf("at most %d queries per request", maxResolve))
	}
	c := s.cat()
	answers := make([]resolveAnswer, 0, len(req.Paths)+len(req.ShortNames))
	add := func(query, kind string, it catalog.Item, ok bool) {
		a := resolveAnswer{Query: query, Kind: kind, Found: ok}
		if ok {
			base := s.downloadBase(it)
			a.Song = &resolvedSong{ShortName: it.ShortName, Version: it.Version, Name: it.Name, Artist: it.Artist.Name,
				MetaPath: it.MetaPath, PakURL: base + it.PakName(), SigURL: base + it.SigName(),
				PakSHA256: it.PakSHA256, PakSize: it.PakSize, SigSHA256: it.SigSHA256, SigSize: it.SigSize,
				Licence: it.Licence, Source: it.Source, SongName: it.SongName, ArtURL: s.artURL(it)}
		}
		answers = append(answers, a)
	}
	found := 0
	for _, p := range req.Paths {
		var it catalog.Item
		ok := false
		if c != nil {
			it, ok = c.ByMetaPath(p)
		}
		add(p, "path", it, ok)
	}
	for _, sn := range req.ShortNames {
		var it catalog.Item
		ok := false
		if c != nil {
			it, ok = c.ByShortName(sn)
		}
		add(sn, "shortName", it, ok)
	}
	s.addCommunityLinks(q, answers)
	var misses []string
	linked := 0
	for _, a := range answers {
		switch {
		case a.Found:
			found++
		case a.Community != nil:
			linked++
		default:
			misses = append(misses, a.Query)
		}
	}
	s.logs.For(logx.Store).Info("resolve", "queries", len(answers), "found", found, "community", linked, "misses", misses)
	return map[string]any{"songs": answers}, nil
}

// ---- api/community/report/ -----------------------------------------------------------------------------------

// communityReasons are the failure reasons a loader may report; "" goes with ok.
var communityReasons = map[string]bool{"": true, "http": true, "quota": true, "html": true, "size": true,
	"extract": true, "layout": true, "sig": true, "install": true}

// communityReport records a loader's outcome for a community entry. Different players reporting a
// bad file (layout, sig) disable the entry; a good install adds its pak hash to the entry's seen_sha256.
func (s *Server) communityReport(q *request) (any, error) {
	var req struct {
		X         string `json:"x"`
		OK        bool   `json:"ok"`
		Reason    string `json:"reason"`
		Detail    string `json:"detail"`
		SHA256    string `json:"sha256"`
		SigSHA256 string `json:"sig_sha256"`
		Size      int64  `json:"size"`
		Ms        int64  `json:"ms"`
	}
	if err := q.decode(&req); err != nil {
		return nil, err
	}
	if !s.limiter.allow(s.remoteHost(q.Request)) {
		return nil, &apiError{http.StatusTooManyRequests, codeUnknown, "too many requests"}
	}
	if req.OK {
		req.Reason = ""
	}
	if !communityReasons[req.Reason] || (!req.OK && req.Reason == "") {
		return nil, errBadRequest("unknown reason")
	}
	for _, h := range []string{req.SHA256, req.SigSHA256} {
		if h != "" && !isHex64(h) {
			return nil, errBadRequest("sha256 must be 64 hex digits")
		}
	}
	if req.OK && req.SHA256 == "" {
		return nil, errBadRequest("a good install reports its sha256")
	}
	if len(req.Detail) > 200 {
		req.Detail = req.Detail[:200]
	}
	disabled, err := s.store.AddCommunityReport(q.Context(), req.X, store.CommunityReport{PlayerID: q.playerID(),
		OK: req.OK, Reason: req.Reason, SHA256: strings.ToLower(req.SHA256), SigSHA256: strings.ToLower(req.SigSHA256),
		Size: req.Size})
	if errors.Is(err, store.ErrNotFound) {
		return nil, &apiError{http.StatusNotFound, codeUnknown, "no community entry for that shortname"}
	}
	if err != nil {
		return nil, err
	}
	lg := s.logs.For(logx.Store)
	lg.Info("community report", "x", req.X, "ok", req.OK, "reason", req.Reason, "detail", req.Detail, "size", req.Size,
		"ms", req.Ms, "sha256", req.SHA256)
	if disabled {
		lg.Warn("community entry disabled", "x", req.X, "reason", req.Reason)
	}
	return map[string]any{"recorded": true, "disabled": disabled}, nil
}

func isHex64(s string) bool {
	_, err := hex.DecodeString(s)
	return len(s) == 64 && err == nil
}

// ---- /dl/<shortname>/<version>/<file> ------------------------------------------------------------------------

// serveDownload serves a catalogue entry's files: <sn>_P.pak, <sn>_P.sig (for the loader) and <sn>.bin (the game's
// container). Only the current version of a catalogue entry is served. Range requests and ETags are handled by
// http.ServeContent; the files never change under one version, so they are cacheable for good.
func (s *Server) serveDownload(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
	defer func() {
		s.logs.For(logx.Store).Info("download", "path", r.URL.Path, "range", r.Header.Get("Range"),
			"status", sw.status, "bytes", sw.bytes, "ms", time.Since(start).Milliseconds(),
			"ua", r.Header.Get("User-Agent"), "client", s.logs.Tag(s.remoteHost(r)))
	}()
	if !s.limiter.allow(s.remoteHost(r)) {
		http.Error(sw, "too many requests", http.StatusTooManyRequests)
		return
	}
	c := s.cat()
	sn, file := r.PathValue("sn"), r.PathValue("file")
	ver, err := strconv.Atoi(r.PathValue("ver"))
	var it catalog.Item
	ok := false
	if c != nil && err == nil {
		it, ok = c.ByShortName(sn)
	}
	if !ok || it.ShortName != sn || int(it.Version) != ver {
		http.NotFound(sw, r)
		return
	}
	dir := c.Dir(it)
	h := sw.Header()
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Cache-Control", "public, max-age=31536000, immutable")
	h.Set("Content-Disposition", `attachment; filename="`+file+`"`)
	switch file {
	case it.PakName(), it.SigName():
		f, err := os.Open(filepath.Join(dir, file))
		if err != nil {
			http.NotFound(sw, r)
			return
		}
		defer f.Close()
		sum := it.PakSHA256
		if file == it.SigName() {
			sum = it.SigSHA256
		}
		h.Set("ETag", `"`+sum+`"`)
		http.ServeContent(sw, r, file, time.Time{}, f)
	case it.BinName():
		sig, err := os.ReadFile(filepath.Join(dir, it.SigName()))
		if err != nil {
			http.NotFound(sw, r)
			return
		}
		pak, err := os.Open(filepath.Join(dir, it.PakName()))
		if err != nil {
			http.NotFound(sw, r)
			return
		}
		defer pak.Close()
		head := make([]byte, 4)
		binary.LittleEndian.PutUint32(head, uint32(len(sig)))
		h.Set("ETag", `"bin-`+it.SigSHA256[:16]+`-`+it.PakSHA256[:16]+`"`)
		http.ServeContent(sw, r, file, time.Time{}, newConcat(head, sig, pak, it.PakSize))
	case it.Art:
		f, err := os.Open(filepath.Join(dir, file))
		if it.Art == "" || err != nil {
			http.NotFound(sw, r)
			return
		}
		defer f.Close()
		h.Set("Content-Type", "image/png")
		if strings.HasSuffix(file, ".jpg") {
			h.Set("Content-Type", "image/jpeg")
		}
		h.Del("Content-Disposition")
		http.ServeContent(sw, r, file, time.Time{}, f)
	default:
		http.NotFound(sw, r)
	}
}

// remoteHost is the client's address, for rate limiting and the (tagged) download log.
func (s *Server) remoteHost(r *http.Request) string {
	if s.cfg.Multiplayer.TrustForwardedFor {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			first, _, _ := strings.Cut(xff, ",")
			return strings.TrimSpace(first)
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}

// concat is a ReadSeeker over head + sig (in memory) + the pak file, for ServeContent's ranges.
type concat struct {
	mem  []byte
	pak  io.ReadSeeker
	size int64
	pos  int64
}

func newConcat(head, sig []byte, pak io.ReadSeeker, pakSize int64) *concat {
	mem := append(append([]byte{}, head...), sig...)
	return &concat{mem: mem, pak: pak, size: int64(len(mem)) + pakSize}
}

func (c *concat) Read(p []byte) (int, error) {
	if c.pos >= c.size {
		return 0, io.EOF
	}
	if c.pos < int64(len(c.mem)) {
		n := copy(p, c.mem[c.pos:])
		c.pos += int64(n)
		return n, nil
	}
	if _, err := c.pak.Seek(c.pos-int64(len(c.mem)), io.SeekStart); err != nil {
		return 0, err
	}
	n, err := c.pak.Read(p)
	c.pos += int64(n)
	if errors.Is(err, io.EOF) && c.pos < c.size {
		err = io.ErrUnexpectedEOF
	}
	return n, err
}

func (c *concat) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		offset += c.pos
	case io.SeekEnd:
		offset += c.size
	}
	if offset < 0 {
		return 0, errors.New("negative position")
	}
	c.pos = offset
	return offset, nil
}

// ---- rate limiting -------------------------------------------------------------------------------------------

// rateLimiter counts requests per client address in one-minute windows.
type rateLimiter struct {
	perMinute int
	mu        sync.Mutex
	window    time.Time
	counts    map[string]int
}

func (l *rateLimiter) allow(key string) bool {
	if l == nil || l.perMinute <= 0 {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now().Truncate(time.Minute)
	if !now.Equal(l.window) {
		l.window, l.counts = now, map[string]int{}
	}
	l.counts[key]++
	return l.counts[key] <= l.perMinute
}
