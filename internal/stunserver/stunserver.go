// Package stunserver is fuserd's STUN service: it answers STUN Binding requests (RFC 8489) with
// the address and port the request came from, so a player's loader learns the public UDP endpoint of its tunnel
// socket. It listens on two ports of the same address: a client whose public port differs between them sits behind
// a NAT whose mapping depends on the destination ("symmetric"), where hole punching is unlikely to work.
//
// Only Binding requests are answered (XOR-MAPPED-ADDRESS, MAPPED-ADDRESS for RFC 3489 clients, SOFTWARE,
// FINGERPRINT). There is no authentication, TURN or OTHER-ADDRESS: the second port is not a second address, so
// RFC 5780's tests don't apply, and the loader learns both ports from p2p/config/ instead.
package stunserver

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/pion/stun/v3"
)

// Software is the SOFTWARE attribute of every answer.
const Software = "fuserd"

// maxPacket bounds a request; a Binding request is 20 bytes plus a few attributes.
const maxPacket = 1500

// Options configure the listeners.
type Options struct {
	Bind              string                    // address to bind ("" = all interfaces)
	Ports             []int                     // one listener each; 0 picks a free port (tests)
	RequestsPerSecond int                       // per client IP; 0 = no limit
	Log               *slog.Logger              // nil = discard
	Tag               func(value string) string // turns a client address into a log tag; nil = log it as is
}

// Server is a running set of STUN listeners.
type Server struct {
	opt   Options
	conns []*net.UDPConn
	wg    sync.WaitGroup
	limit *limiter
	seen  *limiter // logs one line per listener and client endpoint per minute
	soft  stun.Software
}

