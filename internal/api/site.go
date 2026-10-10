package api

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"fuseronline/internal/logx"
	"fuseronline/internal/publichost"
)

// The landing page at "/": the FUSER logo, the player installer (/setup, from the mod pack), the Game.ini lines that
// point the game at this server by hand, and the mod pack download (cfg.ModPackFile at /modpack, with server.txt
// stamped in: modPackView).

//go:embed site/index.html
var siteIndex string

//go:embed site/logo.jpg
var siteLogo []byte // the game's own splash screen (Fuser/Content/Splash/Splash.bmp)

//go:embed site/logo.png
var siteMenuLogo []byte

//go:embed site/logo-mask.png
var siteLogoMask []byte

//go:embed site/favicon.ico
var siteFavicon []byte // the game exe's own icon (16, 32 and 48 px)

var siteTemplate = template.Must(template.New("index").Parse(siteIndex))

var siteStarted = time.Now() // Last-Modified of the embedded logo

// modPackWriteTimeout replaces the server's WriteTimeout for the mod pack download, so a slow connection isn't cut off.
const modPackWriteTimeout = 15 * time.Minute

type modPackInfo struct {
	Name, Size, Updated string
	Version             string // the loader version in the pack; "" when it has no VERSION
	SetupSize           string // the installer's size; "" when the pack has no installer
}

// gameIni is the Game.ini section for this server. Every region URL points here too, so the client never falls
// back to a retired official host.
func (s *Server) gameIni() string {
	u := `"` + s.cfg.PublicURL + Prefix + `"`
	return "[/Script/Hype.BackendServerMgrSettings]\nEnvironmentSettings=((Live, (ClientURL=" + u + ",UsWest1URL=" + u +
		",Northeast2URL=" + u + ",Euwest3URL=" + u + ",UsEast1URL=" + u + ")))"
}

// modPack returns the mod pack's file details, or nil when there is none to offer.
func (s *Server) modPack() *modPackInfo {
	f, fi, v := s.openModPack()
	if f == nil {
		return nil
	}
	f.Close()
	info := &modPackInfo{Name: filepath.Base(s.cfg.ModPackFile), Size: formatSize(v.size),
		Updated: fi.ModTime().UTC().Format("2006-01-02"), Version: v.version}
	if v.setup != nil {
		info.SetupSize = formatSize(int64(len(v.setup)))
	}
	return info
}

func formatSize(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d bytes", n)
}

// siteHome is the landing page's data. CrateLimit, SeparateStock and MainStage are the server settings the mod pack
// text depends on.
type siteHome struct {
	Ini           string
	ModPack       *modPackInfo
	Status        siteStatus
	Diamond       siteDiamond
	CrateLimit    int
	SeparateStock bool
	MainStage     bool
}

func (s *Server) serveHome(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	data := siteHome{Ini: s.gameIni(), ModPack: s.modPack(), Status: s.frontpageStatus(),
		Diamond: s.frontpageDiamond(r.Context()), CrateLimit: s.cfg.Multiplayer.MaxCrateSongs,
		SeparateStock: s.cfg.Multiplayer.SeparateStockPlayers, MainStage: s.cfg.MainStage.Enabled && s.fdsEnabled()}
	if err := siteTemplate.Execute(w, data); err != nil {
		s.logs.For(logx.Web).Error("landing page failed", "err", err)
	}
}

func (s *Server) serveLogo(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	http.ServeContent(w, r, "logo.jpg", siteStarted, bytes.NewReader(siteLogo))
}

func (s *Server) serveMenuLogo(w http.ResponseWriter, r *http.Request) {
	data := siteMenuLogo
	if r.URL.Path == "/logo-mask.png" {
		data = siteLogoMask
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	http.ServeContent(w, r, filepath.Base(r.URL.Path), siteStarted, bytes.NewReader(data))
}

// robotsTxt asks crawlers to stay away: the server is an API and a page for players, not something to index.
const robotsTxt = "User-agent: *\nDisallow: /\n"

func (s *Server) serveRobots(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	http.ServeContent(w, r, "robots.txt", siteStarted, strings.NewReader(robotsTxt))
}

func (s *Server) serveFavicon(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "image/x-icon")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	http.ServeContent(w, r, "favicon.ico", siteStarted, bytes.NewReader(siteFavicon))
}

// openModPack opens cfg.ModPackFile and returns it with its prepared view, or nil when there is no mod pack. The file
// is opened per request, so replacing it on disk takes effect without a restart.
func (s *Server) openModPack() (*os.File, os.FileInfo, *modPackView) {
	if s.cfg.ModPackFile == "" {
		return nil, nil, nil
	}
	f, err := os.Open(s.cfg.ModPackFile)
	if err != nil {
		return nil, nil, nil
	}
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		f.Close()
		return nil, nil, nil
	}
	return f, fi, s.modPackView(f, fi)
}

