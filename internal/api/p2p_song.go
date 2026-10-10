package api

import (
	"errors"
	"fmt"
	"net/http"

	"fuseronline/internal/matchmaking"
	"fuseronline/internal/p2p"
	"fuseronline/internal/songxfer"
)

// Song transfers between players. A player's loader asks for a custom song another member's
// crate holds; the owners are asked one at a time; an owner that sends offers the file's size and hashes and its
// tunnel candidates; the requester answers with its own; both get a pair id and key for their tunnel agents, which
// punch a path and move the file directly. Any session takes part, player-hosted or public (managed). Nothing here
// touches the file.
//
// Every endpoint needs a login and membership of the session, and counts against p2p.requests_per_minute. A
// transfer missing from a player's poll is over for that player: its loader drops the pair.

// songPollMs is how often the loaders poll while a session runs (owners must see requests within a few seconds).
const songPollMs = 2000

type songRequest struct {
	GameSessionID string  `json:"game_session_id"`
	TransferID    string  `json:"transfer_id"`
	SongPath      string  `json:"song_path"`
	Owners        []int64 `json:"owners"`
	Sending       bool    `json:"sending"`
	Syncing       bool    `json:"syncing"`     // poll: this player is fetching custom songs now
	SyncFailed    int     `json:"sync_failed"` // poll: songs its current (or last) batch couldn't get
	Reason        string  `json:"reason"`
	State         string  `json:"state"`
	// report statistics, logged
	Bytes int64  `json:"bytes"`
	Ms    int64  `json:"ms"`
	Path  string `json:"path"`
	// offer
	Size      int64  `json:"size"`
	SHA256    string `json:"sha256"`
	SigSize   int64  `json:"sig_size"`
	SigSHA256 string `json:"sig_sha256"`
	p2p.Endpoint
}

// songSession decodes the request and finds its session, which the caller must be a member of.
func (s *Server) songSession(q *request) (songRequest, matchmaking.Session, error) {
	var req songRequest
	if !s.cfg.P2P.Enabled || !s.cfg.P2P.SongTransfers {
		return req, matchmaking.Session{}, &apiError{http.StatusNotFound, codeUnknown, "song transfers are disabled"}
	}
	if !s.p2pLimit.allow(fmt.Sprint("player:", q.playerID())) { // the same budget as the p2p/ signaling
		return req, matchmaking.Session{}, errTooMany()
	}
	if err := q.decode(&req); err != nil {
		return req, matchmaking.Session{}, err
	}
	sess, ok := s.mm.Session(req.GameSessionID)
	if !ok {
		return req, matchmaking.Session{}, errBadRequest("unknown game session")
	}
	if _, member := sess.Member(q.playerID()); !member {
		return req, matchmaking.Session{}, errBadRequest("not a member of the game session")
	}
	return req, sess, nil
}

// songError maps the registry's errors to client errors.
func songError(err error) error {
	for _, e := range []error{songxfer.ErrBadPath, songxfer.ErrNoOwner, songxfer.ErrTooMany, songxfer.ErrUnknown,
		songxfer.ErrNotYours, songxfer.ErrWrongStep, songxfer.ErrBadOffer, songxfer.ErrBadInput} {
		if errors.Is(err, e) {
			return errBadRequest(err.Error())
		}
	}
	return err // anything else is ours: logged, and answered as an internal error
}

// songView is a transfer as one of its parties sees it. The pair's id and key, the file and the other side's
// candidates appear once the current owner has offered; the owner sees the requester's candidates once it accepted.
func songView(t songxfer.Transfer, viewer int64) map[string]any {
	role := "requester"
	if viewer != t.Requester {
		role = "owner"
	}
	v := map[string]any{"transfer_id": t.ID, "song_path": t.Path, "song": t.Song, "state": t.State, "reason": t.Reason,
		"role": role, "requester": t.Requester, "owner": t.Owner(), "attempt": len(t.Outcomes) + 1,
		"pair_id": nil, "key": nil, "file": nil, "peer": nil, "relay": nil} // relay: reserved
	if t.Offer != nil && !t.Final() {
		v["pair_id"], v["key"] = t.PairID, t.Key
		v["file"] = map[string]any{"size": t.Offer.Size, "sha256": t.Offer.SHA256, "sig_size": t.Offer.SigSize,
			"sig_sha256": t.Offer.SigSHA256}
		if role == "requester" {
			v["peer"] = t.Offer.Endpoint
		} else if t.Want != nil {
			v["peer"] = *t.Want
		}
	}
	return v
}

// logSongEvents writes the registry's owner changes, failures and cancellations.
func (s *Server) logSongEvents() {
	lg := s.p2pLog()
	for _, e := range s.songs.TakeEvents() {
		lg.Info("song transfer "+e.What, "transfer", e.Transfer.ID, "session", s.logs.Tag(e.Transfer.SessionID), "song", e.Transfer.Song,
			"requester", s.logs.TagID(e.Transfer.Requester), "from_owner", s.logs.TagID(e.Owner), "to_owner", s.logs.TagID(e.Transfer.Owner()),
			"reason", e.Reason)
	}
}

