package api

import (
	"bytes"
	"context"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"fuseronline/internal/control"
	"fuseronline/internal/mainstage"
	"fuseronline/internal/store"
	"github.com/google/uuid"
	_ "golang.org/x/image/webp"
	"image"
	"image/draw"
	_ "image/gif"
	_ "image/jpeg"
	"image/png"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

//go:embed site/control.html site/control-login.html site/control.js site/control-login.js
var controlFiles embed.FS

func (s *Server) ControlHandler() (http.Handler, error) {
	auth, err := control.New(s.cfg.Control, s.store, s.logs)
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	auth.Register(mux)
	var edits sync.Mutex
	protectUpload := auth.Protect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		edits.Lock()
		defer edits.Unlock()
		if id, shortname := s.controlAssetUpload(w, r); id != "" {
			auth.Audit(r, "assets.upload", id, shortname)
		}
	}))
	mux.Handle("POST /control/api/assets", protectUpload)
	mux.HandleFunc("GET /control/{$}", func(w http.ResponseWriter, r *http.Request) {
		imgSrc := ""
		if u, err := url.Parse(s.cfg.PublicURL); err == nil && u.Scheme == "http" && (u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1" || u.Hostname() == "::1") {
			imgSrc = " " + u.Scheme + "://" + u.Host
		}
		w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data: https:"+imgSrc+"; font-src data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		p := "site/control-login.html"
		if _, _, e := auth.Session(r); e == nil {
			p = "site/control.html"
		}
		b, _ := controlFiles.ReadFile(p)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(b)
	})
	// All assets are embedded and served locally; the page needs no external script loads.
	for _, file := range []struct{ route, path string }{{"app.js", "site/control.js"}, {"login.js", "site/control-login.js"}} {
		b, _ := controlFiles.ReadFile(file.path)
		mux.HandleFunc("GET /control/"+file.route, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
			w.Write(b)
		})
	}
	protect := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, auth.Protect(h)) }
	s.controlCommunityRoutes(protect, auth, &edits)
	protect("GET /control/api/state", func(w http.ResponseWriter, r *http.Request) {
		v, err := s.controlState(r.Context())
		if err != nil {
			control.Fail(w, 500, "could not load control data")
			return
		}
		control.JSON(w, 200, v)
	})
	protect("GET /control/api/status", func(w http.ResponseWriter, r *http.Request) {
		v, err := s.controlStatus(r.Context())
		if err != nil {
			control.Fail(w, 500, "could not load server status")
			return
		}
		control.JSON(w, 200, v)
	})
	protect("PUT /control/api/{kind}", func(w http.ResponseWriter, r *http.Request) {
		edits.Lock()
		defer edits.Unlock()
		r.Body = http.MaxBytesReader(w, r.Body, 40<<20)
		var change controlChange
		err := s.controlSave(w, r, &change)
		if err != nil {
			control.Fail(w, 400, err.Error())
			return
		}
		auth.Audit(r, r.PathValue("kind")+".save", change.target, change.detail)
		s.invalidateMainStage()
		control.JSON(w, 200, map[string]bool{"ok": true})
	})
	protect("DELETE /control/api/{kind}/{id}", func(w http.ResponseWriter, r *http.Request) {
		edits.Lock()
		defer edits.Unlock()
		var err error
		action := r.PathValue("kind") + ".remove"
		switch r.PathValue("kind") {
		case "events":
			if !mainstage.IsGUID(r.PathValue("id")) {
				err = errors.New("invalid event id")
			} else {
				err = s.store.ControlEventRemove(r.Context(), r.PathValue("id"))
			}
		case "assets":
			action = "assets.archive"
			err = s.controlAssetArchive(r.Context(), r.PathValue("id"), r.Header.Get("If-Match"))
		case "exemptions":
			err = s.store.ControlExemptionRemove(r.Context(), r.PathValue("id"))
		default:
			err = errors.New("unknown resource")
		}
		if err != nil {
			control.Fail(w, 400, err.Error())
			return
		}
		auth.Audit(r, action, r.PathValue("id"), "")
		s.invalidateMainStage()
		control.JSON(w, 200, map[string]bool{"ok": true})
	})
	// Published artwork is intentionally public: the game downloads it without an admin cookie.
	mux.HandleFunc("GET /control/media/{hash}", s.controlMedia)
	mux.HandleFunc("/control/", func(w http.ResponseWriter, r *http.Request) { control.Fail(w, 404, "not found") })
	return auth.Headers(auth.Restrict(mux)), nil
}

