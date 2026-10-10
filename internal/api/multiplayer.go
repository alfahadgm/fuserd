package api

// Handlers for the matchmaking endpoints. internal/matchmaking holds the sessions.

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"fuseronline/internal/logx"
	"fuseronline/internal/matchmaking"
	"fuseronline/internal/publichost"
	"fuseronline/internal/store"
)

// FMatchmakingPlayer
type mmPlayer struct {
	PlayerID            int64       `json:"player_id"`
	Platform            string      `json:"platform"`
	IsCreator           bool        `json:"is_creator"`
	ForceCreateNewMatch int         `json:"force_create_new_match"`
	VenueSelect         string      `json:"venue_select"`
	VenueTimeOfDay      string      `json:"venue_time_of_day"`
	IsDJ                bool        `json:"is_dj"`
	TurnLength          flexStrings `json:"turn_length"` // TArray<FString> on the client, e.g. ["128"]
	NumRounds           flexStrings `json:"num_rounds"`
}

// flexStrings is a JSON array of strings that also accepts numbers. The client sends turn_length / num_rounds as
// strings; a number there must not fail the whole ticket.
type flexStrings []string

func (f *flexStrings) UnmarshalJSON(b []byte) error {
	var raw []json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	out := make(flexStrings, 0, len(raw))
	for _, r := range raw {
		var s string
		if err := json.Unmarshal(r, &s); err != nil {
			var n json.Number
			if err := json.Unmarshal(r, &n); err != nil {
				return fmt.Errorf("array element %s is neither a string nor a number", r)
			}
			s = n.String()
		}
		out = append(out, s)
	}
	*f = out
	return nil
}

// FMultiplayerMatchmakingMatchedPlayerSessions
type matchedPlayerSession struct {
	PlayerID        string `json:"player_id"`
	PlayerSessionID string `json:"player_session_id"`
}

// FMultiplayerMatchmakingGameSessionConnectionInfo
type connectionInfo struct {
	GameSessionID         string                 `json:"game_session_id"`
	IPAddress             string                 `json:"ip_address"`
	Port                  string                 `json:"port"`
	MatchedPlayerSessions []matchedPlayerSession `json:"matched_player_sessions"`
}

// FMultiplayerMatchmakingTicketResponse
type ticketResponse struct {
	TicketID       string         `json:"ticket_id"`
	Status         string         `json:"status"`
	Players        []mmPlayer     `json:"players"`
	ConnectionInfo connectionInfo `json:"game_session_connection_info"`
}

// FGameSessionResponse
type gameSessionResponse struct {
	GameSessionID  string `json:"game_session_id"`
	Status         string `json:"status"`
	IPAddress      string `json:"ip_address"`
	Port           string `json:"port"`
	MatchmakerData string `json:"matchmaker_data"`
}

// sessionStatusActive is the only game session status the client acts on (the session browser skips others).
const sessionStatusActive = "ACTIVE"

func (s *Server) mmLog() *slog.Logger { return s.logs.For(logx.Multiplayer) }

func (s *Server) logEnded(ended []matchmaking.Ended) {
	for _, e := range ended {
		if s.public != nil && e.Session.Managed && e.Reason == "last player left" {
			s.public.Drain(e.Session.SlotID)
		}
		s.p2p.Drop(e.Session.ID)
		s.songs.Drop(e.Session.ID)
		s.logSongEvents()
		s.mmLog().Info("session ended", "session", s.logs.Tag(e.Session.ID), "host", s.logs.TagID(e.Session.HostID),
			"players", len(e.Session.Members), "reason", e.Reason)
	}
}

// regionsHTTP is FMultiplayerRegionsHttp. The client GETs each endpoint_http to measure latency and retries a
// region until that request completes, so the endpoint must always answer: it is our own nop/.
func (s *Server) regionsHTTP(*request) (any, error) {
	return map[string]any{"gamelift_regions_http": []map[string]string{{
		"region":        s.cfg.Multiplayer.Region,
		"endpoint_http": s.cfg.PublicURL + Prefix + "nop/?from=latency",
	}}}, nil
}

