package store

// Main Stage schedule: events added by "fuserd mainstage" and the headliners booked on them.
// Rotation events are not stored; their headliner rows refer to the rotation's stable UUIDs.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"fuseronline/internal/mainstage"
)

var (
	// ErrMainStageOverlap refuses an event that shares time with a stored one.
	ErrMainStageOverlap = errors.New("overlaps a stored event")
	// ErrAlreadyHeadliner refuses a second slot for the same player on one event.
	ErrAlreadyHeadliner = errors.New("already a headliner of this event")
	// ErrEventFull refuses a slot when every slot is taken.
	ErrEventFull = errors.New("every headliner slot is taken")
	// ErrSlotTaken refuses a specific slot that someone else holds.
	ErrSlotTaken = errors.New("that slot is taken")
)

// Headliner sources: claimed through main_stage/purchase_slot/ or assigned by an operator.
const (
	HeadlinerPurchase = "purchase"
	HeadlinerAdmin    = "admin"
)

// MainStageHeadliner is one booked slot. Slots are numbered from 1.
type MainStageHeadliner struct {
	EventUUID string
	PlayerID  int64
	Slot      int
	Source    string
}

// AddMainStageEvent stores a validated event, refusing one that overlaps a stored event.
func (s *Store) AddMainStageEvent(ctx context.Context, e mainstage.Event) error {
	if err := e.Validate(); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var other string
	err = tx.QueryRowContext(ctx, `SELECT uuid FROM mainstage_events WHERE start_at < ? AND end_at > ? LIMIT 1`,
		e.End.Unix(), e.Start.Unix()).Scan(&other)
	if err == nil {
		return fmt.Errorf("%w (%s)", ErrMainStageOverlap, other)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO mainstage_events (uuid, name, type, start_at, end_at, special_name,
		special_type_name, special_description, image_url, slots, turn_minutes, created_at, allow_stock)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, strings.ToLower(e.UUID), e.Name, e.Type, e.Start.Unix(), e.End.Unix(),
		e.SpecialName, e.SpecialTypeName, e.SpecialDescription, e.ImageURL, e.Slots, e.TurnMinutes,
		time.Now().Unix(), e.AllowStock); err != nil {
		return err
	}
	return tx.Commit()
}

// RemoveMainStageEvent deletes a stored event and its headliners.
func (s *Store) RemoveMainStageEvent(ctx context.Context, uuid string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM mainstage_events WHERE uuid = ?`, uuid)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	_, err = s.db.ExecContext(ctx, `DELETE FROM mainstage_headliners WHERE event_uuid = ?`, uuid)
	return err
}

// MainStageEvents returns the stored events that end after from and start before to, by start.
func (s *Store) MainStageEvents(ctx context.Context, from, to time.Time) ([]mainstage.Event, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT uuid, name, type, start_at, end_at, special_name, special_type_name,
		special_description, image_url, slots, turn_minutes, allow_stock FROM mainstage_events WHERE end_at > ? AND start_at < ?
		ORDER BY start_at`, from.Unix(), to.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []mainstage.Event
	for rows.Next() {
		var e mainstage.Event
		var start, end int64
		if err := rows.Scan(&e.UUID, &e.Name, &e.Type, &start, &end, &e.SpecialName, &e.SpecialTypeName,
			&e.SpecialDescription, &e.ImageURL, &e.Slots, &e.TurnMinutes, &e.AllowStock); err != nil {
			return nil, err
		}
		e.Start, e.End = time.Unix(start, 0).UTC(), time.Unix(end, 0).UTC()
		out = append(out, e)
	}
	return out, rows.Err()
}

// MainStageHeadliners returns the headliners of the given events, by event, in slot order.
func (s *Store) MainStageHeadliners(ctx context.Context, uuids []string) (map[string][]MainStageHeadliner, error) {
	out := map[string][]MainStageHeadliner{}
	if len(uuids) == 0 {
		return out, nil
	}
	args := make([]any, len(uuids))
	for i, u := range uuids {
		args[i] = u
	}
	rows, err := s.db.QueryContext(ctx, `SELECT event_uuid, player_id, event_slot, source FROM mainstage_headliners
		WHERE event_uuid IN (?`+strings.Repeat(", ?", len(uuids)-1)+`) ORDER BY event_uuid, event_slot`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var h MainStageHeadliner
		if err := rows.Scan(&h.EventUUID, &h.PlayerID, &h.Slot, &h.Source); err != nil {
			return nil, err
		}
		key := strings.ToLower(h.EventUUID)
		out[key] = append(out[key], h)
	}
	return out, rows.Err()
}

// AddMainStageHeadliner books a slot on an event with the given number of slots: slot (1-based), or the first free
// one when slot is 0. It returns the slot booked.
func (s *Store) AddMainStageHeadliner(ctx context.Context, uuid string, slots int, playerID int64, slot int,
	source string) (int, error) {
	if slot < 0 || slot > slots {
		return 0, fmt.Errorf("slot must be between 1 and %d", slots)
	}
	uuid = strings.ToLower(uuid)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var removed int
	err = tx.QueryRowContext(ctx, `SELECT 1 FROM mainstage_removed WHERE uuid=?`, uuid).Scan(&removed)
	if err == nil {
		return 0, ErrEventFull
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	var currentSlots int
	var currentType string
	err = tx.QueryRowContext(ctx, `SELECT slots,type FROM mainstage_events WHERE uuid=?`, uuid).Scan(&currentSlots, &currentType)
	if err == nil && (currentType == mainstage.TypeMaintenance || currentSlots != slots) {
		return 0, ErrEventFull
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT player_id, event_slot FROM mainstage_headliners WHERE event_uuid = ?`, uuid)
	if err != nil {
		return 0, err
	}
	taken := map[int]bool{}
	for rows.Next() {
		var id int64
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			rows.Close()
			return 0, err
		}
		if id == playerID {
			rows.Close()
			return 0, ErrAlreadyHeadliner
		}
		taken[n] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	blockRows, err := tx.QueryContext(ctx, `SELECT event_slot FROM mainstage_slot_blocks WHERE event_uuid=?`, uuid)
	if err != nil {
		return 0, err
	}
	for blockRows.Next() {
		var n int
		if err := blockRows.Scan(&n); err != nil {
			blockRows.Close()
			return 0, err
		}
		taken[n] = true
	}
	blockRows.Close()
	if err := blockRows.Err(); err != nil {
		return 0, err
	}
	if slot == 0 {
		for n := 1; n <= slots && slot == 0; n++ {
			if !taken[n] {
				slot = n
			}
		}
		if slot == 0 {
			return 0, ErrEventFull
		}
	} else if taken[slot] {
		return 0, ErrSlotTaken
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO mainstage_headliners (event_uuid, player_id, event_slot, source,
		created_at) VALUES (?, ?, ?, ?, ?)`, uuid, playerID, slot, source, time.Now().Unix()); err != nil {
		return 0, err
	}
	return slot, tx.Commit()
}

// RemoveMainStageHeadliner frees a player's slot on an event.
func (s *Store) RemoveMainStageHeadliner(ctx context.Context, uuid string, playerID int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM mainstage_headliners WHERE event_uuid = ? AND player_id = ?`,
		uuid, playerID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// PlayerByHypeUsername finds a player by the exact hype username (with its #NNNN suffix), ignoring case.
func (s *Store) PlayerByHypeUsername(ctx context.Context, name string) (*Player, error) {
	return scanPlayer(s.db.QueryRowContext(ctx, `SELECT `+playerCols+` FROM players WHERE hype_username = ?
		COLLATE NOCASE`, name))
}