func (s *Server) controlItems(ctx context.Context) ([]store.ControlItem, error) {
	var items []store.ControlItem
	ok, err := s.store.ControlDocument(ctx, "items", &items)
	if err != nil || ok {
		for i := range items {
			items[i].AssetID = items[i].ID
		}
		return items, err
	}
	items = make([]store.ControlItem, 8)
	for i := range items {
		items[i] = store.ControlItem{Start: store.ControlStored(time.Now().Add(-24 * time.Hour)), End: store.ControlStored(time.Date(2099, 12, 31, 23, 59, 0, 0, time.UTC))}
	}
	if c := s.cat(); c != nil {
		featured, standard := 0, 2
		for _, it := range c.Items() {
			if it.ElderCredit == nil {
				continue
			}
			pos := standard
			if it.ElderCredit.Featured {
				pos = featured
				if featured >= 2 {
					continue
				}
				featured++
			} else {
				if standard >= 8 {
					continue
				}
				standard++
			}
			l := it.ElderCredit
			st, err := time.Parse(time.RFC3339, l.StartDate)
			if err != nil {
				st = time.Now().Add(-24 * time.Hour)
			}
			end, err := time.Parse(time.RFC3339, l.EndDate)
			if err != nil {
				end = time.Date(2099, 12, 31, 23, 59, 0, 0, time.UTC)
			}
			items[pos] = store.ControlItem{ID: it.UUID, Title: it.Name, Artist: it.Artist.Name, Shortname: it.ShortName, Price: l.Price, Enabled: true, Start: store.ControlStored(st), End: store.ControlStored(end), Image: s.artURL(it)}
		}
	}
	for i := range items {
		items[i].AssetID = items[i].ID
	}
	return items, nil
}

type controlUser struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Platform string `json:"platform"`
	Key      string `json:"key"`
	Online   bool   `json:"online"`
	Banned   bool   `json:"banned"`
	LastSeen string `json:"lastSeen"`
	Notes    string `json:"notes"`
}
type controlEvent struct {
	ID          string              `json:"id"`
	Name        string              `json:"name"`
	Type        string              `json:"type"`
	Start       string              `json:"start"`
	End         string              `json:"end"`
	Turn        int                 `json:"turn"`
	Image       string              `json:"image"`
	Slots       []store.ControlSlot `json:"slots"`
	SpecialName string              `json:"special_name"`
	SpecialType string              `json:"special_type_name"`
	Description string              `json:"special_description"`
	AllowStock  bool                `json:"allow_stock"`
	Rotation    bool                `json:"rotation"`
	// HeldUntil (read-only) is set on the event the game is shown as still running between sets (mainstage.Hold):
	// the wall time the hold ends.
	HeldUntil string `json:"held_until,omitempty"`
}

