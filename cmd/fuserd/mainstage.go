package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"fuseronline/internal/config"
	"fuseronline/internal/mainstage"
	"fuseronline/internal/store"
)

const mainStageUsage = `usage:
  fuserd mainstage add         -config config.json -name <name> -start <time> [-end <time> | -minutes <n>]
                               [-type event|special_event] [-special-name <s>] [-special-type <s>] [-description <s>]
                               [-slots <n>] [-turn-minutes <n>] [-image-url <url>] [-allow-stock]
  fuserd mainstage maintenance -config config.json -start <time> [-end <time> | -minutes <n>] [-name <name>]
  fuserd mainstage remove      -config config.json -event <uuid>
  fuserd mainstage list        -config config.json [-hours <n>]
  fuserd mainstage assign      -config config.json -event <uuid> -player <id | hype username#NNNN> [-slot <n>]
  fuserd mainstage unassign    -config config.json -event <uuid> -player <id | hype username#NNNN>

The Main Stage schedule is the rotation in config.json's main_stage section plus the events
stored here. A stored event, maintenance included, replaces every rotation event it overlaps; stored events can't
overlap each other. Times are RFC 3339 (2026-10-08T20:00:00Z). An event's length defaults to main_stage.event_seconds,
its slots to main_stage.slots. The Main Stage server splits an event's time evenly between the headliners present, in
whole 32-bar blocks; -turn-minutes (default main_stage.turn_minutes, 0 = none) limits each turn. Only players with the
loader may join or headline an event unless it was added with -allow-stock (rotation events:
main_stage.rotation.allow_stock).

assign books a headliner slot on any event that hasn't ended, rotation events included (list shows their UUIDs). It
overrides first come: the purchase window, the cooldown and blocks don't apply. A running fuserd
sees changes within a second.

Between sets the game is shown the last event that ended as still running, until the next event starts and for at
most main_stage.hold_current_minutes (0 = off); list shows it as HELD. Maintenance windows are never held.`

// mainStageCommand is "fuserd mainstage ...": maintenance of the Main Stage schedule.
func mainStageCommand(args []string, out io.Writer) error {
	return mainStageCommandAt(args, out, time.Now)
}

