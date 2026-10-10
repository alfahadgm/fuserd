package songxfer

import (
	"sort"
	"strings"
)

// ---- song versions -------------------------------------------------------------------------------------------
// Each player's loader lists in its poll the custom crate songs this machine holds, with each pak's sha256, and the
// crate songs whose fetch ended without them. Players holding different files under one shortname hear different
// stems (or miss cels) with nothing to say why: the song is a conflict, which every loader announces. The API also
// takes from the reports the songs not every member of a session can play (the FDS server's SpawnDisc guard).
// Shortnames are compared lower-case. A player whose loader sends no report (an older loader) holds nothing here.

// MaxSongReport bounds the songs one report may list; the rest are ignored.
const MaxSongReport = 512

// SongReport is what one player's loader says it holds and lacks, as sent (any case).
type SongReport struct {
	Have    map[string]string // shortname -> its pak's sha256 (64 hex digits)
	Missing []string          // shortnames its fetches ended without
}

type heldSongs struct {
	have    map[string]string // lower-case shortname -> lower-case sha256
	missing map[string]bool
}

// Conflict is a shortname the session's members hold as different files.
type Conflict struct {
	Song     string `json:"song"`     // lower-case
	Versions int    `json:"versions"` // distinct files
	Players  int    `json:"players"`  // members holding one
}

// validShort is the shortname rule of ShortName's <X>.
func validShort(x string) bool {
	if x == "" || len(x) > 96 {
		return false
	}
	for _, c := range x {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

// ReportSongs replaces a player's report for the session. Entries with a malformed shortname or hash are dropped.
// A song the player now holds no longer counts as one it failed to get.
func (r *Registry) ReportSongs(sessionID string, player int64, rep SongReport) {
	h := heldSongs{have: map[string]string{}, missing: map[string]bool{}}
	for x, sum := range rep.Have {
		if len(h.have) >= MaxSongReport {
			break
		}
		if validShort(x) && isHex64(sum) {
			h.have[strings.ToLower(x)] = strings.ToLower(sum)
		}
	}
	for _, x := range rep.Missing {
		if len(h.missing) >= MaxSongReport {
			break
		}
		if x = strings.ToLower(x); validShort(x) && h.have[x] == "" {
			h.missing[x] = true
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.held == nil {
		r.held = map[string]map[int64]heldSongs{}
	}
	if r.held[sessionID] == nil {
		r.held[sessionID] = map[int64]heldSongs{}
	}
	r.held[sessionID][player] = h
	for x := range h.have {
		delete(r.failed[sessionID][x], player)
	}
}

// noteFailed records that a transfer's requester went without its song. Callers hold mu.
func (r *Registry) noteFailed(t *Transfer) {
	if r.failed == nil {
		r.failed = map[string]map[string]map[int64]bool{}
	}
	songs := r.failed[t.SessionID]
	if songs == nil {
		songs = map[string]map[int64]bool{}
		r.failed[t.SessionID] = songs
	}
	x := strings.ToLower(t.Song)
	if songs[x] == nil {
		songs[x] = map[int64]bool{}
	}
	songs[x][t.Requester] = true
}

// Conflicts returns the session's songs that its members hold as different files, sorted, leaving out the songs
// skip names (official and catalogue songs: one file each by construction). fresh are the ones not returned for
// the session before, for the log.
func (r *Registry) Conflicts(sessionID string, isMember func(int64) bool, skip func(string) bool) (all, fresh []Conflict) {
	r.mu.Lock()
	defer r.mu.Unlock()
	sums := map[string]map[string]int{} // song -> sha256 -> players
	for id, h := range r.held[sessionID] {
		if !isMember(id) {
			continue
		}
		for x, sum := range h.have {
			if sums[x] == nil {
				sums[x] = map[string]int{}
			}
			sums[x][sum]++
		}
	}
	all = []Conflict{}
	for x, by := range sums {
		if len(by) < 2 || skip != nil && skip(x) {
			continue
		}
		c := Conflict{Song: x, Versions: len(by)}
		for _, n := range by {
			c.Players += n
		}
		all = append(all, c)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Song < all[j].Song })
	if r.seenConflicts == nil {
		r.seenConflicts = map[string]map[string]bool{}
	}
	seen := r.seenConflicts[sessionID]
	if seen == nil {
		seen = map[string]bool{}
		r.seenConflicts[sessionID] = seen
	}
	for _, c := range all {
		if !seen[c.Song] {
			seen[c.Song] = true
			fresh = append(fresh, c)
		}
	}
	return all, fresh
}

// SessionSongs returns the custom songs the session knows of (lower-case): those its members' loaders hold, lack,
// or asked other players for (known), and those some member went without (missing: a fetch its loader gave up on,
// or a transfer that ran out of owners, until the member reports holding the song).
func (r *Registry) SessionSongs(sessionID string, isMember func(int64) bool) (known, missing map[string]bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	known, missing = map[string]bool{}, map[string]bool{}
	for id, h := range r.held[sessionID] {
		if !isMember(id) {
			continue
		}
		for x := range h.have {
			known[x] = true
		}
		for x := range h.missing {
			known[x], missing[x] = true, true
		}
	}
	for _, t := range r.transfers {
		if t.SessionID == sessionID {
			known[strings.ToLower(t.Song)] = true
		}
	}
	for x, players := range r.failed[sessionID] {
		for id := range players {
			if isMember(id) {
				known[x], missing[x] = true, true
				break
			}
		}
	}
	return known, missing
}
