package api

// Main Stage schedule. main_stage/refresh_urls/ hands the client five
// polling URLs. UMainStageScheduleMgr keeps three of them:
//   - events_url (BackendMainStageMicroservicePollingEventsJob, FMainStageEventsResponse), every
//     SecondsToPollAllEvents (1 s);
//   - current_event_url (BackendMainStageCurrentEventJob, FMainStageCurrentEventResponse), every
//     SecondsToPollCurrentEvent (1 s);
//   - latest_spectators (FMainStageSpectatorsResponse, every 5 s), replaced by each response's refresh_url.
// next_event_url and headliners_url are served too, for completeness.
//
// Wire details from the exe:
//   - the jobs parse start_time / end_time / server_time with FDateTime::ParseIso8601 into the *_date fields, and
//     event_type ("event", "special_event", "maintenance") into event_type_enum;
//   - uuid must be a GUID (FGuid parse);
//   - the headliner array order is the DJ order (MPS slot 147);
//   - these jobs don't flag a required "results" field, unlike the ClientURL jobs, so the payload is sent both
//     inside "results" and at the top level (flatResults).

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"fuseronline/internal/logx"
	"fuseronline/internal/mainstage"
	"fuseronline/internal/publichost"
	"fuseronline/internal/store"
)

// mainStageURLs is FMainStagePollingURLResponse. With the Main Stage off, every URL points at nop/.
func (s *Server) mainStageURLs(*request) (any, error) {
	if !s.cfg.MainStage.Enabled {
		nop := s.cfg.PublicURL + Prefix + "nop/?from="
		return map[string]string{
			"current_event_url": nop + "current_event_url",
			"next_event_url":    nop + "next_event_url",
			"events_url":        nop + "events_url",
			"headliners_url":    nop + "headliners_url",
			"latest_spectators": nop + "latest_spectators",
		}, nil
	}
	base := s.cfg.PublicURL + Prefix
	return map[string]string{
		"current_event_url": base + "mainstage/current_event/",
		"next_event_url":    base + "mainstage/next_event/",
		"events_url":        base + "mainstage/events/",
		"headliners_url":    base + "mainstage/headliners/",
		"latest_spectators": base + "mainstage/latest_spectators/",
	}, nil
}

// msSnapshot is the schedule at one moment, shared by every poll within a second: each client polls two URLs every
// second, so a full rebuild per request would cost several queries each.
type msSnapshot struct {
	at         time.Time
	sched      mainstage.Schedule
	headliners map[string][]map[string]any // rendered FMainStageHeadliner lists, by lower-case event UUID
	headliner  map[string]map[int64]bool   // headliner player IDs, by lower-case event UUID
	images     map[string][]map[string]any // generated event cards (FMainStageImages), by lower-case event UUID
	current    map[string]any              // the current event's performing headliner (current_headliner), if any
	spectators []map[string]any            // the latest spectators (FMiniHypeUser), newest last
	watching   int                         // the spectator count
}

// fansToClient is NumFansToSendToClient (HypeMultiplayerSettings +0x9c, 5): how many latest spectators the hub shows.
const fansToClient = 5

type msCache struct {
	mu   sync.Mutex
	snap *msSnapshot
}

