// Package turnrelay is fuserd's TURN relay, built on pion/turn. When two players' NATs can't be
// punched through (typically a symmetric carrier NAT on a phone connection), the joiner's loader allocates a relay
// address here and the session's host sends to it; the relay forwards between them.
//
// Credentials are the TURN REST kind: username "<expiry unix>:<player id>", password base64(HMAC-SHA1(secret,
// username)). fuserd issues them only to members of a P2P session (p2p/join/), so the relay needs no account store.
// Each player may hold a few allocations at once, and each allocation's traffic is rate limited.
package turnrelay

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pion/logging"
	"github.com/pion/turn/v4"
)

// Options configure the relay.
type Options struct {
	Bind           string // listening address ("" = every interface)
	Port           int    // the TURN listening port (UDP); 0 picks a free one (tests)
	RelayIP        net.IP // the public IP clients are told to send relayed traffic to
	RelayBind      string // the address relay sockets bind ("" = every interface)
	MinPort        uint16 // the relay socket range
	MaxPort        uint16
	Realm          string
	Secret         string // the shared secret behind the credentials
	MaxPerUser     int    // concurrent allocations per player (0 = no limit)
	BytesPerSecond int    // per allocation and direction (0 = no limit)
	Log            *slog.Logger
	Tag            func(string) string // makes a player id or address loggable; nil logs it as is
}

// Relay is a running TURN server.
type Relay struct {
	opt    Options
	conn   net.PacketConn
	server *turn.Server

	mu    sync.Mutex
	users map[string]int // live allocations per username
}

// Listen starts the relay.
func Listen(opt Options) (*Relay, error) {
	if opt.Secret == "" || opt.RelayIP == nil || opt.MinPort == 0 || opt.MaxPort < opt.MinPort {
		return nil, errors.New("turn: secret, relay IP and a relay port range are required")
	}
	if opt.Realm == "" {
		opt.Realm = "fuserd"
	}
	if opt.Log == nil {
		opt.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if opt.Tag == nil {
		opt.Tag = func(v string) string { return v }
	}
	if opt.RelayBind == "" {
		opt.RelayBind = "0.0.0.0"
	}
	conn, err := net.ListenPacket("udp4", net.JoinHostPort(opt.Bind, strconv.Itoa(opt.Port)))
	if err != nil {
		return nil, fmt.Errorf("turn: %w", err)
	}
	r := &Relay{opt: opt, conn: conn, users: map[string]int{}}
	gen := &limitedGenerator{RelayAddressGeneratorPortRange: turn.RelayAddressGeneratorPortRange{
		RelayAddress: opt.RelayIP, Address: opt.RelayBind, MinPort: opt.MinPort, MaxPort: opt.MaxPort,
	}, bytesPerSecond: opt.BytesPerSecond}
	quiet := logging.NewDefaultLoggerFactory()
	quiet.DefaultLogLevel = logging.LogLevelDisabled
	r.server, err = turn.NewServer(turn.ServerConfig{
		Realm:         opt.Realm,
		LoggerFactory: quiet,
		AuthHandler:   r.auth,
		QuotaHandler:  r.quota,
		EventHandler: turn.EventHandler{
			OnAllocationCreated: func(src, _ net.Addr, _, username, _ string, relay net.Addr, _ int) {
				r.mu.Lock()
				r.users[username]++
				r.mu.Unlock()
				r.opt.Log.Info("relay allocated", "player", r.opt.Tag(playerOf(username)), "client", r.opt.Tag(hostOf(src)),
					"relay_port", portOf(relay))
			},
			OnAllocationDeleted: func(src, _ net.Addr, _, username, _ string) {
				r.mu.Lock()
				if r.users[username]--; r.users[username] <= 0 {
					delete(r.users, username)
				}
				r.mu.Unlock()
				r.opt.Log.Info("relay released", "player", r.opt.Tag(playerOf(username)), "client", r.opt.Tag(hostOf(src)))
			},
			OnPermissionCreated: func(_, _ net.Addr, _, username, _ string, _ net.Addr, peer net.IP) {
				r.opt.Log.Info("relay permission", "player", r.opt.Tag(playerOf(username)), "peer", r.opt.Tag(peer.String()))
			},
			OnAuth: func(src, _ net.Addr, _, username, _ string, method string, ok bool) {
				if !ok {
					r.opt.Log.Warn("relay auth refused", "method", method, "player", r.opt.Tag(playerOf(username)),
						"client", r.opt.Tag(hostOf(src)))
				}
			},
		},
		PacketConnConfigs: []turn.PacketConnConfig{{PacketConn: conn, RelayAddressGenerator: gen}},
	})
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("turn: %w", err)
	}
	return r, nil
}

