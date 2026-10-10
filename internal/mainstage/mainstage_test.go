package mainstage

import (
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

func ev(uuid string, start, end time.Duration) Event {
	return Event{UUID: uuid, Name: uuid, Type: TypeEvent, Start: t0.Add(start), End: t0.Add(end), Slots: 3, TurnMinutes: 8}
}

func TestClassifyAcrossWindowEdges(t *testing.T) {
	w := Window{OpenBefore: 2 * time.Hour, CloseBefore: 5 * time.Minute}
	a := ev("a", 0, 30*time.Minute)
	b := ev("b", 2*time.Hour, 2*time.Hour+30*time.Minute)
	c := ev("c", 5*time.Hour, 5*time.Hour+30*time.Minute)
	events := []Event{a, b, c}
	cases := []struct {
		name          string
		now           time.Time
		current, next string
		nextState     State
		upcoming      int
	}{
		{"before everything", t0.Add(-3 * time.Hour), "", "a", NextClosed, 2},
		{"a's window opens", t0.Add(-2 * time.Hour), "", "a", NextOpen, 2},
		{"a's window last second", t0.Add(-5*time.Minute - time.Second), "", "a", NextOpen, 2},
		{"a's window closed", t0.Add(-5 * time.Minute), "", "a", NextClosed, 2},
		{"a starts", t0, "a", "b", NextOpen, 1}, // b's window opened exactly 2 h before it
		{"a's last second", t0.Add(30*time.Minute - time.Second), "a", "b", NextOpen, 1},
		{"a ended", t0.Add(30 * time.Minute), "", "b", NextOpen, 1},
		{"b's window closed", t0.Add(2*time.Hour - 5*time.Minute), "", "b", NextClosed, 1},
		{"b running, c not open yet", t0.Add(2*time.Hour + time.Minute), "b", "c", NextClosed, 0},
		{"after everything", t0.Add(6 * time.Hour), "", "", "", 0},
	}
	for _, tc := range cases {
		s := Classify(events, tc.now, w, 10, nil)
		cur, next := "", ""
		var state State
		if s.Current != nil {
			cur = s.Current.UUID
			if s.Current.State != Current {
				t.Errorf("%s: current state %s", tc.name, s.Current.State)
			}
		}
		if s.Next != nil {
			next, state = s.Next.UUID, s.Next.State
		}
		if cur != tc.current || next != tc.next || state != tc.nextState || len(s.Upcoming) != tc.upcoming {
			t.Errorf("%s: current %q next %q (%s) upcoming %d; want %q %q (%s) %d", tc.name, cur, next, state,
				len(s.Upcoming), tc.current, tc.next, tc.nextState, tc.upcoming)
		}
		for _, u := range s.Upcoming {
			if u.State != Upcoming {
				t.Errorf("%s: upcoming %s has state %s", tc.name, u.UUID, u.State)
			}
		}
	}
}

func TestClassifyClosedWhenFullOrMaintenance(t *testing.T) {
	w := Window{OpenBefore: time.Hour}
	now := t0.Add(-30 * time.Minute)
	full := func(e Event) bool { return e.UUID == "a" }
	if s := Classify([]Event{ev("a", 0, time.Hour)}, now, w, 5, full); s.Next.State != NextClosed {
		t.Errorf("full event: %s", s.Next.State)
	}
	m := ev("m", 0, time.Hour)
	m.Type = TypeMaintenance
	if s := Classify([]Event{m}, now, w, 5, nil); s.Next.State != NextClosed {
		t.Errorf("maintenance: %s", s.Next.State)
	}
	if s := Classify(nil, now, w, 5, nil); s.Current != nil || s.Next != nil {
		t.Errorf("empty schedule: %+v", s)
	}
	s := Classify([]Event{ev("a", 0, time.Hour), ev("b", 2*time.Hour, 3*time.Hour), ev("c", 4*time.Hour, 5*time.Hour)},
		now, w, 1, nil)
	if len(s.Upcoming) != 1 || s.Upcoming[0].UUID != "b" {
		t.Errorf("upcoming limit: %+v", s.Upcoming)
	}
	if got := s.StateAt(ev("x", -2*time.Hour, -time.Hour), now); got != Ended {
		t.Errorf("past event state %s", got)
	}
}

func TestMergeRotation(t *testing.T) {
	rot := Rotation{Enabled: true, Anchor: t0, Interval: 2 * time.Hour, Duration: 30 * time.Minute, Name: "Main Stage",
		Slots: 3, TurnMinutes: 8}
	got := Merge(nil, rot, t0.Add(10*time.Minute), t0.Add(6*time.Hour))
	// The 12:00 event is still running at 12:10; 18:00 starts at the window's end, so it is left out.
	want := []time.Time{t0, t0.Add(2 * time.Hour), t0.Add(4 * time.Hour)}
	if len(got) != len(want) {
		t.Fatalf("got %d events, want %d", len(got), len(want))
	}
	for i, e := range got {
		if !e.Start.Equal(want[i]) || !e.Rotation || e.UUID != RotationUUID(want[i]) || !IsGUID(e.UUID) {
			t.Errorf("event %d: %+v", i, e)
		}
	}
	// Occurrences before the anchor are generated the same way.
	early := Merge(nil, rot, t0.Add(-5*time.Hour), t0.Add(-time.Hour))
	if len(early) != 2 || !early[0].Start.Equal(t0.Add(-4*time.Hour)) || !early[1].Start.Equal(t0.Add(-2*time.Hour)) {
		t.Errorf("before the anchor: %+v", early)
	}

	// A stored event replaces the rotation events it overlaps and keeps the others.
	maint := ev(NewUUID(), 2*time.Hour+15*time.Minute, 3*time.Hour)
	maint.Type = TypeMaintenance
	got = Merge([]Event{maint}, rot, t0, t0.Add(6*time.Hour))
	if len(got) != 3 || got[1].UUID != maint.UUID || got[0].Start != t0 || got[2].Start != t0.Add(4*time.Hour) {
		t.Errorf("with maintenance: %+v", got)
	}
	if got := Merge([]Event{maint}, Rotation{}, t0.Add(4*time.Hour), t0.Add(6*time.Hour)); len(got) != 0 {
		t.Errorf("stored event outside the window: %+v", got)
	}
	if RotationUUID(t0) == RotationUUID(t0.Add(2*time.Hour)) || RotationUUID(t0) != RotationUUID(t0.In(time.Local)) {
		t.Error("rotation UUIDs must be stable per start and differ between starts")
	}
}

func TestValidate(t *testing.T) {
	ok := ev(NewUUID(), 0, time.Hour)
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	bad := map[string]func(*Event){
		"uuid":     func(e *Event) { e.UUID = "not-a-guid" },
		"name":     func(e *Event) { e.Name = " " },
		"type":     func(e *Event) { e.Type = "battle" },
		"order":    func(e *Event) { e.End = e.Start },
		"too long": func(e *Event) { e.End = e.Start.Add(13 * time.Hour) },
		"slots":    func(e *Event) { e.Slots = 0 },
		"turn":     func(e *Event) { e.TurnMinutes = 61 },
		"no turn":  func(e *Event) { e.TurnMinutes = -1 },
	}
	for name, f := range bad {
		e := ok
		f(&e)
		if e.Validate() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	m := ok
	m.Type, m.Slots, m.TurnMinutes = TypeMaintenance, 0, 0
	if err := m.Validate(); err != nil {
		t.Errorf("maintenance without slots: %v", err)
	}
	w := Window{OpenBefore: time.Hour}
	if got := w.SecondsUntilOpen(t0, t0.Add(-90*time.Minute)); got != 1800 {
		t.Errorf("seconds until open %d", got)
	}
	if got := w.SecondsUntilOpen(t0, t0.Add(-time.Minute)); got != 0 {
		t.Errorf("open window: %d", got)
	}
}

func TestMovedRotationOverrideKeepsOneUUID(t *testing.T) {
	anchor := time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC)
	rot := Rotation{Enabled: true, Anchor: anchor, Interval: time.Hour, Duration: 30 * time.Minute, Slots: 4}
	id := RotationUUID(anchor)
	moved := Event{UUID: id, Name: "Moved", Type: TypeEvent, Start: anchor.Add(3 * time.Hour), End: anchor.Add(210 * time.Minute), Slots: 4}
	events := Merge([]Event{moved}, rot, anchor, anchor.Add(4*time.Hour))
	count := 0
	for _, e := range events {
		if e.UUID == id {
			count++
			if e.Rotation {
				t.Fatal("original rotation occurrence survived its moved override")
			}
		}
	}
	if count != 1 {
		t.Fatalf("duplicate event UUID appeared %d times", count)
	}
}

func TestHold(t *testing.T) {
	w := Window{OpenBefore: 2 * time.Hour, CloseBefore: 5 * time.Minute}
	maint := func(uuid string, start, end time.Duration) Event {
		e := ev(uuid, start, end)
		e.Type, e.Slots = TypeMaintenance, 0
		return e
	}
	hold := func(events []Event, at time.Duration, max time.Duration) (string, time.Duration) {
		now := t0.Add(at)
		held, until := Hold(events, now, max, Classify(events, now, w, 6, nil))
		if held == nil {
			return "", 0
		}
		if held.State != Current || !held.End.Equal(events[indexOf(events, held.UUID)].End) {
			t.Fatalf("held %+v keeps its real times and is CURRENT", held)
		}
		return held.UUID, until.Sub(t0)
	}
	// a 12:00-12:30, b 13:00-13:30.
	pair := []Event{ev("a", 0, 30*time.Minute), ev("b", time.Hour, 90*time.Minute)}
	cases := []struct {
		name   string
		events []Event
		at     time.Duration
		max    time.Duration
		want   string
		until  time.Duration
	}{
		{"between sets: until the next start", pair, 40 * time.Minute, time.Hour, "a", time.Hour},
		{"capped at max after the real end", pair, 40 * time.Minute, 20 * time.Minute, "a", 50 * time.Minute},
		{"past the cap", pair, 55 * time.Minute, 20 * time.Minute, "", 0},
		{"off", pair, 40 * time.Minute, 0, "", 0},
		{"an event is running", pair, 10 * time.Minute, time.Hour, "", 0},
		{"no next event", pair, 100 * time.Minute, time.Hour, "", 0},
		{"nothing ended yet", pair[1:], 40 * time.Minute, time.Hour, "", 0},
		{"maintenance is never held", []Event{maint("m", 0, 30*time.Minute), ev("b", time.Hour, 90*time.Minute)},
			40 * time.Minute, time.Hour, "", 0},
		{"a maintenance window that ended after an event holds nothing",
			[]Event{ev("a", 0, 30*time.Minute), maint("m", 30*time.Minute, 40*time.Minute), ev("b", time.Hour, 90*time.Minute)},
			45 * time.Minute, time.Hour, "", 0},
		{"the next event may be maintenance", []Event{ev("a", 0, 30*time.Minute), maint("m", time.Hour, 90*time.Minute)},
			40 * time.Minute, time.Hour, "a", time.Hour},
		{"only the latest ended event", []Event{ev("x", -time.Hour, -30*time.Minute), ev("a", 0, 30*time.Minute),
			ev("b", time.Hour, 90*time.Minute)}, 40 * time.Minute, 2 * time.Hour, "a", time.Hour},
	}
	for _, c := range cases {
		if got, until := hold(c.events, c.at, c.max); got != c.want || until != c.until {
			t.Errorf("%s: held %q until +%s, want %q until +%s", c.name, got, until, c.want, c.until)
		}
	}
}

func indexOf(events []Event, uuid string) int {
	for i, e := range events {
		if e.UUID == uuid {
			return i
		}
	}
	return -1
}
