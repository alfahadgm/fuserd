package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"

	"fuseronline/internal/logx"
	"fuseronline/internal/matchmaking"
	"fuseronline/internal/publichost"
)

// The FuserDedicatedServer registration API. Only FDS containers
// fuserd started call it, each with its slot's registration token as a bearer token:
//   POST /fds/v1/register {"slot_id", "version", "net_version", "port", "beacon_port", "venue", "map",
//                          "time_of_day", "rounds", "turn_measures"} -> {"heartbeat_seconds", "session_id",
//                          "blocked_songs"}
//   POST /fds/v1/status   {"slot_id", "players", "djs", "viewers", "stage", "round", "members", "current_dj", "event",
//                          "recycle_pending"} -> the same reply
//   (the Main Stage server's replies also carry "main_stage": {"server_time_ms", "current", "next"})
//   POST /fds/v1/exit     {"slot_id", "reason"} -> {}
//   POST /fds/v1/mainstage/admit {"slot_id", "user_id", "address"} -> {"admit", "reason"} (Main Stage server only;
//                          mainstage_server.go)
// A register on other ports than the slot's (port, beacon_port = port + 100) is refused and the container stopped;
// FDS reports the ports it bound, so a 0 (not listening) counts as another port. A port left out isn't checked.
// A status's "members" keeps the slot's session in line with who is connected (reconcileMembers); "recycle_pending"
// (past FDS_MAX_UPTIME_HOURS, exiting once empty) stops new players being placed on the slot. Every reply's
// "blocked_songs" are the custom songs whose SpawnDisc actions the server must not relay (blockedSongs).
// An unknown slot and a wrong token both answer 401, so slot ids can't be probed. Tokens are never logged.

// FDSHandler serves only the registration API, for its own loopback listener (multiplayer.public_servers.fds_listen):
// it never shares the public listener.
func (s *Server) FDSHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /fds/v1/register", s.fdsRegister)
	mux.HandleFunc("POST /fds/v1/status", s.fdsStatus)
	mux.HandleFunc("POST /fds/v1/exit", s.fdsExit)
	mux.HandleFunc("POST /fds/v1/mainstage/admit", s.fdsMainStageAdmit)
	return mux
}

type fdsReply struct {
	HeartbeatSeconds int    `json:"heartbeat_seconds"`
	SessionID        string `json:"session_id,omitempty"` // fuserd's game session id, once a ticket completed on the slot
	// BlockedSongs are the lower-case shortnames of the custom songs not every member can play (blockedSongs); always
	// present, so the server replaces its set with each reply.
	BlockedSongs []string `json:"blocked_songs"`
	// MainStage is the schedule the Main Stage server follows; only in its replies.
	MainStage *fdsMainStage `json:"main_stage,omitempty"`
}

// fdsMainStage is the Main Stage schedule as the server needs it: fuserd's clock and the current and next events.
type fdsMainStage struct {
	ServerTimeMs int64          `json:"server_time_ms"`
	Current      *fdsStageEvent `json:"current"`
	Next         *fdsStageEvent `json:"next"`
}

// fdsStageEvent is one event; Headliners are the booked players' ids in slot order, the DJ order.
type fdsStageEvent struct {
	UUID        string  `json:"uuid"`
	Type        string  `json:"type"`
	StartMs     int64   `json:"start_ms"`
	EndMs       int64   `json:"end_ms"`
	Slots       int     `json:"slots"`
	TurnMinutes int     `json:"turn_minutes"`
	Headliners  []int64 `json:"headliners"`
}

// mainStageHeartbeat is the Main Stage server's heartbeat interval: short, so headliner changes reach it before an
// event starts.
const mainStageHeartbeat = 5

func (s *Server) fdsEnabled() bool { return s.public != nil }

// fdsCall reads an FDS request: the bearer token and the JSON body, which always names the slot.
func (s *Server) fdsCall(w http.ResponseWriter, r *http.Request, body any) (slot, token string, ok bool) {
	if !s.fdsEnabled() {
		http.NotFound(w, r)
		return "", "", false
	}
	token, found := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !found || token == "" {
		writeJSON(w, http.StatusUnauthorized, []byte(`{"error":"bearer token required"}`))
		return "", "", false
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, []byte(`{"error":"unreadable body"}`))
		return "", "", false
	}
	var head struct {
		SlotID string `json:"slot_id"`
	}
	if json.Unmarshal(raw, &head) != nil || head.SlotID == "" || json.Unmarshal(raw, body) != nil {
		writeJSON(w, http.StatusBadRequest, []byte(`{"error":"JSON body with slot_id required"}`))
		return "", "", false
	}
	return head.SlotID, token, true
}

func (s *Server) fdsFail(w http.ResponseWriter, slot, call string, err error) {
	status := http.StatusBadRequest
	if errors.Is(err, publichost.ErrUnknownSlot) || errors.Is(err, publichost.ErrUnauthorized) {
		status = http.StatusUnauthorized
	}
	s.mmLog().Warn("fds call refused", "call", call, "slot", slot, "status", status, "err", err)
	writeJSON(w, status, []byte(`{"error":"refused"}`))
}

