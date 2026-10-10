// Package songxfer is the signaling for song transfers between players. A player missing a custom
// song that another player's crate holds asks for it; the request goes to one owner at a time, and an owner that
// sends answers with the file's size and hashes and its tunnel candidates. The requester then registers its own
// candidates, both sides get a pair id and key, and their loaders punch a path and move the file directly. No file
// data passes through here.
//
// A transfer moves on to the next owner when its current owner declines, doesn't answer, can't be reached, stalls or
// sends a file that fails the requester's checks; it fails when the owners run out, and it is cancelled when the
// requester leaves the session or the session ends. Membership comes from the caller on every call (the API checks
// matchmaking), so a player who left is noticed at the next call. State is in memory; finished transfers are kept
// for a minute so both sides see how they ended.
package songxfer

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"fuseronline/internal/p2p"
)

// Transfer states.
const (
	StateAsked        = "asked"        // waiting for the current owner's offer or decline
	StateOffered      = "offered"      // the owner offered; waiting for the requester's candidates
	StateConnecting   = "connecting"   // both sides have the pair; the path isn't up yet
	StateTransferring = "transferring" // a side reported the path up and bytes moving
	StateDone         = "done"
	StateFailed       = "failed"    // no owner left
	StateCancelled    = "cancelled" // the requester left, or the session ended
)

// Limits and timeouts.
const (
	MaxOwners           = 8
	MaxOpenPerRequester = 8                // an owner at its own upload limit declines with "busy"
	AskTimeout          = 15 * time.Second // the owner neither offers nor declines
	AcceptTimeout       = 15 * time.Second // the requester doesn't register its candidates
	ConnectTimeout      = 20 * time.Second // no side reports the path up
	TransferTimeout     = 5 * time.Minute  // one owner's attempt, start to finish
	PresenceTimeout     = 10 * time.Second // a poll older than this: the player isn't polling
	KeepFinished        = time.Minute
)

// Errors the API maps to client errors.
var (
	ErrBadPath   = errors.New("not a custom song path (/Game/DLC/Songs/<X>/Meta_<X>.Meta_<X>)")
	ErrNoOwner   = errors.New("no other member of the session holds the song")
	ErrTooMany   = errors.New("too many open song requests")
	ErrUnknown   = errors.New("unknown transfer")
	ErrNotYours  = errors.New("not a party to this transfer")
	ErrWrongStep = errors.New("the transfer is not at that step")
	ErrBadOffer  = errors.New("offer needs size > 0, sha256 and sig_sha256 (64 hex digits), and sig_size > 0")
	ErrBadInput  = errors.New("bad input") // wraps an endpoint that fails p2p.Endpoint.Validate, or an over-long field
)

// ShortName returns <X> of a crate song path /Game/DLC/Songs/<X>/Meta_<X>.Meta_<X>, the only shape a request may
// name (all community paks use it).
func ShortName(path string) (string, bool) {
	rest, ok := strings.CutPrefix(path, "/Game/DLC/Songs/")
	if !ok {
		return "", false
	}
	x, file, ok := strings.Cut(rest, "/")
	if !ok || !validShort(x) || file != "Meta_"+x+".Meta_"+x {
		return "", false
	}
	return x, true
}

// Offer is the owner's answer: what it will send (pak, then .sig) and where it can be reached.
type Offer struct {
	Size      int64  `json:"size"`
	SHA256    string `json:"sha256"`
	SigSize   int64  `json:"sig_size"`
	SigSHA256 string `json:"sig_sha256"`
	p2p.Endpoint
}

func isHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

// Validate checks an offer's file description and its endpoint.
func (o Offer) Validate() error {
	if o.Size <= 0 || o.SigSize <= 0 || !isHex64(o.SHA256) || !isHex64(o.SigSHA256) {
		return ErrBadOffer
	}
	if err := o.Endpoint.Validate(); err != nil {
		return fmt.Errorf("%w: %v", ErrBadInput, err)
	}
	return nil
}

// Outcome is how one owner's attempt ended.
type Outcome struct {
	Owner  int64
	Reason string
}

