// Package config loads the server configuration file.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	_ "time/tzdata" // timezone names resolve on hosts without a zoneinfo database (Windows, scratch images)

	"fuseronline/internal/mainstage"
)

// Config is the whole server configuration. See config.example.json.
type Config struct {
	// Listen is the TCP address to serve on. The shipping build's own "Local" backend environment points at
	// http://127.0.0.1:8000/api/, so :8000 is the default.
	Listen string `json:"listen"`
	// PublicURL is the externally reachable base of this server (no trailing slash, no /api). It is used to build
	// the absolute asset/polling URLs the client is handed.
	PublicURL string `json:"public_url"`
	TLSCert   string `json:"tls_cert"`
	TLSKey    string `json:"tls_key"`
	// ACME obtains and renews the TLS certificate automatically, in place of tls_cert / tls_key.
	ACME ACMEConfig `json:"acme"`

	DatabasePath string `json:"database_path"`
	// StaticDir, when set, is served at /static/ (the song manifest's asset URLs point there).
	StaticDir string `json:"static_dir"`
	// ModPackFile is the zip the landing page offers at /modpack (relative to the config file; "" = no download).
	ModPackFile string `json:"mod_pack_file"`

	// TokenSecret signs the session tokens this server issues from auth/jwt/. At least 32 bytes.
	TokenSecret          string `json:"token_secret"`
	TokenLifetimeSeconds int    `json:"token_lifetime_seconds"`

	// LogBodies logs request/response bodies, with identifying values replaced by tags. For protocol work.
	LogBodies bool `json:"log_bodies"`
	// LogCategories picks which categories are output ("AUTH", "STORE", "POLL", ...; "*" or empty = all). SERVER
	// lines and errors are always output. The names are checked at startup (internal/logx).
	LogCategories []string `json:"log_categories"`
	// LogDisabledCategories switches categories off again after LogCategories, e.g. ["POLL"] for everything but the
	// clients' polling. SERVER can't be switched off, and errors are always output.
	LogDisabledCategories []string `json:"log_disabled_categories"`
	// LogTagSecret keys the tags that stand in for player IDs, tokens and names in the log. Empty = derived from
	// TokenSecret, so rotating that also changes every tag.
	LogTagSecret string `json:"log_tag_secret"`
	// LogFile, when set, also appends every log line to this file (relative paths are from the working directory,
	// like database_path). Output to stderr is unchanged.
	LogFile string `json:"log_file"`

	Auth         AuthConfig         `json:"auth"`
	SongManifest SongManifestConfig `json:"song_manifest"`
	Store        StoreConfig        `json:"store"`
	BattleSeason BattleSeasonConfig `json:"battle_season"`
	Vivox        VivoxConfig        `json:"vivox"`
	Multiplayer  MultiplayerConfig  `json:"multiplayer"`
	P2P          P2PConfig          `json:"p2p"`
	ProfilePics  ProfilePicsConfig  `json:"profile_pics"`
	Control      ControlConfig      `json:"control"`
	MainStage    MainStageConfig    `json:"main_stage"`
}

// ACMEConfig obtains the TLS certificate from an ACME CA (Let's Encrypt by default) and renews it before it expires.
// The CA's tls-alpn-01 challenge is answered on the TLS listener itself, so no port 80 listener is needed, but the CA
// connects to port 443 of each domain: listen must be ":443" or receive a forward from public port 443. Enabling it
// accepts the CA's terms of service.
type ACMEConfig struct {
	Enabled bool `json:"enabled"`
	// Domains are the host names the certificate covers (DNS names; no IP addresses or wildcards). Empty means
	// public_url's host, plus control.origin's host when the control panel is enabled with an HTTPS origin.
	Domains []string `json:"domains"`
	// Email is an optional contact address registered with the CA account.
	Email string `json:"email"`
	// CacheDir keeps the account key and certificates across restarts; relative to the config file. Keep it: CAs
	// rate-limit reissues (Let's Encrypt allows 5 certificates for the same names a week).
	CacheDir string `json:"cache_dir"`
	// DirectoryURL is the CA's ACME directory. Empty means Let's Encrypt production; try a new setup against
	// staging first: https://acme-staging-v02.api.letsencrypt.org/directory (its certificates aren't trusted).
	DirectoryURL string `json:"directory_url"`
}

// resolveACMEDomains normalises Domains, or fills them from public_url and control.origin when empty.
func (c *Config) resolveACMEDomains() {
	a := &c.ACME
	if !a.Enabled {
		return
	}
	names := a.Domains
	if len(names) == 0 {
		names = c.httpsHosts()
	}
	a.Domains = nil
	for _, d := range names {
		d = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(d)), ".")
		if d != "" && !slices.Contains(a.Domains, d) {
			a.Domains = append(a.Domains, d)
		}
	}
}

// httpsHosts returns the lower-case host names clients reach this listener by over HTTPS: public_url's, and
// control.origin's when the panel is enabled.
func (c Config) httpsHosts() []string {
	var hosts []string
	add := func(raw string) {
		if u, err := url.Parse(raw); err == nil && u.Scheme == "https" && u.Hostname() != "" {
			if h := strings.ToLower(u.Hostname()); !slices.Contains(hosts, h) {
				hosts = append(hosts, h)
			}
		}
	}
	add(c.PublicURL)
	if c.Control.Enabled {
		add(c.Control.Origin)
	}
	return hosts
}

