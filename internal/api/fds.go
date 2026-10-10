package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"fuseronline/internal/logx"
	"fuseronline/internal/publichost"
)

// The FuserDedicatedServer registration API. Only FDS containers
// fuserd started call it, each with its slot's registration token as a bearer token:
//   POST /fds/v1/register {"slot_id", "version", "net_version", "port", "beacon_port", "venue", "map",
//                          "time_of_day", "rounds", "turn_measures"} -> {"heartbeat_seconds", "session_id"}
//   POST /fds/v1/status   {"slot_id", "players", "djs", "viewers", "stage", "round"} -> the same reply
//   POST /fds/v1/exit     {"slot_id", "reason"} -> {}
//   POST /fds/v1/mainstage/admit {"slot_id", "user_id", "address"} -> {"admit", "reason"} (Main Stage server only;
//                          mainstage_server.go)
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
	reply := fdsReply{HeartbeatSeconds: s.public.HeartbeatSeconds()}
	if sess, ok := s.mm.ManagedSession(slot); ok {
		reply.SessionID = sess.ID
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
	s.fdsReply(w, r, slot)
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
	writeJSON(w, http.StatusOK, []byte(`{}`))
}