// controlState is the panel's data. Times are wall times in the session's timezone (control.Location); stored
// document times are converted, and controlSave converts them back.
func (s *Server) controlState(ctx context.Context) (any, error) {
	loc := control.Location(ctx)
	wall := func(stored string) string {
		if t, err := store.ControlTime(stored); err == nil {
			return store.ControlWall(t, loc)
		}
		return stored
	}
	items, err := s.controlItems(ctx)
	if err != nil {
		return nil, err
	}
	for i := range items {
		items[i].Start, items[i].End = wall(items[i].Start), wall(items[i].End)
	}
	ps, err := s.store.ControlUsers(ctx)
	if err != nil {
		return nil, err
	}
	notes, err := s.store.ControlNotes(ctx, "user")
	if err != nil {
		return nil, err
	}
	users := []controlUser{}
	for _, p := range ps {
		users = append(users, controlUser{p.ID, p.HypeUsername, p.Platform, p.PlatformUserID, !p.Banned && onlineStatus(&p) == "online", p.Banned, store.ControlWall(p.LastSeen, loc), notes[fmt.Sprint(p.ID)]})
	}
	ex, err := s.store.ControlExemptions(ctx)
	if err != nil {
		return nil, err
	}
	now := s.clock()
	from, to := now.Add(-24*time.Hour), now.Add(7*24*time.Hour)
	stored, err := s.store.MainStageEvents(ctx, time.Unix(0, 0), time.Date(2200, 1, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		return nil, err
	}
	events := mainstage.Merge(stored, s.cfg.MainStage.RotationSchedule(), from, to)
	for _, e := range stored {
		if !e.End.After(from) || !e.Start.Before(to) {
			events = append(events, e)
		}
	}
	removed, err := s.store.MainStageRemoved(ctx)
	if err != nil {
		return nil, err
	}
	ids := []string{}
	for _, e := range events {
		ids = append(ids, e.UUID)
	}
	heads, err := s.store.MainStageHeadliners(ctx, ids)
	if err != nil {
		return nil, err
	}
	blocks, err := s.store.MainStageBlocks(ctx)
	if err != nil {
		return nil, err
	}
	// The same schedule the game is served, for the held event (the hold rules live in mainstage.Hold only).
	sched, _, err := s.mainStageSchedule(ctx, now)
	if err != nil {
		return nil, err
	}
	ev := []controlEvent{}
	for _, e := range events {
		if removed[strings.ToLower(e.UUID)] {
			continue
		}
		slots := make([]store.ControlSlot, e.Slots)
		for _, h := range heads[strings.ToLower(e.UUID)] {
			if h.Slot >= 1 && h.Slot <= len(slots) {
				pid := h.PlayerID
				slots[h.Slot-1].User = &pid
			}
		}
		for _, b := range blocks[strings.ToLower(e.UUID)] {
			if b >= 1 && b <= len(slots) {
				slots[b-1].Blocked = true
			}
		}
		held := ""
		if sched.Held != nil && strings.EqualFold(sched.Held.UUID, e.UUID) {
			held = store.ControlWall(sched.HeldUntil, loc)
		}
		ev = append(ev, controlEvent{e.UUID, e.Name, e.Type, store.ControlWall(e.Start, loc), store.ControlWall(e.End, loc), e.TurnMinutes, e.ImageURL, slots, e.SpecialName, e.SpecialTypeName, e.SpecialDescription, e.AllowStock, e.Rotation, held})
	}
	promos, err := s.controlPromotions(ctx)
	if err != nil {
		return nil, err
	}
	for i := range promos.Promotions {
		p := &promos.Promotions[i]
		p.Start, p.End = wall(p.Start), wall(p.End)
	}
	versions := map[string]string{}
	for _, key := range []string{"items", "promotions", "assets_hidden"} {
		versions[key], err = s.store.ControlDocumentVersion(ctx, key)
		if err != nil {
			return nil, err
		}
	}
	cat := []map[string]any{}
	if c := s.cat(); c != nil {
		for _, it := range c.Items() {
			cat = append(cat, map[string]any{"id": it.UUID, "shortname": it.ShortName, "title": it.Name, "artist": it.Artist.Name, "image": s.artURL(it)})
		}
	}
	assets, err := s.controlAssets(ctx)
	if err != nil {
		return nil, err
	}
	communitySongs, err := s.store.CommunityCount(ctx)
	if err != nil {
		return nil, err
	}
	return map[string]any{"assets": assets, "community_songs": communitySongs, "max_asset_mb": s.cfg.Store.MaxPakMB, "version": 1, "items": items, "users": users, "exemptions": ex, "events": ev, "promotions": promos.Promotions, "max_promotions": maxPromotions, "catalogue": cat, "versions": versions, "exemptions_enabled": s.cfg.Auth.AllowEpicFailedSignatures, "schedule_enabled": s.cfg.MainStage.Enabled, "hold_current_minutes": s.cfg.MainStage.HoldCurrentMinutes, "timezone": loc.String()}, nil
}
func decodeControl(r *http.Request, v any) error {
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	return d.Decode(v)
}

// controlDates reads a panel start and end: wall times in loc, or RFC 3339 with an explicit offset.
func controlDates(start, end string, loc *time.Location) (time.Time, time.Time, error) {
	parse := func(v string) (time.Time, error) {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			return t, nil
		}
		return store.ControlParseWall(v, loc)
	}
	a, err := parse(start)
	if err != nil {
		return a, a, errors.New("invalid start time")
	}
	b, err := parse(end)
	if err != nil || !b.After(a) {
		return a, b, errors.New("end must be after start")
	}
	return a, b, nil
}