func (c Config) validateACME() error {
	a := c.ACME
	if !a.Enabled {
		return nil
	}
	if c.TLSCert != "" || c.TLSKey != "" {
		return errors.New("acme replaces tls_cert and tls_key: set one or the other")
	}
	if u, err := url.Parse(c.PublicURL); err != nil || u.Scheme != "https" {
		return errors.New("acme requires an https public_url (the listener serves only TLS)")
	}
	if a.CacheDir == "" {
		return errors.New("acme.cache_dir is required (it keeps the certificate across restarts)")
	}
	if len(a.Domains) == 0 {
		return errors.New("acme.domains is empty and public_url has no host name")
	}
	for _, d := range a.Domains {
		if _, err := netip.ParseAddr(d); err == nil {
			return fmt.Errorf("acme.domains: %q is an IP address; use the server's DNS name", d)
		}
		if strings.Contains(d, "*") {
			return fmt.Errorf("acme.domains: %q is a wildcard, which needs the dns-01 challenge; list each name", d)
		}
		if !strings.Contains(d, ".") || strings.Trim(d, "abcdefghijklmnopqrstuvwxyz0123456789-.") != "" {
			return fmt.Errorf("acme.domains: %q is not a DNS name (internationalised names must be in punycode)", d)
		}
	}
	for _, h := range c.httpsHosts() {
		if !slices.Contains(a.Domains, h) {
			return fmt.Errorf("acme.domains must include %q (the host of public_url or control.origin)", h)
		}
	}
	if a.DirectoryURL != "" {
		if u, err := url.Parse(a.DirectoryURL); err != nil || u.Scheme != "https" || u.Host == "" {
			return errors.New("acme.directory_url must be an https URL")
		}
	}
	return nil
}

// ControlConfig enables the separate WebAuthn administrator interface.
type ControlConfig struct {
	Enabled bool   `json:"enabled"`
	Origin  string `json:"origin"`
	// AllowedNetworks lists the CIDR networks (e.g. "203.0.113.7/32", "2001:db8::/48") whose addresses may use the
	// panel, judged by the connection's peer address. Empty allows every address. Published artwork at
	// /control/media/ stays public, because FUSER downloads it.
	AllowedNetworks []string `json:"allowed_networks"`
	// Timezone is the IANA zone (e.g. "Australia/Perth") the panel shows and enters times in when the browser does not
	// report a usable zone at sign-in. Empty means UTC.
	Timezone string `json:"timezone"`
}

// Location loads Timezone. "Local" is refused: it is the server host's zone, which browsers cannot name.
func (c ControlConfig) Location() (*time.Location, error) {
	name := strings.TrimSpace(c.Timezone)
	if name == "" {
		return time.UTC, nil
	}
	loc, err := time.LoadLocation(name)
	if err != nil || name == "Local" {
		return nil, fmt.Errorf("control.timezone: %q is not an IANA timezone name (e.g. UTC or Australia/Perth)", c.Timezone)
	}
	return loc, nil
}

// Networks parses AllowedNetworks.
func (c ControlConfig) Networks() ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, n := range c.AllowedNetworks {
		p, err := netip.ParsePrefix(strings.TrimSpace(n))
		if err != nil {
			return nil, fmt.Errorf("control.allowed_networks: %q is not CIDR notation (e.g. 203.0.113.7/32 or 2001:db8::/48)", n)
		}
		out = append(out, p.Masked())
	}
	return out, nil
}

// MainStageConfig is the Main Stage schedule. Events come from a fixed rotation plus the ones "fuserd mainstage"
// stores. Slots, turn length and event length are defaults; each stored event can set its own.
type MainStageConfig struct {
	// Enabled serves the schedule: main_stage/refresh_urls/ points at the mainstage/ polling endpoints instead of
	// nop/.
	Enabled  bool                    `json:"enabled"`
	Rotation MainStageRotationConfig `json:"rotation"`
	// Slots is the number of headliner slots per event (4, as the FUSER hub offers).
	Slots int `json:"slots"`
	// TurnMinutes optionally limits each headliner's turn (0 = no limit, the default). Turn lengths come from the event
	// length: FDS splits it evenly between the headliners present, in whole 32-bar blocks
	// (288 bars for 3 headliners and 192 for 4 in a 1750 s event).
	TurnMinutes int `json:"turn_minutes"`
	// EventSeconds is an event's length (1750, FUSER's TotalSecondsForMainStageDJPerformances).
	EventSeconds int `json:"event_seconds"`
	// Headliner slots can be claimed from PurchaseOpenMinutes until PurchaseCloseMinutes before an event starts.
	PurchaseOpenMinutes  int `json:"purchase_open_minutes"`
	PurchaseCloseMinutes int `json:"purchase_close_minutes"`
	// UpcomingEvents is how many events after the next one the schedule lists.
	UpcomingEvents int `json:"upcoming_events"`
	// CooldownEvents: a player who headlined one of the last CooldownEvents events can't claim a slot (0 = none).
	// Operator assignments ignore it.
	CooldownEvents int `json:"cooldown_events"`
	// The persistent Main Stage FDS server, started in one of multiplayer.public_servers' slots:
	// MaxPlayers is its capacity, Venue / TimeOfDay what it loads. The client always shows the
	// Main Stage at its own MainStageVenueTimeOfDay (night) and resolves the venue by name (VenueB is FUSER's
	// MainStageVenue).
	MaxPlayers int    `json:"max_players"`
	Venue      string `json:"venue"`
	TimeOfDay  string `json:"time_of_day"`
	// A pass lets a player who asked main_stage/servers/ join the Main Stage server for PassMinutes;
	// with PassCheckIP the server only admits the join from the address that asked.
	PassMinutes int  `json:"pass_minutes"`
	PassCheckIP bool `json:"pass_check_ip"`
	// GeneratedImages renders each event's image from its headliners (profile picture and name per slot, open slots
	// as placeholders), again whenever a slot is claimed or freed. An event's or the
	// rotation's image_url replaces it. ImageDir holds the rendered cards; relative to the config file.
	GeneratedImages bool   `json:"generated_images"`
	ImageDir        string `json:"image_dir"`
	// HoldCurrentMinutes keeps the last event that ended shown as the game's current event until the next one
	// starts, for at most this many minutes after its end (0 = off). Without it, the game's Main Stage tiles show no
	// text between sets. Maintenance events are never held, and the hold is only what the game is told: the Main
	// Stage server, admission and the website follow the real times (mainstage.Hold).
	HoldCurrentMinutes int `json:"hold_current_minutes"`
}

