// Package p2p is the signaling for P2P private sessions: it lets a session's host and each
// joiner swap the UDP endpoints ("candidates") of their loaders' tunnel sockets, and gives every host/joiner pair a
// peer id and a shared key for the tunnel's packets. The loaders then probe each other's candidates (the hole
// punch) and carry FUSER's traffic over the pair that answers.
//
// State is in memory, per game session, and expires when not refreshed: the loaders poll while they set up and
// keep their registration alive afterwards. Who may register is the caller's business (the API checks matchmaking
// membership); this package only keeps the pairs consistent.
package p2p

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"sort"
	"sync"
	"time"
)

// Candidate types.
const (
	TypeHost  = "host"  // an address of one of the machine's own interfaces (LAN)
	TypeSrflx = "srflx" // the public endpoint fuserd's STUN service saw ("server reflexive")
	TypeRelay = "relay" // an address allocated on fuserd's TURN relay
)

// MaxCandidates bounds one side's candidate list.
const MaxCandidates = 8

var (
	ErrNoHost    = errors.New("the host has not registered for P2P")
	ErrNoPeer    = errors.New("not registered as a joiner")
	ErrBadReport = errors.New("unknown peer id")
)

// Candidate is one UDP endpoint a loader can be reached at.
type Candidate struct {
	Type string `json:"type"`
	IP   string `json:"ip"`
	Port int    `json:"port"`
}

// Endpoint is one side's candidates and what its STUN probe said about its NAT (stunserver.Mapping).
type Endpoint struct {
	Candidates []Candidate `json:"candidates"`
	Mapping    string      `json:"mapping"`
}

// Validate checks an endpoint a loader sent: 1-8 candidates of a known type with a usable IP and port.
func (e Endpoint) Validate() error {
	if len(e.Candidates) == 0 || len(e.Candidates) > MaxCandidates {
		return fmt.Errorf("want 1 to %d candidates, got %d", MaxCandidates, len(e.Candidates))
	}
	for _, c := range e.Candidates {
		if c.Type != TypeHost && c.Type != TypeSrflx && c.Type != TypeRelay {
			return fmt.Errorf("candidate type %q is not host, srflx or relay", c.Type)
		}
		ip := net.ParseIP(c.IP)
		if ip == nil || ip.IsUnspecified() || ip.IsLoopback() || ip.IsMulticast() {
			return fmt.Errorf("candidate address %q is not usable", c.IP)
		}
		if c.Port < 1 || c.Port > 65535 {
			return fmt.Errorf("candidate port %d is out of range", c.Port)
		}
	}
	if len(e.Mapping) > 32 {
		return errors.New("mapping is too long")
	}
	return nil
}

// Report is what a loader said about its connection to a peer, for the log and the direct/relay statistics.
type Report struct {
	State string    `json:"state"` // "connected", "failed", ...
	Path  string    `json:"path"`  // "lan", "direct", "relay", ...
	RTTMs int       `json:"rtt_ms"`
	At    time.Time `json:"-"`
}

// Peer is one joiner of a session, as the host sees it.
type Peer struct {
	ID       uint32
	PlayerID int64
	Key      string // 32 random bytes, hex: the tunnel's per-pair key
	Endpoint
	Updated time.Time
	Report  *Report
}

// Registry holds the sessions' signaling. It is safe for concurrent use.
type Registry struct {
	ttl time.Duration
	now func() time.Time

	mu       sync.Mutex
	sessions map[string]*entry
}

type entry struct {
	hostID      int64
	host        *Endpoint
	hostUpdated time.Time
	peers       map[int64]*Peer // by player id
}

// New makes a registry whose registrations expire ttl after their last refresh.
func New(ttl time.Duration) *Registry {
	return &Registry{ttl: ttl, now: time.Now, sessions: map[string]*entry{}}
}

func (r *Registry) entry(sessionID string, hostID int64) *entry {
	e, ok := r.sessions[sessionID]
	if !ok || e.hostID != hostID { // a new session, or the id reused by another host: start over
		e = &entry{hostID: hostID, peers: map[int64]*Peer{}}
		r.sessions[sessionID] = e
	}
	return e
}

