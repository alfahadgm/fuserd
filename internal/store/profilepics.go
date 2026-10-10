package store

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
	"time"
)

// ProfilePic is a player's current profile picture: its id (a new one for each upload, so clients' caches see the
// change) and the sizes stored for it ("large", "medium", "small"). The images are files (api, profile_pics.dir).
type ProfilePic struct {
	PicID int32
	Sizes []string
}

// ErrStalePicture: an upload for an older picture than the player's current one.
var ErrStalePicture = errors.New("a newer picture exists")

// ProfilePic returns the player's current picture, or ErrNotFound.
func (s *Store) ProfilePic(ctx context.Context, playerID int64) (ProfilePic, error) {
	var p ProfilePic
	var sizes string
	err := s.db.QueryRowContext(ctx, `SELECT pic_id, sizes FROM profile_pics WHERE player_id = ?`, playerID).Scan(&p.PicID, &sizes)
	if errors.Is(err, sql.ErrNoRows) {
		return p, ErrNotFound
	}
	if sizes != "" {
		p.Sizes = strings.Split(sizes, ",")
	}
	return p, err
}

// MaxProfilePicID is the highest picture id stored (0 when none): new ids are allocated above it.
func (s *Store) MaxProfilePicID(ctx context.Context) (int32, error) {
	var n sql.NullInt32
	err := s.db.QueryRowContext(ctx, `SELECT MAX(pic_id) FROM profile_pics`).Scan(&n)
	return n.Int32, err
}

// AddProfilePicSize records one stored size of picture picID. A newer picture replaces the player's current one
// (replaced is the old id, whose files the caller deletes; 0 when there was none or it is the same picture); an
// older one is refused with ErrStalePicture.
func (s *Store) AddProfilePicSize(ctx context.Context, playerID int64, picID int32, size string) (replaced int32, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var cur int32
	var sizes string
	err = tx.QueryRowContext(ctx, `SELECT pic_id, sizes FROM profile_pics WHERE player_id = ?`, playerID).Scan(&cur, &sizes)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		sizes = size
	case err != nil:
		return 0, err
	case picID < cur:
		return 0, ErrStalePicture
	case picID > cur:
		replaced, sizes = cur, size
	default:
		list := strings.Split(sizes, ",")
		if !slices.Contains(list, size) {
			sizes = strings.Join(append(list, size), ",")
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO profile_pics (player_id, pic_id, sizes, updated_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(player_id) DO UPDATE SET pic_id = excluded.pic_id, sizes = excluded.sizes, updated_at = excluded.updated_at`,
		playerID, picID, sizes, time.Now().Unix()); err != nil {
		return 0, err
	}
	return replaced, tx.Commit()
}

// RemoveProfilePic forgets the player's picture (the caller deletes the files). It returns the removed picture's id,
// or ErrNotFound.
func (s *Store) RemoveProfilePic(ctx context.Context, playerID int64) (int32, error) {
	var id int32
	err := s.db.QueryRowContext(ctx, `DELETE FROM profile_pics WHERE player_id = ? RETURNING pic_id`, playerID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	return id, err
}