// MaxHoldCurrentMinutes bounds main_stage.hold_current_minutes: an event lasts at most mainstage.MaxDuration.
const MaxHoldCurrentMinutes = 720

// MainStageRotationConfig is the always-on rotation: an event every IntervalMinutes from Anchor.
type MainStageRotationConfig struct {
	Enabled bool `json:"enabled"`
	// Anchor is an RFC 3339 time one rotation event starts at; the others follow every IntervalMinutes.
	Anchor          string `json:"anchor"`
	IntervalMinutes int    `json:"interval_minutes"`
	// Name is the rotation events' name; ImageURL their static event image ("" = none).
	Name     string `json:"name"`
	ImageURL string `json:"image_url"`
	// AllowStock lets players without the loader join and headline rotation events (default false: loader users only).
	AllowStock bool `json:"allow_stock"`
}

// ProfilePicsConfig covers player profile pictures: the game renders one from the player's avatar (three JPEG sizes),
// asks players/profile_pic/ for upload URLs and PUTs each size there; every FHypeUser then carries download URLs
// (profile_pic) that other players' games fetch for player cards, lists and profiles.
type ProfilePicsConfig struct {
	// Enabled turns uploads and downloads on. Off, players/profile_pic/ hands out no upload URLs (the game then
	// skips the upload) and profiles carry no picture: everyone sees the default image.
	Enabled bool `json:"enabled"`
	// Dir holds the pictures: <dir>/<player id>/<pic id>_<size>.jpg. Relative to the config file.
	Dir string `json:"dir"`
	// MaxKB caps one uploaded image. The game's large picture is about 150-190 KB.
	MaxKB int `json:"max_kb"`
	// UploadMinutes is how long an upload URL stays valid.
	UploadMinutes int `json:"upload_minutes"`
}

// P2PConfig covers private sessions hosted on players' own machines and reached without port forwarding
// and the STUN service the players' loaders learn their public UDP endpoints from.
type P2PConfig struct {
	// Enabled starts the STUN listeners and advertises them in p2p/config/.
	Enabled bool `json:"enabled"`
	// StunBind is the address the STUN listeners bind ("" = every interface).
	StunBind string `json:"stun_bind"`
	// StunPort is the standard STUN port, 3478.
	StunPort int `json:"stun_port"`
	// StunAltPort is a second port on the same address, for telling whether a player's NAT maps per destination
	// (3479; 0 = none).
	StunAltPort int `json:"stun_alt_port"`
	// StunHost is the host name or address players reach the STUN ports at. Empty: public_url's host.
	StunHost string `json:"stun_host"`
	// StunRequestsPerSecond limits Binding requests per client address (0 = no limit).
	StunRequestsPerSecond int `json:"stun_requests_per_second"`
	// RequestsPerMinute limits each player's p2p/host, join, poll and report calls, and each address's p2p/config
	// calls (0 = no limit). A host polls once a second while its session lives, so keep it well above 60.
	RequestsPerMinute int `json:"requests_per_minute"`
	// SongTransfers enables p2p/song/: players ask each other for custom songs, and send them over punched paths.
	// It needs enabled; the files never pass through fuserd.
	SongTransfers bool `json:"song_transfers"`

	// TurnEnabled starts the TURN relay (pion/turn) for pairs whose NATs can't be punched. Joiners get credentials
	// in p2p/join/ and relay through it; open turn_port and the relay port range (UDP) on the server.
	TurnEnabled bool `json:"turn_enabled"`
	// TurnPort is the relay's listening port (UDP).
	TurnPort int `json:"turn_port"`
	// TurnRelayAddress is this server's public IPv4 address: where peers send relayed traffic. Required.
	TurnRelayAddress string `json:"turn_relay_address"`
	// TurnRelayPortMin / Max bound the relay sockets, one per allocation.
	TurnRelayPortMin int `json:"turn_relay_port_min"`
	TurnRelayPortMax int `json:"turn_relay_port_max"`
	// TurnSecret signs the relay credentials. Empty: derived from token_secret.
	TurnSecret string `json:"turn_secret"`
	// TurnMaxPerPlayer limits a player's concurrent allocations (0 = no limit).
	TurnMaxPerPlayer int `json:"turn_max_per_player"`
	// TurnBytesPerSecond limits each allocation's traffic per direction (0 = no limit).
	TurnBytesPerSecond int `json:"turn_bytes_per_second"`
	// TurnCredentialMinutes is how long a relay credential lasts. An allocation keeps the username it was made with
	// (RFC 8656), so this bounds how long a relayed session can last.
	TurnCredentialMinutes int `json:"turn_credential_minutes"`
}