// Addr is the TURN listening address.
func (r *Relay) Addr() *net.UDPAddr { return r.conn.LocalAddr().(*net.UDPAddr) }

// Close stops the relay and frees every allocation.
func (r *Relay) Close() error { return r.server.Close() }

// Allocations is the number of live allocations.
func (r *Relay) Allocations() int { return r.server.AllocationCount() }

// Credentials issues a credential for a player, valid for ttl.
func (r *Relay) Credentials(playerID int64, ttl time.Duration) (username, password string, expires time.Time) {
	expires = time.Now().Add(ttl)
	username = strconv.FormatInt(expires.Unix(), 10) + ":" + strconv.FormatInt(playerID, 10)
	return username, Password(r.opt.Secret, username), expires
}

// Realm is the relay's realm.
func (r *Relay) Realm() string { return r.opt.Realm }

// Password is the TURN REST password for a username: base64(HMAC-SHA1(secret, username)).
func Password(secret, username string) string {
	mac := hmac.New(sha1.New, []byte(secret))
	mac.Write([]byte(username))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

func (r *Relay) auth(username, realm string, _ net.Addr) ([]byte, bool) {
	expiry, player, ok := strings.Cut(username, ":")
	t, err := strconv.ParseInt(expiry, 10, 64)
	if !ok || err != nil || player == "" || time.Now().Unix() > t {
		return nil, false
	}
	return turn.GenerateAuthKey(username, realm, Password(r.opt.Secret, username)), true
}

func (r *Relay) quota(username, _ string, src net.Addr) bool {
	if r.opt.MaxPerUser <= 0 {
		return true
	}
	r.mu.Lock()
	n := r.users[username]
	r.mu.Unlock()
	if n >= r.opt.MaxPerUser {
		r.opt.Log.Warn("relay quota reached", "player", r.opt.Tag(playerOf(username)), "client", r.opt.Tag(hostOf(src)),
			"allocations", n)
		return false
	}
	return true
}

func playerOf(username string) string {
	_, player, _ := strings.Cut(username, ":")
	return player
}

func hostOf(a net.Addr) string {
	if u, ok := a.(*net.UDPAddr); ok {
		return u.IP.String()
	}
	if a == nil {
		return ""
	}
	return a.String()
}

func portOf(a net.Addr) int {
	if u, ok := a.(*net.UDPAddr); ok {
		return u.Port
	}
	return 0
}

// ---- per-allocation rate limit -----------------------------------------------------------------------------------

// limitedGenerator hands out relay sockets whose traffic is limited per direction: packets over the budget are
// dropped, as an overloaded network would.
type limitedGenerator struct {
	turn.RelayAddressGeneratorPortRange
	bytesPerSecond int
}

func (g *limitedGenerator) AllocatePacketConn(network string, port int) (net.PacketConn, net.Addr, error) {
	c, addr, err := g.RelayAddressGeneratorPortRange.AllocatePacketConn(network, port)
	if err != nil || g.bytesPerSecond <= 0 {
		return c, addr, err
	}
	return &limitedConn{PacketConn: c, in: newBucket(g.bytesPerSecond), out: newBucket(g.bytesPerSecond)}, addr, nil
}

type limitedConn struct {
	net.PacketConn
	in, out *bucket
}

func (c *limitedConn) ReadFrom(p []byte) (int, net.Addr, error) {
	for {
		n, addr, err := c.PacketConn.ReadFrom(p)
		if err != nil || c.in.take(n) {
			return n, addr, err
		}
	}
}

func (c *limitedConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	if !c.out.take(len(p)) {
		return len(p), nil // dropped
	}
	return c.PacketConn.WriteTo(p, addr)
}

type bucket struct {
	mu     sync.Mutex
	rate   float64
	tokens float64
	last   time.Time
}

func newBucket(perSecond int) *bucket {
	return &bucket{rate: float64(perSecond), tokens: float64(perSecond), last: time.Now()}
}

// take spends n bytes if the bucket (one second of burst) has them.
func (b *bucket) take(n int) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	b.tokens = min(b.rate, b.tokens+now.Sub(b.last).Seconds()*b.rate)
	b.last = now
	if b.tokens < float64(n) {
		return false
	}
	b.tokens -= float64(n)
	return true
}
