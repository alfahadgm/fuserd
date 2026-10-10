// Package store is the persistent state: players and what their platform tickets proved.
package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"
	"unicode"

	_ "modernc.org/sqlite"
)

var ErrPlayerBanned = errors.New("user banned")
var ErrNotFound = errors.New("not found")
var ErrEpicBlocked = errors.New("epic subject blocked")

type Player struct {
	ID               int64
	Platform         string
	PlatformUserID   string
	PlatformUsername string
	HypeUsername     string
	CreatedAt        time.Time
	LastSeen         time.Time
	CrossplayEnabled bool
	Banned           bool
}

type Store struct{ db *sql.DB }

const schema = `
CREATE TABLE IF NOT EXISTS players (
	id                INTEGER PRIMARY KEY AUTOINCREMENT,
	platform          TEXT    NOT NULL,
	platform_user_id  TEXT    NOT NULL,
	platform_username TEXT    NOT NULL,
	hype_username     TEXT    NOT NULL UNIQUE,
	created_at        INTEGER NOT NULL,
	last_seen         INTEGER NOT NULL,
	crossplay_enabled INTEGER NOT NULL DEFAULT 1,
	banned            INTEGER NOT NULL DEFAULT 0,
	UNIQUE (platform, platform_user_id)
);
CREATE TABLE IF NOT EXISTS steam_owned_dlc (
	player_id INTEGER NOT NULL REFERENCES players(id),
	app_id    INTEGER NOT NULL,
	PRIMARY KEY (player_id, app_id)
);
CREATE TABLE IF NOT EXISTS follows (
	follower_id INTEGER NOT NULL REFERENCES players(id),
	followee_id INTEGER NOT NULL REFERENCES players(id),
	created_at  INTEGER NOT NULL,
	PRIMARY KEY (follower_id, followee_id)
);
CREATE INDEX IF NOT EXISTS follows_by_followee ON follows (followee_id);
CREATE TABLE IF NOT EXISTS platform_friends (
	player_id        INTEGER NOT NULL REFERENCES players(id),
	platform_user_id TEXT    NOT NULL,
	PRIMARY KEY (player_id, platform_user_id)
);
CREATE INDEX IF NOT EXISTS platform_friends_by_friend ON platform_friends (platform_user_id);
CREATE TABLE IF NOT EXISTS elder_credit_purchases (
	player_id  INTEGER NOT NULL REFERENCES players(id),
	item_uuid  TEXT    NOT NULL,
	short_name TEXT    NOT NULL,
	price      INTEGER NOT NULL,
	created_at INTEGER NOT NULL,
	PRIMARY KEY (player_id, item_uuid)
);
CREATE TABLE IF NOT EXISTS community_songs (
	x               TEXT    NOT NULL PRIMARY KEY COLLATE NOCASE,
	entry           TEXT    NOT NULL,
	creator         TEXT    NOT NULL,
	disabled        INTEGER NOT NULL DEFAULT 0,
	disabled_reason TEXT    NOT NULL DEFAULT '',
	added_at        INTEGER NOT NULL,
	updated_at      INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS community_reports (
	x          TEXT    NOT NULL COLLATE NOCASE,
	player_id  INTEGER NOT NULL,
	ok         INTEGER NOT NULL,
	reason     TEXT    NOT NULL,
	sha256     TEXT    NOT NULL,
	sig_sha256 TEXT    NOT NULL,
	size       INTEGER NOT NULL,
	created_at INTEGER NOT NULL,
	PRIMARY KEY (x, player_id)
);
CREATE TABLE IF NOT EXISTS player_stats (
	player_id            INTEGER NOT NULL PRIMARY KEY REFERENCES players(id),
	num_discs            INTEGER,
	matchmaking_rating   INTEGER,
	total_battle_wins    INTEGER,
	total_battle_losses  INTEGER,
	cached_elder_credits INTEGER,
	updated_at           INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS profile_pics (
	player_id  INTEGER NOT NULL PRIMARY KEY REFERENCES players(id),
	pic_id     INTEGER NOT NULL,
	sizes      TEXT    NOT NULL,
	updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS blocks (
	blocker_id INTEGER NOT NULL REFERENCES players(id),
	blocked_id INTEGER NOT NULL REFERENCES players(id),
	created_at INTEGER NOT NULL,
	PRIMARY KEY (blocker_id, blocked_id)
);
CREATE INDEX IF NOT EXISTS blocks_by_blocked ON blocks (blocked_id);
CREATE TABLE IF NOT EXISTS epic_auth_policy (
	sub   TEXT NOT NULL PRIMARY KEY,
	dn    TEXT NOT NULL DEFAULT '',
	state TEXT NOT NULL CHECK (state IN ('allowed', 'verified', 'blocked'))
);
CREATE TABLE IF NOT EXISTS mainstage_events (
	uuid                TEXT    NOT NULL PRIMARY KEY COLLATE NOCASE,
	name                TEXT    NOT NULL,
	type                TEXT    NOT NULL CHECK (type IN ('event', 'special_event', 'maintenance')),
	start_at            INTEGER NOT NULL,
	end_at              INTEGER NOT NULL,
	special_name        TEXT    NOT NULL DEFAULT '',
	special_type_name   TEXT    NOT NULL DEFAULT '',
	special_description TEXT    NOT NULL DEFAULT '',
	image_url           TEXT    NOT NULL DEFAULT '',
	slots               INTEGER NOT NULL,
	turn_minutes        INTEGER NOT NULL,
	created_at          INTEGER NOT NULL,
	allow_stock         INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS mainstage_events_by_end ON mainstage_events (end_at);
CREATE TABLE IF NOT EXISTS mainstage_headliners (
	event_uuid TEXT    NOT NULL COLLATE NOCASE,
	player_id  INTEGER NOT NULL REFERENCES players(id),
	event_slot INTEGER NOT NULL,
	source     TEXT    NOT NULL CHECK (source IN ('purchase', 'admin')),
	created_at INTEGER NOT NULL,
	PRIMARY KEY (event_uuid, player_id),
	UNIQUE (event_uuid, event_slot)
);
`

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // SQLite has one writer; serialise instead of fighting over the lock
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("schema: %w", err)
	}
	if _, err := db.Exec(controlSchema); err != nil {
		db.Close()
		return nil, fmt.Errorf("control schema: %w", err)
	}
	if err := controlMigrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("control schema migration: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// MarkEpicVerified permanently upgrades an Epic identity to signature-only
// authentication. A blocked subject cannot be upgraded by logging in.
func (s *Store) MarkEpicVerified(ctx context.Context, sub string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `INSERT INTO epic_auth_policy (sub, state) VALUES (?, 'verified')
		ON CONFLICT(sub) DO UPDATE SET state = 'verified', dn = ''
		WHERE epic_auth_policy.state != 'blocked'`, sub)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// AllowEpicFallback accepts only an explicitly provisioned exact sub/dn pair.
// Verified and blocked identities cannot use the fallback.
func (s *Store) AllowEpicFallback(ctx context.Context, sub, dn string) (bool, error) {
	var state, knownDN string
	err := s.db.QueryRowContext(ctx, `SELECT state, dn FROM epic_auth_policy WHERE sub = ?`, sub).Scan(&state, &knownDN)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return state == "allowed" && knownDN == dn, nil
}

// AllowEpicExemption provisions an exact sub/dn pair for the failed-signature fallback, or changes the dn of an
// existing allowed sub. It refuses a sub that is verified or blocked, so it can never undo either. updated reports
// whether the sub was already allowed.
func (s *Store) AllowEpicExemption(ctx context.Context, sub, dn string) (updated bool, err error) {
	var state string
	err = s.db.QueryRowContext(ctx, `SELECT state FROM epic_auth_policy WHERE sub = ?`, sub).Scan(&state)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	if err == nil && state != "allowed" {
		return false, fmt.Errorf("key is already %s; it cannot be exempted", state)
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO epic_auth_policy (sub, dn, state) VALUES (?, ?, 'allowed')
		ON CONFLICT(sub) DO UPDATE SET dn = excluded.dn WHERE epic_auth_policy.state = 'allowed'`, sub, dn)
	if err != nil {
		return false, err
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return false, errors.Join(errors.New("key changed state while being exempted"), err)
	}
	return state == "allowed", nil
}

func (s *Store) CheckEpicBlocked(ctx context.Context, sub string) error {
	var state string
	err := s.db.QueryRowContext(ctx, `SELECT state FROM epic_auth_policy WHERE sub = ?`, sub).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if state == "blocked" {
		return ErrEpicBlocked
	}
	return nil
}

const playerCols = `id, platform, platform_user_id, platform_username, hype_username, created_at, last_seen,
	crossplay_enabled, banned`

func scanPlayer(row interface{ Scan(...any) error }) (*Player, error) {
	var p Player
	var created, seen int64
	err := row.Scan(&p.ID, &p.Platform, &p.PlatformUserID, &p.PlatformUsername, &p.HypeUsername, &created, &seen,
		&p.CrossplayEnabled, &p.Banned)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	p.CreatedAt, p.LastSeen = time.Unix(created, 0), time.Unix(seen, 0)
	return &p, nil
}

func (s *Store) Player(ctx context.Context, id int64) (*Player, error) {
	return scanPlayer(s.db.QueryRowContext(ctx, `SELECT `+playerCols+` FROM players WHERE id = ?`, id))
}

// LoginPlayer finds the player for a validated platform identity, creating the account on first login, and
// records the platform username and login time.
func (s *Store) LoginPlayer(ctx context.Context, platform, platformUserID, platformUsername string) (*Player, error) {
	now := time.Now().Unix()
	res, err := s.db.ExecContext(ctx, `UPDATE players SET platform_username = ?, last_seen = ?
		WHERE platform = ? AND platform_user_id = ?`, platformUsername, now, platform, platformUserID)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		if err := s.createPlayer(ctx, platform, platformUserID, platformUsername, now); err != nil {
			return nil, err
		}
	}
	return scanPlayer(s.db.QueryRowContext(ctx, `SELECT `+playerCols+` FROM players
		WHERE platform = ? AND platform_user_id = ?`, platform, platformUserID))
}

func (s *Store) createPlayer(ctx context.Context, platform, platformUserID, platformUsername string, now int64) error {
	base := hypeUsernameBase(platformUsername)
	for attempt := 0; attempt < 20; attempt++ {
		n, err := rand.Int(rand.Reader, big.NewInt(10000))
		if err != nil {
			return err
		}
		_, err = s.db.ExecContext(ctx, `INSERT INTO players
			(platform, platform_user_id, platform_username, hype_username, created_at, last_seen)
			VALUES (?, ?, ?, ?, ?, ?)`,
			platform, platformUserID, platformUsername, fmt.Sprintf("%s#%04d", base, n.Int64()), now, now)
		if err == nil || !strings.Contains(err.Error(), "UNIQUE") {
			return err
		}
		if strings.Contains(err.Error(), "platform_user_id") {
			return nil // a concurrent login created this player first
		}
	}
	return errors.New("could not allocate a unique hype username")
}

// hypeUsernameBase keeps the letters and digits of the platform name, capped, so the in-game name is readable.
func hypeUsernameBase(name string) string {
	var b strings.Builder
	for _, r := range name {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' {
			b.WriteRune(r)
		}
		if b.Len() >= 16 {
			break
		}
	}
	if b.Len() == 0 {
		return "Player"
	}
	return b.String()
}

func (s *Store) TouchPlayer(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE players SET last_seen = ? WHERE id = ?`, time.Now().Unix(), id)
	return err
}

func (s *Store) SetCrossplay(ctx context.Context, id int64, enabled bool) error {
	_, err := s.db.ExecContext(ctx, `UPDATE players SET crossplay_enabled = ? WHERE id = ?`, enabled, id)
	return err
}

// SetSteamOwnedDLC replaces the player's DLC list with what their latest ownership ticket proved.
func (s *Store) SetSteamOwnedDLC(ctx context.Context, id int64, appIDs []uint32) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM steam_owned_dlc WHERE player_id = ?`, id); err != nil {
		return err
	}
	for _, app := range appIDs {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO steam_owned_dlc (player_id, app_id) VALUES (?, ?)`,
			id, app); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) SteamOwnedDLC(ctx context.Context, id int64) ([]int32, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT app_id FROM steam_owned_dlc WHERE player_id = ? ORDER BY app_id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []int32{}
	for rows.Next() {
		var app int32
		if err := rows.Scan(&app); err != nil {
			return nil, err
		}
		out = append(out, app)
	}
	return out, rows.Err()
}
