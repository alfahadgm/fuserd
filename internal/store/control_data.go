package store

import "time"

type ControlItem struct {
	AssetID   string `json:"assetId"`
	ID        string `json:"id"`
	Title     string `json:"title"`
	Artist    string `json:"artist"`
	Shortname string `json:"shortname"`
	Price     int32  `json:"price"`
	Enabled   bool   `json:"enabled"`
	Start     string `json:"start"`
	End       string `json:"end"`
	Image     string `json:"image"`
}
type ControlPromotion struct {
	ID      int    `json:"promotion_id"`
	Type    string `json:"promotion_type"`
	Name    string `json:"name"`
	Message string `json:"message"`
	Extra   string `json:"extra_text"`
	Enabled bool   `json:"enabled"`
	Start   string `json:"start"`
	End     string `json:"end"`
	Image   string `json:"image"`
}

// ControlPromotions is the "promotions" control document: the main-menu carousel's announcements, in carousel order.
// NextID is the next unused promotion_id. Ids are never reused, because a running FUSER keeps a promotion's image by
// its id. It replaces the single "promotion" document, which is read as a one-item list until the first save.
type ControlPromotions struct {
	NextID     int                `json:"next_id"`
	Promotions []ControlPromotion `json:"promotions"`
}

// legacyZone is the fixed Perth offset that control documents were written in before the panel's timezone became
// configurable; those documents hold wall times without an offset.
var legacyZone = time.FixedZone("Perth", 8*3600)

const wallLayout = "2006-01-02T15:04:05"

// ControlTime reads a time stored in a control document: RFC 3339, or a legacy Perth wall time without an offset.
func ControlTime(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	return ControlParseWall(s, legacyZone)
}

// ControlStored formats a time for a control document, independent of any timezone setting.
func ControlStored(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// ControlParseWall reads a panel wall time ("2006-01-02T15:04", seconds optional) in loc.
func ControlParseWall(s string, loc *time.Location) (time.Time, error) {
	t, err := time.ParseInLocation(wallLayout, s, loc)
	if err != nil {
		t, err = time.ParseInLocation("2006-01-02T15:04", s, loc)
	}
	return t, err
}

// ControlWall formats t as a panel wall time in loc.
func ControlWall(t time.Time, loc *time.Location) string { return t.In(loc).Format(wallLayout) }
