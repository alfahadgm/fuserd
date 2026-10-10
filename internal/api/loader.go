package api

// The stock / modded split. A stock FUSER client can't play a custom song
// another player drops: it asks the server for the missing cel, which never answers, and stops playing that DJ's
// actions; a stock late joiner never finishes its sync. So players running the loader and players on stock FUSER are
// kept apart in public matchmaking, the session browser and the Main Stage. An invite overrides the split, as it
// does a block; a Main Stage event may allow stock players (allow_stock).
//
// fuserd knows a loader user from the report the loader sends with multiplayer/limits/ (logged in; at login and on
// each entry to multiplayer). A player who never reported counts as stock. Reports live in memory only: after a
// restart a loader user counts as stock until its next entry to multiplayer, which reports again before its ticket.

import (
	"context"
	"strings"
	"sync"
	"time"

	"fuseronline/internal/logx"
)

type loaderReport struct {
	version, build string
	seen           time.Time
}

// loaderReports is every loader report since fuserd started, by player.
type loaderReports struct {
	mu sync.Mutex
	m  map[int64]loaderReport
}

// record stores a report; it returns true for a player's first report or a new version / build.
func (r *loaderReports) record(player int64, version, build string, now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.m == nil {
		r.m = map[int64]loaderReport{}
	}
	old, ok := r.m[player]
	r.m[player] = loaderReport{version, build, now}
	return !ok || old.version != version || old.build != build
}

func (r *loaderReports) has(player int64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.m[player]
	return ok
}

// modded reports whether a player runs the loader. With multiplayer.separate_stock_players off everyone counts as
// modded, which turns the split off.
func (s *Server) modded(player int64) bool {
	return !s.cfg.Multiplayer.SeparateStockPlayers || s.loaders.has(player)
}

// groupName is how logs name a player's side of the split.
func groupName(modded bool) string {
	if modded {
		return "loader"
	}
	return "stock"
}

// keepApart is whom a player must not be placed with or shown in the session browser: the players it blocked or was
// blocked by (blocks.go), and the players on the other side of the split. nil when there is no one. An open invite
// overrides it (blockedFrom).
func (s *Server) keepApart(ctx context.Context, player int64) (func(int64) bool, error) {
	blocked, err := s.blockedEitherWay(ctx, player)
	if err != nil || player == 0 || !s.cfg.Multiplayer.SeparateStockPlayers {
		return blocked, err
	}
	mine := s.modded(player)
	return func(id int64) bool { return s.modded(id) != mine || blocked != nil && blocked(id) }, nil
}

// apartReason names why keepApart keeps a player out of a session, for the join log.
func (s *Server) apartReason(ctx context.Context, player int64, sessionID string) string {
	if blocked, err := s.blockedEitherWay(ctx, player); err == nil && anyBlocked(blocked, s.mm.Occupants(sessionID)) {
		return "a blocked player is in the session"
	}
	return "the session is for " + groupName(!s.modded(player)) + " players (this player: " + groupName(s.modded(player)) + ")"
}

// recordLoader stores the report a logged-in loader sends with multiplayer/limits/ ({"loader", "build"}).
func (s *Server) recordLoader(q *request) error {
	var req struct {
		Loader string `json:"loader"`
		Build  string `json:"build"`
	}
	if err := q.decode(&req); err != nil {
		return err
	}
	if q.playerID() == 0 || strings.TrimSpace(req.Loader) == "" {
		return nil
	}
	clip := func(v string) string {
		v = strings.TrimSpace(v)
		if len(v) > 64 {
			v = v[:64]
		}
		return v
	}
	version, build := clip(req.Loader), clip(req.Build)
	if s.loaders.record(q.playerID(), version, build, time.Now()) {
		s.logs.For(logx.Multiplayer).Info("loader seen", "player", s.logs.TagID(q.playerID()), "version", version, "build", build)
	}
	return nil
}
