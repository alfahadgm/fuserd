// Package api serves the FUSER web-services REST API: <ClientURL><module>/<action>/, JSON in, {"results": ...}
// out.
package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"fuseronline/internal/catalog"
	"fuseronline/internal/config"
	"fuseronline/internal/fdstoken"
	"fuseronline/internal/logx"
	"fuseronline/internal/matchmaking"
	"fuseronline/internal/p2p"
	"fuseronline/internal/platform"
	"fuseronline/internal/publichost"
	"fuseronline/internal/songxfer"
	"fuseronline/internal/store"
	"fuseronline/internal/token"
	"fuseronline/internal/turnrelay"
)

// Prefix is the path the client's ClientURL points at.
const Prefix = "/api/"

const maxBodyBytes = 1 << 20

type Server struct {
	cfg         config.Config
	store       *store.Store
	tokens      *token.Issuer
	verifiers   map[string]platform.Verifier
	logs        *logx.Logs
	mm          *matchmaking.Manager
	public      *publichost.Manager
	catalog     *catalog.Live
	limiter     *rateLimiter
	p2pLimit    *rateLimiter // p2p/ signaling, per player (per address for p2p/config/)
	p2p         *p2p.Registry
	songs       *songxfer.Registry // song transfers between players
	relay       *turnrelay.Relay
	dlcManifest *dlcManifest  // pc_asset_download_url's file; nil = not served
	pics        picUploads    // profile picture upload URLs handed out (profilepic.go)
	ms          msCache       // the Main Stage schedule snapshot (mainstage.go)
	msPass      msPasses      // Main Stage join passes (mainstage_server.go)
	loaders     loaderReports // who runs the loader: the stock / modded split (loader.go)
	featured    featuredCrops // featured Diamond Store art cropped to 8:15 (featured.go)
	now         func() time.Time
	// liveOverride replaces the Main Stage server's live player list in tests (mainstage.go).
	liveOverride func() (publichost.Live, bool)
}

// clock is the time the schedule is computed at; tests replace now.
func (s *Server) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

func New(cfg config.Config, st *store.Store, tokens *token.Issuer, verifiers map[string]platform.Verifier,
	logs *logx.Logs) *Server {
	mmOptions := matchmaking.Options{
		HostPort:    cfg.Multiplayer.HostPort,
		HostTimeout: time.Duration(cfg.Multiplayer.HostTimeoutSeconds) * time.Second,
	}
	tokenPublicKey := ""
	if p := cfg.Multiplayer.PublicServers; p.Enabled && p.FDSSessionTokens {
		// Players placed in an FDS container get a signed session token as their player session id.
		key := fdstoken.KeyFromSecret(cfg.TokenSecret)
		tokenPublicKey = fdstoken.PublicKeyString(key)
		mmOptions.ManagedPlayerSession = func(sess matchmaking.Session, playerID int64) string {
			token, err := fdstoken.Issue(key, sess.SlotID, sess.ID, playerID, time.Now())
			if err != nil {
				logs.For(logx.Multiplayer).Error("fds session token not issued", "err", err)
				return ""
			}
			return token
		}
	}
	mm := matchmaking.New(mmOptions)
	s := &Server{cfg: cfg, store: st, tokens: tokens, verifiers: verifiers, logs: logs, mm: mm,
		limiter: &rateLimiter{perMinute: cfg.Store.RequestsPerMinute}, p2pLimit: &rateLimiter{perMinute: cfg.P2P.RequestsPerMinute},
		p2p: p2p.New(p2pTTL), songs: songxfer.New()}
	if !cfg.Multiplayer.PublicServers.Enabled {
		logs.For(logx.Multiplayer).Warn("private sessions unavailable: they need FDS servers (multiplayer.public_servers enabled)")
	}
	if n := cfg.Multiplayer.MaxCrateSongs; n > 0 {
		logs.For(logx.Multiplayer).Info("crate song limit", "max_crate_songs", n)
	}
	if p := cfg.Multiplayer.PublicServers; p.Enabled {
		// FUSER's pak key lives in fuserd's environment only; each FDS container gets it through its own.
		pakKey := os.Getenv(p.FDSPakKeyEnv)
		if pakKey == "" {
			logs.For(logx.Multiplayer).Warn("public fds: no pak key in the environment; containers run without the FUSER install (Venue01 only)",
				"env", p.FDSPakKeyEnv)
		}
		// Unknown config keys are ignored, so a misplaced key or an older binary would otherwise be silent.
		logs.For(logx.Multiplayer).Info("public fds: settings", "image", p.Image, "max_servers", p.MaxServers,
			"session_tokens", p.FDSSessionTokens, "pak_key", pakKey != "")
		s.public = publichost.New(publichost.Config{Image: p.Image, GameRoot: p.GameRoot,
			Address: p.Address, MaxServers: p.MaxServers, BasePort: p.BasePort, PortStep: p.PortStep,
			MaxPlayers: p.MaxPlayers, MaxDJs: p.MaxDJs, StartupTimeout: time.Duration(p.StartupTimeoutSeconds) * time.Second,
			RegisterURL: p.RegisterURL(), PakKey: pakKey, TokenPublicKey: tokenPublicKey,
			Heartbeat: time.Duration(p.FDSHeartbeatSeconds) * time.Second},
			logs.For(logx.Multiplayer))
		go func() {
			for ev := range s.public.Events() {
				if ev.Stopped {
					s.mm.FailManaged(ev.SlotID)
					s.mmLog().Warn("public server stopped", "slot", ev.SlotID, "err", ev.Err)
				}
				if ev.Ready {
					s.mmLog().Info("public server ready", "slot", ev.SlotID)
				}
				if !ev.Ready && !ev.Stopped {
					s.mmLog().Info("public server status", "slot", ev.SlotID,
						"remote_players", ev.RemotePlayers, "stage", ev.Stage)
				}
			}
		}()
	}
	s.superviseMainStage()
	return s
}

