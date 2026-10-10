package api

// The Main Stage server. FUSER's Main Stage join has no ticket: matchmaking queue 4 goes
// straight to state 9, main_stage/servers/ (StartMatchmaking FUN_140718720), then travels to the answer's ip:Port with
// no player session id (_OnGetMainStageInfo FUN_14071ce70 clears it) - only gameSessionId and hypeUserId. So:
//   - fuserd keeps one persistent FDS container in Main Stage mode (publichost.EnsureMainStage);
//   - main_stage/servers/ answers its address and the venue, and gives the logged-in caller a pass;
//   - after each login the server asks POST /fds/v1/mainstage/admit whether the claimed hypeUserId holds a pass from
//     the connecting address, and kicks the player when not.

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"fuseronline/internal/logx"
	"fuseronline/internal/mainstage"
	"fuseronline/internal/publichost"
)

// msPasses are the outstanding Main Stage passes, by player.
type msPasses struct {
	mu sync.Mutex
	m  map[int64]msPass
}

type msPass struct {
	ip      string
	expires time.Time
}

func (p *msPasses) give(player int64, ip string, expires time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.m == nil {
		p.m = map[int64]msPass{}
	}
	now := time.Now()
	for id, old := range p.m { // drop expired passes as new ones arrive
		if now.After(old.expires) {
			delete(p.m, id)
		}
	}
	p.m[player] = msPass{ip: ip, expires: expires}
}

func (p *msPasses) get(player int64) (msPass, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	pass, ok := p.m[player]
	return pass, ok
}

// sameIP compares two addresses as IPs (an IPv4 address and its IPv4-mapped IPv6 form match); a port is ignored.
func sameIP(a, b string) bool {
	parse := func(s string) net.IP {
		if host, _, err := net.SplitHostPort(s); err == nil {
			s = host
		}
		return net.ParseIP(s)
	}
	x, y := parse(a), parse(b)
	return x != nil && y != nil && x.Equal(y)
}

// superviseMainStage keeps the Main Stage server running while the Main Stage is enabled: it is started at once and
// started again within 15 s of stopping (it also exits by itself for a scheduled restart, FDS_MAX_UPTIME_HOURS), or
// after a backoff when it keeps failing (publichost.ErrPaused). It runs once at startup, so it first takes over the
// containers a previous fuserd left running (stray Main Stage servers are stopped before a new one starts) and starts
// the warm pool.
func (s *Server) superviseMainStage() {
	if s.fdsEnabled() {
		s.public.Recover()
		s.public.KeepWarm(s.cfg.Multiplayer.PublicServers.WarmServers, s.cfg.Multiplayer.PublicServers.WarmRounds,
			s.cfg.MainStage.Enabled)
	}
	if !s.cfg.MainStage.Enabled || !s.fdsEnabled() {
		if s.cfg.MainStage.Enabled {
			s.logs.For(logx.MainStage).Warn("main stage server unavailable: it needs multiplayer.public_servers enabled")
		}
		return
	}
	ms := s.cfg.MainStage
	paused := false
	ensure := func() {
		_, err := s.public.EnsureMainStage(ms.Venue, ms.TimeOfDay, ms.MaxPlayers)
		switch {
		case errors.Is(err, publichost.ErrPaused):
			if !paused { // logged once per backoff, not every 15 s
				s.logs.For(logx.MainStage).Warn("main stage server start backing off", "err", err)
			}
		case err != nil:
			s.logs.For(logx.MainStage).Warn("main stage server not started", "err", err)
		}
		paused = errors.Is(err, publichost.ErrPaused)
	}
	ensure()
	go func() {
		tick := time.NewTicker(15 * time.Second)
		defer tick.Stop()
		for range tick.C {
			ensure()
		}
	}()
}

// msServers is main_stage/servers/: FMainStageServerInfosResponse { main_stage_servers: FMainStageServerInfo { Main:
// FServerInfo { ip, Port, venue: FRecordingVenueData { venue_name } } } }. The client looks the venue up by name and
// fails with matchmaking error 8 when it doesn't know it. The caller gets a pass for the server's admission check.
func (s *Server) msServers(q *request) (any, error) {
	unavailable := &apiError{http.StatusServiceUnavailable, codeUnknown, "the main stage server is not running"}
	if !s.cfg.MainStage.Enabled || !s.fdsEnabled() {
		return nil, unavailable
	}
	address, port, ready := s.public.MainStageServer()
	if !ready {
		return nil, unavailable
	}
	ms := s.cfg.MainStage
	if !s.modded(q.playerID()) {
		// The stock / modded split (loader.go): a stock player joins only during an event that allows stock players.
		sched, _, err := s.mainStageSchedule(q.Context(), s.clock())
		if err != nil {
			return nil, err
		}
		if !stockAllowed(sched) {
			s.logs.For(logx.MainStage).Info("main stage join refused", "player", s.logs.TagID(q.playerID()),
				"reason", "stock player; the event doesn't allow stock players")
			return nil, &apiError{http.StatusForbidden, codeUnknown, "the main stage is for loader users during this event"}
		}
	}
	if live, ok := s.mainStageLive(); mainStageFull(live, ok, ms.MaxPlayers) {
		s.logs.For(logx.MainStage).Info("main stage full", "player", s.logs.TagID(q.playerID()), "players", len(live.Members))
		return nil, &apiError{http.StatusConflict, codeMainStageFull, "the main stage is full"}
	}
	s.msPass.give(q.playerID(), s.clientIP(q), time.Now().Add(time.Duration(ms.PassMinutes)*time.Minute))
	s.logs.For(logx.MainStage).Info("main stage pass given", "player", s.logs.TagID(q.playerID()))
	return map[string]any{"main_stage_servers": map[string]any{"Main": map[string]any{
		"ip": address, "Port": port, "venue": map[string]any{"venue_name": ms.Venue}}}}, nil
}