// AuthConfig holds the per-platform ticket validation settings. The optional
// Epic fallback admits only explicitly provisioned identities after a signature failure.
type AuthConfig struct {
	Steam  SteamConfig `json:"steam"`
	Epic   JWTConfig   `json:"epic"`
	Switch JWTConfig   `json:"switch"`
	// AllowEpicFailedSignatures permits only DB-allowlisted Epic sub/dn pairs
	// after a valid-claims JWT fails signature verification. Default: false.
	AllowEpicFailedSignatures bool `json:"allow_epic_failed_signatures"`
}

type SteamConfig struct {
	Enabled bool   `json:"enabled"`
	AppID   uint32 `json:"app_id"`
	// PublicKeyPEM overrides the built-in Steam ticket-signing public key.
	PublicKeyPEM string `json:"public_key_pem"`
}

type JWTConfig struct {
	Enabled  bool   `json:"enabled"`
	JWKSURL  string `json:"jwks_url"`
	Issuer   string `json:"issuer"`
	Audience string `json:"audience"`
	// RequiredClaims are extra claims that must match exactly. A dotted key reaches into a nested object
	// ("nintendo.ai").
	RequiredClaims map[string]string `json:"required_claims"`
}

type SongManifestConfig struct {
	ReleaseID   string `json:"release_id"`
	ReleaseDate string `json:"release_date"`
	Version     int32  `json:"version"`
	// SongsFile is a JSON array of DLC song shortnames, relative to the config file.
	SongsFile string   `json:"songs_file"`
	Songs     []string `json:"-"`
	// PakFile is the official DLC manifest pak (Fuser/PersistentDownloadDir/DLC/Manifest/dlcmanifest.pak), with its
	// .sig beside it, relative to the config file. Served as pc_asset_download_url to clients that have none
	// installed. "" or a missing file = not served.
	PakFile string `json:"pak_file"`
}

// StoreConfig covers the community song catalogue: the songs fuserd serves, how the loader finds
// them (api/community/resolve/) and downloads them (/dl/), and how the game's store endpoints show them.
type StoreConfig struct {
	// CatalogueFile is the catalogue (a JSON array, written by "fuserd catalogue add"), relative to the config file.
	// A missing file is an empty catalogue. Empty disables the catalogue.
	CatalogueFile string `json:"catalogue_file"`
	// FilesDir holds the catalogue's paks: <files_dir>/<shortname>/<version>/<shortname>_P.pak + .sig. Relative to
	// the config file.
	FilesDir string `json:"files_dir"`
	// ReservedShortnamesFile is an optional JSON array of shortnames no catalogue song may use (base-game songs);
	// relative to the config file. The song manifest's official DLC shortnames are always reserved.
	ReservedShortnamesFile string   `json:"reserved_shortnames_file"`
	Reserved               []string `json:"-"`
	// ListInStore shows catalogue songs as store tiles (store/get_list/). Off by default: the loader doesn't need them,
	// and what the store screen does with an unowned free item is untested.
	ListInStore bool `json:"list_in_store"`
	// AddToSongManifest adds catalogue shortnames to store/song_manifest/get/'s songs. Off by default: it would put
	// them in front of the game's DLC machinery, which expects metadata they don't have.
	AddToSongManifest bool `json:"add_to_song_manifest"`
	// ResolveRequiresLogin makes api/community/resolve/ require a session token.
	ResolveRequiresLogin bool `json:"resolve_requires_login"`
	// MaxPakMB is the largest pak "fuserd catalogue add" accepts.
	MaxPakMB int `json:"max_pak_mb"`
	// RequestsPerMinute limits resolve and download requests per client address (0 = no limit).
	RequestsPerMinute int `json:"requests_per_minute"`
	// ElderCreditBalance is every player's Elder Credit balance: players/xp/ reports it, the game
	// stores it in the player's profile, and purchases don't lower it. 0 leaves players/xp/'s answer empty, and
	// fuserd refuses any purchase that costs more than 0.
	ElderCreditBalance int `json:"elder_credit_balance"`
	// XPPerElderCredit is the XP the game is told one Elder Credit takes (its credit-progress display).
	XPPerElderCredit int `json:"xp_per_elder_credit"`
}

type BattleSeasonConfig struct {
	// Enabled reports Name as the active battle season. Off (the default), players/active_battle_season/ answers
	// the client's own "noseason": its Battle hub then stops at sign-in with the BattleModeOffline error and the
	// game's "battles season down" popup, instead of reaching battle matchmaking, which fuserd refuses (it has no
	// battle servers). Only the Battle hub checks the season; multiplayer, Main Stage, social hub and store don't.
	Enabled bool `json:"enabled"`
	// Name must be a key of BattleSeasonDataAssets in the game's DefaultGame.ini ("season1", "season2").
	Name    string `json:"name"`
	EndDate string `json:"end_date"`
}

// NoBattleSeason is the season name the client treats as "no active season" (FName "noseason" in
// UBattleSeasonMgr).
const NoBattleSeason = "noseason"

type VivoxConfig struct {
	Domain string `json:"domain"`
	Server string `json:"server"`
	Issuer string `json:"issuer"`
}

