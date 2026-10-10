package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"fuseronline/internal/config"
	"fuseronline/internal/mainstage"
	"fuseronline/internal/platform"
	"fuseronline/internal/publichost"
	"fuseronline/internal/store"
)

// rawGet fetches a URL the way the polling jobs do and returns the whole JSON object.
func rawGet(t *testing.T, url, auth string) map[string]any {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var m map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		t.Fatalf("%s: %v", url, err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%s: status %d %v", url, resp.StatusCode, m)
	}
	return m
}

func TestMainStageDisabledPointsAtNop(t *testing.T) {
	srv := newTestServer(t)
	_, env := call(t, srv, "GET", "main_stage/refresh_urls/", "", nil)
	if !strings.Contains(string(env.Results), "nop/?from=events_url") {
		t.Fatalf("refresh urls: %s", env.Results)
	}
	if status, _ := call(t, srv, "GET", "mainstage/events/", "", nil); status != http.StatusNotFound {
		t.Fatalf("events while disabled: %d", status)
	}
}

func TestMainStageSchedule(t *testing.T) {
	t0 := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	srv, s := newTestServerAndAPI(t, func(c *config.Config) {
		c.MainStage.Enabled = true
		c.MainStage.Rotation.Anchor = t0.Format(time.RFC3339)
		c.MainStage.Rotation.IntervalMinutes = 120 // these tests assume a 2-hour rotation
		c.MainStage.Rotation.ImageURL = "https://img.example/ms.jpg"
	})
	s.cfg.PublicURL = srv.URL
	now := t0.Add(-30 * time.Minute) // the 12:00 rotation event is next and open for purchase
	s.now = func() time.Time { return now }
	ctx := context.Background()

	_, env := call(t, srv, "GET", "main_stage/refresh_urls/", "", nil)
	var urls map[string]string
	json.Unmarshal(env.Results, &urls)
	if urls["events_url"] != srv.URL+Prefix+"mainstage/events/" || urls["latest_spectators"] != srv.URL+Prefix+"mainstage/latest_spectators/" {
		t.Fatalf("refresh urls: %v", urls)
	}

	auth, me := login(t, srv, platform.Steam, "76561190000000001")
	next := mainstage.RotationUUID(t0)
	if _, err := s.store.AddMainStageHeadliner(ctx, next, 3, int64(me.UserID), 0, store.HeadlinerAdmin); err != nil {
		t.Fatal(err)
	}
	// Maintenance from 14:00 replaces that rotation event.
	maint := mainstage.Event{UUID: mainstage.NewUUID(), Name: "Maintenance", Type: mainstage.TypeMaintenance,
		Start: t0.Add(2 * time.Hour), End: t0.Add(3 * time.Hour)}
	if err := s.store.AddMainStageEvent(ctx, maint); err != nil {
		t.Fatal(err)
	}

	m := rawGet(t, urls["events_url"], auth)
	// Both envelope forms carry the same payload.
	results, _ := m["results"].(map[string]any)
	if results == nil || m["server_time"] != "2026-10-08T11:30:00.000Z" || results["server_time"] != m["server_time"] {
		t.Fatalf("envelope: %v", m)
	}
	nx := m["next_event"].(map[string]any)
	if nx["uuid"] != next || nx["state"] != "NEXT_OPEN" || nx["open_for_purchase"] != true || nx["headliner"] != true ||
		nx["start_time"] != "2026-10-08T12:00:00.000Z" || nx["end_time"] != "2026-10-08T12:29:10.000Z" ||
		nx["event_type"] != "event" || nx["time_to_start"] != float64(1800) || nx["current_price"] != float64(0) {
		t.Fatalf("next event: %v", nx)
	}
	hl := nx["headliners"].([]any)
	if len(hl) != 1 || hl[0].(map[string]any)["event_slot"] != float64(1) || hl[0].(map[string]any)["user_id"] != float64(me.UserID) {
		t.Fatalf("headliners: %v", hl)
	}
	if imgs := nx["images"].([]any); len(imgs) != 1 || !mainstage.IsGUID(imgs[0].(map[string]any)["uuid"].(string)) {
		t.Fatalf("images: %v", imgs)
	}
	if cur := m["current_event"].(map[string]any); cur["uuid"] != "" || cur["event_type"] != "" {
		t.Fatalf("current event before the first one: %v", cur)
	}
	up := m["upcoming_events"].([]any)
	if len(up) != 6 {
		t.Fatalf("upcoming: %d", len(up))
	}
	first := up[0].(map[string]any)
	if first["uuid"] != maint.UUID || first["event_type"] != "maintenance" || first["state"] != "UPCOMING" ||
		first["time_to_purchase"] != float64(30*60) {
		t.Fatalf("first upcoming: %v", first)
	}
	// Without a login the viewer is nobody's headliner.
	if nx := rawGet(t, urls["next_event_url"], "")["next_event"]; nx != nil {
		t.Fatal("next_event_url must be the bare FMainStageNextEventResponse")
	}
	if nx := rawGet(t, urls["next_event_url"], ""); nx["headliner"] != false || nx["uuid"] != next {
		t.Fatalf("anonymous next event: %v", nx)
	}

	// During the event: current, with the next one (maintenance) closed for purchase. The snapshot is rebuilt
	// once the clock moves on by a second.
	now = t0.Add(10 * time.Minute)
	cur := rawGet(t, urls["current_event_url"], auth)
	if cur["uuid"] != next || len(cur["headliners"].([]any)) != 1 {
		t.Fatalf("current event: %v", cur)
	}
	if nx := rawGet(t, urls["next_event_url"], auth); nx["uuid"] != maint.UUID || nx["state"] != "NEXT_CLOSED" ||
		nx["event_type"] != "maintenance" || nx["open_for_purchase"] != false {
		t.Fatalf("maintenance next: %v", nx)
	}
	h := rawGet(t, urls["headliners_url"], auth)
	if h["current_event"].(map[string]any)["uuid"] != next || len(h["next_event"].(map[string]any)["headliners"].([]any)) != 0 {
		t.Fatalf("headliners response: %v", h)
	}
	sp := rawGet(t, urls["latest_spectators"], "")
	if sp["count"] != float64(0) || sp["refresh_url"] != urls["latest_spectators"] {
		t.Fatalf("spectators: %v", sp)
	}
}

