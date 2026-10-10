package songxfer

import (
	"strings"
	"testing"
)

func sum(c string) string { return strings.Repeat(c, 64) }

// Different files under one shortname (any case) are a conflict among members; skipped songs and non-members don't
// count, and a conflict is fresh once.
func TestConflicts(t *testing.T) {
	r, _ := newTest()
	r.ReportSongs(session, 1, SongReport{Have: map[string]string{"Song_A": sum("a"), "Song_B": sum("b"), "Official": sum("1")}})
	r.ReportSongs(session, 2, SongReport{Have: map[string]string{"song_a": strings.ToUpper(sum("a")), "SONG_B": sum("c"), "official": sum("2")}})
	r.ReportSongs(session, 3, SongReport{Have: map[string]string{"Song_A": sum("d")}}) // not a member
	r.ReportSongs(session, 4, SongReport{Have: map[string]string{"bad name!": sum("e"), "Song_C": "nothex"}})
	skip := func(x string) bool { return x == "official" }
	all, fresh := r.Conflicts(session, members(1, 2, 4), skip)
	if len(all) != 1 || all[0] != (Conflict{Song: "song_b", Versions: 2, Players: 2}) || len(fresh) != 1 {
		t.Fatalf("conflicts %+v, fresh %+v", all, fresh)
	}
	if all, fresh = r.Conflicts(session, members(1, 2, 3), skip); len(all) != 2 || all[0].Song != "song_a" || len(fresh) != 1 || fresh[0].Song != "song_a" {
		t.Fatalf("with player 3: %+v, fresh %+v", all, fresh)
	}
	if all, fresh = r.Conflicts(session, members(1, 2), skip); len(all) != 1 || len(fresh) != 0 {
		t.Fatalf("again: %+v, fresh %+v", all, fresh)
	}
	r.Drop(session)
	if all, _ = r.Conflicts(session, members(1, 2), skip); all == nil || len(all) != 0 {
		t.Fatalf("after Drop: %#v", all)
	}
}

// SessionSongs: known songs come from reports and transfers; missing ones from reports and failed transfers, until
// the member reports holding the song or leaves.
func TestSessionSongs(t *testing.T) {
	r, _ := newTest()
	r.ReportSongs(session, 1, SongReport{Have: map[string]string{"Song_A": sum("a")}, Missing: []string{"Song_M", "song_a"}})
	tr, err := r.Request(session, 2, path, []int64{1}, members(1, 2))
	if err != nil {
		t.Fatal(err)
	}
	known, missing := r.SessionSongs(session, members(1, 2))
	if len(known) != 3 || !known["song_a"] || !known["song_m"] || !known["jowo_seinthem_shpqn"] || len(missing) != 1 || !missing["song_m"] {
		t.Fatalf("known %v, missing %v", known, missing)
	}
	if _, err := r.Decline(session, tr.ID, 1, "not_found", members(1, 2)); err != nil {
		t.Fatal(err)
	}
	if _, missing = r.SessionSongs(session, members(1, 2)); !missing["jowo_seinthem_shpqn"] {
		t.Fatalf("the failed transfer's song: %v", missing)
	}
	if _, missing = r.SessionSongs(session, members(1)); missing["jowo_seinthem_shpqn"] {
		t.Fatalf("its requester left: %v", missing)
	}
	r.ReportSongs(session, 2, SongReport{Have: map[string]string{"JoWo_SeinThem_ShpQn": sum("f")}})
	if _, missing = r.SessionSongs(session, members(1, 2)); missing["jowo_seinthem_shpqn"] {
		t.Fatalf("the requester has it now: %v", missing)
	}
}