// multiplayerLimits is our own multiplayer/limits/ (not a game endpoint): the limits the loader enforces before the
// game goes online. Its crate check runs before the game logs in, so a login is optional. A logged-in loader also
// reports itself here ({"loader": version, "build": exe build}): the stock / modded split (loader.go).
// max_crate_songs: multiplayer.max_crate_songs, 0 = no limit.
func (s *Server) multiplayerLimits(q *request) (any, error) {
	if err := s.recordLoader(q); err != nil {
		return nil, err
	}
	return map[string]any{"max_crate_songs": s.cfg.Multiplayer.MaxCrateSongs}, nil
}

// createTicket is FMultiplayerMatchmakingCreateTicketRequest. Private tickets get an FDS container of their own;
// Docker-backed public tickets remain PLACING until their server is ready. Without public servers, a public ticket
// is hosted by the creating player.
func (s *Server) createTicket(q *request) (any, error) {
	var req struct {
		Queue      string     `json:"queue"`
		Players    []mmPlayer `json:"players"`
		Changelist int        `json:"changelist"`
	}
	if err := q.decode(&req); err != nil {
		return nil, err
	}
	self := mmPlayer{PlayerID: q.playerID()}
	selfMatched := false
	for _, p := range req.Players {
		if p.PlayerID == q.playerID() {
			self = p
			selfMatched = true
		}
	}
	createNew := self.IsCreator || self.ForceCreateNewMatch != 0
	if req.Queue == matchmaking.QueueFreestylePublic && s.public != nil {
		if !selfMatched {
			s.mmLog().Warn("public ticket has no player matching authenticated user", "player", s.logs.TagID(q.playerID()),
				"players", len(req.Players))
			return nil, errBadRequest("public ticket has no matching player")
		}
		return s.createPublicTicket(q, self, createNew)
	}
	if req.Queue == matchmaking.QueueFreestylePrivate {
		// Private sessions run only on FDS containers; players never host them.
		if !s.fdsEnabled() {
			s.mmLog().Warn("ticket refused", "player", s.logs.TagID(q.playerID()), "queue", req.Queue,
				"reason", "private sessions need FDS servers (multiplayer.public_servers)")
			return nil, &apiError{http.StatusServiceUnavailable, codeUnknown, "private sessions are not available on this server"}
		}
		return s.createPrivateFDSTicket(q, self)
	}
	avoid, err := s.keepApart(q.Context(), q.playerID())
	if err != nil {
		return nil, err
	}
	t, sess, ended, err := s.mm.CreateTicketAvoiding(req.Queue, toPlayer(q.playerID(), self), createNew, s.hostAddress(q), avoid)
	s.logEnded(ended)
	if errors.Is(err, matchmaking.ErrNoPublicSession) {
		s.mmLog().Info("watch refused: no public show running", "player", s.logs.TagID(q.playerID()),
			"ticket", s.logs.Tag(t.ID), "status", t.Status)
		return s.ticketResult(t, matchmaking.Session{}), nil
	}
	if errors.Is(err, matchmaking.ErrUnsupportedQueue) {
		s.mmLog().Warn("ticket refused", "player", s.logs.TagID(q.playerID()), "queue", req.Queue,
			"reason", "only freestyle queues are supported")
		return nil, errBadRequest("queue not supported by this server")
	}
	if err != nil {
		return nil, err
	}
	role := "joined"
	if sess.HostID == q.playerID() {
		role = "host"
		if strings.Contains(sess.Addr, ":") {
			s.mmLog().Warn("host address is IPv6; the client cannot join <ip>:<port> with it",
				"session", s.logs.Tag(sess.ID))
		}
		s.mmLog().Info("session created", "session", s.logs.Tag(sess.ID), "host", s.logs.TagID(sess.HostID),
			"queue", sess.Queue, "venue", self.VenueSelect, "time_of_day", self.VenueTimeOfDay,
			"addr", s.logs.Tag(sess.Addr), "port", sess.Port, "changelist", req.Changelist)
	}
	s.mmLog().Info("ticket", "player", s.logs.TagID(q.playerID()), "ticket", s.logs.Tag(t.ID), "queue", req.Queue,
		"create_new", createNew, "role", role, "session", s.logs.Tag(sess.ID), "players", len(sess.Members))
	return s.ticketResult(t, sess), nil
}