func (s *Server) Close() {
	if s.public != nil {
		s.public.Close()
	}
}

// pollPaths are called every few seconds for as long as the client is online. They log as POLL instead of under
// their module, so they don't bury the other lines.
// p2p/song/poll/ is every member's 2 s song-transfer heartbeat.
// The mainstage/ URLs are each client's 1 s Main Stage schedule polls (mainstage.go).
var pollPaths = map[string]bool{"players/ping/": true, "feed/pull_notifications/": true, "nop/": true, "p2p/song/poll/": true,
	"mainstage/current_event/": true, "mainstage/next_event/": true, "mainstage/events/": true,
	"mainstage/headliners/": true, "mainstage/latest_spectators/": true}

// categoryOf is the log category of a route: its module upper-cased ("main_menu/..." is MAIN_MENU), or POLL.
func categoryOf(path string) logx.Category {
	if pollPaths[path] {
		return logx.Poll
	}
	module, _, _ := strings.Cut(path, "/")
	if module == "community" { // our own catalogue endpoints log with the game's store calls
		return logx.Store
	}
	return logx.Category(strings.ToUpper(module))
}

// request is what a handler sees: the HTTP request, its already-read body and the session claims.
type request struct {
	*http.Request
	body   []byte
	claims *token.Claims
}

// decode unmarshals the JSON body into v. An empty body leaves v at its zero value. Field matching is
// case-insensitive, like the client's FJsonObjectConverter.
func (q *request) decode(v any) error {
	if len(bytes.TrimSpace(q.body)) == 0 {
		return nil
	}
	if err := json.Unmarshal(q.body, v); err != nil {
		// The decoder's message names the field and the types involved, never the value.
		return errBadRequest("malformed JSON body: " + err.Error())
	}
	return nil
}

func (q *request) playerID() int64 {
	if q.claims == nil {
		return 0
	}
	return q.claims.PlayerID
}

// handlerFunc returns the value for "results", or an error (an *apiError is sent as-is, anything else as a 500).
type handlerFunc func(*request) (any, error)

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /control/media/{hash}", s.controlMedia)
	for _, rt := range s.routes() {
		mux.Handle(Prefix+rt.path+"{$}", s.wrap(rt))
	}
	// Our own endpoints, outside the exe's literal list: the loader's catalogue lookup and the catalogue downloads.
	resolveAuth := authOptional
	if s.cfg.Store.ResolveRequiresLogin {
		resolveAuth = authRequired
	}
	mux.Handle(Prefix+"community/resolve/{$}", s.wrap(route{"community/resolve/", resolveAuth, s.resolveSongs}))
	mux.Handle(Prefix+"community/report/{$}", s.wrap(route{"community/report/", authRequired, s.communityReport}))
	for path, h := range map[string]handlerFunc{"mainstage/current_event/": s.msCurrentEvent,
		"mainstage/next_event/": s.msNextEvent, "mainstage/events/": s.msEvents,
		"mainstage/headliners/": s.msHeadliners, "mainstage/latest_spectators/": s.msLatestSpectators} {
		mux.Handle(Prefix+path+"{$}", s.wrap(route{path, authOptional, h}))
	}
	mux.Handle(Prefix+"multiplayer/limits/{$}", s.wrap(route{"multiplayer/limits/", authOptional, s.multiplayerLimits}))
	mux.Handle(Prefix+"p2p/config/{$}", s.wrap(route{"p2p/config/", authOptional, s.p2pConfig}))
	mux.Handle(Prefix+"p2p/host/{$}", s.wrap(route{"p2p/host/", authRequired, s.p2pHost}))
	mux.Handle(Prefix+"p2p/join/{$}", s.wrap(route{"p2p/join/", authRequired, s.p2pJoin}))
	mux.Handle(Prefix+"p2p/poll/{$}", s.wrap(route{"p2p/poll/", authRequired, s.p2pPoll}))
	mux.Handle(Prefix+"p2p/report/{$}", s.wrap(route{"p2p/report/", authRequired, s.p2pReport}))
	mux.Handle(Prefix+"p2p/song/request/{$}", s.wrap(route{"p2p/song/request/", authRequired, s.songRequestHandler}))
	mux.Handle(Prefix+"p2p/song/poll/{$}", s.wrap(route{"p2p/song/poll/", authRequired, s.songPoll}))
	mux.Handle(Prefix+"p2p/song/offer/{$}", s.wrap(route{"p2p/song/offer/", authRequired, s.songOffer}))
	mux.Handle(Prefix+"p2p/song/decline/{$}", s.wrap(route{"p2p/song/decline/", authRequired, s.songDecline}))
	mux.Handle(Prefix+"p2p/song/accept/{$}", s.wrap(route{"p2p/song/accept/", authRequired, s.songAccept}))
	mux.Handle(Prefix+"p2p/song/report/{$}", s.wrap(route{"p2p/song/report/", authRequired, s.songReport}))
	mux.HandleFunc("GET /dl/{sn}/{ver}/{file}", s.serveDownload)
	mux.HandleFunc("PUT /pic/upload/{token}", s.picUpload)
	mux.HandleFunc("POST /pic/upload/{token}", s.picUpload)
	mux.HandleFunc("GET /pic/{player}/{pic}/{file}", s.servePic)
	mux.HandleFunc("GET "+Prefix+"mainstage/image/{key}/{file}", s.serveCard)
	mux.HandleFunc("GET "+dlcManifestPath+"{file}", s.serveDLCManifest)
	mux.HandleFunc("GET /{$}", s.serveHome)
	mux.HandleFunc("GET /venues/{file}", s.serveVenueImage)
	mux.HandleFunc("GET /logo.jpg", s.serveLogo)
	mux.HandleFunc("GET /logo.png", s.serveMenuLogo)
	mux.HandleFunc("GET /logo-mask.png", s.serveMenuLogo)
	mux.HandleFunc("GET /favicon.ico", s.serveFavicon)
	mux.HandleFunc("GET /robots.txt", s.serveRobots)
	mux.HandleFunc("GET /modpack", s.serveModPack)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		s.logs.For(logx.HTTP).Warn("unknown endpoint", "method", r.Method, "path", r.URL.Path)
		writeJSON(w, http.StatusNotFound, encodeError(&apiError{http.StatusNotFound, codeUnknown, "unknown endpoint"}))
	})
	return mux
}

