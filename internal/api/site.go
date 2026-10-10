package api

import (
	"bytes"
	_ "embed"
	"fmt"
	"html/template"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"fuseronline/internal/logx"
	"fuseronline/internal/publichost"
)

// The landing page at "/": the FUSER logo, the Game.ini lines that point the game at this server, and the mod
// pack download (cfg.ModPackFile at /modpack).

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
	if s.cfg.ModPackFile == "" {
		return nil
	}
	fi, err := os.Stat(s.cfg.ModPackFile)
	if err != nil || !fi.Mode().IsRegular() {
		return nil
	}
	return &modPackInfo{Name: filepath.Base(s.cfg.ModPackFile), Size: formatSize(fi.Size()),
		Updated: fi.ModTime().UTC().Format("2006-01-02")}
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

// serveModPack sends cfg.ModPackFile as a download. The file is opened per request, so replacing it on disk takes
// effect without a restart.
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
	if s.cfg.ModPackFile == "" {
		http.NotFound(sw, r)
		return
	}
	f, err := os.Open(s.cfg.ModPackFile)
	if err != nil {
		http.NotFound(sw, r)
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		http.NotFound(sw, r)
		return
	}
	http.NewResponseController(w).SetWriteDeadline(time.Now().Add(modPackWriteTimeout))
	name := filepath.Base(s.cfg.ModPackFile)
	h := sw.Header()
	h.Set("Cache-Control", "no-cache")
	h.Set("Content-Disposition", `attachment; filename="`+name+`"`)
	http.ServeContent(sw, r, name, fi.ModTime(), f)
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