func (s *Server) fdsReply(w http.ResponseWriter, r *http.Request, slot string) {
	reply := fdsReply{HeartbeatSeconds: s.public.HeartbeatSeconds(), BlockedSongs: []string{}}
	if sess, ok := s.mm.ManagedSession(slot); ok {
		reply.SessionID = sess.ID
		reply.BlockedSongs = s.blockedSongs(slot, sess)
	}
	if s.cfg.MainStage.Enabled && s.public.IsMainStage(slot) {
		reply.HeartbeatSeconds = min(reply.HeartbeatSeconds, mainStageHeartbeat)
		ms, err := s.fdsMainStageSchedule(r)
		if err != nil {
			s.logs.For(logx.MainStage).Error("main stage schedule for the server", "err", err)
		}
		reply.MainStage = ms
	}
	body, _ := json.Marshal(reply)
	writeJSON(w, http.StatusOK, body)
}

func (s *Server) fdsRegister(w http.ResponseWriter, r *http.Request) {
	var body publichost.Registration
	slot, token, ok := s.fdsCall(w, r, &body)
	if !ok {
		return
	}
	if err := s.public.Register(slot, token, body); err != nil {
		s.fdsFail(w, slot, "register", err)
		return
	}
	s.fdsReply(w, r, slot)
}

func (s *Server) fdsStatus(w http.ResponseWriter, r *http.Request) {
	var body publichost.Status
	slot, token, ok := s.fdsCall(w, r, &body)
	if !ok {
		return
	}
	if err := s.public.UpdateStatus(slot, token, body); err != nil {
		s.fdsFail(w, slot, "status", err)
		return
	}
	s.reconcileMembers(slot, body.Members)
	s.fdsReply(w, r, slot)
}

// reconcileMembers drops the players a heartbeat no longer lists from the slot's session (matchmaking
// ReconcileManaged): a player who quit the game sends fuserd nothing, and would otherwise hold a seat for good.
func (s *Server) reconcileMembers(slot string, members []publichost.Member) {
	present := make([]int64, 0, len(members))
	for _, m := range members {
		present = append(present, m.UserID)
	}
	dropped, ended := s.mm.ReconcileManaged(slot, present, matchmaking.ReconcileGrace)
	for _, id := range dropped {
		s.mmLog().Info("player left the server", "player", s.logs.TagID(id), "slot", slot)
	}
	s.logEnded(ended)
}

func (s *Server) fdsExit(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Reason string `json:"reason"`
	}
	slot, token, ok := s.fdsCall(w, r, &body)
	if !ok {
		return
	}
	if len(body.Reason) > 200 {
		body.Reason = body.Reason[:200]
	}
	if err := s.public.ReportExit(slot, token, body.Reason); err != nil {
		s.fdsFail(w, slot, "exit", err)
		return
	}
	s.blocked.forget(slot)
	writeJSON(w, http.StatusOK, []byte(`{}`))
}

// blockedSongs is a reply's "blocked_songs": the custom songs (lower-case shortnames) not every member of the slot's
// session can play. A stock client asks the server for a cel it doesn't have, never gets it, and stops playing that
// DJ's actions (loader.go), so the server holds back the SpawnDisc actions of these songs (its
// bBlockUnreadyCustomSongs). A song is held back
//   - while a member counts as stock (no loader report, whatever multiplayer.separate_stock_players says): every
//     custom song the members' loaders report holding, lacking or asking other players for;
//   - while a member went without it: its loader's fetch gave up, or its transfer ran out of owners;
//   - while members hold different files of it (songxfer conflicts; not catalogue songs).
//
// Official songs never are. What fuserd knows of the session's songs comes from the loaders' p2p/song/poll/, so with
// song transfers off (p2p.enabled, p2p.song_transfers) the list stays empty.
func (s *Server) blockedSongs(slot string, sess matchmaking.Session) []string {
	isMember := memberSet(sess)
	known, missing := s.songs.SessionSongs(sess.ID, isMember)
	stock := 0
	for _, m := range sess.Members {
		if !s.loaders.has(m.ID) {
			stock++
		}
	}
	blocked := missing
	if stock > 0 {
		blocked = known
	}
	conflicts, _ := s.songs.Conflicts(sess.ID, isMember, s.sharedSong)
	for _, c := range conflicts {
		blocked[c.Song] = true
	}
	out := make([]string, 0, len(blocked))
	for x := range blocked {
		if !s.reservedShortName(x) {
			out = append(out, x)
		}
	}
	sort.Strings(out)
	if s.blocked.changed(slot, out) {
		s.mmLog().Info("custom songs held back on the server", "slot", slot, "songs", len(out), "stock_players", stock,
			"missing", len(missing), "conflicts", len(conflicts), "list", strings.Join(out[:min(len(out), 20)], ","))
	}
	return out
}

// blockedLog is the last "blocked_songs" sent to each slot, so a change is logged once.
type blockedLog struct {
	mu   sync.Mutex
	last map[string]string
}

func (b *blockedLog) changed(slot string, songs []string) bool {
	joined := strings.Join(songs, ",")
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.last == nil {
		b.last = map[string]string{}
	}
	old, ok := b.last[slot]
	b.last[slot] = joined
	return ok && old != joined || !ok && joined != ""
}

func (b *blockedLog) forget(slot string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.last, slot)
}
