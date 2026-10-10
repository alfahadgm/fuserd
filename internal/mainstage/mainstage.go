// Package mainstage is the Main Stage schedule: events stored by "fuserd mainstage" merged
// with a fixed rotation, and the event states the client's schedule screens show. It has no storage or HTTP of its
// own, so the state rules can be tested on their own.
package mainstage

import (
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Event types, as the client's event_type strings (FUN_1406fbaa0 maps them to EMainStageEventType).
const (
	TypeEvent        = "event"
	TypeSpecialEvent = "special_event"
	TypeMaintenance  = "maintenance"
)

// States are EMainStageEventState names; the client's JSON converter reads an enum by its name.
type State string

const (
	Current    State = "CURRENT"
	NextOpen   State = "NEXT_OPEN"
	NextClosed State = "NEXT_CLOSED"
	Upcoming   State = "UPCOMING"
	Ended      State = "ENDED"
	Offline    State = "OFFLINE" // no next event at all
)

// Event is one Main Stage event. Slots is its headliner slots; TurnMinutes optionally limits each turn (0 = none): FDS
// splits the event's time evenly between the headliners present, in whole 32-bar blocks.
type Event struct {
	UUID               string
	Name               string
	Type               string
	Start, End         time.Time
	SpecialName        string
	SpecialTypeName    string
	SpecialDescription string
	ImageURL           string // one static image, served at every size
	Slots              int
	TurnMinutes        int
	// AllowStock lets players without the loader join the Main Stage and book a slot during this event (stock and
	// modded players are otherwise kept apart: fuserd's loader split).
	AllowStock bool
	Rotation   bool // generated from the rotation, not stored
}

// Overlaps reports whether two events share any time. Touching events (one ends as the next starts) don't overlap.
func (e Event) Overlaps(o Event) bool { return e.Start.Before(o.End) && o.Start.Before(e.End) }

// Purchasable reports whether players can claim headliner slots on the event at all.
func (e Event) Purchasable() bool { return e.Type != TypeMaintenance && e.Slots > 0 }

// Limits bound what an operator may store.
const (
	MaxSlots       = 8
	MaxTurnMinutes = 60
	MaxDuration    = 12 * time.Hour
)

// Validate checks an event before it is stored.
func (e Event) Validate() error {
	switch {
	case !IsGUID(e.UUID):
		return fmt.Errorf("uuid %q is not a GUID", e.UUID)
	case strings.TrimSpace(e.Name) == "":
		return errors.New("name is required")
	case e.Type != TypeEvent && e.Type != TypeSpecialEvent && e.Type != TypeMaintenance:
		return fmt.Errorf("type %q: must be %s, %s or %s", e.Type, TypeEvent, TypeSpecialEvent, TypeMaintenance)
	case !e.End.After(e.Start):
		return errors.New("the end must be after the start")
	case e.End.Sub(e.Start) > MaxDuration:
		return fmt.Errorf("an event lasts at most %s", MaxDuration)
	case e.Type != TypeMaintenance && (e.Slots < 1 || e.Slots > MaxSlots):
		return fmt.Errorf("slots must be between 1 and %d", MaxSlots)
	case e.Type != TypeMaintenance && (e.TurnMinutes < 0 || e.TurnMinutes > MaxTurnMinutes):
		return fmt.Errorf("turn minutes must be between 0 (no limit) and %d", MaxTurnMinutes)
	}
	return nil
}

// Rotation generates the always-on events: one every Interval from Anchor, each lasting
// Duration. A stored event (including maintenance) replaces every rotation event it overlaps.
type Rotation struct {
	Enabled     bool
	Anchor      time.Time
	Interval    time.Duration
	Duration    time.Duration
	Name        string
	ImageURL    string
	Slots       int
	TurnMinutes int
	AllowStock  bool
}

// Window is when headliner slots can be claimed, relative to the event start: from OpenBefore until CloseBefore.
type Window struct {
	OpenBefore  time.Duration
	CloseBefore time.Duration
}

// OpenAt reports whether the purchase window of an event starting at start is open at now.
func (w Window) OpenAt(start, now time.Time) bool {
	return !now.Before(start.Add(-w.OpenBefore)) && now.Before(start.Add(-w.CloseBefore))
}

// SecondsUntilOpen is FMainStageUpcomingEvent.time_to_purchase: 0 once the window has opened.
func (w Window) SecondsUntilOpen(start, now time.Time) int {
	if d := start.Add(-w.OpenBefore).Sub(now); d > 0 {
		return int((d + time.Second - 1) / time.Second)
	}
	return 0
}

// Merge returns the events that end after from and start before to: the stored ones plus the rotation events that
// no stored event overlaps, sorted by start.
func Merge(stored []Event, rot Rotation, from, to time.Time) []Event {
	var out []Event
	for _, e := range stored {
		if e.End.After(from) && e.Start.Before(to) {
			out = append(out, e)
		}
	}
	if rot.Enabled && rot.Interval > 0 && rot.Duration > 0 {
		// The first occurrence that can still be running at from.
		k := int64(from.Add(-rot.Duration).Sub(rot.Anchor) / rot.Interval)
		if from.Add(-rot.Duration).Before(rot.Anchor) {
			k-- // integer division truncates towards zero
		}
		for ; ; k++ {
			start := rot.Anchor.Add(time.Duration(k) * rot.Interval)
			if !start.Before(to) {
				break
			}
			e := Event{UUID: RotationUUID(start), Name: rot.Name, Type: TypeEvent, Start: start,
				End: start.Add(rot.Duration), ImageURL: rot.ImageURL, Slots: rot.Slots, TurnMinutes: rot.TurnMinutes,
				AllowStock: rot.AllowStock, Rotation: true}
			if !e.End.After(from) {
				continue
			}
			replaced := false
			for _, s := range stored {
				if strings.EqualFold(s.UUID, e.UUID) || s.Overlaps(e) {
					replaced = true
					break
				}
			}
			if !replaced {
				out = append(out, e)
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Start.Before(out[j].Start) })
	return out
}

// Classified is an event with its state at one moment.
type Classified struct {
	Event
	State State
}

// Schedule is the schedule as the client sees it at one moment.
type Schedule struct {
	Current  *Classified // running now, if any
	Next     *Classified // the first event that hasn't started, if any
	Upcoming []Classified
	// Held is the event the game is shown as its current event between sets, until HeldUntil (see Hold). It keeps its
	// real times. It is for the game's view only: the FDS feed, admission and the website use Current.
	Held      *Classified
	HeldUntil time.Time
}

// Classify finds the current and next events and the upcoming ones after them (at most maxUpcoming). The next event
// is NEXT_OPEN while its purchase window is open, it takes headliners and full doesn't report it full; otherwise it
// is NEXT_CLOSED. events must be sorted by start (Merge does it).
func Classify(events []Event, now time.Time, w Window, maxUpcoming int, full func(Event) bool) Schedule {
	var s Schedule
	for _, e := range events {
		switch {
		case !now.Before(e.End):
			// ENDED events aren't shown: the client drops them 30 minutes after their end anyway.
		case !now.Before(e.Start):
			if s.Current == nil {
				s.Current = &Classified{e, Current}
			}
		case s.Next == nil:
			state := NextClosed
			if e.Purchasable() && w.OpenAt(e.Start, now) && (full == nil || !full(e)) {
				state = NextOpen
			}
			s.Next = &Classified{e, state}
		case len(s.Upcoming) < maxUpcoming:
			s.Upcoming = append(s.Upcoming, Classified{e, Upcoming})
		}
	}
	return s
}

// Hold returns the event the game should keep seeing as its current event between sets, or nil. Without a current
// event the game's Main Stage tiles (main menu and hub) show no text and a stale image or a spinner, so the last
// event that ended is reported as still running. The rules (MS_EVENT_IMAGES, 2026-10-10):
//   - only when no event is running and a next event exists;
//   - only the most recent event that ended, and never a maintenance event (a current maintenance event makes the
//     game refuse to join), so a maintenance window that just ended holds nothing;
//   - until the next event starts, and at most max after the event's real end (max <= 0 turns holding off).
//
// until is when the hold ends; the game is sent it as the event's end. The game keeps two events whose times only
// touch, so ending the hold exactly at the next start is safe. events must be sorted by start (Merge does it) and
// include the events that ended within max before now.
func Hold(events []Event, now time.Time, max time.Duration, s Schedule) (held *Classified, until time.Time) {
	if max <= 0 || s.Current != nil || s.Next == nil {
		return nil, time.Time{}
	}
	var last *Event
	for i := range events {
		if e := &events[i]; !now.Before(e.End) && (last == nil || !e.End.Before(last.End)) {
			last = e
		}
	}
	if last == nil || last.Type == TypeMaintenance {
		return nil, time.Time{}
	}
	until = last.End.Add(max)
	if s.Next.Start.Before(until) {
		until = s.Next.Start
	}
	if !now.Before(until) {
		return nil, time.Time{}
	}
	return &Classified{*last, Current}, until
}

// StateAt is one event's state at now, for listings: ENDED, CURRENT, or the state Classify gives it.
func (s Schedule) StateAt(e Event, now time.Time) State {
	switch {
	case !now.Before(e.End):
		return Ended
	case s.Current != nil && s.Current.UUID == e.UUID:
		return Current
	case s.Next != nil && s.Next.UUID == e.UUID:
		return s.Next.State
	}
	return Upcoming
}

// IsGUID reports whether s has the 8-4-4-4-12 hex form that the client's FGuid parse accepts.
func IsGUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !strings.ContainsRune("0123456789abcdefABCDEF", c) {
				return false
			}
		}
	}
	return true
}

func formatGUID(b []byte) string {
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// NewUUID is a random (version 4) GUID for a stored event.
func NewUUID() string {
	var b [16]byte
	rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return formatGUID(b[:])
}

// RotationUUID is the stable GUID of the rotation event starting at start, so headliners assigned to it stay with it
// across restarts.
func RotationUUID(start time.Time) string {
	return DerivedUUID("mainstage-rotation", start.UTC().Format(time.RFC3339))
}

// DerivedUUID is a GUID derived from a namespace and a name (such as an event's image).
func DerivedUUID(namespace, name string) string {
	sum := sha256.Sum256([]byte(namespace + "\x00" + name))
	b := sum[:16]
	b[6] = b[6]&0x0f | 0x50
	b[8] = b[8]&0x3f | 0x80
	return formatGUID(b)
}
