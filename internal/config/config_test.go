package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func publicServers(registerURL string) Config {
	c := Default()
	c.TokenSecret = "test-secret-test-secret-test-secret-0123"
	p := &c.Multiplayer.PublicServers
	p.Enabled, p.Image, p.GameRoot, p.Address, p.FDSRegisterURL = true, "img", "/srv/fuser", "198.51.100.10", registerURL
	return c
}

func TestPublicServers(t *testing.T) {
	if err := publicServers("http://127.0.0.1:8000").validate(); err != nil {
		t.Fatalf("explicit register URL: %v", err)
	}
	if got := publicServers("").Multiplayer.PublicServers.RegisterURL(); got != "http://127.0.0.1:8001" {
		t.Errorf("derived register URL %q", got)
	}
	if err := publicServers("").validate(); err != nil {
		t.Errorf("without an explicit register URL: %v", err)
	}
	wildcard := publicServers("")
	wildcard.Multiplayer.PublicServers.FDSListen = "0.0.0.0:9001"
	if got := wildcard.Multiplayer.PublicServers.RegisterURL(); got != "http://127.0.0.1:9001" {
		t.Errorf("wildcard listen register URL %q", got)
	}
	noPort := publicServers("")
	noPort.Multiplayer.PublicServers.FDSListen = "127.0.0.1"
	if err := noPort.validate(); err == nil {
		t.Error("accepted fds_listen without a port")
	}
	for _, bad := range []string{"127.0.0.1:8000", "ftp://h/"} {
		if err := publicServers(bad).validate(); err == nil {
			t.Errorf("accepted register URL %q", bad)
		}
	}
	noImage := publicServers("")
	noImage.Multiplayer.PublicServers.Image = ""
	if err := noImage.validate(); err == nil {
		t.Error("accepted public servers without an image")
	}
	c := publicServers("http://127.0.0.1:8000")
	c.Multiplayer.PublicServers.FDSHeartbeatSeconds = 0
	if err := c.validate(); err == nil {
		t.Error("accepted a zero heartbeat")
	}
}

// A config still using the removed public_sidecars section fails to load, naming its replacement.
func TestPublicSidecarsRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"multiplayer": {"public_sidecars": {"enabled": false}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "multiplayer.public_servers") {
		t.Fatalf("Load: %v", err)
	}
}

