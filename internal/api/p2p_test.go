package api

import (
	"net/http"
	"testing"

	"fuseronline/internal/config"
)

type p2pConfigResult struct {
	Enabled bool `json:"enabled"`
	Stun    []struct {
		Host string `json:"host"`
		Port int    `json:"port"`
	} `json:"stun"`
}

func TestP2PConfig(t *testing.T) {
	get := func(adjust func(*config.Config)) p2pConfigResult {
		t.Helper()
		srv := newTestServerWith(t, adjust)
		status, env := call(t, srv, "GET", "p2p/config/", "", nil)
		if status != http.StatusOK {
			t.Fatalf("p2p/config/: %d", status)
		}
		var r p2pConfigResult
		decodeResults(t, env, &r)
		return r
	}

	if r := get(nil); r.Enabled || len(r.Stun) != 0 {
		t.Fatalf("disabled by default: %+v", r)
	}

	r := get(func(c *config.Config) {
		c.P2P.Enabled = true
		c.PublicURL = "http://119.252.75.26:8080"
	})
	if !r.Enabled || len(r.Stun) != 2 || r.Stun[0].Host != "119.252.75.26" || r.Stun[0].Port != 3478 ||
		r.Stun[1].Port != 3479 {
		t.Fatalf("defaults from public_url: %+v", r)
	}

	r = get(func(c *config.Config) {
		c.P2P = config.P2PConfig{Enabled: true, StunHost: "stun.example.org", StunPort: 3500}
		c.PublicURL = "http://fuser.example.org"
	})
	if !r.Enabled || len(r.Stun) != 1 || r.Stun[0].Host != "stun.example.org" || r.Stun[0].Port != 3500 {
		t.Fatalf("stun_host, one port: %+v", r)
	}

	r = get(func(c *config.Config) {
		c.P2P.Enabled = true
		c.PublicURL = ""
	})
	if r.Enabled || len(r.Stun) != 0 {
		t.Fatalf("no host to advertise must read as disabled: %+v", r)
	}
}