// Transfer is one request, as handed out (a copy).
type Transfer struct {
	ID        string
	SessionID string
	Path      string
	Song      string
	Requester int64
	Owners    []int64
	Current   int // index into Owners; len(Owners) when they ran out
	State     string
	Reason    string // why it failed or was cancelled
	// The current attempt.
	PairID       uint32
	Key          string
	Offer        *Offer
	Want         *p2p.Endpoint // the requester's candidates
	AttemptStart time.Time
	Outcomes     []Outcome
	Created      time.Time
	Updated      time.Time
}

// Owner is the current owner's player id, or 0.
func (t Transfer) Owner() int64 {
	if t.Current < len(t.Owners) {
		return t.Owners[t.Current]
	}
	return 0
}

// Final reports whether the transfer has ended.
func (t Transfer) Final() bool {
	return t.State == StateDone || t.State == StateFailed || t.State == StateCancelled
}

func (t Transfer) active() bool { return t.State == StateConnecting || t.State == StateTransferring }

// Event is something the API logs: a transfer moving on to another owner, failing, or being cancelled.
type Event struct {
	Transfer Transfer
	What     string // "next owner", "failed", "cancelled"
	Owner    int64  // the owner the attempt was with
	Reason   string
}

type presence struct {
	seen    time.Time
	sending bool
}

// Registry holds the transfers. It is safe for concurrent use.
type Registry struct {
	now func() time.Time

	mu        sync.Mutex
	transfers map[string]*Transfer
	presence  map[string]map[int64]presence // session -> player -> last poll
	events    []Event
	syncs     map[string]map[int64]syncPlayer // session -> player -> its sync state (Sync)
	rounds    map[string]*syncRound
	// song versions (versions.go)
	held          map[string]map[int64]heldSongs       // session -> player -> its last report
	failed        map[string]map[string]map[int64]bool // session -> song -> requesters whose transfer failed
	seenConflicts map[string]map[string]bool           // session -> conflicts already returned
}

// New makes an empty registry.
func New() *Registry {
	return &Registry{now: time.Now, transfers: map[string]*Transfer{}, presence: map[string]map[int64]presence{}}
}

// TakeEvents returns the events since the last call, for logging.
func (r *Registry) TakeEvents() []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.events
	r.events = nil
	return out
}

func randomID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "xfer-" + hex.EncodeToString(b[:]), nil
}

func newPair() (uint32, string, error) {
	var b [36]byte
	for {
		if _, err := rand.Read(b[:]); err != nil {
			return 0, "", err
		}
		if id := binary.LittleEndian.Uint32(b[:4]); id != 0 {
			return id, hex.EncodeToString(b[4:]), nil
		}
	}
}

// uploads counts the owner's attempts in progress. Callers hold mu.
func (r *Registry) uploads(owner int64) int {
	n := 0
	for _, t := range r.transfers {
		if t.Owner() == owner && t.active() {
			n++
		}
	}
	return n
}

// sending tells whether a player polls and says it sends: true, false, or unknown (never polled, or not lately).
func (r *Registry) sending(sessionID string, player int64) (known, sends bool) {
	p, ok := r.presence[sessionID][player]
	if !ok || r.now().Sub(p.seen) > PresenceTimeout {
		return false, false
	}
	return true, p.sending
}

// order sorts owners for asking: polling senders first, then the less busy, then unknowns; players known not to
// send are left out. Callers hold mu.
func (r *Registry) order(sessionID string, owners []int64) []int64 {
	type ranked struct {
		id          int64
		rank, busy  int
		originalPos int
	}
	var rs []ranked
	for i, id := range owners {
		known, sends := r.sending(sessionID, id)
		if known && !sends {
			continue
		}
		rank := 1
		if known && sends {
			rank = 0
		}
		rs = append(rs, ranked{id, rank, r.uploads(id), i})
	}
	sort.SliceStable(rs, func(i, j int) bool {
		if rs[i].rank != rs[j].rank {
			return rs[i].rank < rs[j].rank
		}
		if rs[i].busy != rs[j].busy {
			return rs[i].busy < rs[j].busy
		}
		return rs[i].originalPos < rs[j].originalPos
	})
	out := make([]int64, 0, len(rs))
	for _, x := range rs {
		out = append(out, x.id)
	}
	return out
}