func acceptedInts(values []string, fallback int) []int {
	out := []int{}
	for _, value := range values {
		n, err := strconv.Atoi(value)
		if err != nil || n <= 0 {
			continue
		}
		seen := false
		for _, old := range out {
			if old == n {
				seen = true
				break
			}
		}
		if !seen {
			out = append(out, n)
		}
	}
	if len(out) == 0 {
		return []int{fallback}
	}
	return out
}

func publicSetKeys(self mmPlayer) ([]publichost.Key, []int, []int) {
	var keys []publichost.Key
	roundsOptions := acceptedInts(self.NumRounds, 3)
	measureOptions := acceptedInts(self.TurnLength, 128)
	// turn_length only seeds a new server's turn length (FDS_TURN_MEASURES), its fallback. A DJ's public ticket
	// pairs it with the round choice (the client reads _publicCoopRoundNumIndex for both: 1 round + 64 bars arrives
	// as 1/32). FDS leaves the session's set length unset (-1), so the first player's client sends
	// ServerSetCoopRoundNumTurnMeasures; a public server keeps the ticket's round count from it and takes only the
	// turn length (the fallback applies if no choice arrives in time). Slots are matched on rounds only
	// (publichost.ReserveAny). Only a viewer ticket lists several values; the longest is preferred.
	sort.Sort(sort.Reverse(sort.IntSlice(measureOptions)))
	for _, rounds := range roundsOptions {
		for _, measures := range measureOptions {
			keys = append(keys, publichost.Key{Rounds: rounds, Measures: measures})
		}
	}
	return keys, roundsOptions, measureOptions
}

// isViewer is a "Watch a Public Show" ticket: public, not a DJ (FMatchmakingPlayer.is_dj, FUN_140717e90) and not
// creating a match. A viewer only joins a running show.
func isViewer(self mmPlayer, createNew bool) bool { return !self.IsDJ && !createNew }

func (s *Server) createPublicTicket(q *request, self mmPlayer, createNew bool) (any, error) {
	keys, roundsOptions, measureOptions := publicSetKeys(self)
	viewer := isViewer(self, createNew)
	s.mmLog().Info("public set options", "player", s.logs.TagID(q.playerID()),
		"round_options", roundsOptions, "measure_options", measureOptions,
		"raw_rounds", []string(self.NumRounds), "raw_turn_length", []string(self.TurnLength),
		"is_dj", self.IsDJ, "create_new", createNew, "viewer", viewer)
	// Never a slot holding someone this player blocked or was blocked by (blocks.go), or a player on the other side of
	// the stock / modded split (loader.go): a new server instead.
	avoid, err := s.keepApart(q.Context(), q.playerID())
	if err != nil {
		return nil, err
	}
	var skip func(string) bool
	if avoid != nil {
		skip = func(slotID string) bool { return anyBlocked(avoid, s.mm.SlotOccupants(slotID)) }
	}
	var slot *publichost.Slot
	if viewer {
		// Watching never starts a server: with no show running the ticket times out at once and the client shows
		// its matchmaking error, instead of the viewer waiting in an empty server.
		if slot, err = s.public.ReserveExistingExcept(skip); errors.Is(err, publichost.ErrCapacity) {
			t, ended := s.mm.CreateUnmatchedTicket(matchmaking.QueueFreestylePublic, toPlayer(q.playerID(), self))
			s.logEnded(ended)
			s.mmLog().Info("watch refused: no public show running", "player", s.logs.TagID(q.playerID()),
				"ticket", s.logs.Tag(t.ID), "status", t.Status)
			return s.ticketResult(t, matchmaking.Session{}), nil
		}
	} else {
		// A new server is started for the creator's venue and time of day (an FDS container loads them).
		// "Join a public session": a server with a free performer slot, else a new one (max_djs).
		slot, err = s.public.ReserveAnyForExcept(keys, self.VenueSelect, self.VenueTimeOfDay, skip, true)
	}
	if errors.Is(err, publichost.ErrCapacity) || errors.Is(err, publichost.ErrPaused) {
		if errors.Is(err, publichost.ErrPaused) {
			s.mmLog().Warn("public ticket refused: server launches paused", "player", s.logs.TagID(q.playerID()))
		}
		return nil, &apiError{http.StatusServiceUnavailable, codeUnknown, "no public server available"}
	}
	if err != nil {
		return nil, err
	}
	// The session's venue_time_of_day (matchmaker_data) is the server's, so it matches the sky everyone sees.
	creator := toPlayer(q.playerID(), self)
	if slot.TimeOfDay != "" {
		creator.TimeOfDay = slot.TimeOfDay
	}
	// The container loaded the venue of the ticket that started it; the session (whichever ticket completes
	// first) must announce that one, so every player travels to the server's world.
	if slot.Venue != "" {
		creator.Venue = slot.Venue
	}
	// The ticket's reservation took a performer slot unless it is a viewer's (ReserveAnyForExcept with dj).
	t, ended := s.mm.CreateManagedPendingAs(creator, slot.ID, matchmaking.QueueFreestylePublic, !viewer)
	s.logEnded(ended)
	s.placeWhenReady(t, slot)
	s.mmLog().Info("public ticket placing", "ticket", s.logs.Tag(t.ID), "slot", slot.ID,
		"rounds", slot.Key.Rounds, "measures", slot.Key.Measures, "viewer", viewer, "slot_time_of_day", slot.TimeOfDay,
		"group", groupName(s.modded(q.playerID())))
	return s.ticketResult(t, matchmaking.Session{}), nil
}