// MultiplayerConfig covers the multiplayer/* matchmaking endpoints. Public sessions run on PublicServers, or without
// them on players' own games; private sessions need PublicServers.
type MultiplayerConfig struct {
	// Region is the single region name regions_http/ offers. The client measures latency to it with an HTTP GET
	// of this server's nop/ endpoint.
	Region string `json:"region"`
	// HostPort is the game port a hosting player listens on (UDP). Its beacon must listen on HostPort+100, which
	// is where the session browser queries it.
	HostPort int `json:"host_port"`
	// HostTimeoutSeconds ends a session once its host's players/ping/ polling has stopped for this long.
	HostTimeoutSeconds int `json:"host_timeout_seconds"`
	// OpenPrivateSessions is a test switch: every player's session browser lists every private session, marked
	// as an invite, so it can be joined without an invite, a follow or a platform friendship.
	OpenPrivateSessions bool `json:"open_private_sessions"`
	// TrustForwardedFor takes a host's address from the first X-Forwarded-For entry. Only enable it behind a
	// reverse proxy that sets that header; otherwise a client could claim any address.
	TrustForwardedFor bool `json:"trust_forwarded_for"`
	// LoopbackHostAddress is handed out as a session's address when its host reached this server over loopback
	// (the host runs on this machine), since 127.0.0.1 would send every joiner to itself. Empty = this machine's
	// outbound IPv4 address, detected at the time.
	LoopbackHostAddress string `json:"loopback_host_address"`
	// MaxCrateSongs caps the songs in the crate a player enters multiplayer or the Main Stage with, so a session's
	// custom songs can sync in time. The loader reads it from multiplayer/limits/ and fails the
	// game's own crate check (the stock "Crate Error" popup) above it. 0: no limit. Otherwise at least
	// MinCrateSongsLimit, the stock crate size: stock players have no loader, so a lower value would bind only
	// modded ones.
	MaxCrateSongs int `json:"max_crate_songs"`
	// SeparateStockPlayers keeps players without the loader (stock FUSER) apart from loader users in public
	// matchmaking, the session browser and the Main Stage: a stock client can't play another player's custom songs.
	// An invite overrides it; a Main Stage event may allow stock players (allow_stock). Default true.
	SeparateStockPlayers bool               `json:"separate_stock_players"`
	PublicServers        PublicServerConfig `json:"public_servers"`
}

// MinCrateSongsLimit is the lowest non-zero multiplayer.max_crate_songs: the stock game's crate size (8 songs x 3
// tabs), which every stock player's crate fits.
const MinCrateSongsLimit = 24

// PublicServerConfig enables Docker-managed FuserDedicatedServer (FDS) containers for public and private sessions
// and the Main Stage. Each container registers with fuserd over /fds/v1 with a per-container token. Image is the FDS
// container image; GameRoot is a FUSER install, mounted read-only at /fuser.
type PublicServerConfig struct {
	Enabled    bool   `json:"enabled"`
	Image      string `json:"image"`
	GameRoot   string `json:"game_root"`
	Address    string `json:"address"`
	MaxServers int    `json:"max_servers"`
	BasePort   int    `json:"base_port"`
	PortStep   int    `json:"port_step"`
	MaxPlayers int    `json:"max_players"`
	// MaxDJs is the performer slots of a lobby (FUSER's MaxNumDJsInSession, 4). "Join a public session" (a DJ
	// ticket) only goes to a server with a free one - its reported DJs plus the DJs being placed there are below
	// this - and starts a new server otherwise. "View a public session" and browser joins only need max_players.
	// 0: no performer check (DJ tickets fill servers up to max_players).
	MaxDJs                int `json:"max_djs"`
	StartupTimeoutSeconds int `json:"startup_timeout_seconds"`
	// FDSListen is the plain-HTTP address of the FDS registration API (/fds/v1), a listener of its own so the
	// internal API stays off the public, possibly TLS, one. Host-networked containers reach a loopback address
	// directly, with no TLS and no NAT hairpin to fuserd's published name. Default 127.0.0.1:8001.
	FDSListen string `json:"fds_listen"`
	// FDSRegisterURL is that API's base URL as a container reaches it. Empty: http://<fds_listen>, with a
	// wildcard host read as 127.0.0.1.
	FDSRegisterURL string `json:"fds_register_url"`
	// FDSPakKeyEnv names the environment variable of fuserd's process that holds FUSER's pak key (default
	// FDS_PAK_KEY). The key is handed to each container through its environment, never a command line or this file.
	FDSPakKeyEnv string `json:"fds_pak_key_env"`
	// FDSHeartbeatSeconds is how often an FDS container reports its status (default 10).
	FDSHeartbeatSeconds int `json:"fds_heartbeat_seconds"`
	// FDSSessionTokens makes player session ids signed tokens (fdstoken, key derived from token_secret) that FDS
	// containers require at login (default true). False: containers admit any client (troubleshooting only).
	FDSSessionTokens bool `json:"fds_session_tokens"`
	// WarmServers keeps this many public servers up with no player, ready for the next public tickets, so players
	// don't wait for a container to boot (default 0: off). They count against max_servers.
	WarmServers int `json:"warm_servers"`
	// WarmRounds is the round count warm servers start with (default 1); public tickets are matched on rounds.
	WarmRounds int `json:"warm_rounds"`
}

// RegisterURL is the registration API's base URL for FDS containers: fds_register_url, else http://<fds_listen>.
func (p PublicServerConfig) RegisterURL() string {
	if p.FDSRegisterURL != "" {
		return strings.TrimRight(p.FDSRegisterURL, "/")
	}
	host, port, err := net.SplitHostPort(p.FDSListen)
	if err != nil {
		return ""
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port)
}

