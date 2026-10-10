// Command fuserd is the FUSER community web-services server.
package main

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"time"

	"golang.org/x/crypto/acme/autocert"

	"fuseronline/internal/api"
	"fuseronline/internal/catalog"
	"fuseronline/internal/config"
	"fuseronline/internal/logx"
	"fuseronline/internal/platform"
	"fuseronline/internal/store"
	"fuseronline/internal/stunserver"
	"fuseronline/internal/token"
	"fuseronline/internal/turnrelay"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "control" {
		if err := controlCommand(os.Args[2:], os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "control:", err)
			os.Exit(1)
		}
		return
	}

	if len(os.Args) > 1 && os.Args[1] == "catalogue" {
		if err := catalogueCommand(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "catalogue:", err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "community" {
		if err := communityCommand(os.Args[2:], os.Stdin, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "community:", err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "mainstage" {
		if err := mainStageCommand(os.Args[2:], os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "mainstage:", err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "stun-probe" {
		if err := stunProbeCommand(os.Args[2:], os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "stun-probe:", err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "profile-pic" {
		if err := profilePicCommand(os.Args[2:], os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "profile-pic:", err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "exempt" {
		if err := exemptCommand(os.Args[2:], os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "exempt:", err)
			os.Exit(1)
		}
		return
	}
	configPath := flag.String("config", "config.json", "path to the configuration file")
	tag := flag.String("tag", "", "print the log tag of this value (a player ID, SteamID, Epic account ID, ...) and exit")
	flag.Parse()
	h := slog.Handler(slog.NewTextHandler(os.Stderr, nil))
	fatal := func(err error) {
		slog.New(h).Error("fatal", "cat", logx.Server, "err", err)
		os.Exit(1)
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		fatal(err)
	}
	closeLog := func() error { return nil }
	if cfg.LogFile != "" && *tag == "" {
		var w io.Writer
		if w, closeLog, err = openLogFile(cfg.LogFile); err != nil {
			fatal(err)
		}
		h = slog.NewTextHandler(w, nil)
	}
	err = run(cfg, *tag, h)
	if err != nil {
		slog.New(h).Error("fatal", "cat", logx.Server, "err", err)
	}
	closeLog()
	if err != nil {
		os.Exit(1)
	}
}

// openLogFile opens path for appending and returns a writer that sends every line to both stderr and the file.
func openLogFile(path string) (io.Writer, func() error, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		return nil, nil, fmt.Errorf("log_file: %w", err)
	}
	return io.MultiWriter(os.Stderr, f), f.Close, nil
}

func run(cfg config.Config, tag string, h slog.Handler) error {
	tagKey := []byte(cfg.LogTagSecret)
	if len(tagKey) == 0 {
		tagKey = logx.DeriveTagKey(cfg.TokenSecret)
	}
	logs, err := logx.NewFiltered(h, cfg.LogCategories, cfg.LogDisabledCategories, tagKey)
	if err != nil {
		return err
	}
	if tag != "" {
		fmt.Println(logs.Tag(tag))
		return nil
	}
	log := logs.For(logx.Server)

	st, err := store.Open(cfg.DatabasePath)
	if err != nil {
		return err
	}
	defer st.Close()

	verifiers, err := buildVerifiers(cfg.Auth)
	if err != nil {
		return err
	}
	if len(verifiers) == 0 {
		log.Warn("no platform is enabled in auth; nobody can log in")
	}
	tokens := token.NewIssuer(cfg.TokenSecret, time.Duration(cfg.TokenLifetimeSeconds)*time.Second)

	mux := http.NewServeMux()
	apiServer := api.New(cfg, st, tokens, verifiers, logs)
	defer apiServer.Close()
	if cfg.Store.CatalogueFile != "" {
		storeLog := logs.For(logx.Store)
		live, err := catalog.NewLive(cfg.Store.CatalogueFile, cfg.Store.FilesDir, func(err error) {
			storeLog.Error("catalogue reload failed; keeping the previous one", "err", err)
		})
		if err != nil {
			return fmt.Errorf("store catalogue: %w", err)
		}
		apiServer.SetCatalogue(live)
		log.Info("store catalogue", "file", cfg.Store.CatalogueFile, "songs", len(live.Get().Items()),
			"list_in_store", cfg.Store.ListInStore, "add_to_song_manifest", cfg.Store.AddToSongManifest)
	}
	if b := cfg.BattleSeason; b.Enabled {
		log.Info("battle mode", "enabled", true, "season", b.Name, "end_date", b.EndDate)
	} else {
		log.Info("battle mode", "enabled", false, "season", config.NoBattleSeason)
	}
	if cfg.P2P.Enabled {
		ports := []int{cfg.P2P.StunPort}
		if cfg.P2P.StunAltPort != 0 {
			ports = append(ports, cfg.P2P.StunAltPort)
		}
		p2pLog := logs.For(logx.P2P)
		stunSrv, err := stunserver.Listen(stunserver.Options{Bind: cfg.P2P.StunBind, Ports: ports,
			RequestsPerSecond: cfg.P2P.StunRequestsPerSecond, Log: p2pLog, Tag: logs.Tag})
		if err != nil {
			return err
		}
		defer stunSrv.Close()
		log.Info("stun listening", "addrs", fmt.Sprint(stunSrv.Addrs()), "advertised_host", cfg.P2P.StunHost,
			"requests_per_second", cfg.P2P.StunRequestsPerSecond)
		if p := cfg.P2P; p.TurnEnabled {
			secret := p.TurnSecret
			if secret == "" {
				sum := sha256.Sum256([]byte("fuserd turn|" + cfg.TokenSecret))
				secret = hex.EncodeToString(sum[:])
			}
			relay, err := turnrelay.Listen(turnrelay.Options{Bind: p.StunBind, Port: p.TurnPort,
				RelayIP: net.ParseIP(p.TurnRelayAddress).To4(), RelayBind: p.StunBind, MinPort: uint16(p.TurnRelayPortMin),
				MaxPort: uint16(p.TurnRelayPortMax), Secret: secret, MaxPerUser: p.TurnMaxPerPlayer,
				BytesPerSecond: p.TurnBytesPerSecond, Log: p2pLog, Tag: logs.Tag})
			if err != nil {
				return err
			}
			defer relay.Close()
			apiServer.SetRelay(relay)
			log.Info("turn relay listening", "addr", relay.Addr().String(), "relay_address", p.TurnRelayAddress,
				"relay_ports", fmt.Sprintf("%d-%d", p.TurnRelayPortMin, p.TurnRelayPortMax),
				"max_per_player", p.TurnMaxPerPlayer, "bytes_per_second", p.TurnBytesPerSecond)
		}
	}
	if f := cfg.SongManifest.PakFile; f != "" {
		if m, err := api.LoadDLCManifest(f); err != nil {
			log.Warn("dlc manifest not served; clients without one fail their DLC scan", "file", f, "err", err)
		} else {
			apiServer.SetDLCManifest(m)
			log.Info("dlc manifest", "file", f, "bytes", m.Size(), "version", cfg.SongManifest.Version)
		}
	}
	if cfg.Control.Enabled {
		controlHandler, err := apiServer.ControlHandler()
		if err != nil {
			return fmt.Errorf("control: %w", err)
		}
		mux.Handle("/control/", controlHandler)
	}
	apiHandler := apiServer.Handler()
	mux.Handle("/", apiHandler)
	if cfg.StaticDir != "" {
		mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.Dir(cfg.StaticDir))))
		mux.Handle("/static/dlcmanifest/", apiHandler) // ours even when static_dir is set
	}
	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}

	var certs *autocert.Manager
	if a := cfg.ACME; a.Enabled {
		certs = newACMEManager(a, log)
		srv.TLSConfig = acmeTLSConfig(certs, a.Domains[0])
		log.Info("acme enabled", "domains", strings.Join(a.Domains, ","), "directory", cmp.Or(a.DirectoryURL, autocert.DefaultACMEDirectory),
			"cache_dir", a.CacheDir)
	}
	// Listening before serving: tls-alpn-01 challenges arrive on this listener as soon as watchACME orders a certificate.
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	errc := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.Listen, "tls", cfg.TLSCert != "" || certs != nil, "client_url", cfg.PublicURL+api.Prefix,
			"platforms", len(verifiers), "log_categories", logs.Enabled(), "log_bodies", cfg.LogBodies,
			"log_file", cfg.LogFile)
		switch {
		case certs != nil:
			errc <- srv.ServeTLS(ln, "", "")
		case cfg.TLSCert != "":
			errc <- srv.ServeTLS(ln, cfg.TLSCert, cfg.TLSKey)
		default:
			errc <- srv.Serve(ln)
		}
	}()
	if certs != nil {
		go watchACME(ctx, certs, cfg.ACME.Domains, log)
	}
	// The FDS registration API on its own plain-HTTP listener (normally loopback), never the public one.
	var fdsSrv *http.Server
	if p := cfg.Multiplayer.PublicServers; p.Enabled {
		fdsSrv = &http.Server{Addr: p.FDSListen, Handler: apiServer.FDSHandler(), ReadHeaderTimeout: 10 * time.Second,
			ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: time.Minute}
		go func() {
			log.Info("fds registration listening", "addr", p.FDSListen, "register_url", p.RegisterURL())
			errc <- fdsSrv.ListenAndServe()
		}()
	}
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if fdsSrv != nil {
		if err := fdsSrv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Warn("fds registration listener shutdown", "err", err)
		}
	}
	if err := srv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func buildVerifiers(a config.AuthConfig) (map[string]platform.Verifier, error) {
	v := map[string]platform.Verifier{}
	if a.Steam.Enabled {
		sv, err := platform.NewSteamVerifier(a.Steam.AppID, a.Steam.PublicKeyPEM)
		if err != nil {
			return nil, err
		}
		v[platform.Steam] = sv
	}
	if a.Epic.Enabled {
		v[platform.Epic] = platform.NewJWTVerifier(platform.Epic, a.Epic)
	}
	if a.Switch.Enabled {
		v[platform.Nintendo] = platform.NewJWTVerifier(platform.Nintendo, a.Switch)
	}
	return v, nil
}
