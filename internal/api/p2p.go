package api

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"fuseronline/internal/logx"
	"fuseronline/internal/matchmaking"
	"fuseronline/internal/p2p"
	"fuseronline/internal/turnrelay"
)

// P2P for player-hosted sessions. The creator's game hosts; the players' loaders reach it through a UDP tunnel
// whose sockets learn their public endpoints from fuserd's STUN service (p2p/config/), swap them here, and punch
// through their NATs.
//
// Roles come from matchmaking: the session's host is the creating player (its loader runs the tunnel's host end in
// the creator's game), and a joiner is any other member. Only unmanaged (player-hosted) sessions take part.

// p2pPollMs is how often the loaders are asked to poll while they set up; p2pTTL is how long a registration lives
// without a poll.
const (
	p2pPollMs = 1000
	p2pTTL    = 2 * time.Minute
)

type stunEndpoint struct {
	Host string `json:"host"`
	Port int    `json:"port"`
}

// stunHost is where players reach the STUN ports: p2p.stun_host, else public_url's host.
func (s *Server) stunHost() string {
	if s.cfg.P2P.StunHost != "" {
		return s.cfg.P2P.StunHost
	}
	if u, err := url.Parse(s.cfg.PublicURL); err == nil {
		return u.Hostname()
	}
	return ""
}

// p2pConfig is GET p2p/config/: whether P2P is on and the STUN endpoints, primary first. The loader binds its
// tunnel socket, asks both, and compares the answers (the alternate port tells whether its NAT maps per
// destination).
func (s *Server) p2pConfig(q *request) (any, error) {
	if !s.p2pLimit.allow("addr:" + s.remoteHost(q.Request)) {
		return nil, errTooMany()
	}
	p := s.cfg.P2P
	stun := []stunEndpoint{}
	if host := s.stunHost(); p.Enabled && host != "" {
		stun = append(stun, stunEndpoint{host, p.StunPort})
		if p.StunAltPort != 0 {
			stun = append(stun, stunEndpoint{host, p.StunAltPort})
		}
	}
	return map[string]any{"enabled": p.Enabled && len(stun) > 0, "stun": stun, "relay": s.relay != nil}, nil
}

func (s *Server) p2pLog() interface {
	Info(string, ...any)
	Warn(string, ...any)
} {
	return s.logs.For(logx.P2P)
}

type p2pRequest struct {
	GameSessionID string `json:"game_session_id"`
	p2p.Endpoint
	// p2p/report/
	PeerID uint32 `json:"peer_id"`
	State  string `json:"state"`
	Path   string `json:"path"`
	RTTMs  int    `json:"rtt_ms"`
}

// p2pSession decodes the request and finds its unmanaged session, which the caller must be a member of.
func (s *Server) p2pSession(q *request) (p2pRequest, matchmaking.Session, error) {
	var req p2pRequest
	if !s.cfg.P2P.Enabled {
		return req, matchmaking.Session{}, &apiError{http.StatusNotFound, codeUnknown, "p2p is disabled"}
	}
	if !s.p2pLimit.allow(fmt.Sprint("player:", q.playerID())) {
		return req, matchmaking.Session{}, errTooMany()
	}
	if err := q.decode(&req); err != nil {
		return req, matchmaking.Session{}, err
	}
	sess, ok := s.mm.Session(req.GameSessionID)
	if !ok || sess.Managed {
		return req, matchmaking.Session{}, errBadRequest("unknown game session")
	}
	if _, member := sess.Member(q.playerID()); !member {
		return req, matchmaking.Session{}, errBadRequest("not a member of the game session")
	}
	return req, sess, nil
}

// errTooMany answers a caller over p2p.requests_per_minute. The loader logs it and retries on its next poll.
func errTooMany() error {
	return &apiError{http.StatusTooManyRequests, codeUnknown, "too many requests"}
}

func memberSet(sess matchmaking.Session) func(int64) bool {
	ids := map[int64]bool{}
	for _, m := range sess.Members {
		ids[m.ID] = true
	}
	return func(id int64) bool { return ids[id] }
}

func peerJSON(p p2p.Peer) map[string]any {
	return map[string]any{"peer_id": p.ID, "key": p.Key, "candidates": p.Candidates, "mapping": p.Mapping}
}

func hostAnswer(peers []p2p.Peer) map[string]any {
	out := make([]any, 0, len(peers))
	for _, p := range peers {
		out = append(out, peerJSON(p))
	}
	return map[string]any{"role": "host", "peers": out, "poll_ms": p2pPollMs}
}

func (s *Server) joinerAnswer(p p2p.Peer, host *p2p.Endpoint) map[string]any {
	var h any // null until the host registers
	if host != nil {
		h = host
	}
	return map[string]any{"role": "joiner", "peer_id": p.ID, "key": p.Key, "host": h, "relay": s.relayCredentials(p.PlayerID),
		"poll_ms": p2pPollMs}
}

// SetRelay attaches the TURN relay (nil = none). Joiners then get credentials for it.
func (s *Server) SetRelay(r *turnrelay.Relay) { s.relay = r }