// mainStageSnapshot returns the schedule now, rebuilding it at most once a second.
func (s *Server) mainStageSnapshot(ctx context.Context) (*msSnapshot, error) {
	if !s.cfg.MainStage.Enabled {
		return nil, &apiError{http.StatusNotFound, codeUnknown, "the main stage is not enabled"}
	}
	now := s.clock()
	s.ms.mu.Lock()
	defer s.ms.mu.Unlock()
	if sn := s.ms.snap; sn != nil && !now.Before(sn.at) && now.Sub(sn.at) < time.Second {
		return sn, nil
	}
	sched, booked, err := s.mainStageSchedule(ctx, now)
	if err != nil {
		return nil, err
	}
	blockedSlots, err := s.store.MainStageBlocks(ctx)
	if err != nil {
		return nil, err
	}
	sn := &msSnapshot{at: now, sched: sched, headliners: map[string][]map[string]any{},
		headliner: map[string]map[int64]bool{}, images: map[string][]map[string]any{}}
	// The Main Stage server's live player list fills current_dj / in_main_stage and the spectators.
	var live publichost.Live
	present := map[int64]bool{}
	if l, ok := s.mainStageLive(); ok {
		live = l
		for _, m := range l.Members {
			present[m.UserID] = true
		}
	}
	for _, c := range []*mainstage.Classified{sn.sched.Current, sn.sched.Next, sn.sched.Held} {
		if c == nil {
			continue
		}
		// A held event keeps its card (the same key, so no re-render) but tells the game nothing about its
		// headliners: no headliner list and no current_headliner. The hub's "Now Playing" list then stays empty and the
		// tiles read "Next Diamond Stage set coming soon!" without the LIVE badge.
		held := c == sn.sched.Held
		key := strings.ToLower(c.UUID)
		list, ids := []map[string]any{}, map[int64]bool{}
		card := s.cfg.MainStage.GeneratedImages && c.ImageURL == "" && c.Type != mainstage.TypeMaintenance
		// Open slots show only while they can be claimed: the current event shows its headliners alone.
		spec := cardSpec{Event: key, Title: c.Name, Open: c == sn.sched.Next && c.State == mainstage.NextOpen}
		if c.Type == mainstage.TypeSpecialEvent && c.SpecialName != "" {
			spec.Title = c.SpecialName
		}
		for i := 1; i <= c.Slots; i++ {
			blocked := false
			for _, n := range blockedSlots[key] {
				if n == i {
					blocked = true
				}
			}
			spec.Slots = append(spec.Slots, cardSlot{Slot: i, Blocked: blocked})
		}
		for _, h := range booked[key] {
			p, err := s.store.Player(ctx, h.PlayerID)
			if errors.Is(err, store.ErrNotFound) {
				continue
			}
			if err != nil {
				return nil, err
			}
			if card && h.Slot >= 1 && h.Slot <= len(spec.Slots) {
				if spec.Slots[h.Slot-1], err = s.cardSlotFor(ctx, h.Slot, p); err != nil {
					return nil, err
				}
			}
			if held {
				continue
			}
			pic, err := s.profilePicMeta(ctx, p.ID)
			if err != nil {
				return nil, err
			}
			isDJ := c == sn.sched.Current && live.CurrentDJ == p.ID && strings.EqualFold(live.Event, c.UUID)
			m := map[string]any{"event_slot": h.Slot, "user_id": p.ID, "hype_username": p.HypeUsername,
				"online_status": onlineStatus(p), "current_dj": isDJ, "in_main_stage": present[p.ID],
				"platform_profile": platformProfile(p)}
			if pic != nil {
				m["profile_pic"] = pic
			}
			if isDJ {
				sn.current = m
			}
			list = append(list, m)
			ids[p.ID] = true
		}
		sn.headliners[key], sn.headliner[key] = list, ids
		if card {
			if ck, rendered, err := s.eventCard(spec); err != nil {
				s.logs.For(logx.MainStage).Error("event image not rendered", "event", c.UUID, "err", err)
			} else {
				sn.images[key] = s.cardImagesJSON(key, ck, rendered)
			}
		}
	}
	for _, member := range live.Members {
		if member.Team != "spectator" {
			continue
		}
		sn.watching++
		p, err := s.store.Player(ctx, member.UserID)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		pic, err := s.profilePicMeta(ctx, p.ID)
		if err != nil {
			return nil, err
		}
		u := map[string]any{"user_id": p.ID, "hype_username": p.HypeUsername, "platform_profile": platformProfile(p),
			"online_status": onlineStatus(p)}
		if pic != nil {
			u["profile_pic"] = pic
		}
		sn.spectators = append(sn.spectators, u)
		if len(sn.spectators) > fansToClient {
			sn.spectators = sn.spectators[1:] // members come in join order: keep the newest
		}
	}
	s.ms.snap = sn
	return sn, nil
}