// Default returns a configuration with every field that has a sensible default filled in.
func Default() Config {
	return Config{
		Listen:               ":8000",
		PublicURL:            "http://127.0.0.1:8000",
		DatabasePath:         "fuseronline.db",
		ModPackFile:          "data/FUSERNativeLoader.zip",
		TokenLifetimeSeconds: 3600,
		ACME:                 ACMEConfig{CacheDir: "data/acme"},
		Auth: AuthConfig{
			Steam: SteamConfig{Enabled: true, AppID: 1331440},
			Epic: JWTConfig{
				Enabled:  true,
				JWKSURL:  "https://api.epicgames.dev/epic/oauth/v1/.well-known/jwks.json",
				Issuer:   "https://api.epicgames.dev/epic/oauth/v1",
				Audience: "xyza7891gD3tPKNdn0blQ6Ktyc0tRINH", // [OnlineSubsystemEpic] ClientId in DefaultEngine.ini
			},
			Switch: JWTConfig{
				Enabled:        false,
				JWKSURL:        "https://e0d67c509fb203858ebcb2fe3f88c2aa.baas.nintendo.com/1.0.0/certificates",
				Issuer:         "https://e0d67c509fb203858ebcb2fe3f88c2aa.baas.nintendo.com",
				Audience:       "ed9e2f05d286f7b8",
				RequiredClaims: map[string]string{"nintendo.ai": "0100e1f013674000"}, // FUSER's Switch title ID
			},
		},
		SongManifest: SongManifestConfig{
			ReleaseID:   "week43",
			ReleaseDate: "2021-12-02",
			Version:     738126010,
			SongsFile:   "data/dlc_songs.json",
			PakFile:     "data/dlcmanifest.pak",
		},
		Store: StoreConfig{CatalogueFile: "data/store_catalogue.json", FilesDir: "data/store_files",
			ResolveRequiresLogin: true, MaxPakMB: 256, RequestsPerMinute: 120, XPPerElderCredit: 1000},
		BattleSeason: BattleSeasonConfig{Name: "season2", EndDate: "2099-12-31T00:00:00.000Z"},
		P2P: P2PConfig{StunPort: 3478, StunAltPort: 3479, StunRequestsPerSecond: 20, RequestsPerMinute: 300, SongTransfers: true, TurnPort: 3480,
			TurnRelayPortMin: 49160, TurnRelayPortMax: 49199, TurnMaxPerPlayer: 2, TurnBytesPerSecond: 65536,
			TurnCredentialMinutes: 720},
		ProfilePics: ProfilePicsConfig{Enabled: true, Dir: "data/profile_pics", MaxKB: 512, UploadMinutes: 10},
		MainStage: MainStageConfig{Slots: 4, TurnMinutes: 0, EventSeconds: 1750, PurchaseOpenMinutes: 120,
			PurchaseCloseMinutes: 5, UpcomingEvents: 6, CooldownEvents: 1, MaxPlayers: 50, Venue: "VenueB",
			TimeOfDay: "night", PassMinutes: 10, PassCheckIP: true, GeneratedImages: true,
			ImageDir: "data/mainstage_images", HoldCurrentMinutes: 60, Rotation: MainStageRotationConfig{Enabled: true,
				Anchor: "2026-01-01T00:00:00Z", IntervalMinutes: 60, Name: "Main Stage"}},
		Multiplayer: MultiplayerConfig{
			Region:               "fuserd",
			HostPort:             7777,
			HostTimeoutSeconds:   60,
			SeparateStockPlayers: true,
			PublicServers: PublicServerConfig{MaxServers: 4,
				BasePort: 7777, PortStep: 200, MaxPlayers: 11, MaxDJs: 4, StartupTimeoutSeconds: 120,
				FDSPakKeyEnv: "FDS_PAK_KEY", FDSHeartbeatSeconds: 10, FDSListen: "127.0.0.1:8001", FDSSessionTokens: true,
				WarmRounds: 1},
		},
	}
}

// Load reads path over the defaults and validates the result.
func Load(path string) (Config, error) {
	cfg := Default()
	raw, err := os.ReadFile(path)
	if err != nil {
		return cfg, err
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return cfg, fmt.Errorf("%s: %w", path, err)
	}
	var removed struct {
		Multiplayer struct {
			PublicSidecars json.RawMessage `json:"public_sidecars"`
		} `json:"multiplayer"`
	}
	if json.Unmarshal(raw, &removed) == nil && removed.Multiplayer.PublicSidecars != nil {
		return cfg, fmt.Errorf("%s: multiplayer.public_sidecars is not supported: rename it to multiplayer.public_servers "+
			"(FDS containers only) and remove its kind and log_dir keys", path)
	}
	cfg.PublicURL = strings.TrimRight(cfg.PublicURL, "/")

	if cfg.SongManifest.SongsFile != "" {
		songsPath := cfg.SongManifest.SongsFile
		if !filepath.IsAbs(songsPath) {
			songsPath = filepath.Join(filepath.Dir(path), songsPath)
		}
		raw, err := os.ReadFile(songsPath)
		if err != nil {
			return cfg, fmt.Errorf("song manifest: %w", err)
		}
		if err := json.Unmarshal(raw, &cfg.SongManifest.Songs); err != nil {
			return cfg, fmt.Errorf("%s: %w", songsPath, err)
		}
	}
	cfg.Store.CatalogueFile = relativeTo(path, cfg.Store.CatalogueFile)
	cfg.Store.FilesDir = relativeTo(path, cfg.Store.FilesDir)
	cfg.ModPackFile = relativeTo(path, cfg.ModPackFile)
	cfg.SongManifest.PakFile = relativeTo(path, cfg.SongManifest.PakFile)
	cfg.ProfilePics.Dir = relativeTo(path, cfg.ProfilePics.Dir)
	cfg.MainStage.ImageDir = relativeTo(path, cfg.MainStage.ImageDir)
	cfg.ACME.CacheDir = relativeTo(path, cfg.ACME.CacheDir)
	cfg.resolveACMEDomains()
	if f := relativeTo(path, cfg.Store.ReservedShortnamesFile); f != "" {
		raw, err := os.ReadFile(f)
		if err != nil {
			return cfg, fmt.Errorf("store reserved shortnames: %w", err)
		}
		if err := json.Unmarshal(raw, &cfg.Store.Reserved); err != nil {
			return cfg, fmt.Errorf("%s: %w", f, err)
		}
	}
	return cfg, cfg.validate()
}