// serveModPack sends cfg.ModPackFile as a download, with server.txt stamped in when it holds the player setup.
func (s *Server) serveModPack(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
	defer func() {
		s.logs.For(logx.Web).Info("mod pack download", "range", r.Header.Get("Range"), "status", sw.status,
			"bytes", sw.bytes, "ms", time.Since(start).Milliseconds(), "client", s.logs.Tag(s.remoteHost(r)))
	}()
	if !s.limiter.allow(s.remoteHost(r)) {
		http.Error(sw, "too many requests", http.StatusTooManyRequests)
		return
	}
	f, fi, v := s.openModPack()
	if f == nil {
		http.NotFound(sw, r)
		return
	}
	defer f.Close()
	http.NewResponseController(w).SetWriteDeadline(time.Now().Add(modPackWriteTimeout))
	name := filepath.Base(s.cfg.ModPackFile)
	h := sw.Header()
	h.Set("Cache-Control", "no-cache")
	h.Set("Content-Disposition", `attachment; filename="`+name+`"`)
	if v.etag != "" {
		h.Set("ETag", v.etag)
	}
	if v.data != nil {
		http.ServeContent(sw, r, name, fi.ModTime(), bytes.NewReader(v.data))
		return
	}
	http.ServeContent(sw, r, name, fi.ModTime(), f)
}

// modPackVersion is GET /modpack/version, the installer's update check: the mod pack's loader version (its VERSION
// file) and the size and SHA-256 of exactly what GET /modpack serves. 404 without a mod pack or a VERSION in it.
type modPackVersion struct {
	Version string `json:"version"`
	Size    int64  `json:"size"`
	SHA256  string `json:"sha256"`
	URL     string `json:"url"`
}

func (s *Server) serveModPackVersion(w http.ResponseWriter, r *http.Request) {
	if !s.limiter.allow(s.remoteHost(r)) {
		writeJSON(w, http.StatusTooManyRequests, []byte(`{"error":"too many requests"}`))
		return
	}
	f, _, v := s.openModPack()
	if f == nil {
		writeJSON(w, http.StatusNotFound, []byte(`{"error":"no mod pack"}`))
		return
	}
	f.Close()
	if v.version == "" || v.sum == "" {
		writeJSON(w, http.StatusNotFound, []byte(`{"error":"mod pack has no VERSION"}`))
		return
	}
	body, _ := json.Marshal(modPackVersion{Version: v.version, Size: v.size, SHA256: v.sum, URL: "/modpack"})
	w.Header().Set("Cache-Control", "no-cache")
	writeJSON(w, http.StatusOK, body)
}

// serveSetup is GET /setup: the player installer from the mod pack, with this server's address stamped on the end
// (setupTrailer). 404 without a mod pack or an installer in it.
func (s *Server) serveSetup(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
	defer func() {
		s.logs.For(logx.Web).Info("setup download", "range", r.Header.Get("Range"), "status", sw.status,
			"bytes", sw.bytes, "ms", time.Since(start).Milliseconds(), "client", s.logs.Tag(s.remoteHost(r)))
	}()
	if !s.limiter.allow(s.remoteHost(r)) {
		http.Error(sw, "too many requests", http.StatusTooManyRequests)
		return
	}
	f, fi, v := s.openModPack()
	if f == nil {
		http.NotFound(sw, r)
		return
	}
	f.Close()
	if v.setup == nil {
		http.NotFound(sw, r)
		return
	}
	http.NewResponseController(w).SetWriteDeadline(time.Now().Add(modPackWriteTimeout))
	h := sw.Header()
	h.Set("Cache-Control", "no-cache")
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Content-Disposition", `attachment; filename="`+modPackInstaller+`"`)
	h.Set("ETag", v.setupEtag)
	http.ServeContent(sw, r, modPackInstaller, fi.ModTime(), bytes.NewReader(v.setup))
}

// The mod pack's player setup takes the server address from server.txt beside it: FUSEROnline-Setup.exe
// (FUSERNativeLoader's .\build.ps1 -Package) or, in older packs, Setup-FuserOnline.ps1 run by Install.bat. So a zip
// holding the setup is served with server.txt set to public_url (any server.txt it had is replaced): the player's
// install points the game here with no typing. A public_url on a loopback address (the default) isn't stamped;
// neither is a file that isn't a zip or has no setup. The installer itself is also offered on its own at /setup,
// with the same address stamped on its end (setupTrailer).
const (
	modPackInstaller   = "FUSEROnline-Setup.exe"
	modPackSetupScript = "Setup-FuserOnline.ps1"
	modPackVersionFile = "VERSION"
	maxInstallerBytes  = 128 << 20
)