// mainStageSchedule computes the schedule at now, uncached, with the headliners booked on its events (by lower-case
// UUID).
func (s *Server) mainStageSchedule(ctx context.Context, now time.Time) (mainstage.Schedule,
	map[string][]store.MainStageHeadliner, error) {
	cfg := s.cfg.MainStage
	to := now.Add(30 * 24 * time.Hour)
	stored, err := s.store.MainStageEvents(ctx, now.Add(-mainstage.MaxDuration-cfg.HoldCurrent()), to)
	if err != nil {
		return mainstage.Schedule{}, nil, err
	}
	// From now minus the hold: the events that ended within it are candidates for the held event.
	events := mainstage.Merge(stored, cfg.RotationSchedule(), now.Add(-cfg.HoldCurrent()), to)
	removed, err := s.store.MainStageRemoved(ctx)
	if err != nil {
		return mainstage.Schedule{}, nil, err
	}
	overrides, err := s.store.MainStageOverrideIDs(ctx)
	if err != nil {
		return mainstage.Schedule{}, nil, err
	}
	filtered := events[:0]
	for _, e := range events {
		if !removed[strings.ToLower(e.UUID)] && !(e.Rotation && overrides[strings.ToLower(e.UUID)]) {
			filtered = append(filtered, e)
		}
	}
	events = filtered
	// The ended events come first (sorted by start, and stored events can't overlap), then the current, next and
	// upcoming events. Only the latest ended event can be held, so keep that one and drop the older ones.
	ended := 0
	for ended < len(events) && !now.Before(events[ended].End) {
		ended++
	}
	if ended > 1 {
		events = events[ended-1:]
		ended = 1
	}
	if n := ended + cfg.UpcomingEvents + 2; len(events) > n {
		events = events[:n]
	}
	uuids := make([]string, len(events))
	for i, e := range events {
		uuids[i] = strings.ToLower(e.UUID)
	}
	booked, err := s.store.MainStageHeadliners(ctx, uuids)
	if err != nil {
		return mainstage.Schedule{}, nil, err
	}
	blocks, err := s.store.MainStageBlocks(ctx)
	if err != nil {
		return mainstage.Schedule{}, nil, err
	}
	full := func(e mainstage.Event) bool {
		return len(booked[strings.ToLower(e.UUID)])+len(blocks[strings.ToLower(e.UUID)]) >= e.Slots
	}
	sched := mainstage.Classify(events, now, cfg.PurchaseWindow(), cfg.UpcomingEvents, full)
	sched.Held, sched.HeldUntil = mainstage.Hold(events, now, cfg.HoldCurrent(), sched)
	return sched, booked, nil
}