// relayCredentials are a joiner's TURN credentials, or nil without a relay. The joiner allocates a relay address
// and registers it as a "relay" candidate; the host just sends to it.
func (s *Server) relayCredentials(playerID int64) any {
	if s.relay == nil {
		return nil
	}
	user, pass, expires := s.relay.Credentials(playerID, time.Duration(s.cfg.P2P.TurnCredentialMinutes)*time.Minute)
	return map[string]any{"host": s.stunHost(), "port": s.cfg.P2P.TurnPort, "realm": s.relay.Realm(), "username": user,
		"password": pass, "expires": expires.Unix()}
}

func candidateTypes(cs []p2p.Candidate) string {
	types := make([]string, 0, len(cs))
	for _, c := range cs {
		types = append(types, c.Type)
	}
	return strings.Join(types, ",")
}

// p2pHost is POST p2p/host/ {game_session_id, candidates, mapping}: the session's host registers its tunnel
// socket's candidates. The answer lists the joiners so far, each with its peer id, key and candidates.
func (s *Server) p2pHost(q *request) (any, error) {
	req, sess, err := s.p2pSession(q)
	if err != nil {
		return nil, err
	}
	if sess.HostID != q.playerID() {
		return nil, errBadRequest("only the session's host registers as host")
	}
	if err := req.Endpoint.Validate(); err != nil {
		return nil, errBadRequest(err.Error())
	}
	peers := s.p2p.SetHost(sess.ID, sess.HostID, req.Endpoint, memberSet(sess))
	s.p2pLog().Info("host registered", "session", s.logs.Tag(sess.ID), "host", s.logs.TagID(sess.HostID),
		"candidates", candidateTypes(req.Candidates), "mapping", req.Mapping, "joiners", len(peers))
	return hostAnswer(peers), nil
}

// p2pJoin is POST p2p/join/ {game_session_id, candidates, mapping}: a joiner registers. It gets its peer id and key
// (the same on every call) and the host's candidates, or "host": null until the host has registered.
func (s *Server) p2pJoin(q *request) (any, error) {
	req, sess, err := s.p2pSession(q)
	if err != nil {
		return nil, err
	}
	if sess.HostID == q.playerID() {
		return nil, errBadRequest("the host registers with p2p/host/")
	}
	if err := req.Endpoint.Validate(); err != nil {
		return nil, errBadRequest(err.Error())
	}
	peer, host, err := s.p2p.SetJoiner(sess.ID, sess.HostID, q.playerID(), req.Endpoint)
	if err != nil {
		return nil, err
	}
	s.p2pLog().Info("joiner registered", "session", s.logs.Tag(sess.ID), "player", s.logs.TagID(q.playerID()),
		"peer", peer.ID, "candidates", candidateTypes(req.Candidates), "mapping", req.Mapping, "host_ready", host != nil)
	return s.joinerAnswer(peer, host), nil
}

// p2pPoll is POST p2p/poll/ {game_session_id}: either side refreshes its registration and gets the other side's
// current state (the host: every joiner; a joiner: the host). Registrations expire unless polled.
func (s *Server) p2pPoll(q *request) (any, error) {
	_, sess, err := s.p2pSession(q)
	if err != nil {
		return nil, err
	}
	if sess.HostID == q.playerID() {
		peers, err := s.p2p.HostPoll(sess.ID, sess.HostID, memberSet(sess))
		if errors.Is(err, p2p.ErrNoHost) {
			return nil, errBadRequest("register with p2p/host/ first")
		}
		return hostAnswer(peers), err
	}
	peer, host, err := s.p2p.JoinerPoll(sess.ID, sess.HostID, q.playerID())
	if errors.Is(err, p2p.ErrNoPeer) {
		return nil, errBadRequest("register with p2p/join/ first")
	}
	if err != nil {
		return nil, err
	}
	return s.joinerAnswer(peer, host), nil
}

// p2pReport is POST p2p/report/ {game_session_id, peer_id, state, path, rtt_ms}: either side of a pair says how
// its connection went. It is logged (the direct / relay statistics come from these lines).
func (s *Server) p2pReport(q *request) (any, error) {
	req, sess, err := s.p2pSession(q)
	if err != nil {
		return nil, err
	}
	if len(req.State) > 32 || len(req.Path) > 32 {
		return nil, errBadRequest("state or path too long")
	}
	joiner, err := s.p2p.SetReport(sess.ID, sess.HostID, q.playerID(), req.PeerID,
		p2p.Report{State: req.State, Path: req.Path, RTTMs: req.RTTMs})
	if err != nil {
		return nil, errBadRequest(err.Error())
	}
	side := "joiner"
	if q.playerID() == sess.HostID {
		side = "host"
	}
	s.p2pLog().Info("connection report", "session", s.logs.Tag(sess.ID), "from", side, "peer", req.PeerID,
		"joiner", s.logs.TagID(joiner), "state", req.State, "path", req.Path, "rtt_ms", req.RTTMs)
	return map[string]any{}, nil
}