// encodePNG8 encodes im as an 8-bit PNG. FUSER makes a downloaded image's texture from its bit depth
// (FUN_1407b6970): 8-bit becomes PF_R8G8B8A8, but 16-bit becomes PF_FloatRGBA, so the 16-bit integers are read as
// half floats - a hugely overbright texture whose bloom covers the screen. image/png writes
// 16 bits for any colour model but RGBA, NRGBA, Alpha, Gray and paletted - a decoded JPEG or WebP (YCbCr) included -
// so the image goes through NRGBA first.
func encodePNG8(im image.Image) ([]byte, error) {
	if _, ok := im.(*image.NRGBA); !ok {
		n := image.NewNRGBA(im.Bounds())
		draw.Draw(n, n.Bounds(), im, im.Bounds().Min, draw.Src)
		im = n
	}
	var out bytes.Buffer
	if err := png.Encode(&out, im); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// pngBitDepth is a PNG's IHDR bit depth (byte 24: signature 8, chunk length 4, "IHDR" 4, width 4, height 4), or 0.
func pngBitDepth(b []byte) int {
	if len(b) < 25 || !bytes.HasPrefix(b, []byte("\x89PNG\r\n\x1a\n")) || string(b[12:16]) != "IHDR" {
		return 0
	}
	return int(b[24])
}

func (s *Server) controlImage(ctx context.Context, value string) (string, error) {
	if value == "" {
		return "", nil
	}
	if strings.HasPrefix(value, "data:") {
		parts := strings.SplitN(value, ",", 2)
		if len(parts) != 2 || !strings.HasSuffix(parts[0], ";base64") {
			return "", errors.New("invalid image")
		}
		b, err := base64.StdEncoding.DecodeString(parts[1])
		if err != nil || len(b) > 3<<20 {
			return "", errors.New("image must be under 3 MB")
		}
		cfg, _, err := image.DecodeConfig(bytes.NewReader(b))
		if err != nil || cfg.Width < 1 || cfg.Height < 1 || cfg.Width > 4096 || cfg.Height > 4096 || int64(cfg.Width)*int64(cfg.Height) > 12000000 {
			return "", errors.New("invalid image dimensions")
		}
		im, _, err := image.Decode(bytes.NewReader(b))
		if err != nil {
			return "", errors.New("invalid image data")
		}
		out, err := encodePNG8(im)
		if err != nil {
			return "", err
		}
		hash, err := s.store.ControlMediaSave(ctx, out)
		return s.cfg.PublicURL + "/control/media/" + hash, err
	}
	u, err := url.Parse(value)
	if err != nil || u.User != nil || u.Fragment != "" || len(value) > 2000 {
		return "", errors.New("invalid image URL")
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && strings.HasPrefix(value, s.cfg.PublicURL+"/")) {
		return "", errors.New("image URL must use HTTPS")
	}
	return value, nil
}

// controlChange is what a saved control edit records in the audit trail. detail must not identify a player: the
// CONTROL log carries it as-is.
type controlChange struct{ target, detail string }

func (s *Server) controlSave(w http.ResponseWriter, r *http.Request, change *controlChange) error {
	ctx := r.Context()
	loc := control.Location(ctx)
	switch r.PathValue("kind") {
	case "items":
		var list []store.ControlItem
		if err := decodeControl(r, &list); err != nil {
			return errors.New("invalid store data")
		}
		if len(list) != 8 {
			return errors.New("store must contain eight slots")
		}
		seen := map[string]bool{}
		for i := range list {
			it := &list[i]
			start, end, err := controlDates(it.Start, it.End, loc)
			if err != nil {
				return err
			}
			it.Start, it.End = store.ControlStored(start), store.ControlStored(end)
			if it.Price < 0 || it.Price > 999999 || len(it.Title) > 80 || len(it.Artist) > 80 || it.Enabled && (strings.TrimSpace(it.Title) == "" || strings.TrimSpace(it.Artist) == "") {
				return errors.New("invalid item fields")
			}
			if it.Shortname == "" && !it.Enabled {
				it.ID = ""
				it.AssetID = ""
				it.Image = ""
				it.Title = ""
				it.Artist = ""
				continue
			}
			c := s.cat()
			if c == nil {
				return errors.New("catalogue unavailable")
			}
			song, ok := c.ByShortName(it.Shortname)
			if it.AssetID != "" {
				song, ok = c.ByUUID(it.AssetID)
				ok = ok && strings.EqualFold(song.ShortName, it.Shortname)
			}
			hidden, err := s.controlHiddenAssets(ctx)
			if err != nil {
				return err
			}
			if hidden[song.UUID] {
				return errors.New("asset has been archived")
			}
			if !ok {
				return fmt.Errorf("asset %s is not in the catalogue", it.Shortname)
			}
			it.ID = song.UUID
			it.AssetID = song.UUID
			if seen[it.ID] {
				return errors.New("a song may occupy only one store slot")
			}
			seen[it.ID] = true
			it.Image, err = s.controlImage(ctx, it.Image)
			if err != nil {
				return err
			}
		}
		return s.store.ControlSaveDocument(ctx, "items", list, r.Header.Get("If-Match"))
	case "promotions":
		return s.savePromotions(ctx, r, change)
	case "users":
		var u controlUser
		if err := decodeControl(r, &u); err != nil {
			return errors.New("invalid user")
		}
		change.target, change.detail = fmt.Sprint(u.ID), fmt.Sprintf("banned=%t", u.Banned)
		return s.store.ControlUserSave(ctx, u.ID, u.Name, u.Banned, u.Notes)
	case "exemptions":
		var req struct {
			Old   string                 `json:"old"`
			Value store.ControlExemption `json:"value"`
		}
		if err := decodeControl(r, &req); err != nil {
			return errors.New("invalid exemption")
		}
		change.target = req.Value.Key
		return s.store.ControlExemptionSave(ctx, req.Old, &req.Value)
	case "events":
		var req struct {
			Value  controlEvent        `json:"value"`
			Before []store.ControlSlot `json:"before"`
		}
		if err := decodeControl(r, &req); err != nil {
			return errors.New("invalid event")
		}
		v := req.Value
		if v.ID == "" {
			v.ID = uuid.NewString()
		}
		start, end, err := controlDates(v.Start, v.End, loc)
		if err != nil {
			return err
		}
		if v.Type == mainstage.TypeMaintenance {
			v.Slots = nil
		}
		if len(v.Description) > 4000 || len(v.SpecialName) > 100 || len(v.SpecialType) > 100 {
			return errors.New("special event text too long")
		}
		img, err := s.controlImage(ctx, v.Image)
		if err != nil {
			return err
		}
		change.target, change.detail = v.ID, fmt.Sprintf("%s %q allow_stock=%t", v.Type, v.Name, v.AllowStock)
		return s.store.ControlEventSave(ctx, mainstage.Event{UUID: v.ID, Name: v.Name, Type: v.Type, Start: start, End: end, Slots: len(v.Slots), TurnMinutes: v.Turn, ImageURL: img, SpecialName: v.SpecialName, SpecialTypeName: v.SpecialType, SpecialDescription: v.Description, AllowStock: v.AllowStock}, v.Slots, req.Before)
	}
	return errors.New("unknown resource")
}
func (s *Server) invalidateMainStage() { s.ms.mu.Lock(); s.ms.snap = nil; s.ms.mu.Unlock() }

// media8 caches the 8-bit form of media stored as 16-bit PNG, by hash: such images are converted when served, so
// their URLs don't produce FUSER's overbright texture.
var media8 sync.Map

func media8Bit(hash string, b []byte) []byte {
	if v, ok := media8.Load(hash); ok {
		return v.([]byte)
	}
	im, err := png.Decode(bytes.NewReader(b))
	if err != nil {
		return b
	}
	out, err := encodePNG8(im)
	if err != nil {
		return b
	}
	media8.Store(hash, out)
	return out
}

// Published media remains downloadable when the administrator interface is disabled.
func (s *Server) controlMedia(w http.ResponseWriter, r *http.Request) {
	h := r.PathValue("hash")
	if len(h) != 64 {
		http.NotFound(w, r)
		return
	}
	if _, err := hex.DecodeString(h); err != nil {
		http.NotFound(w, r)
		return
	}
	b, err := s.store.ControlMedia(r.Context(), h)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if pngBitDepth(b) == 16 {
		b = media8Bit(h, b)
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(b)
}
