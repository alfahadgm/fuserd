package store

// The social graph: who follows whom, and the platform friend lists clients upload. Both decide who can see and
// join a private multiplayer session.

import (
	"context"
	"database/sql"
	"strings"
	"time"
)

// Follow makes follower follow followee. Following twice, or following yourself, is a no-op.
func (s *Store) Follow(ctx context.Context, follower, followee int64) error {
	if follower == followee {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO follows (follower_id, followee_id, created_at)
		SELECT ?, id, ? FROM players WHERE id = ?`, follower, time.Now().Unix(), followee)
	return err
}

func (s *Store) Unfollow(ctx context.Context, follower, followee int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM follows WHERE follower_id = ? AND followee_id = ?`, follower, followee)
	return err
}

// Relation reports whether a follows b and whether b follows a.
func (s *Store) Relation(ctx context.Context, a, b int64) (aFollowsB, bFollowsA bool, err error) {
	rows, err := s.db.QueryContext(ctx, `SELECT follower_id FROM follows
		WHERE (follower_id = ? AND followee_id = ?) OR (follower_id = ? AND followee_id = ?)`, a, b, b, a)
	if err != nil {
		return false, false, err
	}
	defer rows.Close()
	for rows.Next() {
		var f int64
		if err := rows.Scan(&f); err != nil {
			return false, false, err
		}
		aFollowsB = aFollowsB || f == a
		bFollowsA = bFollowsA || f == b
	}
	return aFollowsB, bFollowsA, rows.Err()
}

// Following lists the players id follows, newest follow first, and their total count.
func (s *Store) Following(ctx context.Context, id int64, offset, limit int) ([]*Player, int, error) {
	return s.pagedPlayers(ctx, `FROM follows f JOIN players p ON p.id = f.followee_id WHERE f.follower_id = ?`,
		`ORDER BY f.created_at DESC, p.id`, []any{id}, offset, limit)
}

// Followers lists the players following id, newest first, and their total count.
func (s *Store) Followers(ctx context.Context, id int64, offset, limit int) ([]*Player, int, error) {
	return s.pagedPlayers(ctx, `FROM follows f JOIN players p ON p.id = f.follower_id WHERE f.followee_id = ?`,
		`ORDER BY f.created_at DESC, p.id`, []any{id}, offset, limit)
}

// SearchPlayers finds players whose hype username contains q (case-insensitive), excluding one player (the
// searcher).
func (s *Store) SearchPlayers(ctx context.Context, q string, exclude int64, offset, limit int) ([]*Player, int, error) {
	pattern := "%" + likeEscaper.Replace(q) + "%"
	return s.pagedPlayers(ctx, `FROM players p WHERE p.hype_username LIKE ? ESCAPE '\' AND p.id != ? AND p.banned = 0`,
		`ORDER BY length(p.hype_username), p.hype_username`, []any{pattern, exclude}, offset, limit)
}

var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

func (s *Store) pagedPlayers(ctx context.Context, from, order string, args []any, offset, limit int) ([]*Player,
	int, error) {
	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) `+from, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+prefixed("p.", playerCols)+` `+from+` `+order+` LIMIT ? OFFSET ?`,
		append(args, limit, offset)...)
	if err != nil {
		return nil, 0, err
	}
	list, err := scanPlayers(rows)
	return list, total, err
}

func scanPlayers(rows *sql.Rows) ([]*Player, error) {
	defer rows.Close()
	out := []*Player{}
	for rows.Next() {
		p, err := scanPlayer(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// prefixed qualifies every column of a comma-separated list with a table alias.
func prefixed(alias, cols string) string {
	parts := strings.Split(cols, ",")
	for i, c := range parts {
		parts[i] = alias + strings.TrimSpace(c)
	}
	return strings.Join(parts, ", ")
}

// SetPlatformFriends replaces the platform user IDs the player's client reported as its platform friends.
func (s *Store) SetPlatformFriends(ctx context.Context, id int64, platformUserIDs []string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM platform_friends WHERE player_id = ?`, id); err != nil {
		return err
	}
	for _, f := range platformUserIDs {
		if f = strings.TrimSpace(f); f == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO platform_friends (player_id, platform_user_id)
			VALUES (?, ?)`, id, f); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// PlatformFriends returns the registered players on the same platform as id who are platform friends with it in
// either direction: in id's uploaded list, or listing id in theirs. A platform friendship is mutual, but only one
// side may have uploaded (a client that reports no friends); this must agree with ArePlatformFriends, or a player
// drops out of both the friends and the followers-without-friends lists.
func (s *Store) PlatformFriends(ctx context.Context, id int64) ([]*Player, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+prefixed("p.", playerCols)+` FROM players p
		JOIN players me ON me.id = ? AND p.platform = me.platform AND p.id != me.id
		WHERE EXISTS (SELECT 1 FROM platform_friends f WHERE f.player_id = me.id AND f.platform_user_id = p.platform_user_id)
		   OR EXISTS (SELECT 1 FROM platform_friends f WHERE f.player_id = p.id AND f.platform_user_id = me.platform_user_id)
		ORDER BY p.hype_username`, id)
	if err != nil {
		return nil, err
	}
	return scanPlayers(rows)
}

// ArePlatformFriends reports whether either player listed the other as a platform friend (same platform only).
func (s *Store) ArePlatformFriends(ctx context.Context, a, b int64) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM platform_friends f
		JOIN players owner ON owner.id = f.player_id
		JOIN players other ON other.platform = owner.platform AND other.platform_user_id = f.platform_user_id
		WHERE (f.player_id = ? AND other.id = ?) OR (f.player_id = ? AND other.id = ?)`, a, b, b, a).Scan(&n)
	return n > 0, err
}