// relativeTo resolves a config-relative path ("" stays "").
func relativeTo(configPath, p string) string {
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(filepath.Dir(configPath), p)
}

func (c Config) validate() error {
	if c.Control.Enabled {
		u, err := url.Parse(c.Control.Origin)
		if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" {
			return errors.New("control.origin must be an exact origin without a path")
		}
		if u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1" || u.Hostname() == "::1")) {
			return errors.New("control.origin requires HTTPS or HTTP localhost")
		}
		if _, err := c.Control.Networks(); err != nil {
			return err
		}
		if _, err := c.Control.Location(); err != nil {
			return err
		}
	}

	if len(c.TokenSecret) < 32 {
		return errors.New("token_secret must be at least 32 characters")
	}
	if c.TokenLifetimeSeconds <= 0 {
		return errors.New("token_lifetime_seconds must be positive")
	}
	if c.Multiplayer.HostPort <= 0 || c.Multiplayer.HostPort > 65535-100 {
		return errors.New("multiplayer.host_port must be between 1 and 65435 (the beacon uses host_port+100)")
	}
	if c.Multiplayer.Region == "" {
		return errors.New("multiplayer.region must not be empty (an empty region list fails matchmaking)")
	}
	if n := c.Multiplayer.MaxCrateSongs; n < 0 || (n > 0 && n < MinCrateSongsLimit) {
		return fmt.Errorf("multiplayer.max_crate_songs must be 0 (no limit) or at least %d (the stock crate size)", MinCrateSongsLimit)
	}
	if p := c.Multiplayer.PublicServers; p.Enabled {
		if p.Image == "" || p.GameRoot == "" || p.Address == "" || p.MaxServers < 1 || p.MaxPlayers < 1 || p.MaxDJs < 0 ||
			p.BasePort < 1 || p.PortStep < 101 || p.BasePort+(p.MaxServers-1)*p.PortStep > 65435 || p.StartupTimeoutSeconds < 1 {
			return errors.New("multiplayer.public_servers requires image, game_root, address, positive limits, valid ports and startup timeout")
		}
		if _, port, err := net.SplitHostPort(p.FDSListen); err != nil || port == "" {
			return errors.New("multiplayer.public_servers.fds_listen must be host:port (e.g. 127.0.0.1:8001)")
		}
		if u, err := url.Parse(p.RegisterURL()); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return errors.New("multiplayer.public_servers.fds_register_url must be an http(s) URL the containers reach the registration API at")
		}
		if p.FDSPakKeyEnv == "" || p.FDSHeartbeatSeconds < 1 {
			return errors.New("multiplayer.public_servers: fds_pak_key_env must be set and fds_heartbeat_seconds positive")
		}
		// The warm pool leaves a slot for ticket-started servers, and one for the Main Stage server when it is on.
		warmMax := p.MaxServers - 1
		if c.MainStage.Enabled {
			warmMax--
		}
		if p.WarmServers < 0 || (p.WarmServers > 0 && p.WarmServers > warmMax) || p.WarmRounds < 1 || p.WarmRounds > 10 {
			return errors.New("multiplayer.public_servers: warm_servers must be 0..max_servers-1 (max_servers-2 with " +
				"main_stage enabled) and warm_rounds 1..10")
		}
	}
	if c.Store.CatalogueFile != "" && c.Store.FilesDir == "" {
		return errors.New("store.files_dir is required with store.catalogue_file")
	}
	if c.Store.MaxPakMB < 1 || c.Store.RequestsPerMinute < 0 {
		return errors.New("store.max_pak_mb must be positive and store.requests_per_minute not negative")
	}
	if p := c.P2P; p.StunPort < 1 || p.StunPort > 65535 || p.StunAltPort < 0 || p.StunAltPort > 65535 ||
		p.StunAltPort == p.StunPort || p.StunRequestsPerSecond < 0 || p.RequestsPerMinute < 0 {
		return errors.New("p2p: stun_port must be a port, stun_alt_port another port or 0, stun_requests_per_second and requests_per_minute not negative")
	}
	if p := c.P2P; p.TurnEnabled {
		ip := net.ParseIP(p.TurnRelayAddress)
		switch {
		case p.TurnPort < 1 || p.TurnPort > 65535 || p.TurnPort == p.StunPort || p.TurnPort == p.StunAltPort:
			return errors.New("p2p.turn_port must be a port other than the STUN ports")
		case ip == nil || ip.To4() == nil:
			return errors.New("p2p.turn_relay_address must be this server's public IPv4 address")
		case p.TurnRelayPortMin < 1 || p.TurnRelayPortMax > 65535 || p.TurnRelayPortMin > p.TurnRelayPortMax:
			return errors.New("p2p.turn_relay_port_min/max must be a port range")
		case p.TurnMaxPerPlayer < 0 || p.TurnBytesPerSecond < 0 || p.TurnCredentialMinutes < 1:
			return errors.New("p2p: turn_max_per_player and turn_bytes_per_second must not be negative, turn_credential_minutes positive")
		}
	}
	if p := c.ProfilePics; p.Enabled && (p.Dir == "" || p.MaxKB < 16 || p.UploadMinutes < 1) {
		return errors.New("profile_pics: dir is required, max_kb must be at least 16 and upload_minutes positive")
	}
	if c.Store.ElderCreditBalance < 0 || c.Store.XPPerElderCredit < 1 {
		return errors.New("store.elder_credit_balance must not be negative and store.xp_per_elder_credit must be positive")
	}
	if err := c.MainStage.validate(); err != nil {
		return err
	}
	if b := c.BattleSeason; b.Enabled && (b.Name != "season1" && b.Name != "season2") {
		return errors.New(`battle_season.name must be "season1" or "season2" (a BattleSeasonDataAssets key) when battle_season.enabled is set`)
	}
	if (c.TLSCert == "") != (c.TLSKey == "") {
		return errors.New("tls_cert and tls_key must be set together")
	}
	return c.validateACME()
}