func (s *Server) wrap(rt route) http.Handler {
	lg := s.logs.For(categoryOf(rt.path))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
		if err != nil {
			writeJSON(w, http.StatusRequestEntityTooLarge, encodeError(errBadRequest("body too large")))
			return
		}
		q := &request{Request: r, body: body}

		var result any
		if rt.auth != authNone {
			q.claims, err = s.authenticate(r)
			if err != nil && (rt.auth == authRequired || errors.Is(err, store.ErrEpicBlocked) || errors.Is(err, store.ErrPlayerBanned)) {
				result, err = nil, errUnauthorized(err.Error())
			} else {
				err = nil
			}
		}
		if err == nil {
			if rt.handler == nil {
				lg.Warn("stub endpoint", "path", rt.path)
			} else {
				result, err = rt.handler(q)
			}
		}

		status, out, failure := http.StatusOK, []byte(nil), ""
		if err != nil {
			var ae *apiError
			if !errors.As(err, &ae) {
				lg.Error("handler failed", "path", rt.path, "err", err)
				ae = errInternal()
			}
			status, out, failure = ae.Status, encodeError(ae), ae.Message
		} else {
			out = encodeResults(result)
		}
		writeJSON(w, status, out)

		if !lg.Enabled(r.Context(), slog.LevelInfo) {
			return
		}
		attrs := []any{"method", r.Method, "path", rt.path, "status", status, "ms", time.Since(start).Milliseconds()}
		if id := q.playerID(); id != 0 {
			attrs = append(attrs, "player", s.logs.TagID(id))
		}
		if cl := r.Header.Get("Changelist"); cl != "" {
			attrs = append(attrs, "changelist", cl)
		}
		if failure != "" {
			attrs = append(attrs, "error", failure) // our own messages: no identifying values
		}
		if s.cfg.LogBodies {
			attrs = append(attrs, "req", s.logs.TagJSON(body), "resp", s.logs.TagJSON(out))
		}
		lg.Info("request", attrs...)
	})
}

// authenticate reads "Authorization: JWT <token>" (the client also knows a "Token " prefix).
func (s *Server) authenticate(r *http.Request) (*token.Claims, error) {
	h := r.Header.Get("Authorization")
	if h == "" {
		return nil, errors.New("missing Authorization header")
	}
	scheme, tok, ok := strings.Cut(h, " ")
	if !ok || (scheme != "JWT" && scheme != "Token") {
		return nil, errors.New("unsupported Authorization scheme")
	}
	c, err := s.tokens.Verify(strings.TrimSpace(tok))
	if err != nil {
		return nil, err
	}
	if p, err := s.store.Player(r.Context(), c.PlayerID); err != nil {
		return nil, err
	} else if p.Banned {
		return nil, store.ErrPlayerBanned
	}
	if c.Platform == platform.Epic {
		if err := s.store.CheckEpicBlocked(r.Context(), c.PlatformUserID); err != nil {
			return nil, err
		}
	}
	return &c, nil
}

func writeJSON(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(body)
}
