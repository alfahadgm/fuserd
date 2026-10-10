package api

// Main-menu promotions: the announcements FUSER's main-menu carousel shows (WBP_MainMenu_Carousel). The client builds
// one tile per promotion in the order given (UHypeCarouselWidget::BuildCarousel, no cap), with pips, W/E (LB/RB) to
// cycle and a 5 s auto-cycle; Select opens the promotion's full text. Each promotion's image is matched to it, and
// cached for the rest of the game's run, by promotion_id (FUN_1406df020; UNetCacheMgr key {2, size, id}): so ids are
// unique, never reused, and a promotion whose image changes gets a new one.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"fuseronline/internal/control"
	"fuseronline/internal/store"
)

// maxPromotions bounds the carousel (the client has no cap of its own; each tile is a widget and an image).
const maxPromotions = 8

// controlPromotions reads the "promotions" document, or, before its first save, the single "promotion" document
// it replaces as a one-item list. NextID is at least one past every id in the list.
func (s *Server) controlPromotions(ctx context.Context) (store.ControlPromotions, error) {
	var ps store.ControlPromotions
	ok, err := s.store.ControlDocument(ctx, "promotions", &ps)
	if err != nil {
		return ps, err
	}
	if !ok {
		var legacy store.ControlPromotion
		found, err := s.store.ControlDocument(ctx, "promotion", &legacy)
		if err != nil {
			return ps, err
		}
		if found && legacy.ID > 0 {
			ps.Promotions = []store.ControlPromotion{legacy}
		}
	}
	if ps.Promotions == nil {
		ps.Promotions = []store.ControlPromotion{}
	}
	ps.NextID = max(ps.NextID, 1)
	for _, p := range ps.Promotions {
		ps.NextID = max(ps.NextID, p.ID+1)
	}
	return ps, nil
}

// activePromotions is main_menu/promotions/get_active/: FActivePromotions, every enabled promotion within its window,
// in carousel order.
func (s *Server) activePromotions(q *request) (any, error) {
	ps, err := s.controlPromotions(q.Context())
	if err != nil {
		return nil, err
	}
	now := time.Now()
	list := []any{}
	for _, p := range ps.Promotions {
		start, e1 := store.ControlTime(p.Start)
		end, e2 := store.ControlTime(p.End)
		if !p.Enabled || e1 != nil || e2 != nil || now.Before(start) || !now.Before(end) {
			continue
		}
		list = append(list, map[string]any{"promotion_id": p.ID, "promotion_type": p.Type, "name": p.Name, "message": p.Message,
			"extra_text": p.Extra, "image_download_url": p.Image, "image_medium_download_url": p.Image,
			"image_small_download_url": p.Image, "event": nil, "recording": nil, "dlc": nil, "bundled_dlc": []any{}})
	}
	return map[string]any{"promotions": list}, nil
}

// savePromotions stores the panel's list (PUT /control/api/promotions): validated, times stored absolute, images
// published, and ids assigned. A promotion keeps its id only while its image is unchanged; a new one, a changed image
// or a repeated id takes the next unused id.
func (s *Server) savePromotions(ctx context.Context, r *http.Request, change *controlChange) error {
	var list []store.ControlPromotion
	if err := decodeControl(r, &list); err != nil {
		return errors.New("invalid promotions")
	}
	if len(list) > maxPromotions {
		return fmt.Errorf("at most %d promotions", maxPromotions)
	}
	old, err := s.controlPromotions(ctx)
	if err != nil {
		return err
	}
	byID := map[int]store.ControlPromotion{}
	for _, p := range old.Promotions {
		byID[p.ID] = p
	}
	loc := control.Location(ctx)
	used := map[int]bool{}
	next := old.NextID
	for i := range list {
		p := &list[i]
		start, end, err := controlDates(p.Start, p.End, loc)
		if err != nil {
			return fmt.Errorf("promotion %d: %w", i+1, err)
		}
		p.Start, p.End = store.ControlStored(start), store.ControlStored(end)
		p.Type = "hmx_promotion"
		if len(p.Name) > 100 || len(p.Message) > 1000 || len(p.Extra) > 10000 || p.Enabled &&
			(strings.TrimSpace(p.Name) == "" || strings.TrimSpace(p.Message) == "" || strings.TrimSpace(p.Extra) == "") {
			return fmt.Errorf("promotion %d: invalid fields", i+1)
		}
		if p.Image, err = s.controlImage(ctx, p.Image); err != nil {
			return fmt.Errorf("promotion %d: %w", i+1, err)
		}
		if prev, ok := byID[p.ID]; !ok || used[p.ID] || prev.Image != p.Image {
			p.ID = next
			next++
		}
		used[p.ID] = true
	}
	change.detail = fmt.Sprintf("%d promotion(s)", len(list))
	return s.store.ControlSaveDocument(ctx, "promotions", store.ControlPromotions{NextID: next, Promotions: list},
		r.Header.Get("If-Match"))
}