// placeWhenReady completes a PLACING ticket once its server is ready (the container registered), or fails it if the
// server never comes up.
func (s *Server) placeWhenReady(t matchmaking.Ticket, slot *publichost.Slot) {
	go func() {
		<-slot.Ready()
		ready, startErr := s.public.State(slot.ID)
		if !ready {
			s.mm.FailManaged(slot.ID)
			s.public.Release(slot.ID, t.Performer)
			s.mmLog().Warn("managed match placement failed", "slot", slot.ID, "queue", t.Queue, "err", startErr)
		} else if _, full := s.mm.PlaceManaged(t.ID, slot.ID, slot.Address, slot.Port, s.cfg.Multiplayer.PublicServers.MaxPlayers); full {
			// Only a ticket that failed here gives its reservation back: a cancelled one already did (cancelTicket), and
			// releasing it again would free another ticket's place.
			s.public.Release(slot.ID, t.Performer)
		}
	}()
}

// createPrivateFDSTicket gives a private ticket an FDS container of its own: the creator joins it like any server
// and invitees join through player_sessions/. The container reports itself private (lobby, session browser) and
// never takes public tickets or viewers.
func (s *Server) createPrivateFDSTicket(q *request, self mmPlayer) (any, error) {
	keys, _, _ := publicSetKeys(self)
	slot, err := s.public.ReservePrivate(keys[0], self.VenueSelect, self.VenueTimeOfDay)
	if errors.Is(err, publichost.ErrCapacity) || errors.Is(err, publichost.ErrPaused) {
		return nil, &apiError{http.StatusServiceUnavailable, codeUnknown, "no server available"}
	}
	if err != nil {
		return nil, err
	}
	creator := toPlayer(q.playerID(), self)
	if slot.TimeOfDay != "" {
		creator.TimeOfDay = slot.TimeOfDay
	}
	if slot.Venue != "" {
		creator.Venue = slot.Venue
	}
	t, ended := s.mm.CreateManagedPendingAs(creator, slot.ID, matchmaking.QueueFreestylePrivate, true) // ReservePrivate: a DJ
	s.logEnded(ended)
	s.placeWhenReady(t, slot)
	s.mmLog().Info("private ticket placing", "ticket", s.logs.Tag(t.ID), "slot", slot.ID, "rounds", slot.Key.Rounds,
		"measures", slot.Key.Measures, "venue", slot.Venue, "time_of_day", slot.TimeOfDay)
	return s.ticketResult(t, matchmaking.Session{}), nil
}