// isoTime is the form FDateTime::ParseIso8601 reads.
func isoTime(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

// eventJSON is the part FMainStageCurrentEventResponse and FMainStageNextEventResponse share. No event gives empty
// fields: an invalid type and zero times, which the client's schedule drops.
func (sn *msSnapshot) eventJSON(c *mainstage.Classified) map[string]any {
	m := map[string]any{"uuid": "", "event_name": "", "start_time": "", "end_time": "", "headliners": []any{},
		"event_type": "", "special_event_name": "", "special_event_type_name": "", "special_event_description": "",
		"images": []any{}}
	if c == nil {
		return m
	}
	m["uuid"], m["event_name"], m["event_type"] = c.UUID, c.Name, c.Type
	m["start_time"], m["end_time"] = isoTime(c.Start), isoTime(c.End)
	m["special_event_name"], m["special_event_type_name"] = c.SpecialName, c.SpecialTypeName
	m["special_event_description"] = c.SpecialDescription
	if list := sn.headliners[strings.ToLower(c.UUID)]; list != nil {
		m["headliners"] = list
	}
	if imgs := sn.images[strings.ToLower(c.UUID)]; imgs != nil {
		m["images"] = imgs
	} else if c.ImageURL != "" { // one static image at every size
		m["images"] = []map[string]any{{"uuid": mainstage.DerivedUUID("mainstage-image", c.UUID+"\n"+c.ImageURL),
			"image_download_url": c.ImageURL, "image_medium_download_url": c.ImageURL,
			"image_small_download_url": c.ImageURL, "modified_time": isoTime(c.Start), "image_upload_url": ""}}
	}
	return m
}

// nextEventJSON is FMainStageNextEventResponse for the viewer. Slots cost nothing (balances never go down), so
// the price is 0 and never drops: countdown and next_price_drop count to the start.
func (sn *msSnapshot) nextEventJSON(viewer int64) map[string]any {
	c := sn.sched.Next
	m := sn.eventJSON(c)
	m["server_time"] = isoTime(sn.at)
	m["current_price"] = 0
	if c == nil {
		m["state"], m["open_for_purchase"], m["time_to_start"], m["countdown"] = mainstage.Offline, false, 0, 0
		m["headliner"], m["next_price_drop"] = false, ""
		return m
	}
	toStart := int(c.Start.Sub(sn.at) / time.Second)
	m["state"], m["open_for_purchase"] = c.State, c.State == mainstage.NextOpen
	m["time_to_start"], m["countdown"], m["next_price_drop"] = toStart, toStart, isoTime(c.Start)
	m["headliner"] = viewer != 0 && sn.headliner[strings.ToLower(c.UUID)][viewer]
	return m
}

// shownCurrent is the event the game is told is current: the running one, or between sets the held one, ending when
// the hold does.
func (sn *msSnapshot) shownCurrent() *mainstage.Classified {
	if sn.sched.Current != nil || sn.sched.Held == nil {
		return sn.sched.Current
	}
	held := *sn.sched.Held
	held.End = sn.sched.HeldUntil
	return &held
}

func (sn *msSnapshot) currentEventJSON() map[string]any {
	m := sn.eventJSON(sn.shownCurrent())
	m["server_time"] = isoTime(sn.at)
	if sn.current != nil {
		m["current_headliner"] = sn.current
	}
	return m
}

// msCurrentEvent is current_event_url: FMainStageCurrentEventResponse.
func (s *Server) msCurrentEvent(q *request) (any, error) {
	sn, err := s.mainStageSnapshot(q.Context())
	if err != nil {
		return nil, err
	}
	return flatResults(sn.currentEventJSON()), nil
}

// msNextEvent is next_event_url: FMainStageNextEventResponse.
func (s *Server) msNextEvent(q *request) (any, error) {
	sn, err := s.mainStageSnapshot(q.Context())
	if err != nil {
		return nil, err
	}
	return flatResults(sn.nextEventJSON(q.playerID())), nil
}

// msEvents is events_url: FMainStageEventsResponse.
func (s *Server) msEvents(q *request) (any, error) {
	sn, err := s.mainStageSnapshot(q.Context())
	if err != nil {
		return nil, err
	}
	w := s.cfg.MainStage.PurchaseWindow()
	upcoming := make([]map[string]any, 0, len(sn.sched.Upcoming))
	for _, u := range sn.sched.Upcoming {
		upcoming = append(upcoming, map[string]any{"uuid": u.UUID, "event_name": u.Name,
			"start_time": isoTime(u.Start), "end_time": isoTime(u.End), "event_type": u.Type, "state": u.State,
			"time_to_purchase": w.SecondsUntilOpen(u.Start, sn.at)})
	}
	return flatResults{"server_time": isoTime(sn.at), "current_event": sn.currentEventJSON(),
		"next_event": sn.nextEventJSON(q.playerID()), "upcoming_events": upcoming}, nil
}

// msHeadliners is headliners_url: FMainStageHeadlinersResponse.
func (s *Server) msHeadliners(q *request) (any, error) {
	sn, err := s.mainStageSnapshot(q.Context())
	if err != nil {
		return nil, err
	}
	part := func(c *mainstage.Classified) map[string]any {
		if c == nil {
			return map[string]any{"uuid": "", "headliners": []any{}}
		}
		list := sn.headliners[strings.ToLower(c.UUID)]
		if list == nil {
			list = []map[string]any{}
		}
		return map[string]any{"uuid": c.UUID, "headliners": list}
	}
	return flatResults{"server_time": isoTime(sn.at), "current_event": part(sn.shownCurrent()),
		"next_event": part(sn.sched.Next)}, nil
}

// msLatestSpectators is latest_spectators: FMainStageSpectatorsResponse, from the Main Stage server's status:
// the newest NumFansToSendToClient spectators (FMiniHypeUser) and how many there are. refresh_url replaces the
// client's polling URL, so it names this endpoint again.
func (s *Server) msLatestSpectators(q *request) (any, error) {
	sn, err := s.mainStageSnapshot(q.Context())
	if err != nil {
		return nil, err
	}
	list := sn.spectators
	if list == nil {
		list = []map[string]any{}
	}
	return flatResults{"latest_spectators": list, "count": sn.watching,
		"refresh_url": s.cfg.PublicURL + Prefix + "mainstage/latest_spectators/"}, nil
}

// Purchase error codes: the purchase job's error handler (FUN_14070fb70) maps each to EMainStagePurchaseErrorType.
// user_banned (BannedUserIsHeadliner) is left out: it is also the generic ban code.
const (
	codeAlreadyHeadliner = "user_already_purchased_event" // UserIsAlreadyHeadliner
	codeNotAvailable     = "event_not_available_for_purchase"
	codeBlockedHeadliner = "blocked_user" // BlockedUserIsHeadliner
	codeMainStageBan     = "main_stage_ban"
)

func errPurchase(code, msg string) *apiError {
	status := http.StatusConflict
	if code == codeMainStageBan {
		status = http.StatusForbidden
	}
	return &apiError{status, code, msg}
}

// msPurchaseSlot is main_stage/purchase_slot/ (FMainStagePurchaseSlotRequest {event}): first come, one slot per
// player per event, with a cooldown across events, refused when the buyer and a booked headliner have blocked
// each other. Slots cost nothing: balances never go down. The client treats any reply without an error as a
// purchase (_OnTryPurchaseSlotComplete ignores event_purchased), so every refusal must be an error.
func (s *Server) msPurchaseSlot(q *request) (any, error) {
	if !s.cfg.MainStage.Enabled {
		return nil, errPurchase(codeNotAvailable, "the main stage is not enabled")
	}
	var req struct {
		Event string `json:"event"`
	}
	if err := q.decode(&req); err != nil {
		return nil, err
	}
	ctx, buyer := q.Context(), q.playerID()
	p, err := s.store.Player(ctx, buyer)
	if err != nil {
		return nil, err
	}
	if p.Banned {
		return nil, errPurchase(codeMainStageBan, "banned from the main stage")
	}
	now := s.clock()
	sched, booked, err := s.mainStageSchedule(ctx, now)
	if err != nil {
		return nil, err
	}
	next := sched.Next
	if next == nil || !strings.EqualFold(next.UUID, strings.TrimSpace(req.Event)) {
		return nil, errPurchase(codeNotAvailable, "not the next event")
	}
	if !s.modded(buyer) && !next.AllowStock { // the stock / modded split (loader.go)
		s.logs.For(logx.MainStage).Info("headliner slot refused", "event", next.UUID, "player", s.logs.TagID(buyer),
			"reason", "stock player; the event doesn't allow stock players")
		return nil, errPurchase(codeNotAvailable, "this event is for loader users")
	}
	key := strings.ToLower(next.UUID)
	for _, h := range booked[key] {
		if h.PlayerID == buyer {
			return nil, errPurchase(codeAlreadyHeadliner, "already a headliner of this event")
		}
	}
	if next.State != mainstage.NextOpen {
		return nil, errPurchase(codeNotAvailable, "the event is not open for purchase")
	}
	if cooling, err := s.inMainStageCooldown(ctx, next.Event, buyer); err != nil {
		return nil, err
	} else if cooling {
		return nil, errPurchase(codeNotAvailable, "headlined a recent event")
	}
	blocked, err := s.store.BlockedEitherWay(ctx, buyer)
	if err != nil {
		return nil, err
	}
	for _, h := range booked[key] {
		if blocked[h.PlayerID] {
			return nil, errPurchase(codeBlockedHeadliner, "a block exists with a headliner of this event")
		}
	}
	slot, err := s.store.AddMainStageHeadliner(ctx, next.UUID, next.Slots, buyer, 0, store.HeadlinerPurchase)
	switch {
	case errors.Is(err, store.ErrAlreadyHeadliner):
		return nil, errPurchase(codeAlreadyHeadliner, "already a headliner of this event")
	case errors.Is(err, store.ErrEventFull):
		return nil, errPurchase(codeNotAvailable, "every slot is taken")
	case err != nil:
		return nil, err
	}
	s.ms.mu.Lock()
	s.ms.snap = nil // the hub shows the new headliner on its next poll
	s.ms.mu.Unlock()
	s.logs.For(logx.MainStage).Info("headliner slot claimed", "event", next.UUID, "slot", slot,
		"player", s.logs.TagID(buyer))
	return map[string]any{"event_purchased": true, "purchased_price": 0, "message": ""}, nil
}

// inMainStageCooldown reports whether the player headlines one of the main_stage.cooldown_events events before e.
func (s *Server) inMainStageCooldown(ctx context.Context, e mainstage.Event, player int64) (bool, error) {
	n := s.cfg.MainStage.CooldownEvents
	if n <= 0 {
		return false, nil
	}
	from := e.Start.Add(-7 * 24 * time.Hour)
	stored, err := s.store.MainStageEvents(ctx, from, e.Start)
	if err != nil {
		return false, err
	}
	removed, err := s.store.MainStageRemoved(ctx)
	if err != nil {
		return false, err
	}
	overrides, err := s.store.MainStageOverrideIDs(ctx)
	if err != nil {
		return false, err
	}
	var before []string
	for _, p := range mainstage.Merge(stored, s.cfg.MainStage.RotationSchedule(), from, e.Start) {
		if p.Purchasable() && !strings.EqualFold(p.UUID, e.UUID) && !removed[strings.ToLower(p.UUID)] && !(p.Rotation && overrides[strings.ToLower(p.UUID)]) {
			before = append(before, strings.ToLower(p.UUID))
		}
	}
	if len(before) > n {
		before = before[len(before)-n:]
	}
	booked, err := s.store.MainStageHeadliners(ctx, before)
	if err != nil {
		return false, err
	}
	for _, list := range booked {
		for _, h := range list {
			if h.PlayerID == player {
				return true, nil
			}
		}
	}
	return false, nil
}

// mainStageLive is the Main Stage server's newest player list; tests replace it through liveOverride.
func (s *Server) mainStageLive() (publichost.Live, bool) {
	if s.liveOverride != nil {
		return s.liveOverride()
	}
	if s.public == nil {
		return publichost.Live{}, false
	}
	return s.public.MainStageLive()
}