// songRequestHandler is POST p2p/song/request/ {game_session_id, song_path, owners}: ask for a song the owners'
// crates hold. The answer is the transfer (an open one for the same song is returned as is).
func (s *Server) songRequestHandler(q *request) (any, error) {
	req, sess, err := s.songSession(q)
	if err != nil {
		return nil, err
	}
	t, err := s.songs.Request(sess.ID, q.playerID(), req.SongPath, req.Owners, memberSet(sess))
	if err != nil {
		return nil, songError(err)
	}
	s.p2pLog().Info("song requested", "transfer", t.ID, "session", s.logs.Tag(sess.ID), "song", t.Song,
		"requester", s.logs.TagID(t.Requester), "owners", len(t.Owners), "first_owner", s.logs.TagID(t.Owner()))
	return map[string]any{"transfer": songView(t, q.playerID())}, nil
}

// songPoll is POST p2p/song/poll/ {game_session_id, sending}: the player's transfers, as requester or as the owner
// asked now. sending=false declines every request waiting on this player.
func (s *Server) songPoll(q *request) (any, error) {
	req, sess, err := s.songSession(q)
	if err != nil {
		return nil, err
	}
	list := s.songs.Poll(sess.ID, q.playerID(), req.Sending, memberSet(sess))
	s.logSongEvents()
	views := make([]any, 0, len(list))
	for _, t := range list {
		views = append(views, songView(t, q.playerID()))
	}
	// The session's sync round: every loader shows "all players synchronised" once a round finishes.
	sync, change := s.songs.Sync(sess.ID, q.playerID(), req.Syncing, req.SyncFailed, memberSet(sess))
	switch change {
	case songxfer.SyncStarted:
		s.p2pLog().Info("song sync round started", "session", s.logs.Tag(sess.ID), "round", sync.Round, "by", s.logs.TagID(q.playerID()))
	case songxfer.SyncFinished:
		s.p2pLog().Info("song sync round finished", "session", s.logs.Tag(sess.ID), "round", sync.Done, "players", sync.Players, "failed", sync.Failed)
	}
	return map[string]any{"transfers": views, "poll_ms": songPollMs, "sync": sync}, nil
}

// songOffer is POST p2p/song/offer/ {game_session_id, transfer_id, size, sha256, sig_size, sig_sha256, candidates,
// mapping}: the current owner will send.
func (s *Server) songOffer(q *request) (any, error) {
	req, sess, err := s.songSession(q)
	if err != nil {
		return nil, err
	}
	t, err := s.songs.Offer(sess.ID, req.TransferID, q.playerID(), songxfer.Offer{Size: req.Size, SHA256: req.SHA256,
		SigSize: req.SigSize, SigSHA256: req.SigSHA256, Endpoint: req.Endpoint})
	if err != nil {
		return nil, songError(err)
	}
	s.p2pLog().Info("song offered", "transfer", t.ID, "song", t.Song, "owner", s.logs.TagID(q.playerID()), "bytes", req.Size,
		"candidates", candidateTypes(req.Candidates), "mapping", req.Mapping)
	return map[string]any{"transfer": songView(t, q.playerID())}, nil
}

// songDecline is POST p2p/song/decline/ {game_session_id, transfer_id, reason}: the current owner won't send
// (not_found, not_custom, disabled, busy); the next owner is asked.
func (s *Server) songDecline(q *request) (any, error) {
	req, sess, err := s.songSession(q)
	if err != nil {
		return nil, err
	}
	if len(req.Reason) > 32 {
		return nil, errBadRequest("reason too long")
	}
	t, err := s.songs.Decline(sess.ID, req.TransferID, q.playerID(), req.Reason, memberSet(sess))
	if err != nil {
		return nil, songError(err)
	}
	s.logSongEvents()
	return map[string]any{"transfer": songView(t, q.playerID())}, nil
}

// songAccept is POST p2p/song/accept/ {game_session_id, transfer_id, candidates, mapping}: the requester's
// candidates for the offered pair (again later, if its network changed).
func (s *Server) songAccept(q *request) (any, error) {
	req, sess, err := s.songSession(q)
	if err != nil {
		return nil, err
	}
	t, err := s.songs.Accept(sess.ID, req.TransferID, q.playerID(), req.Endpoint)
	if err != nil {
		return nil, songError(err)
	}
	s.p2pLog().Info("song accepted", "transfer", t.ID, "song", t.Song, "requester", s.logs.TagID(q.playerID()),
		"candidates", candidateTypes(req.Candidates), "mapping", req.Mapping)
	return map[string]any{"transfer": songView(t, q.playerID())}, nil
}

// songReport is POST p2p/song/report/ {game_session_id, transfer_id, state, reason, bytes, ms, path}: how the
// current attempt is going. See songxfer.Report for the states.
func (s *Server) songReport(q *request) (any, error) {
	req, sess, err := s.songSession(q)
	if err != nil {
		return nil, err
	}
	if len(req.Path) > 32 {
		return nil, errBadRequest("path too long")
	}
	t, err := s.songs.Report(sess.ID, req.TransferID, q.playerID(), req.State, req.Reason, memberSet(sess))
	if err != nil {
		return nil, songError(err)
	}
	side := "requester"
	if q.playerID() != t.Requester {
		side = "owner"
	}
	s.p2pLog().Info("song report", "transfer", t.ID, "song", t.Song, "from", side, "state", req.State, "reason", req.Reason,
		"bytes", req.Bytes, "ms", req.Ms, "path", req.Path, "now", t.State)
	s.logSongEvents()
	return map[string]any{"transfer": songView(t, q.playerID())}, nil
}