// Listen binds every port and starts answering. On error nothing is left open.
func Listen(opt Options) (*Server, error) {
	if len(opt.Ports) == 0 {
		return nil, errors.New("stun: no ports")
	}
	if opt.Log == nil {
		opt.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if opt.Tag == nil {
		opt.Tag = func(v string) string { return v }
	}
	s := &Server{opt: opt, limit: newLimiter(time.Second, opt.RequestsPerSecond), seen: newLimiter(time.Minute, 1),
		soft: stun.NewSoftware(Software)}
	for _, port := range opt.Ports {
		addr, err := net.ResolveUDPAddr("udp", net.JoinHostPort(opt.Bind, strconv.Itoa(port)))
		if err != nil {
			s.Close()
			return nil, fmt.Errorf("stun: %w", err)
		}
		c, err := net.ListenUDP("udp", addr)
		if err != nil {
			s.Close()
			return nil, fmt.Errorf("stun: %w", err)
		}
		s.conns = append(s.conns, c)
	}
	for _, c := range s.conns {
		s.wg.Add(1)
		go s.serve(c)
	}
	return s, nil
}

// Addrs are the bound addresses, in Options.Ports order.
func (s *Server) Addrs() []*net.UDPAddr {
	out := make([]*net.UDPAddr, 0, len(s.conns))
	for _, c := range s.conns {
		out = append(out, c.LocalAddr().(*net.UDPAddr))
	}
	return out
}

// Close stops every listener and waits for them.
func (s *Server) Close() error {
	for _, c := range s.conns {
		c.Close()
	}
	s.wg.Wait()
	return nil
}

func (s *Server) serve(c *net.UDPConn) {
	defer s.wg.Done()
	port := c.LocalAddr().(*net.UDPAddr).Port
	buf := make([]byte, maxPacket)
	for {
		n, from, err := c.ReadFromUDP(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue // e.g. an ICMP error surfaced on Windows; keep serving
		}
		resp, ok := s.answer(buf[:n], from)
		if !ok {
			continue
		}
		// The client's IP is tagged (stable per player); its port is logged as is, which shows what the NAT did.
		if _, err := c.WriteToUDP(resp, from); err != nil {
			s.opt.Log.Warn("stun reply failed", "port", port, "client", s.opt.Tag(from.IP.String()), "client_port", from.Port,
				"err", err)
			continue
		}
		if s.seen.allow(strconv.Itoa(port) + " " + from.String()) {
			s.opt.Log.Info("stun binding", "port", port, "client", s.opt.Tag(from.IP.String()), "client_port", from.Port)
		}
	}
}

// answer builds the Binding success response to a request from addr, or reports false for anything that isn't a
// well-formed Binding request (or is over the client's rate limit): those get no answer at all.
func (s *Server) answer(packet []byte, from *net.UDPAddr) ([]byte, bool) {
	if !stun.IsMessage(packet) {
		return nil, false
	}
	req := &stun.Message{Raw: append([]byte(nil), packet...)}
	if err := req.Decode(); err != nil || req.Type != stun.BindingRequest {
		return nil, false
	}
	if req.Contains(stun.AttrFingerprint) && stun.Fingerprint.Check(req) != nil {
		return nil, false
	}
	if !s.limit.allow(from.IP.String()) {
		return nil, false
	}
	ip := from.IP
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	resp, err := stun.Build(stun.NewTransactionIDSetter(req.TransactionID), stun.BindingSuccess,
		&stun.XORMappedAddress{IP: ip, Port: from.Port}, &stun.MappedAddress{IP: ip, Port: from.Port},
		s.soft, stun.Fingerprint)
	if err != nil {
		return nil, false
	}
	return resp.Raw, true
}

// ---- rate limiting ---------------------------------------------------------------------------------------------

// limiter allows max events per key in each window (max <= 0: unlimited).
type limiter struct {
	window time.Duration
	max    int
	mu     sync.Mutex
	start  time.Time
	counts map[string]int
}

func newLimiter(window time.Duration, max int) *limiter {
	return &limiter{window: window, max: max, counts: map[string]int{}}
}

func (l *limiter) allow(key string) bool {
	if l.max <= 0 {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if now.Sub(l.start) >= l.window {
		l.start, l.counts = now, map[string]int{}
	}
	l.counts[key]++
	return l.counts[key] <= l.max
}

// ---- client --------------------------------------------------------------------------------------------------

// ErrNoAnswer is Probe's error when no answer arrived in time.
var ErrNoAnswer = errors.New("no STUN answer")

// Probe sends a Binding request from conn to server and returns the endpoint the server saw (XOR-MAPPED-ADDRESS).
// It tries attempts times, waiting wait for each answer, and skips packets that aren't the answer.
func Probe(conn *net.UDPConn, server *net.UDPAddr, attempts int, wait time.Duration) (*net.UDPAddr, error) {
	req, err := stun.Build(stun.TransactionID, stun.BindingRequest, stun.Fingerprint)
	if err != nil {
		return nil, err
	}
	buf := make([]byte, maxPacket)
	for i := 0; i < attempts; i++ {
		if _, err := conn.WriteToUDP(req.Raw, server); err != nil {
			return nil, err
		}
		deadline := time.Now().Add(wait)
		for {
			if err := conn.SetReadDeadline(deadline); err != nil {
				return nil, err
			}
			n, _, err := conn.ReadFromUDP(buf)
			if err != nil {
				var ne net.Error
				if errors.As(err, &ne) && ne.Timeout() {
					break
				}
				if time.Now().Before(deadline) {
					continue // an ICMP error from an earlier send, on Windows
				}
				return nil, err
			}
			resp := &stun.Message{Raw: append([]byte(nil), buf[:n]...)}
			if resp.Decode() != nil || resp.TransactionID != req.TransactionID || resp.Type != stun.BindingSuccess {
				continue
			}
			var xor stun.XORMappedAddress
			if err := xor.GetFrom(resp); err != nil {
				return nil, fmt.Errorf("answer without XOR-MAPPED-ADDRESS: %w", err)
			}
			conn.SetReadDeadline(time.Time{})
			return &net.UDPAddr{IP: xor.IP, Port: xor.Port}, nil
		}
	}
	conn.SetReadDeadline(time.Time{})
	return nil, ErrNoAnswer
}

// Mapping classifies a NAT from one socket's endpoints as seen by the two STUN ports of one server.
type Mapping string

const (
	MappingNone        Mapping = "none"                 // the public endpoint is the local one: no NAT
	MappingIndependent Mapping = "endpoint-independent" // same public endpoint toward both ports: punching should work
	MappingDependent   Mapping = "endpoint-dependent"   // a different public port per destination: punching unlikely
	MappingUnknown     Mapping = "unknown"              // only one answer
)

// Classify compares the endpoints the primary and alternate ports saw (alt may be nil) with the local address.
func Classify(local, primary, alt *net.UDPAddr) Mapping {
	if primary == nil || alt == nil {
		if primary != nil && local != nil && primary.Port == local.Port && primary.IP.Equal(local.IP) {
			return MappingNone
		}
		return MappingUnknown
	}
	if !primary.IP.Equal(alt.IP) || primary.Port != alt.Port {
		return MappingDependent
	}
	if local != nil && primary.Port == local.Port && primary.IP.Equal(local.IP) {
		return MappingNone
	}
	return MappingIndependent
}
