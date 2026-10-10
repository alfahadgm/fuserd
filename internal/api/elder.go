package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"fuseronline/internal/catalog"
	"fuseronline/internal/logx"
	"fuseronline/internal/store"
)

// The Elder Credit ("Diamond") store. Catalogue songs with an elder_credit listing are offered there.
// A purchase records ownership; the player's loader then installs the song (the game itself only unlocks content
// already on disk). There is no economy: every player's balance is store.elder_credit_balance, which players/xp/
// reports and purchases don't lower.

// purchaseRefused is a purchase error. The game maps error.code to EElderCreditPurchaseError (FUN_14070fcf0):
// price_changed → PriceChanged, not_enough_credits → NotEnoughCredits, anything else → NotPurchasable.
func purchaseRefused(code, msg string) *apiError {
	return &apiError{http.StatusBadRequest, code, msg}
}

// songAsset is an FAssetDetails for a song. The store list reads asset_type_val (EUnlockableType, Song = 1); the
// claimed-assets handler reads asset_type ("Song").
func songAsset(sn string) map[string]any {
	return map[string]any{"asset_id": 1, "asset_name": sn, "asset_type": "Song", "asset_type_val": "Song"}
}

// ownedItems is the set of item uuids (lower case) the player bought, and the purchases themselves.
func (s *Server) ownedItems(q *request) (map[string]bool, []store.ElderCreditPurchase, error) {
	owned := map[string]bool{}
	if q.playerID() == 0 {
		return owned, nil, nil
	}
	list, err := s.store.ElderCreditPurchases(q.Context(), q.playerID())
	if err != nil {
		return nil, nil, err
	}
	for _, p := range list {
		owned[strings.ToLower(p.ItemUUID)] = true
	}
	return owned, list, nil
}

// elderCreditStore is store/elder_credit_store/get_list/ (FGetElderCreditStoreItemsResponse): the open listings, as
// FElderCreditStoreItem, featured ones apart. The game shows at most 2 featured and 8 standard items.
func (s *Server) elderCreditStore(q *request) (any, error) {
	featured, standard := []any{}, []any{}
	c := s.cat()
	if c == nil {
		return map[string]any{"featured_items": featured, "store_items": standard}, nil
	}
	owned, _, err := s.ownedItems(q)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	offers, artwork, err := s.diamondOffers(q.Context())
	if err != nil {
		return nil, err
	}
	for _, it := range offers {
		l := it.ElderCredit
		if !l.Active(now) {
			continue
		}
		art := s.artURL(it)
		if a, ok := artwork[it.UUID]; ok {
			art = a
		}
		if l.Featured { // the featured tile is 8:15 and stretches its image (featured.go)
			art = s.featuredArt(q.Context(), it, art)
		}
		item := map[string]any{
			"uuid": it.UUID, "assets": []any{songAsset(it.ShortName)}, "start_date": l.StartDate,
			"end_date": l.EndDate, "price": l.Price, "is_owned": owned[strings.ToLower(it.UUID)],
			"featured_image_url": art, "featured_image_medium_url": art, "featured_image_small_url": art,
		}
		if l.Featured {
			featured = append(featured, item)
		} else {
			standard = append(standard, item)
		}
	}
	return map[string]any{"featured_items": featured, "store_items": standard}, nil
}

// elderCreditPurchase is store/elder_credit_store/purchase/ (FElderCreditPurchaseJobRequest {uuid,
// expected_price}). It answers FElderCreditPurchaseJobResponse {num_elder_credits_remaining}: the unchanged balance.
func (s *Server) elderCreditPurchase(q *request) (any, error) {
	var req struct {
		UUID          string `json:"uuid"`
		ExpectedPrice int32  `json:"expected_price"`
	}
	if err := q.decode(&req); err != nil {
		return nil, err
	}
	lg := s.logs.For(logx.Store)
	refuse := func(code, msg string) (any, error) {
		lg.Info("elder credit purchase refused", "player", q.playerID(), "uuid", req.UUID, "code", code)
		return nil, purchaseRefused(code, msg)
	}
	if q.playerID() == 0 {
		return refuse("not_logged_in", "log in to buy")
	}
	var it catalog.Item
	ok := false
	offers, _, err := s.diamondOffers(q.Context())
	if err != nil {
		return nil, err
	}
	for _, offer := range offers {
		if strings.EqualFold(offer.UUID, req.UUID) {
			it = offer
			ok = true
			break
		}
	}
	switch {
	case !ok || it.ElderCredit == nil:
		return refuse("does_not_exist", "no such offer")
	case !it.ElderCredit.Active(time.Now()):
		return refuse("no_longer_available", "the offer has ended")
	case req.ExpectedPrice != it.ElderCredit.Price:
		return refuse("price_changed", "the price has changed")
	case int(it.ElderCredit.Price) > s.cfg.Store.ElderCreditBalance:
		return refuse("not_enough_credits", "not enough Elder Credits")
	}
	added, err := s.store.RecordElderCreditPurchase(q.Context(), q.playerID(), store.ElderCreditPurchase{
		ItemUUID: it.UUID, ShortName: it.ShortName, Price: it.ElderCredit.Price, CreatedAt: time.Now()})
	if err != nil {
		return nil, err
	}
	if !added {
		return refuse("already_owned", "already owned")
	}
	lg.Info("elder credit purchase", "player", q.playerID(), "shortName", it.ShortName, "price", it.ElderCredit.Price)
	return map[string]any{"num_elder_credits_remaining": s.cfg.Store.ElderCreditBalance}, nil
}