// fdsMainStageAdmit is POST /fds/v1/mainstage/admit {"slot_id", "user_id", "address"} from the Main Stage server, after
// a login: {"admit": true} when the user holds an unexpired pass (from that address, with pass_check_ip), else
// {"admit": false, "reason"}.
func (s *Server) fdsMainStageAdmit(w http.ResponseWriter, r *http.Request) {
	var body struct {
		UserID  int64  `json:"user_id"`
		Address string `json:"address"`
	}
	slot, token, ok := s.fdsCall(w, r, &body)
	if !ok {
		return
	}
	mainStage, err := s.public.Authorize(slot, token)
	if err != nil {
		s.fdsFail(w, slot, "mainstage/admit", err)
		return
	}
	pass, found := s.msPass.get(body.UserID)
	reason := admitReason(mainStage, pass, found, time.Now(), s.cfg.MainStage.PassCheckIP, body.Address)
	lg := s.logs.For(logx.MainStage)
	if reason == "" {
		lg.Info("main stage join admitted", "player", s.logs.TagID(body.UserID))
	} else {
		lg.Warn("main stage join refused", "player", s.logs.TagID(body.UserID), "reason", reason)
	}
	out, _ := json.Marshal(map[string]any{"admit": reason == "", "reason": reason})
	writeJSON(w, http.StatusOK, out)
}

// admitReason is why a Main Stage join is refused ("" = admitted).
func admitReason(mainStage bool, pass msPass, found bool, now time.Time, checkIP bool, address string) string {
	switch {
	case !mainStage:
		return "not the main stage server"
	case !found:
		return "no pass"
	case now.After(pass.expires):
		return "pass expired"
	case checkIP && !sameIP(pass.ip, address):
		return "pass from another address"
	}
	return ""
}

// fdsMainStageSchedule is the current and next event for the Main Stage server's heartbeat reply, uncached: the
// server decides team placement and the set start from it.
func (s *Server) fdsMainStageSchedule(r *http.Request) (*fdsMainStage, error) {
	now := s.clock()
	sched, booked, err := s.mainStageSchedule(r.Context(), now)
	if err != nil {
		return nil, err
	}
	event := func(c *mainstage.Classified) *fdsStageEvent {
		if c == nil {
			return nil
		}
		e := &fdsStageEvent{UUID: c.UUID, Type: c.Type, StartMs: c.Start.UnixMilli(), EndMs: c.End.UnixMilli(),
			Slots: c.Slots, TurnMinutes: c.TurnMinutes, Headliners: []int64{}}
		for _, h := range booked[strings.ToLower(c.UUID)] { // slot order
			e.Headliners = append(e.Headliners, h.PlayerID)
		}
		return e
	}
	return &fdsMainStage{ServerTimeMs: now.UnixMilli(), Current: event(sched.Current), Next: event(sched.Next)}, nil
}

// stockAllowed reports whether stock players may join the Main Stage now: the current event's allow_stock, or between
// events the next one's. Checked at join only: a stock player already in stays when the next event is loader-only.
// The refusal has no client error of its own (only main_stage_spectator_slots_full has one, FUN_14070f9a0), so the
// player sees the generic matchmaking error.
func stockAllowed(sched mainstage.Schedule) bool {
	switch {
	case sched.Current != nil:
		return sched.Current.AllowStock
	case sched.Next != nil:
		return sched.Next.AllowStock
	}
	return false
}

// codeMainStageFull makes the client show its own "Main Stage full" error (matchmaking error 0x10; FUN_14070f9a0).
const codeMainStageFull = "main_stage_spectator_slots_full"

// mainStageFull reports whether the Main Stage server's newest player list is at its capacity.
func mainStageFull(live publichost.Live, ok bool, maxPlayers int) bool {
	return ok && maxPlayers > 0 && len(live.Members) >= maxPlayers
}
