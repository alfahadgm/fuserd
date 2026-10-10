package store

import (
	"context"
	"time"
)

// Block adds blocked to blocker's block list. It reports false when blocker already blocked them.
func (s *Store) Block(ctx context.Context, blocker, blocked int64) (bool, error) {
	res, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO blocks (blocker_id, blocked_id, created_at) VALUES (?, ?, ?)`,
		blocker, blocked, time.Now().Unix())
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// Unblock removes blocked from blocker's block list. It reports false when they were not on it.
func (s *Store) Unblock(ctx context.Context, blocker, blocked int64) (bool, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM blocks WHERE blocker_id = ? AND blocked_id = ?`, blocker, blocked)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// BlockedPlayers is one page of blocker's block list, most recently blocked first, and the list's length.
func (s *Store) BlockedPlayers(ctx context.Context, blocker int64, offset, limit int) ([]*Player, int, error) {
	return s.pagedPlayers(ctx, `FROM blocks b JOIN players p ON p.id = b.blocked_id WHERE b.blocker_id = ?`,
		`ORDER BY b.created_at DESC, p.id`, []any{blocker}, offset, limit)
}

// BlockRelation reports whether viewer blocked other (isBlocked) and whether other blocked viewer (blockedYou).
func (s *Store) BlockRelation(ctx context.Context, viewer, other int64) (isBlocked, blockedYou bool, err error) {
	rows, err := s.db.QueryContext(ctx, `SELECT blocker_id FROM blocks WHERE (blocker_id = ? AND blocked_id = ?)
		OR (blocker_id = ? AND blocked_id = ?)`, viewer, other, other, viewer)
	if err != nil {
		return false, false, err
	}
	defer rows.Close()
	for rows.Next() {
		var blocker int64
		if err := rows.Scan(&blocker); err != nil {
			return false, false, err
		}
		if blocker == viewer {
			isBlocked = true
		} else {
			blockedYou = true
		}
	}
	return isBlocked, blockedYou, rows.Err()
}

// BlockedEitherWay is everyone the player blocked or was blocked by: the players it must not share a session with.
func (s *Store) BlockedEitherWay(ctx context.Context, player int64) (map[int64]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT blocked_id FROM blocks WHERE blocker_id = ?
		UNION SELECT blocker_id FROM blocks WHERE blocked_id = ?`, player, player)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// HasBlocked reports whether blocker blocked blocked.
func (s *Store) HasBlocked(ctx context.Context, blocker, blocked int64) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM blocks WHERE blocker_id = ? AND blocked_id = ?`, blocker, blocked).Scan(&n)
	return n > 0, err
}