func toPlayer(id int64, p mmPlayer) matchmaking.Player {
	return matchmaking.Player{ID: id, Platform: p.Platform, Venue: p.VenueSelect, TimeOfDay: p.VenueTimeOfDay,
		IsDJ: p.IsDJ, TurnLength: p.TurnLength, NumRounds: p.NumRounds}
}

func (s *Server) ticketResult(t matchmaking.Ticket, sess matchmaking.Session) ticketResponse {
	r := ticketResponse{TicketID: t.ID, Status: t.Status, Players: []mmPlayer{}}
	r.ConnectionInfo.MatchedPlayerSessions = []matchedPlayerSession{}
	if sess.ID == "" {
		if t.Status == matchmaking.StatusCompleted {
			// The session ended between creation and this poll: report the ticket as failed, not as a match.
			r.Status = "FAILED"
		}
		return r
	}
	r.ConnectionInfo.GameSessionID = sess.ID
	r.ConnectionInfo.IPAddress = sess.Addr
	r.ConnectionInfo.Port = strconv.Itoa(sess.Port)
	// The client picks the entry whose player_id is its own user id printed with %d.
	r.ConnectionInfo.MatchedPlayerSessions = append(r.ConnectionInfo.MatchedPlayerSessions, matchedPlayerSession{
		PlayerID: strconv.FormatInt(t.PlayerID, 10), PlayerSessionID: t.PlayerSessionID})
	return r
}

// getTicket is FMultiplayerMatchmakingTicketRequest, polled until COMPLETED.
func (s *Server) getTicket(q *request) (any, error) {
	var req struct {
		TicketID string `json:"ticket_id"`
	}
	if err := q.decode(&req); err != nil {
		return nil, err
	}
	t, sess, ok := s.mm.Ticket(req.TicketID, q.playerID())
	if !ok {
		return nil, errBadRequest("unknown ticket")
	}
	if s.public != nil && t.Status == matchmaking.StatusPlacing && t.SlotID != "" {
		// The player is still waiting for its server: its place there stays reserved.
		s.public.Refresh(t.SlotID, t.Performer)
	}
	return s.ticketResult(t, sess), nil
}

// cancelTicket is FMultiplayerMatchmakingCancelTicketResponse { Status }.
func (s *Server) cancelTicket(q *request) (any, error) {
	var req struct {
		TicketID string `json:"ticket_id"`
	}
	if err := q.decode(&req); err != nil {
		return nil, err
	}
	prior, _, found := s.mm.Ticket(req.TicketID, q.playerID())
	ended, err := s.mm.CancelTicket(req.TicketID, q.playerID())
	if s.public != nil && found && prior.Status == matchmaking.StatusPlacing {
		// A cancelled pending ticket no longer needs its reserved connection slot.
		s.public.Release(prior.SlotID, prior.Performer)
	}
	s.logEnded(ended)
	if err != nil {
		return nil, errBadRequest("unknown ticket")
	}
	s.mmLog().Info("ticket cancelled", "player", s.logs.TagID(q.playerID()), "ticket", s.logs.Tag(req.TicketID))
	return map[string]string{"status": matchmaking.StatusCancelled}, nil
}

// acceptMatch would answer REQUIRES_ACCEPTANCE, which this server never sends.
func (s *Server) acceptMatch(q *request) (any, error) {
	s.mmLog().Warn("unexpected accept_match", "player", s.logs.TagID(q.playerID()))
	return nil, nil
}