// maxStampBytes is the largest mod pack stamped with server.txt. The stamped zip is kept in memory, so a bigger one is
// streamed from disk as it is (the setup still finds the server through /setup's stamp or Game.ini). A var for tests.
var maxStampBytes int64 = 64 << 20

// setupTrailer is the server stamp GET /setup appends to the installer, after its PE image: "server=<public_url>",
// that text's length as a little-endian int32, then "FUSERCFG". The installer reads it from the end of its own file.
func setupTrailer(serverURL string) []byte {
	payload := []byte("server=" + serverURL)
	out := binary.LittleEndian.AppendUint32(payload, uint32(len(payload)))
	return append(out, "FUSERCFG"...)
}

// modPackView is one mod pack file version prepared for serving, for one public_url.
type modPackView struct {
	data      []byte // the zip with server.txt stamped in; nil: the file is served as is
	etag      string
	size      int64  // of what GET /modpack serves
	sum       string // its SHA-256, lowercase hex; "" when the file couldn't be read
	version   string // the VERSION file at the zip root; "" when there is none
	setup     []byte // the installer at the zip root, stamped when public_url is stampable; nil when there is none
	setupEtag string
}

// modPackStamp caches the view of one mod pack file version and public_url.
type modPackStamp struct {
	mu   sync.Mutex
	key  string
	view *modPackView
}

// modPackView returns the mod pack's view, built once per file version and public_url.
func (s *Server) modPackView(f *os.File, fi os.FileInfo) *modPackView {
	key := fmt.Sprint(s.cfg.ModPackFile, "|", fi.Size(), "|", fi.ModTime().UnixNano(), "|", s.cfg.PublicURL)
	c := &s.modPackStamp
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.key == key && c.view != nil {
		return c.view
	}
	lg := s.logs.For(logx.Web)
	v := &modPackView{size: fi.Size()}
	stamp := stampableURL(s.cfg.PublicURL)
	if strings.EqualFold(filepath.Ext(fi.Name()), ".zip") {
		if zr, err := zip.NewReader(f, fi.Size()); err != nil {
			lg.Warn("mod pack isn't a readable zip", "err", err)
		} else {
			stampZip := stamp
			if stampZip && fi.Size() > maxStampBytes {
				lg.Info("mod pack too large to stamp with server.txt: served as is", "bytes", fi.Size(), "limit", maxStampBytes)
				stampZip = false
			}
			if stampZip {
				data, err := stampModPack(zr, fi, s.cfg.PublicURL)
				switch {
				case err != nil:
					lg.Warn("mod pack served without server.txt", "err", err)
				case data != nil:
					v.data = data
					lg.Info("mod pack stamped with server.txt", "bytes", len(data))
				}
			}
			if b, err := readZipRoot(zr, modPackVersionFile, 256); err != nil {
				lg.Warn("mod pack VERSION unreadable", "err", err)
			} else if b != nil {
				line, _, _ := strings.Cut(strings.TrimPrefix(string(b), "\xef\xbb\xbf"), "\n")
				v.version = strings.TrimSpace(line)
			}
			if b, err := readZipRoot(zr, modPackInstaller, maxInstallerBytes); err != nil {
				lg.Warn("mod pack installer unreadable", "err", err)
			} else if b != nil {
				if stamp {
					b = append(b, setupTrailer(s.cfg.PublicURL)...)
				}
				sum := sha256.Sum256(b)
				v.setup, v.setupEtag = b, `"`+hex.EncodeToString(sum[:16])+`"`
			}
		}
	}
	h := sha256.New()
	if v.data != nil {
		h.Write(v.data)
		v.size = int64(len(v.data))
	} else if _, err := io.Copy(h, io.NewSectionReader(f, 0, fi.Size())); err != nil {
		lg.Warn("mod pack unreadable", "err", err)
		return &modPackView{size: fi.Size()} // not cached: the next request tries again
	}
	sum := h.Sum(nil)
	v.sum, v.etag = hex.EncodeToString(sum), `"`+hex.EncodeToString(sum[:16])+`"`
	c.key, c.view = key, v
	return v
}