// claimedAssets is store/elder_credit_store/get_claimed_assets/ (FClaimedAssetsResponse), read at login: every song
// the player bought, so the game (and the loader, for one not installed) can unlock it on any machine.
func (s *Server) claimedAssets(q *request) (any, error) {
	_, list, err := s.ownedItems(q)
	if err != nil {
		return nil, err
	}
	assets := []any{}
	for _, p := range list {
		assets = append(assets, songAsset(p.ShortName))
	}
	return map[string]any{"assets": assets, "credits": []any{}}, nil
}

// updateXP is players/xp/ (FUpdateXPRequest {hype_profile, current_level} → FUpdateXPResponse {profile: FHypeUser,
// elder_credits_earned, xp_per_elder_credit}). The game applies only the credit fields of the answer
// (_OnServerXPUpdated FUN_1407479b0: profile.elder_credits.amount and progress_to_next_credit, xp_per_elder_credit),
// and it calls this after every purchase. With no store.elder_credit_balance the answer is empty.
// Either way the stats that players/get/ gives back for the player's own id are stored first (store.PlayerStats).
func (s *Server) updateXP(q *request) (any, error) {
	var req struct {
		CurrentLevel int             `json:"current_level"`
		HypeProfile  json.RawMessage `json:"hype_profile"`
	}
	if err := q.decode(&req); err != nil {
		if s.cfg.Store.ElderCreditBalance <= 0 {
			return nil, nil // empty answer, whatever the body
		}
		return nil, err
	}
	if id := q.playerID(); id != 0 && len(req.HypeProfile) > 0 {
		if err := s.storeOwnStats(q, id, req.HypeProfile); err != nil {
			return nil, err
		}
	}
	if s.cfg.Store.ElderCreditBalance <= 0 {
		return nil, nil
	}
	profile := map[string]any{}
	if id := q.playerID(); id != 0 {
		if p, err := s.store.Player(q.Context(), id); err == nil {
			if profile, err = s.hypeUser(q.Context(), id, p); err != nil {
				return nil, err
			}
		}
	}
	if len(req.HypeProfile) > 0 {
		profile["hype_profile"] = req.HypeProfile // echoed unchanged; the game does not apply it
	}
	profile["elder_credits"] = map[string]any{"amount": s.cfg.Store.ElderCreditBalance, "progress_to_next_credit": 0}
	return map[string]any{"profile": profile, "elder_credits_earned": 0,
		"xp_per_elder_credit": s.cfg.Store.XPPerElderCredit}, nil
}

// diamondOffers is shared by listing and purchasing: a saved panel storefront supersedes catalogue listings.
func (s *Server) diamondOffers(ctx context.Context) ([]catalog.Item, map[string]string, error) {
	var slots []store.ControlItem
	managed, err := s.store.ControlDocument(ctx, "items", &slots)
	if err != nil {
		return nil, nil, err
	}
	art := map[string]string{}
	c := s.cat()
	if c == nil {
		return nil, art, nil
	}
	if !managed {
		return c.Items(), art, nil
	}
	out := []catalog.Item{}
	for i, slot := range slots {
		if !slot.Enabled {
			continue
		}
		it, ok := c.ByUUID(slot.ID)
		if !ok || !strings.EqualFold(it.ShortName, slot.Shortname) {
			continue
		}
		start, e1 := store.ControlTime(slot.Start)
		end, e2 := store.ControlTime(slot.End)
		if e1 != nil || e2 != nil {
			continue
		}
		it.ElderCredit = &catalog.Listing{Price: slot.Price, Featured: i < 2, StartDate: start.UTC().Format(time.RFC3339), EndDate: end.UTC().Format(time.RFC3339)}
		it.Name = slot.Title
		it.Artist.Name = slot.Artist
		out = append(out, it)
		art[it.UUID] = slot.Image
	}
	return out, art, nil
}