// getGameSession is FGameSessionGetRequest to FGameSessionResponse. The client parses matchmaker_data for the
// venue; a missing session gives it FailedToGetGameSession.
func (s *Server) getGameSession(q *request) (any, error) {
	var req struct {
		GameSessionID string `json:"game_session_id"`
	}
	if err := q.decode(&req); err != nil {
		return nil, err
	}
	sess, ok := s.mm.Session(req.GameSessionID)
	if !ok {
		s.mmLog().Info("game session not found", "player", s.logs.TagID(q.playerID()),
			"session", s.logs.Tag(req.GameSessionID))
		return nil, errBadRequest("unknown game session")
	}
	return s.gameSessionResult(sess), nil
}

func (s *Server) gameSessionResult(sess matchmaking.Session) gameSessionResponse {
	return gameSessionResponse{
		GameSessionID:  sess.ID,
		Status:         sessionStatusActive,
		IPAddress:      sess.Addr,
		Port:           strconv.Itoa(sess.Port),
		MatchmakerData: matchmakerData(sess),
	}
}

// matchmakerData renders GameLift's matchmaker data JSON, which the client parses in FUN_14070ff00. Every
// attribute is {"valueAttribute": v}. The client takes the venue from the player marked is_creator.
func matchmakerData(sess matchmaking.Session) string {
	type attr struct {
		Type  string `json:"attributeType"`
		Value any    `json:"valueAttribute"`
	}
	str := func(v string) attr { return attr{"STRING", v} }
	num := func(v int) attr { return attr{"DOUBLE", v} }
	type player struct {
		PlayerID   string          `json:"playerId"`
		Attributes map[string]attr `json:"attributes"`
	}
	type team struct {
		Name    string   `json:"name"`
		Players []player `json:"players"`
	}
	djs := team{Name: "djs", Players: []player{}}
	spectators := team{Name: "spectators", Players: []player{}}
	for i, m := range sess.Members {
		creator := 0
		if i == 0 {
			creator = 1
		}
		id := strconv.FormatInt(m.ID, 10)
		venue, tod := m.Venue, m.TimeOfDay
		if sess.Managed {
			venue, tod = sess.Venue, sess.TimeOfDay
		} else if i != 0 {
			// Joiners from the session browser send no ticket; give them the session's venue.
			venue, tod = sess.Host().Venue, sess.Host().TimeOfDay
		}
		p := player{PlayerID: id, Attributes: map[string]attr{
			"hype_user_id":                str(id),
			"is_creator":                  num(creator),
			"venue_select":                str(venue),
			"venue_time_of_day":           str(tod),
			"platform":                    str(m.Platform),
			"crossplay_required_platform": num(0),
			"is_ranked":                   num(0),
			"matchmaking_rating":          num(0),
		}}
		if m.IsDJ || i == 0 {
			djs.Players = append(djs.Players, p)
		} else {
			spectators.Players = append(spectators.Players, p)
		}
	}
	b, _ := json.Marshal(map[string]any{
		"matchId":                     sess.ID,
		"matchmakingConfigurationArn": "arn:aws:gamelift:fuserd::matchmakingconfiguration/" + sess.Queue,
		"teams":                       []team{djs, spectators},
	})
	return string(b)
}

// createPlayerSession is FPlayerSessionRequest to FPlayerSessionResponse: joining a session picked in the
// session browser.
// hostDevSessionPrefix starts the game session id of our hosts' game server (UHypeMultiplayerDevelopmentServer
// init, slot 74). The stock server's id was the backend's GameLift session id.
const hostDevSessionPrefix = "local-dev-gamesession-"