func mainStageCommandAt(args []string, out io.Writer, clock func() time.Time) error {
	if len(args) == 0 {
		return errors.New(mainStageUsage)
	}
	cmd := args[0]
	fs := flag.NewFlagSet("mainstage "+cmd, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	configPath := fs.String("config", "config.json", "path to the configuration file")
	var name, start, end, typ, specialName, specialType, description, imageURL, eventID, player *string
	var minutes, slots, turn, slot, hours *int
	var allowStock *bool
	switch cmd {
	case "add", "maintenance":
		defName := ""
		if cmd == "maintenance" {
			defName = "Maintenance"
		}
		name = fs.String("name", defName, "the event's name")
		start = fs.String("start", "", "start time (RFC 3339)")
		end = fs.String("end", "", "end time (RFC 3339)")
		minutes = fs.Int("minutes", 0, "length in minutes, instead of -end")
		if cmd == "add" {
			typ = fs.String("type", mainstage.TypeEvent, "event or special_event")
			specialName = fs.String("special-name", "", "special event name")
			specialType = fs.String("special-type", "", "special event type name")
			description = fs.String("description", "", "special event description")
			slots = fs.Int("slots", 0, "headliner slots (default main_stage.slots)")
			turn = fs.Int("turn-minutes", -1, "limit each turn to this many minutes (0 = no limit; default main_stage.turn_minutes)")
			imageURL = fs.String("image-url", "", "static event image URL")
			allowStock = fs.Bool("allow-stock", false, "let players without the loader join and headline this event")
		}
	case "remove":
		eventID = fs.String("event", "", "the event's UUID")
	case "list":
		hours = fs.Int("hours", 24, "how far ahead to list")
	case "assign", "unassign":
		eventID = fs.String("event", "", "the event's UUID")
		player = fs.String("player", "", "player ID or hype username with its #NNNN suffix")
		if cmd == "assign" {
			slot = fs.Int("slot", 0, "slot number (default: the first free one)")
		}
	default:
		return errors.New(mainStageUsage)
	}
	if err := fs.Parse(args[1:]); err != nil || fs.NArg() > 0 {
		return errors.New(mainStageUsage)
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	st, err := store.Open(cfg.DatabasePath)
	if err != nil {
		return err
	}
	defer st.Close()
	ctx := context.Background()
	ms := cfg.MainStage
	now := clock()

	switch cmd {
	case "add", "maintenance":
		e := mainstage.Event{UUID: mainstage.NewUUID(), Name: *name, Type: mainstage.TypeMaintenance}
		if e.Start, err = time.Parse(time.RFC3339, *start); err != nil {
			return fmt.Errorf("-start: %w", err)
		}
		e.Start = e.Start.UTC()
		switch {
		case *end != "" && *minutes != 0:
			return errors.New("give -end or -minutes, not both")
		case *end != "":
			if e.End, err = time.Parse(time.RFC3339, *end); err != nil {
				return fmt.Errorf("-end: %w", err)
			}
			e.End = e.End.UTC()
		case *minutes != 0:
			e.End = e.Start.Add(time.Duration(*minutes) * time.Minute)
		case cmd == "maintenance":
			return errors.New("maintenance needs -end or -minutes")
		default:
			e.End = e.Start.Add(time.Duration(ms.EventSeconds) * time.Second)
		}
		if cmd == "add" {
			e.Type, e.SpecialName, e.SpecialTypeName = *typ, *specialName, *specialType
			e.SpecialDescription, e.ImageURL, e.AllowStock = *description, *imageURL, *allowStock
			e.Slots, e.TurnMinutes = ms.Slots, ms.TurnMinutes
			if *slots != 0 {
				e.Slots = *slots
			}
			if *turn >= 0 {
				e.TurnMinutes = *turn
			}
			if e.Type == mainstage.TypeMaintenance {
				return errors.New("use maintenance for maintenance windows")
			}
		}
		if !e.End.After(now) {
			return errors.New("the event would already have ended")
		}
		if err := st.AddMainStageEvent(ctx, e); err != nil {
			return err
		}
		replaced := 0
		for _, r := range mainstage.Merge(nil, ms.RotationSchedule(), e.Start, e.End) {
			if r.Overlaps(e) {
				replaced++
			}
		}
		fmt.Fprintf(out, "added %s %s %q %s - %s", e.UUID, e.Type, e.Name, e.Start.Format(time.RFC3339),
			e.End.Format(time.RFC3339))
		if replaced > 0 {
			fmt.Fprintf(out, " (replaces %d rotation event(s))", replaced)
		}
		fmt.Fprintln(out)
		return nil

	case "remove":
		if err := st.RemoveMainStageEvent(ctx, strings.ToLower(*eventID)); err != nil {
			return fmt.Errorf("%s: %w", *eventID, err)
		}
		fmt.Fprintf(out, "removed %s and its headliners\n", *eventID)
		return nil

	case "list":
		return listMainStage(ctx, st, ms, now, time.Duration(*hours)*time.Hour, out)
	}

	// assign / unassign
	p, err := findPlayer(ctx, st, *player)
	if err != nil {
		return err
	}
	if cmd == "unassign" {
		if err := st.RemoveMainStageHeadliner(ctx, strings.ToLower(*eventID), p.ID); err != nil {
			return fmt.Errorf("%s on %s: %w", p.HypeUsername, *eventID, err)
		}
		fmt.Fprintf(out, "unassigned %s from %s\n", p.HypeUsername, *eventID)
		return nil
	}
	e, err := findMainStageEvent(ctx, st, ms, now, *eventID)
	if err != nil {
		return err
	}
	if !e.Purchasable() {
		return fmt.Errorf("%s is a maintenance window", e.UUID)
	}
	n, err := st.AddMainStageHeadliner(ctx, e.UUID, e.Slots, p.ID, *slot, store.HeadlinerAdmin)
	if err != nil {
		return fmt.Errorf("%s on %s: %w", p.HypeUsername, e.UUID, err)
	}
	fmt.Fprintf(out, "assigned %s to slot %d of %d on %s %q (%s)\n", p.HypeUsername, n, e.Slots, e.UUID, e.Name,
		e.Start.Format(time.RFC3339))
	return nil
}

// mainStageEvents is the merged schedule from now to now+ahead, including the event running now.
func mainStageEvents(ctx context.Context, st *store.Store, ms config.MainStageConfig, now time.Time,
	ahead time.Duration) ([]mainstage.Event, error) {
	stored, err := st.MainStageEvents(ctx, now.Add(-mainstage.MaxDuration), now.Add(ahead))
	if err != nil {
		return nil, err
	}
	return mainstage.Merge(stored, ms.RotationSchedule(), now, now.Add(ahead)), nil
}

// findMainStageEvent finds an event that hasn't ended, within the next 30 days.
func findMainStageEvent(ctx context.Context, st *store.Store, ms config.MainStageConfig, now time.Time,
	uuid string) (mainstage.Event, error) {
	events, err := mainStageEvents(ctx, st, ms, now, 30*24*time.Hour)
	if err != nil {
		return mainstage.Event{}, err
	}
	for _, e := range events {
		if strings.EqualFold(e.UUID, uuid) {
			return e, nil
		}
	}
	return mainstage.Event{}, fmt.Errorf("no event %s that hasn't ended in the next 30 days (see mainstage list)", uuid)
}

func findPlayer(ctx context.Context, st *store.Store, who string) (*store.Player, error) {
	var p *store.Player
	var err error
	if id, perr := strconv.ParseInt(who, 10, 64); perr == nil {
		p, err = st.Player(ctx, id)
	} else {
		p, err = st.PlayerByHypeUsername(ctx, who)
	}
	if err != nil {
		return nil, fmt.Errorf("player %q: %w", who, err)
	}
	return p, nil
}

func listMainStage(ctx context.Context, st *store.Store, ms config.MainStageConfig, now time.Time, ahead time.Duration,
	out io.Writer) error {
	events, err := mainStageEvents(ctx, st, ms, now, ahead)
	if err != nil {
		return err
	}
	// Between sets the game is shown the last event that ended as still running (main_stage.hold_current_minutes,
	// mainstage.Hold); it is listed first, as HELD.
	recent, err := mainStageEvents(ctx, st, ms, now.Add(-ms.HoldCurrent()), ahead+ms.HoldCurrent())
	if err != nil {
		return err
	}
	held, until := mainstage.Hold(recent, now, ms.HoldCurrent(),
		mainstage.Classify(events, now, ms.PurchaseWindow(), len(events), nil))
	if held != nil {
		events = append([]mainstage.Event{held.Event}, events...)
	}
	uuids := make([]string, len(events))
	for i, e := range events {
		uuids[i] = strings.ToLower(e.UUID)
	}
	booked, err := st.MainStageHeadliners(ctx, uuids)
	if err != nil {
		return err
	}
	full := func(e mainstage.Event) bool { return len(booked[strings.ToLower(e.UUID)]) >= e.Slots }
	sched := mainstage.Classify(events, now, ms.PurchaseWindow(), len(events), full)
	if !ms.Enabled {
		fmt.Fprintln(out, "note: main_stage.enabled is false; clients don't see this schedule")
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "START\tEND\tSTATE\tTYPE\tSLOTS\tNAME\tUUID")
	for _, e := range events {
		source := ""
		if e.Rotation {
			source = " (rotation)"
		}
		if e.AllowStock {
			source += " (stock allowed)"
		}
		hl := booked[strings.ToLower(e.UUID)]
		state := string(sched.StateAt(e, now))
		if held != nil && e.UUID == held.UUID {
			state = "HELD until " + until.Format(time.RFC3339)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d/%d\t%s%s\t%s\n", e.Start.Format(time.RFC3339), e.End.Format(time.RFC3339),
			state, e.Type, len(hl), e.Slots, e.Name, source, e.UUID)
		for _, h := range hl {
			name := strconv.FormatInt(h.PlayerID, 10)
			if p, err := st.Player(ctx, h.PlayerID); err == nil {
				name = p.HypeUsername
			}
			fmt.Fprintf(tw, "\t\t\t\t\tslot %d: %s (%s)\t\n", h.Slot, name, h.Source)
		}
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if ms.HoldCurrentMinutes > 0 {
		fmt.Fprintf(out, "\nBetween sets the game is shown the last event that ended as still running, until the next "+
			"event starts and for at most %d min (main_stage.hold_current_minutes). Maintenance windows are never held, "+
			"and the Main Stage server, admission and the website follow the real times.\n", ms.HoldCurrentMinutes)
	}
	return nil
}