func TestMainStageFullEventCloses(t *testing.T) {
	t0 := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	srv, s := newTestServerAndAPI(t, func(c *config.Config) {
		c.MainStage.Enabled = true
		c.MainStage.Slots = 1
		c.MainStage.Rotation.Anchor = t0.Format(time.RFC3339)
		c.MainStage.Rotation.IntervalMinutes = 120 // these tests assume a 2-hour rotation
	})
	s.now = func() time.Time { return t0.Add(-time.Hour) }
	_, me := login(t, srv, platform.Steam, "76561190000000002")
	if _, err := s.store.AddMainStageHeadliner(context.Background(), mainstage.RotationUUID(t0), 1, int64(me.UserID), 0,
		store.HeadlinerPurchase); err != nil {
		t.Fatal(err)
	}
	m := rawGet(t, srv.URL+Prefix+"mainstage/next_event/", "")
	if m["state"] != "NEXT_CLOSED" || m["open_for_purchase"] != false {
		t.Fatalf("full event: %v", m)
	}
}

func TestMainStagePurchaseSlot(t *testing.T) {
	t0 := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	srv, s := newTestServerAndAPI(t, func(c *config.Config) {
		c.MainStage.Enabled = true
		c.MainStage.Slots = 2
		c.MainStage.Rotation.Anchor = t0.Format(time.RFC3339)
		c.MainStage.Rotation.IntervalMinutes = 120 // these tests assume a 2-hour rotation
	})
	now := t0.Add(-30 * time.Minute)
	s.now = func() time.Time { return now }
	ctx := context.Background()
	next := mainstage.RotationUUID(t0)
	type buyer struct {
		auth string
		id   int64
	}
	var p []buyer
	for i := 0; i < 5; i++ {
		auth, ar := login(t, srv, platform.Steam, "7656119000000010"+string(rune('0'+i)))
		reportLoader(t, srv, auth) // the rotation's events are for loader users (loader_test.go covers stock buyers)
		p = append(p, buyer{auth, int64(ar.UserID)})
	}
	buy := func(b buyer, event string) (int, string) {
		t.Helper()
		status, env := call(t, srv, "POST", "main_stage/purchase_slot/", b.auth, map[string]string{"event": event})
		if env.Error != nil {
			return status, env.Error.Code
		}
		var r map[string]any
		json.Unmarshal(env.Results, &r)
		if r["event_purchased"] != true || r["purchased_price"] != float64(0) {
			t.Fatalf("purchase result %v", r)
		}
		return status, ""
	}

	if status, _ := call(t, srv, "POST", "main_stage/purchase_slot/", "", map[string]string{"event": next}); status != http.StatusUnauthorized {
		t.Fatalf("anonymous purchase: %d", status)
	}
	if _, code := buy(p[0], mainstage.RotationUUID(t0.Add(2*time.Hour))); code != "event_not_available_for_purchase" {
		t.Fatalf("a later event: %q", code)
	}
	if status, code := buy(p[0], strings.ToUpper(next)); status != http.StatusOK || code != "" {
		t.Fatalf("first purchase: %d %q", status, code)
	}
	if nx := rawGet(t, srv.URL+Prefix+"mainstage/next_event/", p[0].auth); nx["headliner"] != true ||
		len(nx["headliners"].([]any)) != 1 {
		t.Fatalf("after the purchase: %v", nx)
	}
	if _, code := buy(p[0], next); code != "user_already_purchased_event" {
		t.Fatalf("second purchase: %q", code)
	}
	// A block either way with a booked headliner refuses the slot.
	if _, err := s.store.Block(ctx, p[0].id, p[2].id); err != nil {
		t.Fatal(err)
	}
	if _, code := buy(p[2], next); code != "blocked_user" {
		t.Fatalf("blocked buyer: %q", code)
	}
	// Headlining the previous event puts a player in the cooldown.
	if _, err := s.store.AddMainStageHeadliner(ctx, mainstage.RotationUUID(t0.Add(-2*time.Hour)), 2, p[3].id, 0,
		store.HeadlinerAdmin); err != nil {
		t.Fatal(err)
	}
	if _, code := buy(p[3], next); code != "event_not_available_for_purchase" {
		t.Fatalf("cooldown: %q", code)
	}
	if _, code := buy(p[1], next); code != "" {
		t.Fatalf("second slot: %q", code)
	}
	if _, code := buy(p[4], next); code != "event_not_available_for_purchase" {
		t.Fatalf("full event: %q", code)
	}
	booked, _ := s.store.MainStageHeadliners(ctx, []string{next})
	if l := booked[next]; len(l) != 2 || l[0].PlayerID != p[0].id || l[1].PlayerID != p[1].id || l[1].Source != store.HeadlinerPurchase {
		t.Fatalf("booked %+v", l)
	}
	// The window closes 5 minutes before the start.
	now = t0.Add(-4 * time.Minute)
	if err := s.store.RemoveMainStageHeadliner(ctx, next, p[1].id); err != nil {
		t.Fatal(err)
	}
	if _, code := buy(p[4], next); code != "event_not_available_for_purchase" {
		t.Fatalf("closed window: %q", code)
	}
}