// resolveBrowserSession maps the id a session-browser join sends to a fuserd session. The browser row joins with
// the host's own metadata (WBP_Multiplayer_Browse_Session: JoinMatchmakingSessionByGameSessionId(
// GameSession.GameSessionId), the FGameSessionMetadata the host sent over the beacon, FUN_140708b10), and our
// hosts report their dev server's id, not fuserd's. So: fuserd's own id as is; otherwise, for a host id, the only
// session this player can see (the client lists only sessions fuserd listed, so a single visible one is the clicked
// one). The client then uses the game_session_id this returns for game_sessions/get/ and the connect URL
// (FUN_14071c890).
func (s *Server) resolveBrowserSession(q *request, id string) (string, string, error) {
	if _, ok := s.mm.Session(id); ok || !strings.HasPrefix(id, hostDevSessionPrefix) {
		return id, "", nil
	}
	sessions, ended := s.mm.List()
	s.logEnded(ended)
	avoid, err := s.keepApart(q.Context(), q.playerID())
	if err != nil {
		return "", "", err
	}
	match := ""
	for _, sess := range sessions {
		if sess.Managed && s.public != nil && !s.public.Available(sess.SlotID) {
			continue
		}
		if s.blockedFrom(q.playerID(), sess.ID, avoid) {
			continue
		}
		source, err := s.sessionSource(q.Context(), q.playerID(), sess)
		if err != nil {
			return "", "", err
		}
		if source == "" {
			continue
		}
		if match != "" {
			return id, "", nil // ambiguous: leave it unknown
		}
		match = sess.ID
	}
	if match == "" {
		return id, "", nil
	}
	return match, "only visible session", nil
}

func (s *Server) createPlayerSession(q *request) (any, error) {
	var req struct {
		GameSessionID string `json:"game_session_id"`
	}
	if err := q.decode(&req); err != nil {
		return nil, err
	}
	resolved, how, err := s.resolveBrowserSession(q, req.GameSessionID)
	if err != nil {
		return nil, err
	}
	if how != "" {
		s.mmLog().Info("browser join: host session id resolved", "player", s.logs.TagID(q.playerID()),
			"host_session", s.logs.Tag(req.GameSessionID), "session", s.logs.Tag(resolved), "via", how)
		req.GameSessionID = resolved
	}
	avoid, err := s.keepApart(q.Context(), q.playerID())
	if err != nil {
		return nil, err
	}
	if s.blockedFrom(q.playerID(), req.GameSessionID, avoid) {
		s.mmLog().Info("join refused", "player", s.logs.TagID(q.playerID()), "session", s.logs.Tag(req.GameSessionID),
			"reason", s.apartReason(q.Context(), q.playerID(), req.GameSessionID))
		return nil, &apiError{http.StatusServiceUnavailable, codeUnknown, "session unavailable"}
	}
	var managedSlot string
	if current, ok := s.mm.Session(req.GameSessionID); ok && current.Managed && s.public != nil {
		managedSlot = current.SlotID
		if err := s.public.ReserveSlot(managedSlot); err != nil {
			return nil, &apiError{http.StatusServiceUnavailable, codeUnknown, "public session is full or unavailable"}
		}
	}
	mem, sess, ended, err := s.mm.Join(req.GameSessionID, matchmaking.Player{ID: q.playerID()})
	if managedSlot != "" && err != nil {
		s.public.Release(managedSlot, false)
	}
	s.logEnded(ended)
	if err != nil {
		s.mmLog().Info("join refused", "player", s.logs.TagID(q.playerID()), "session", s.logs.Tag(req.GameSessionID),
			"reason", err)
		return nil, errBadRequest("unknown game session")
	}
	s.mmLog().Info("player session", "player", s.logs.TagID(q.playerID()), "session", s.logs.Tag(sess.ID),
		"host", s.logs.TagID(sess.HostID), "players", len(sess.Members),
		"is_host", strings.HasPrefix(mem.PlayerSessionID, matchmaking.HostPrefix))
	return map[string]string{
		"player_session_id": mem.PlayerSessionID,
		"player_id":         strconv.FormatInt(q.playerID(), 10),
		"game_session_id":   sess.ID,
		"status":            "RESERVED",
		"ip_address":        sess.Addr,
		"port":              strconv.Itoa(sess.Port),
	}, nil
}

// blockedFrom reports whether a player is kept out of a session by one of its occupants: a block either way, or the
// stock / modded split (avoid: keepApart). An open invite to the session overrides it.
func (s *Server) blockedFrom(player int64, sessionID string, avoid func(int64) bool) bool {
	if avoid == nil || s.mm.Invited(player, sessionID) {
		return false
	}
	return anyBlocked(avoid, s.mm.Occupants(sessionID))
}