func TestMainStage(t *testing.T) {
	c := Default()
	c.TokenSecret = "test-secret-test-secret-test-secret-0123"
	c.MainStage.Enabled = true
	if err := c.validate(); err != nil {
		t.Fatalf("defaults: %v", err)
	}
	if c.MainStage.HoldCurrentMinutes != 60 || c.MainStage.HoldCurrent().Minutes() != 60 {
		t.Errorf("default hold %s", c.MainStage.HoldCurrent())
	}
	if r := c.MainStage.RotationSchedule(); r.Anchor.IsZero() || r.Interval.Minutes() != 60 || r.Duration.Seconds() != 1750 ||
		r.Slots != 4 || r.TurnMinutes != 0 {
		t.Errorf("rotation %+v", r)
	}
	for name, f := range map[string]func(*MainStageConfig){
		"slots":    func(m *MainStageConfig) { m.Slots = 0 },
		"turn":     func(m *MainStageConfig) { m.TurnMinutes = 61 },
		"length":   func(m *MainStageConfig) { m.EventSeconds = 30 },
		"window":   func(m *MainStageConfig) { m.PurchaseOpenMinutes = m.PurchaseCloseMinutes },
		"anchor":   func(m *MainStageConfig) { m.Rotation.Anchor = "tomorrow" },
		"interval": func(m *MainStageConfig) { m.Rotation.IntervalMinutes = 20 },
		"hold<0":   func(m *MainStageConfig) { m.HoldCurrentMinutes = -1 },
		"hold>max": func(m *MainStageConfig) { m.HoldCurrentMinutes = MaxHoldCurrentMinutes + 1 },
	} {
		bad := c
		f(&bad.MainStage)
		if bad.validate() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	off := c
	off.MainStage.Enabled, off.MainStage.Slots = false, 0
	if err := off.validate(); err != nil {
		t.Errorf("disabled main stage is not checked: %v", err)
	}
}

func TestControlAllowedNetworks(t *testing.T) {
	c := Default()
	c.TokenSecret = "test-secret-test-secret-test-secret-0123"
	c.Control = ControlConfig{Enabled: true, Origin: "https://control.example.org", AllowedNetworks: []string{"203.0.113.0/24", "2001:db8::/48"}}
	if err := c.validate(); err != nil {
		t.Fatalf("valid networks: %v", err)
	}
	c.Control.AllowedNetworks = []string{"203.0.113.7"}
	if err := c.validate(); err == nil {
		t.Fatal("an address without a prefix length was accepted")
	}
}

// multiplayer.max_crate_songs: 0 (no limit) or at least the stock crate size, which every stock player's crate fits.
func TestMaxCrateSongs(t *testing.T) {
	for n, ok := range map[int]bool{0: true, MinCrateSongsLimit: true, 96: true, -1: false, 1: false, MinCrateSongsLimit - 1: false} {
		c := Default()
		c.TokenSecret = "test-secret-test-secret-test-secret-0123"
		c.Multiplayer.MaxCrateSongs = n
		if err := c.validate(); (err == nil) != ok {
			t.Errorf("max_crate_songs %d: err %v, want accepted %v", n, err, ok)
		}
	}
}

// acme: domains default to the HTTPS hosts clients use, and every one of those hosts must be covered.
func TestACME(t *testing.T) {
	acmeConfig := func(publicURL string, domains ...string) Config {
		c := Default()
		c.TokenSecret = "test-secret-test-secret-test-secret-0123"
		c.PublicURL = publicURL
		c.ACME.Enabled, c.ACME.Domains = true, domains
		c.resolveACMEDomains()
		return c
	}
	c := Default()
	c.TokenSecret = "test-secret-test-secret-test-secret-0123"
	c.PublicURL = "https://Fuser.Example.org"
	c.Control = ControlConfig{Enabled: true, Origin: "https://admin.example.org"}
	c.ACME.Enabled = true
	c.resolveACMEDomains()
	if got := strings.Join(c.ACME.Domains, ","); got != "fuser.example.org,admin.example.org" {
		t.Errorf("derived domains %q", got)
	}
	if err := c.validate(); err != nil {
		t.Errorf("derived domains: %v", err)
	}
	if got := strings.Join(acmeConfig("https://fuser.example.org", " FUSER.example.org. ", "www.example.org", "fuser.example.org").ACME.Domains, ","); got != "fuser.example.org,www.example.org" {
		t.Errorf("normalised domains %q", got)
	}
	if err := acmeConfig("https://fuser.example.org:8443", "fuser.example.org", "www.example.org").validate(); err != nil {
		t.Errorf("explicit domains: %v", err)
	}
	for name, bad := range map[string]Config{
		"http public_url":       acmeConfig("http://fuser.example.org"),
		"IP public_url":         acmeConfig("https://203.0.113.7"),
		"public host not given": acmeConfig("https://fuser.example.org", "www.example.org"),
		"wildcard":              acmeConfig("https://fuser.example.org", "fuser.example.org", "*.example.org"),
		"single label":          acmeConfig("https://fuser.example.org", "fuser.example.org", "localhost"),
		"non-punycode":          acmeConfig("https://fuser.example.org", "fuser.example.org", "bücher.example"),
	} {
		if err := bad.validate(); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	withCert := acmeConfig("https://fuser.example.org")
	withCert.TLSCert, withCert.TLSKey = "cert.pem", "key.pem"
	if err := withCert.validate(); err == nil {
		t.Error("acme accepted together with tls_cert")
	}
	staging := acmeConfig("https://fuser.example.org")
	staging.ACME.DirectoryURL = "http://acme.example.org/directory"
	if err := staging.validate(); err == nil {
		t.Error("an http directory_url was accepted")
	}
	noCache := acmeConfig("https://fuser.example.org")
	noCache.ACME.CacheDir = ""
	if err := noCache.validate(); err == nil {
		t.Error("an empty cache_dir was accepted")
	}
}

// battle_season: off by default; when on, the name must be one of the client's BattleSeasonDataAssets keys.
func TestBattleSeason(t *testing.T) {
	c := Default()
	c.TokenSecret = "test-secret-test-secret-test-secret-0123"
	if c.BattleSeason.Enabled {
		t.Fatal("battle mode is on by default")
	}
	c.BattleSeason.Name = NoBattleSeason
	if err := c.validate(); err != nil {
		t.Errorf("disabled, any name: %v", err)
	}
	c.BattleSeason.Enabled = true
	if err := c.validate(); err == nil {
		t.Error(`enabled with "noseason" accepted`)
	}
	c.BattleSeason.Name = "season1"
	if err := c.validate(); err != nil {
		t.Errorf("enabled season1: %v", err)
	}
}