// Request opens a request (or returns the requester's open one for the same song). owners are the players whose
// crates hold the song; non-members, the requester and repeats are dropped.
func (r *Registry) Request(sessionID string, requester int64, path string, owners []int64, isMember func(int64) bool) (Transfer, error) {
	song, ok := ShortName(path)
	if !ok {
		return Transfer{}, ErrBadPath
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	open := 0
	for _, t := range r.transfers {
		if t.Requester != requester || t.Final() {
			continue
		}
		if t.SessionID == sessionID && t.Path == path {
			return r.copyOf(t), nil
		}
		open++
	}
	if open >= MaxOpenPerRequester {
		return Transfer{}, ErrTooMany
	}
	seen := map[int64]bool{requester: true}
	var valid []int64
	for _, id := range owners {
		if !seen[id] && isMember(id) && len(valid) < MaxOwners {
			valid = append(valid, id)
		}
		seen[id] = true
	}
	valid = r.order(sessionID, valid)
	if len(valid) == 0 {
		return Transfer{}, ErrNoOwner
	}
	id, err := randomID()
	if err != nil {
		return Transfer{}, err
	}
	now := r.now()
	t := &Transfer{ID: id, SessionID: sessionID, Path: path, Song: song, Requester: requester, Owners: valid,
		State: StateAsked, AttemptStart: now, Created: now, Updated: now}
	r.transfers[id] = t
	return r.copyOf(t), nil
}

func (r *Registry) copyOf(t *Transfer) Transfer {
	c := *t
	c.Owners = append([]int64(nil), t.Owners...)
	c.Outcomes = append([]Outcome(nil), t.Outcomes...)
	if t.Offer != nil {
		o := *t.Offer
		c.Offer = &o
	}
	if t.Want != nil {
		w := *t.Want
		c.Want = &w
	}
	return c
}

// advance ends the current owner's attempt with reason and asks the next owner that is still a member and not
// known to refuse; the transfer fails when none is left. Callers hold mu.
func (r *Registry) advance(t *Transfer, reason string, isMember func(int64) bool) {
	from := t.Owner()
	t.Outcomes = append(t.Outcomes, Outcome{from, reason})
	t.PairID, t.Key, t.Offer, t.Want = 0, "", nil, nil
	now := r.now()
	t.Updated, t.AttemptStart = now, now
	for t.Current++; t.Current < len(t.Owners); t.Current++ {
		next := t.Owners[t.Current]
		known, sends := r.sending(t.SessionID, next)
		if isMember != nil && !isMember(next) {
			t.Outcomes = append(t.Outcomes, Outcome{next, "left the session"})
			continue
		}
		if known && !sends {
			t.Outcomes = append(t.Outcomes, Outcome{next, "disabled"})
			continue
		}
		t.State = StateAsked
		r.events = append(r.events, Event{r.copyOf(t), "next owner", from, reason})
		return
	}
	t.State, t.Reason = StateFailed, "no owner could send it (last: "+reason+")"
	r.noteFailed(t)
	r.events = append(r.events, Event{r.copyOf(t), "failed", from, reason})
}

func (r *Registry) cancel(t *Transfer, reason string) {
	t.State, t.Reason, t.Updated = StateCancelled, reason, r.now()
	r.events = append(r.events, Event{r.copyOf(t), "cancelled", t.Owner(), reason})
}

// sweep applies the timeouts and membership to one session's transfers, and forgets old finished ones. Callers
// hold mu.
func (r *Registry) sweep(sessionID string, isMember func(int64) bool) {
	now := r.now()
	for id, t := range r.transfers {
		if t.Final() {
			if now.Sub(t.Updated) > KeepFinished {
				delete(r.transfers, id)
			}
			continue
		}
		if t.SessionID != sessionID {
			continue
		}
		if !isMember(t.Requester) {
			r.cancel(t, "the requester left the session")
			continue
		}
		if !isMember(t.Owner()) {
			r.advance(t, "the owner left the session", isMember)
			continue
		}
		age := now.Sub(t.AttemptStart)
		switch {
		case t.State == StateAsked && age > AskTimeout:
			r.advance(t, "no answer", isMember)
		case t.State == StateOffered && now.Sub(t.Updated) > AcceptTimeout:
			r.advance(t, "the requester didn't accept", isMember)
		case t.State == StateConnecting && now.Sub(t.Updated) > ConnectTimeout:
			r.advance(t, "no path", isMember)
		case t.active() && age > TransferTimeout:
			r.advance(t, "took too long", isMember)
		}
	}
}

// Poll records that player polls in the session and whether it sends songs, applies the session's timeouts, and
// returns the transfers the player is a party to (finished ones for a minute). A player that doesn't send declines
// every request waiting on it.
func (r *Registry) Poll(sessionID string, player int64, sending bool, isMember func(int64) bool) []Transfer {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.presence[sessionID] == nil {
		r.presence[sessionID] = map[int64]presence{}
	}
	r.presence[sessionID][player] = presence{r.now(), sending}
	r.sweep(sessionID, isMember)
	var out []Transfer
	for _, t := range r.transfers {
		if t.SessionID != sessionID {
			continue
		}
		if !sending && t.State == StateAsked && t.Owner() == player {
			r.advance(t, "disabled", isMember)
		}
		if t.Requester == player || t.Owner() == player {
			out = append(out, r.copyOf(t))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.Before(out[j].Created) })
	return out
}

// get finds a live transfer of the session. Callers hold mu.
func (r *Registry) get(id, sessionID string) (*Transfer, error) {
	t, ok := r.transfers[id]
	if !ok || t.SessionID != sessionID {
		return nil, ErrUnknown
	}
	return t, nil
}

// Offer is the current owner's answer to a waiting request. It gets the pair id and key.
func (r *Registry) Offer(sessionID, id string, owner int64, o Offer) (Transfer, error) {
	if err := o.Validate(); err != nil {
		return Transfer{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	t, err := r.get(id, sessionID)
	if err != nil {
		return Transfer{}, err
	}
	if t.Owner() != owner {
		return Transfer{}, ErrNotYours
	}
	if t.State != StateAsked {
		return Transfer{}, ErrWrongStep
	}
	if t.PairID, t.Key, err = newPair(); err != nil {
		return Transfer{}, err
	}
	t.Offer, t.State, t.Updated = &o, StateOffered, r.now()
	return r.copyOf(t), nil
}

// Decline is the current owner turning a waiting request down; the next owner is asked.
func (r *Registry) Decline(sessionID, id string, owner int64, reason string, isMember func(int64) bool) (Transfer, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, err := r.get(id, sessionID)
	if err != nil {
		return Transfer{}, err
	}
	if t.Owner() != owner {
		return Transfer{}, ErrNotYours
	}
	if t.State != StateAsked {
		return Transfer{}, ErrWrongStep
	}
	r.advance(t, "declined: "+reason, isMember)
	return r.copyOf(t), nil
}

// Accept is the requester registering its candidates for the offered pair (again later, if its network changed).
func (r *Registry) Accept(sessionID, id string, requester int64, ep p2p.Endpoint) (Transfer, error) {
	if err := ep.Validate(); err != nil {
		return Transfer{}, fmt.Errorf("%w: %v", ErrBadInput, err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	t, err := r.get(id, sessionID)
	if err != nil {
		return Transfer{}, err
	}
	if t.Requester != requester {
		return Transfer{}, ErrNotYours
	}
	if t.State != StateOffered && !t.active() {
		return Transfer{}, ErrWrongStep
	}
	t.Want, t.Updated = &ep, r.now()
	if t.State == StateOffered {
		t.State = StateConnecting
	}
	return r.copyOf(t), nil
}

// Report outcomes. From the requester: "transferring", "done", or a failure that moves on to the next owner
// ("no_path", "stalled", "rejected", "failed"). From the owner: "transferring", or "failed". "cancel" from the
// requester withdraws the request.
const (
	ReportTransferring = "transferring"
	ReportDone         = "done"
	ReportCancel       = "cancel"
)

// Report applies a party's report to the current attempt.
func (r *Registry) Report(sessionID, id string, player int64, state, reason string, isMember func(int64) bool) (Transfer, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, err := r.get(id, sessionID)
	if err != nil {
		return Transfer{}, err
	}
	requester, owner := player == t.Requester, player == t.Owner() && t.Owner() != 0
	if !requester && !owner {
		return Transfer{}, ErrNotYours
	}
	if t.Final() {
		return r.copyOf(t), nil // late reports about an ended transfer change nothing
	}
	switch {
	case state == ReportTransferring:
		if !t.active() {
			return Transfer{}, ErrWrongStep
		}
		t.State, t.Updated = StateTransferring, r.now()
	case state == ReportDone && requester:
		if !t.active() {
			return Transfer{}, ErrWrongStep
		}
		t.State, t.Updated = StateDone, r.now()
	case state == ReportCancel && requester:
		r.cancel(t, "withdrawn: "+reason)
	case state == ReportDone || state == ReportCancel:
		return Transfer{}, ErrNotYours // only the requester knows the file arrived, or may withdraw
	default:
		if len(state) > 32 || len(reason) > 160 {
			return Transfer{}, fmt.Errorf("%w: state or reason too long", ErrBadInput)
		}
		if t.State == StateAsked && owner {
			return Transfer{}, ErrWrongStep // an owner turns a request down with decline
		}
		side := "requester"
		if owner {
			side = "owner"
		}
		why := side + ": " + state
		if reason != "" {
			why += " (" + reason + ")"
		}
		r.advance(t, why, isMember)
	}
	return r.copyOf(t), nil
}

// Drop cancels a session's transfers and forgets its presence, sync rounds and song reports (the session ended).
func (r *Registry) Drop(sessionID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, t := range r.transfers {
		if t.SessionID == sessionID && !t.Final() {
			r.cancel(t, "the session ended")
		}
	}
	delete(r.presence, sessionID)
	delete(r.syncs, sessionID)
	delete(r.rounds, sessionID)
	delete(r.held, sessionID)
	delete(r.failed, sessionID)
	delete(r.seenConflicts, sessionID)
}

// ---- sync rounds ---------------------------------------------------------------------------------------------
// Every player's loader says in its poll whether it is fetching custom songs right now (from fuserd or from other
// players) and how many of its current batch it couldn't get. A round starts when any player starts fetching and
// ends when no player still polling is fetching; every loader then tells its player once that all players are done.

// SyncView is the session's custom-song synchronisation, as a poll answer carries it.
type SyncView struct {
	Round   int  `json:"round"`      // the current or last round (0: none yet)
	Active  bool `json:"active"`     // a round is under way
	Done    int  `json:"done_round"` // the last round that finished (0: none)
	Syncing int  `json:"syncing"`    // players fetching now
	Players int  `json:"players"`    // players who fetched songs in the last finished round
	Failed  int  `json:"failed"`     // songs they couldn't get
}

// SyncChange says what a Sync call changed, for the log.
type SyncChange int

const (
	SyncNoChange SyncChange = iota
	SyncStarted
	SyncFinished
)

type syncPlayer struct {
	seen    time.Time
	syncing bool
	failed  int
}

type syncRound struct {
	round, done             int
	active                  bool
	players                 map[int64]int // this round's fetching players -> their failed count
	donePlayers, doneFailed int
}

// Sync records a player's sync state and returns the session's.
func (r *Registry) Sync(sessionID string, player int64, syncing bool, failed int, isMember func(int64) bool) (SyncView, SyncChange) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.syncs == nil {
		r.syncs, r.rounds = map[string]map[int64]syncPlayer{}, map[string]*syncRound{}
	}
	if failed < 0 {
		failed = 0
	}
	now := r.now()
	players := r.syncs[sessionID]
	if players == nil {
		players = map[int64]syncPlayer{}
		r.syncs[sessionID] = players
	}
	players[player] = syncPlayer{now, syncing, failed}
	round := r.rounds[sessionID]
	if round == nil {
		round = &syncRound{}
		r.rounds[sessionID] = round
	}
	fetching := 0
	for id, p := range players {
		if !isMember(id) || now.Sub(p.seen) > PresenceTimeout {
			delete(players, id) // left, or stopped polling: no longer holds the round open
			continue
		}
		if p.syncing {
			fetching++
		}
	}
	change := SyncNoChange
	if fetching > 0 && !round.active {
		round.round++
		round.active, round.players = true, map[int64]int{}
		change = SyncStarted
	}
	if round.active {
		for id, p := range players {
			if _, took := round.players[id]; took || p.syncing {
				round.players[id] = p.failed // the latest count of everyone who fetched in this round
			}
		}
		if fetching == 0 {
			round.active, round.done = false, round.round
			round.donePlayers, round.doneFailed = len(round.players), 0
			for _, f := range round.players {
				round.doneFailed += f
			}
			change = SyncFinished
		}
	}
	return SyncView{Round: round.round, Active: round.active, Done: round.done, Syncing: fetching,
		Players: round.donePlayers, Failed: round.doneFailed}, change
}