// sweep drops stale registrations. Callers hold mu.
func (r *Registry) sweep() {
	now := r.now()
	for id, e := range r.sessions {
		if e.host != nil && now.Sub(e.hostUpdated) > r.ttl {
			e.host = nil
		}
		for pid, p := range e.peers {
			if now.Sub(p.Updated) > r.ttl {
				delete(e.peers, pid)
			}
		}
		if e.host == nil && len(e.peers) == 0 {
			delete(r.sessions, id)
		}
	}
}

// Drop forgets a session (it ended).
func (r *Registry) Drop(sessionID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.sessions, sessionID)
}

// SetHost records (or refreshes) the host's endpoint and returns the session's current joiners, filtered by
// isMember (players who left the session are forgotten).
func (r *Registry) SetHost(sessionID string, hostID int64, ep Endpoint, isMember func(int64) bool) []Peer {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sweep()
	e := r.entry(sessionID, hostID)
	e.host, e.hostUpdated = &ep, r.now()
	return e.list(isMember)
}

// HostPoll refreshes the host's registration and returns its joiners. ErrNoHost when it never registered (or
// expired).
func (r *Registry) HostPoll(sessionID string, hostID int64, isMember func(int64) bool) ([]Peer, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sweep()
	e, ok := r.sessions[sessionID]
	if !ok || e.hostID != hostID || e.host == nil {
		return nil, ErrNoHost
	}
	e.hostUpdated = r.now()
	return e.list(isMember), nil
}

func (e *entry) list(isMember func(int64) bool) []Peer {
	out := make([]Peer, 0, len(e.peers))
	for pid, p := range e.peers {
		if isMember != nil && !isMember(pid) {
			delete(e.peers, pid)
			continue
		}
		out = append(out, *p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// SetJoiner records (or refreshes) a joiner's endpoint. A joiner keeps its peer id and key across calls. It
// returns the joiner's peer and the host's endpoint (nil while the host hasn't registered).
func (r *Registry) SetJoiner(sessionID string, hostID, playerID int64, ep Endpoint) (Peer, *Endpoint, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sweep()
	e := r.entry(sessionID, hostID)
	p, ok := e.peers[playerID]
	if !ok {
		id, key, err := e.newPeerID()
		if err != nil {
			return Peer{}, nil, err
		}
		p = &Peer{ID: id, PlayerID: playerID, Key: key}
		e.peers[playerID] = p
	}
	p.Endpoint, p.Updated = ep, r.now()
	return *p, e.hostEndpoint(), nil
}

// JoinerPoll refreshes a joiner and returns its peer and the host's endpoint (nil while the host hasn't
// registered). ErrNoPeer when it never registered (or expired).
func (r *Registry) JoinerPoll(sessionID string, hostID, playerID int64) (Peer, *Endpoint, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sweep()
	e, ok := r.sessions[sessionID]
	if !ok || e.hostID != hostID {
		return Peer{}, nil, ErrNoPeer
	}
	p, ok := e.peers[playerID]
	if !ok {
		return Peer{}, nil, ErrNoPeer
	}
	p.Updated = r.now()
	return *p, e.hostEndpoint(), nil
}

func (e *entry) hostEndpoint() *Endpoint {
	if e.host == nil {
		return nil
	}
	h := *e.host
	return &h
}

// SetReport records a connection report about peer id from either side of the pair: the host (any of its peers)
// or the joiner (its own peer only). It returns the peer's player id.
func (r *Registry) SetReport(sessionID string, hostID, playerID int64, peerID uint32, rep Report) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.sessions[sessionID]
	if !ok || e.hostID != hostID {
		return 0, ErrBadReport
	}
	for pid, p := range e.peers {
		if p.ID == peerID && (playerID == hostID || playerID == pid) {
			rep.At = r.now()
			p.Report = &rep
			return pid, nil
		}
	}
	return 0, ErrBadReport
}

// newPeerID picks a random non-zero peer id unused in the session, and a key.
func (e *entry) newPeerID() (uint32, string, error) {
	var b [36]byte
	for tries := 0; tries < 8; tries++ {
		if _, err := rand.Read(b[:]); err != nil {
			return 0, "", err
		}
		id := binary.LittleEndian.Uint32(b[:4])
		if id == 0 {
			continue
		}
		taken := false
		for _, p := range e.peers {
			taken = taken || p.ID == id
		}
		if !taken {
			return id, hex.EncodeToString(b[4:]), nil
		}
	}
	return 0, "", errors.New("could not pick a peer id")
}