// listGameSessions is FGameSessionListRequest to FGameSessionListResponse { session_browser_entries[] of
// FGameSessionBrowserResponse }. Each entry's "source" is the viewer's relation to the host (sessionSource); the
// client derives source_val from it. The client then asks each host for its lobby state over a beacon on Port+100
// and only lists the sessions that answer.
func (s *Server) listGameSessions(q *request) (any, error) {
	sessions, ended := s.mm.List()
	s.logEnded(ended)
	avoid, err := s.keepApart(q.Context(), q.playerID())
	if err != nil {
		return nil, err
	}
	entries := make([]map[string]any, 0, len(sessions))
	for _, sess := range sessions {
		if sess.Managed && s.public != nil && !s.public.Available(sess.SlotID) {
			continue
		}
		if s.blockedFrom(q.playerID(), sess.ID, avoid) {
			continue
		}
		source, err := s.sessionSource(q.Context(), q.playerID(), sess)
		if err != nil {
			return nil, err
		}
		if source == "" {
			continue
		}
		user := map[string]any{"user_id": sess.HostID, "online_status": "online"}
		if p, err := s.store.Player(q.Context(), sess.HostID); err == nil {
			if user, err = s.hypeUser(q.Context(), q.playerID(), p); err != nil {
				return nil, err
			}
		} else if !errors.Is(err, store.ErrNotFound) {
			return nil, err
		}
		entries = append(entries, map[string]any{
			"game_session": s.gameSessionResult(sess),
			"user":         user,
			"source":       source,
		})
	}
	return map[string]any{"session_browser_entries": entries}, nil
}

// clientIP is the address a hosting player is reached at: the connection's remote address, or the first
// X-Forwarded-For entry when the config says a trusted proxy sets it.
func (s *Server) clientIP(q *request) string {
	if s.cfg.Multiplayer.TrustForwardedFor {
		if xff := q.Header.Get("X-Forwarded-For"); xff != "" {
			first, _, _ := strings.Cut(xff, ",")
			if ip := net.ParseIP(strings.TrimSpace(first)); ip != nil {
				return ipString(ip)
			}
		}
	}
	host, _, err := net.SplitHostPort(q.RemoteAddr)
	if err != nil {
		host = q.RemoteAddr
	}
	if ip := net.ParseIP(host); ip != nil {
		return ipString(ip)
	}
	return host
}

// hostAddress is where joiners reach a player who hosts: its client address, except that a host on this machine
// (loopback) is given this machine's LAN address, or multiplayer.loopback_host_address.
func (s *Server) hostAddress(q *request) string {
	addr := s.clientIP(q)
	ip := net.ParseIP(addr)
	if ip == nil || !ip.IsLoopback() {
		return addr
	}
	sub := s.cfg.Multiplayer.LoopbackHostAddress
	source := "config"
	if sub == "" {
		sub, source = outboundIPv4(), "detected"
	}
	if sub == "" {
		s.mmLog().Warn("host is on loopback and no LAN address was found; joiners on other machines cannot reach it",
			"player", s.logs.TagID(q.playerID()))
		return addr
	}
	s.mmLog().Info("host on loopback: using this machine's address for the session", "player", s.logs.TagID(q.playerID()),
		"addr", s.logs.Tag(sub), "source", source)
	return sub
}

// outboundIPv4 is the local address of the interface that routes to the internet. The UDP "dial" only picks a
// route; nothing is sent.
func outboundIPv4() string {
	c, err := net.Dial("udp4", "192.0.2.1:9") // TEST-NET-1: never contacted
	if err != nil {
		return ""
	}
	defer c.Close()
	if a, ok := c.LocalAddr().(*net.UDPAddr); ok && a.IP.To4() != nil && !a.IP.IsLoopback() {
		return a.IP.To4().String()
	}
	return ""
}

// ipString prints IPv4 (including IPv4-mapped IPv6) in dotted form. The client joins "<ip>:<port>", so a bare
// IPv6 address would not parse there.
func ipString(ip net.IP) string {
	if v4 := ip.To4(); v4 != nil {
		return v4.String()
	}
	return ip.String()
}