func TestMainStageAdmission(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	var p msPasses
	p.give(7, "203.0.113.5", now.Add(10*time.Minute))
	pass, found := p.get(7)
	cases := []struct {
		name, address, want  string
		mainStage, found, ip bool
		at                   time.Time
	}{
		{"admitted", "203.0.113.5", "", true, true, true, now},
		{"mapped IPv6 and a port", "[::ffff:203.0.113.5]:7777", "", true, true, true, now},
		{"other address", "198.51.100.1", "pass from another address", true, true, true, now},
		{"other address without the IP check", "198.51.100.1", "", true, true, false, now},
		{"expired", "203.0.113.5", "pass expired", true, true, true, now.Add(11 * time.Minute)},
		{"no pass", "203.0.113.5", "no pass", true, false, true, now},
		{"another slot", "203.0.113.5", "not the main stage server", false, true, true, now},
	}
	for _, c := range cases {
		f := found && c.found
		if got := admitReason(c.mainStage, pass, f, c.at, c.ip, c.address); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
	if _, ok := p.get(8); ok {
		t.Error("pass for a player who never asked")
	}
}

func TestMainStageServersUnavailable(t *testing.T) {
	srv := newTestServerWith(t, func(c *config.Config) { c.MainStage.Enabled = true })
	auth, _ := login(t, srv, platform.Steam, "76561190000000300")
	if status, env := call(t, srv, "POST", "main_stage/servers/", auth, nil); status != http.StatusServiceUnavailable || env.Error == nil {
		t.Fatalf("without an FDS main stage server: %d %+v", status, env.Error)
	}
	if status, _ := call(t, srv, "POST", "main_stage/servers/", "", nil); status != http.StatusUnauthorized {
		t.Fatalf("anonymous: %d", status)
	}
}

// The Main Stage server's heartbeat reply carries the current and next event with the headliners in slot order.
func TestMainStageServerSchedule(t *testing.T) {
	t0 := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	srv, s := newTestServerAndAPI(t, func(c *config.Config) {
		c.MainStage.Enabled = true
		c.MainStage.Rotation.Anchor = t0.Format(time.RFC3339)
		c.MainStage.Rotation.IntervalMinutes = 120 // these tests assume a 2-hour rotation
	})
	now := t0.Add(10 * time.Minute)
	s.now = func() time.Time { return now }
	ctx := context.Background()
	_, a := login(t, srv, platform.Steam, "76561190000000401")
	_, b := login(t, srv, platform.Steam, "76561190000000402")
	next := mainstage.RotationUUID(t0.Add(2 * time.Hour))
	s.store.AddMainStageHeadliner(ctx, next, 3, int64(b.UserID), 1, store.HeadlinerAdmin)
	s.store.AddMainStageHeadliner(ctx, next, 3, int64(a.UserID), 2, store.HeadlinerPurchase)
	ms, err := s.fdsMainStageSchedule(httptest.NewRequest("POST", "/fds/v1/status", nil))
	if err != nil {
		t.Fatal(err)
	}
	if ms.ServerTimeMs != now.UnixMilli() || ms.Current == nil || ms.Current.StartMs != t0.UnixMilli() ||
		ms.Current.EndMs != t0.Add(1750*time.Second).UnixMilli() || len(ms.Current.Headliners) != 0 {
		t.Fatalf("current %+v", ms.Current)
	}
	if ms.Next == nil || ms.Next.UUID != next || ms.Next.Type != "event" || ms.Next.Slots != 4 || ms.Next.TurnMinutes != 0 ||
		len(ms.Next.Headliners) != 2 || ms.Next.Headliners[0] != int64(b.UserID) || ms.Next.Headliners[1] != int64(a.UserID) {
		t.Fatalf("next %+v", ms.Next)
	}
}

// The Main Stage server's live player list fills the headliners' current_dj / in_main_stage, the current
// headliner and the latest spectators.
func TestMainStageLiveState(t *testing.T) {
	t0 := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	srv, s := newTestServerAndAPI(t, func(c *config.Config) {
		c.MainStage.Enabled = true
		c.MainStage.Rotation.Anchor = t0.Format(time.RFC3339)
		c.MainStage.Rotation.IntervalMinutes = 120 // these tests assume a 2-hour rotation
	})
	now := t0.Add(5 * time.Minute)
	s.now = func() time.Time { return now }
	ctx := context.Background()
	var ids []int64
	for i := 0; i < 8; i++ {
		_, ar := login(t, srv, platform.Steam, "7656119000000050"+string(rune('0'+i)))
		ids = append(ids, int64(ar.UserID))
	}
	current := mainstage.RotationUUID(t0)
	s.store.AddMainStageHeadliner(ctx, current, 3, ids[0], 1, store.HeadlinerAdmin)
	s.store.AddMainStageHeadliner(ctx, current, 3, ids[1], 2, store.HeadlinerAdmin)
	members := []publichost.Member{{UserID: ids[0], Team: "dj"}, {UserID: ids[1], Team: "dj"}}
	for _, id := range ids[2:] {
		members = append(members, publichost.Member{UserID: id, Team: "spectator"})
	}
	s.liveOverride = func() (publichost.Live, bool) {
		return publichost.Live{Members: members, CurrentDJ: ids[1], Event: current, At: now}, true
	}
	cur := rawGet(t, srv.URL+Prefix+"mainstage/current_event/", "")
	hl := cur["headliners"].([]any)
	first, second := hl[0].(map[string]any), hl[1].(map[string]any)
	if first["current_dj"] != false || first["in_main_stage"] != true || second["current_dj"] != true {
		t.Fatalf("headliners %v", hl)
	}
	if ch, _ := cur["current_headliner"].(map[string]any); ch == nil || ch["user_id"] != float64(ids[1]) {
		t.Fatalf("current headliner %v", cur["current_headliner"])
	}
	sp := rawGet(t, srv.URL+Prefix+"mainstage/latest_spectators/", "")
	list := sp["latest_spectators"].([]any)
	if sp["count"] != float64(6) || len(list) != 5 || list[4].(map[string]any)["user_id"] != float64(ids[7]) ||
		list[0].(map[string]any)["user_id"] != float64(ids[3]) {
		t.Fatalf("spectators %v", sp)
	}
	page := s.frontpageDiamond(ctx)
	if !page.Active || !page.CountsAvailable || page.Viewers != 6 || page.Headliners != 2 || page.Image == "" {
		t.Fatalf("website event: %+v", page)
	}
	_, body := getPage(t, srv.URL+"/")
	for _, want := range []string{page.Image, "Headliners online", "<strong>6</strong>", "<strong>2</strong>"} {
		if !strings.Contains(body, want) {
			t.Fatalf("website lacks %q", want)
		}
	}
	// A heartbeat for another event must not supply this event's counts.
	s.liveOverride = func() (publichost.Live, bool) {
		return publichost.Live{Members: members, Event: "previous-event", At: now}, true
	}
	if page := s.frontpageDiamond(ctx); !page.Active || page.CountsAvailable {
		t.Fatalf("mismatched event: %+v", page)
	}
	// Without a report the flags stay off and the list empty.
	s.liveOverride = func() (publichost.Live, bool) { return publichost.Live{}, false }
	now = now.Add(2 * time.Second) // past the one-second snapshot
	if sp := rawGet(t, srv.URL+Prefix+"mainstage/latest_spectators/", ""); sp["count"] != float64(0) {
		t.Fatalf("no report: %v", sp)
	}
	if page := s.frontpageDiamond(ctx); !page.Active || page.CountsAvailable {
		t.Fatalf("missing heartbeat: %+v", page)
	}
}

func TestMainStageFull(t *testing.T) {
	live := publichost.Live{Members: make([]publichost.Member, 50)}
	if !mainStageFull(live, true, 50) || mainStageFull(live, true, 51) || mainStageFull(live, false, 50) || mainStageFull(live, true, 0) {
		t.Fatal("capacity check")
	}
}

// Between sets the game is shown the last event that ended as its current event (mainstage.Hold): until the next
// event starts, with its card but no headliners. The Main Stage server's feed keeps the real schedule, and a
// maintenance window is never held.
func TestMainStageHoldBetweenSets(t *testing.T) {
	t0 := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	srv, s := newTestServerAndAPI(t, func(c *config.Config) {
		c.MainStage.Enabled = true
		c.MainStage.Rotation.Anchor = t0.Format(time.RFC3339)
		c.MainStage.Rotation.IntervalMinutes = 120
		c.MainStage.HoldCurrentMinutes = 120
	})
	s.cfg.PublicURL = srv.URL
	now := t0.Add(40 * time.Minute) // the 12:00 event ended at 12:29:10; the next starts at 14:00
	s.now = func() time.Time { return now }
	ctx := context.Background()
	auth, me := login(t, srv, platform.Steam, "76561190000000501")
	ended := mainstage.RotationUUID(t0)
	if _, err := s.store.AddMainStageHeadliner(ctx, ended, 4, int64(me.UserID), 1, store.HeadlinerAdmin); err != nil {
		t.Fatal(err)
	}
	s.liveOverride = func() (publichost.Live, bool) {
		return publichost.Live{Event: ended, CurrentDJ: int64(me.UserID),
			Members: []publichost.Member{{UserID: int64(me.UserID), Team: "dj"}}}, true
	}
	base := srv.URL + Prefix

	cur := rawGet(t, base+"mainstage/current_event/", auth)
	if cur["uuid"] != ended || cur["start_time"] != "2026-10-08T12:00:00.000Z" || cur["end_time"] != "2026-10-08T14:00:00.000Z" ||
		len(cur["headliners"].([]any)) != 0 || cur["current_headliner"] != nil || len(cur["images"].([]any)) != 1 {
		t.Fatalf("held current event: %v", cur)
	}
	all := rawGet(t, base+"mainstage/events/", auth)
	if c := all["current_event"].(map[string]any); c["uuid"] != ended || c["end_time"] != "2026-10-08T14:00:00.000Z" {
		t.Fatalf("events current_event: %v", c)
	}
	if n := all["next_event"].(map[string]any); n["uuid"] != mainstage.RotationUUID(t0.Add(2*time.Hour)) {
		t.Fatalf("events next_event: %v", n)
	}
	if h := rawGet(t, base+"mainstage/headliners/", auth)["current_event"].(map[string]any); h["uuid"] != ended ||
		len(h["headliners"].([]any)) != 0 {
		t.Fatalf("headliners current_event: %v", h)
	}
	// The Main Stage server, admission and the website see no current event.
	ms, err := s.fdsMainStageSchedule(httptest.NewRequest("POST", "/fds/v1/status", nil))
	if err != nil {
		t.Fatal(err)
	}
	if ms.Current != nil || ms.Next == nil {
		t.Fatalf("FDS feed during a hold: current %+v next %+v", ms.Current, ms.Next)
	}
	// The control panel marks the held event and states the hold.
	state, err := s.controlState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	panel := state.(map[string]any)
	heldRows := 0
	for _, e := range panel["events"].([]controlEvent) {
		if e.HeldUntil != "" {
			heldRows++
			if !strings.EqualFold(e.ID, ended) || !strings.HasPrefix(e.HeldUntil, "2026-10-08T14:00") {
				t.Fatalf("panel held event: %+v", e)
			}
		}
	}
	if heldRows != 1 || panel["hold_current_minutes"] != 120 {
		t.Fatalf("panel: %d held rows, hold %v", heldRows, panel["hold_current_minutes"])
	}

	// A maintenance window that just ended holds nothing, even right after an event.
	maint := mainstage.Event{UUID: mainstage.NewUUID(), Name: "Maintenance", Type: mainstage.TypeMaintenance,
		Start: t0.Add(30 * time.Minute), End: t0.Add(45 * time.Minute)}
	if err := s.store.AddMainStageEvent(ctx, maint); err != nil {
		t.Fatal(err)
	}
	now = t0.Add(50 * time.Minute)
	if cur := rawGet(t, base+"mainstage/current_event/", auth); cur["uuid"] != "" {
		t.Fatalf("held after maintenance: %v", cur)
	}

	// Off: nothing is held.
	s.cfg.MainStage.HoldCurrentMinutes = 0
	if err := s.store.RemoveMainStageEvent(ctx, strings.ToLower(maint.UUID)); err != nil {
		t.Fatal(err)
	}
	now = t0.Add(55 * time.Minute)
	if cur := rawGet(t, base+"mainstage/current_event/", auth); cur["uuid"] != "" {
		t.Fatalf("held with the hold off: %v", cur)
	}
}
