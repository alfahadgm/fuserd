package store

import (
	"context"
	"strings"
	"time"
)

// ElderCreditPurchase is a song a player bought in the Elder Credit store. Purchases never expire;
// unpublishing a song doesn't take it back.
type ElderCreditPurchase struct {
	ItemUUID  string
	ShortName string
	Price     int32
	CreatedAt time.Time
}

// RecordElderCreditPurchase stores a purchase. It reports false when the player already owned the item.
func (s *Store) RecordElderCreditPurchase(ctx context.Context, playerID int64, p ElderCreditPurchase) (bool, error) {
	res, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO elder_credit_purchases
		(player_id, item_uuid, short_name, price, created_at) VALUES (?, ?, ?, ?, ?)`,
		playerID, strings.ToLower(p.ItemUUID), p.ShortName, p.Price, p.CreatedAt.Unix())
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// ElderCreditPurchases lists a player's purchases, oldest first.
func (s *Store) ElderCreditPurchases(ctx context.Context, playerID int64) ([]ElderCreditPurchase, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT item_uuid, short_name, price, created_at FROM elder_credit_purchases
		WHERE player_id = ? ORDER BY created_at, item_uuid`, playerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ElderCreditPurchase{}
	for rows.Next() {
		var p ElderCreditPurchase
		var at int64
		if err := rows.Scan(&p.ItemUUID, &p.ShortName, &p.Price, &at); err != nil {
			return nil, err
		}
		p.CreatedAt = time.Unix(at, 0).UTC()
		out = append(out, p)
	}
	return out, rows.Err()
}
