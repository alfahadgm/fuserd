package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// PlayerStats is the part of a player's FHypeProfile that the game takes back from players/get/ for its own id: the
// profile callback (FUN_140745570) writes num_discs, matchmaking_rating, total_battle_wins and total_battle_losses
// into its local profile data, and the Elder Credit balance from elder_credits.amount. fuserd keeps what the client
// last reported in players/xp/, so that answer gives the client its own values back instead of zeros.
type PlayerStats struct {
	NumDiscs           int32
	MatchmakingRating  int32
	TotalBattleWins    int32
	TotalBattleLosses  int32
	CachedElderCredits int32
}

// PlayerStatsUpdate is one players/xp/ report. A nil field was missing or negative: the login burst's second
// players/xp/ sends -1 for most stats, and an unknown value must not replace a known one.
type PlayerStatsUpdate struct {
	NumDiscs           *int32
	MatchmakingRating  *int32
	TotalBattleWins    *int32
	TotalBattleLosses  *int32
	CachedElderCredits *int32
}

// UpdatePlayerStats merges a report into the player's stored stats; nil fields keep the stored values.
func (s *Store) UpdatePlayerStats(ctx context.Context, playerID int64, u PlayerStatsUpdate) error {
	v := func(p *int32) any {
		if p == nil {
			return nil
		}
		return *p
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO player_stats (player_id, num_discs, matchmaking_rating, total_battle_wins,
		total_battle_losses, cached_elder_credits, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(player_id) DO UPDATE SET
			num_discs = COALESCE(excluded.num_discs, player_stats.num_discs),
			matchmaking_rating = COALESCE(excluded.matchmaking_rating, player_stats.matchmaking_rating),
			total_battle_wins = COALESCE(excluded.total_battle_wins, player_stats.total_battle_wins),
			total_battle_losses = COALESCE(excluded.total_battle_losses, player_stats.total_battle_losses),
			cached_elder_credits = COALESCE(excluded.cached_elder_credits, player_stats.cached_elder_credits),
			updated_at = excluded.updated_at`,
		playerID, v(u.NumDiscs), v(u.MatchmakingRating), v(u.TotalBattleWins), v(u.TotalBattleLosses),
		v(u.CachedElderCredits), time.Now().Unix())
	return err
}

// PlayerStats returns the player's stored stats (a value never reported reads as 0), or ErrNotFound when the
// player has sent no players/xp/ yet.
func (s *Store) PlayerStats(ctx context.Context, playerID int64) (PlayerStats, error) {
	var n [5]sql.NullInt32
	err := s.db.QueryRowContext(ctx, `SELECT num_discs, matchmaking_rating, total_battle_wins, total_battle_losses,
		cached_elder_credits FROM player_stats WHERE player_id = ?`, playerID).Scan(&n[0], &n[1], &n[2], &n[3], &n[4])
	if errors.Is(err, sql.ErrNoRows) {
		return PlayerStats{}, ErrNotFound
	}
	if err != nil {
		return PlayerStats{}, err
	}
	return PlayerStats{NumDiscs: n[0].Int32, MatchmakingRating: n[1].Int32, TotalBattleWins: n[2].Int32,
		TotalBattleLosses: n[3].Int32, CachedElderCredits: n[4].Int32}, nil
}