// readZipRoot returns the named file at the zip's root (case-insensitive), or nil when there is none. A file larger
// than limit is an error.
func readZipRoot(zr *zip.Reader, name string, limit int64) ([]byte, error) {
	for _, zf := range zr.File {
		if !strings.EqualFold(strings.TrimPrefix(zf.Name, "./"), name) {
			continue
		}
		if zf.UncompressedSize64 > uint64(limit) {
			return nil, fmt.Errorf("%s is %d bytes, over %d", name, zf.UncompressedSize64, limit)
		}
		rc, err := zf.Open()
		if err != nil {
			return nil, err
		}
		defer rc.Close()
		b, err := io.ReadAll(io.LimitReader(rc, limit+1))
		if err != nil {
			return nil, err
		}
		if int64(len(b)) > limit {
			return nil, fmt.Errorf("%s is over %d bytes", name, limit)
		}
		return b, nil
	}
	return nil, nil
}

// stampableURL reports whether public_url names a host other players can reach (not loopback or unspecified).
func stampableURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return false
	}
	host := u.Hostname()
	if strings.EqualFold(host, "localhost") {
		return false
	}
	if ip := net.ParseIP(host); ip != nil && (ip.IsLoopback() || ip.IsUnspecified()) {
		return false
	}
	return true
}

// stampModPack rewrites the zip with server.txt (serverURL) beside its setup; nil without a setup. The other entries
// are copied as they are, still compressed.
func stampModPack(zr *zip.Reader, fi os.FileInfo, serverURL string) ([]byte, error) {
	target := ""
	for _, zf := range zr.File {
		if base := path.Base(zf.Name); strings.EqualFold(base, modPackInstaller) || strings.EqualFold(base, modPackSetupScript) {
			target = strings.TrimPrefix(path.Join(path.Dir(zf.Name), "server.txt"), "./")
			break
		}
	}
	if target == "" {
		return nil, nil
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, zf := range zr.File {
		if strings.EqualFold(zf.Name, target) {
			continue
		}
		raw, err := zf.OpenRaw()
		if err != nil {
			return nil, err
		}
		hdr := zf.FileHeader
		out, err := zw.CreateRaw(&hdr)
		if err != nil {
			return nil, err
		}
		if _, err := io.Copy(out, raw); err != nil {
			return nil, err
		}
	}
	out, err := zw.CreateHeader(&zip.FileHeader{Name: target, Method: zip.Deflate, Modified: fi.ModTime()})
	if err != nil {
		return nil, err
	}
	if _, err := io.WriteString(out, serverURL+"\r\n"); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

type siteLobby struct {
	VenueName, TimeOfDay, Image                                  string
	Number, Performers, Viewers, Players, Capacity, Spaces, Fill int
	State, StateClass                                            string
	Rounds, Bars                                                 int
}

type siteStatus struct {
	Online, Performers, Viewers, InPublic, Elsewhere int
	Lobbies                                          []siteLobby
	Updated                                          string
}

func siteGameState(stage int) (string, string) {
	switch stage {
	case 0, 4:
		return "Loading", "loading"
	case 1, 12:
		return "In lobby", "waiting"
	case 2, 3, 5, 6, 7, 8, 9, 13, 14, 15, 16, 17:
		return "In gameplay", "gameplay"
	case 10, 11, 18, 19, 20:
		return "Results", "waiting"
	default:
		return "State unavailable", "unknown"
	}
}

func (s *Server) frontpageStatus() siteStatus {
	online, sessions, ended := s.mm.SiteSnapshot()
	s.logEnded(ended)
	status := siteStatus{Online: online, Updated: time.Now().UTC().Format("15:04:05 UTC")}
	var managed map[string]publichost.SiteStatus
	if s.public != nil {
		managed = s.public.SiteSnapshot()
	}
	counted := make(map[int64]bool)
	for _, session := range sessions {
		if !session.Managed {
			continue
		}
		metadata, ok := managed[session.SlotID]
		if !ok {
			continue
		}
		lobby := siteLobby{Capacity: session.MaxPlayers, Rounds: metadata.Rounds, Bars: metadata.Bars}
		lobby.State, lobby.StateClass = siteGameState(metadata.Stage)
		lobby.VenueName, lobby.TimeOfDay, lobby.Image = siteVenue(metadata.Venue, metadata.TimeOfDay)
		for _, member := range session.Members {
			if member.IsDJ {
				lobby.Performers++
			} else {
				lobby.Viewers++
			}
			if !counted[member.ID] {
				counted[member.ID] = true
				if member.IsDJ {
					status.Performers++
				} else {
					status.Viewers++
				}
			}
		}
		lobby.Players = lobby.Performers + lobby.Viewers
		if lobby.Capacity > 0 {
			lobby.Spaces = max(0, lobby.Capacity-lobby.Players)
			lobby.Fill = min(100, lobby.Players*100/lobby.Capacity)
		}
		lobby.Number = len(status.Lobbies) + 1
		status.Lobbies = append(status.Lobbies, lobby)
	}
	status.InPublic = len(counted)
	status.Elsewhere = max(0, status.Online-status.InPublic)
	return status
}