func (m MainStageConfig) validate() error {
	if !m.Enabled {
		return nil
	}
	switch {
	case m.Slots < 1 || m.Slots > 8:
		return errors.New("main_stage.slots must be between 1 and 8")
	case m.TurnMinutes < 0 || m.TurnMinutes > 60:
		return errors.New("main_stage.turn_minutes must be between 0 (no limit) and 60")
	case m.EventSeconds < 60 || m.EventSeconds > 12*3600:
		return errors.New("main_stage.event_seconds must be between 60 and 43200")
	case m.PurchaseCloseMinutes < 0 || m.PurchaseOpenMinutes <= m.PurchaseCloseMinutes:
		return errors.New("main_stage: purchase_open_minutes must be greater than purchase_close_minutes, which must not be negative")
	case m.UpcomingEvents < 0 || m.UpcomingEvents > 50:
		return errors.New("main_stage.upcoming_events must be between 0 and 50")
	case m.CooldownEvents < 0 || m.CooldownEvents > 24:
		return errors.New("main_stage.cooldown_events must be between 0 and 24")
	case m.MaxPlayers < 1 || m.MaxPlayers > 300:
		return errors.New("main_stage.max_players must be between 1 and 300 (FUSER's MaxPlayersInMainStage)")
	case m.Venue == "" || m.PassMinutes < 1 || m.PassMinutes > 120:
		return errors.New("main_stage: venue is required and pass_minutes must be between 1 and 120")
	case m.TimeOfDay != "morning" && m.TimeOfDay != "day" && m.TimeOfDay != "sunset" && m.TimeOfDay != "night":
		return errors.New("main_stage.time_of_day must be morning, day, sunset or night")
	case m.GeneratedImages && m.ImageDir == "":
		return errors.New("main_stage.image_dir is required with generated_images")
	case m.HoldCurrentMinutes < 0 || m.HoldCurrentMinutes > MaxHoldCurrentMinutes:
		return fmt.Errorf("main_stage.hold_current_minutes must be between 0 (off) and %d", MaxHoldCurrentMinutes)
	}
	if r := m.Rotation; r.Enabled {
		if _, err := time.Parse(time.RFC3339, r.Anchor); err != nil {
			return fmt.Errorf("main_stage.rotation.anchor: %w", err)
		}
		if r.IntervalMinutes*60 < m.EventSeconds || strings.TrimSpace(r.Name) == "" {
			return errors.New("main_stage.rotation: interval_minutes must cover event_seconds and name must not be empty")
		}
	}
	return nil
}

// RotationSchedule is the rotation as the mainstage package uses it.
func (m MainStageConfig) RotationSchedule() mainstage.Rotation {
	anchor, _ := time.Parse(time.RFC3339, m.Rotation.Anchor)
	return mainstage.Rotation{Enabled: m.Rotation.Enabled, Anchor: anchor,
		Interval: time.Duration(m.Rotation.IntervalMinutes) * time.Minute,
		Duration: time.Duration(m.EventSeconds) * time.Second, Name: m.Rotation.Name, ImageURL: m.Rotation.ImageURL,
		Slots: m.Slots, TurnMinutes: m.TurnMinutes, AllowStock: m.Rotation.AllowStock}
}

// HoldCurrent is main_stage.hold_current_minutes as a duration: the longest the game keeps seeing an ended event as
// current (mainstage.Hold).
func (m MainStageConfig) HoldCurrent() time.Duration {
	return time.Duration(m.HoldCurrentMinutes) * time.Minute
}

// PurchaseWindow is when headliner slots can be claimed.
func (m MainStageConfig) PurchaseWindow() mainstage.Window {
	return mainstage.Window{OpenBefore: time.Duration(m.PurchaseOpenMinutes) * time.Minute,
		CloseBefore: time.Duration(m.PurchaseCloseMinutes) * time.Minute}
}
