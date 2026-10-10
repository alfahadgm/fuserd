package songxfer

import (
	"errors"
	"strings"
	"testing"
	"time"

	"fuseronline/internal/p2p"
)

const (
	session = "gsess-1"
	path    = "/Game/DLC/Songs/JoWo_SeinThem_ShpQn/Meta_JoWo_SeinThem_ShpQn.Meta_JoWo_SeinThem_ShpQn"
	req     = int64(1)
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time             { return c.t }
func (c *clock) add(d time.Duration)        { c.t = c.t.Add(d) }
func members(ids ...int64) func(int64) bool { return func(id int64) bool { return contains(ids, id) } }

func contains(ids []int64, id int64) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}

func newTest() (*Registry, *clock) {
	c := &clock{time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)}
	r := New()
	r.now = c.now
	return r, c
}

func endpoint(port int) p2p.Endpoint {
	return p2p.Endpoint{Candidates: []p2p.Candidate{{Type: p2p.TypeSrflx, IP: "61.245.133.92", Port: port}}, Mapping: "endpoint-independent"}
}

func offer(port int) Offer {
	return Offer{Size: 7404350, SHA256: strings.Repeat("ab", 32), SigSize: 980, SigSHA256: strings.Repeat("cd", 32), Endpoint: endpoint(port)}
}

func TestShortName(t *testing.T) {
	for p, want := range map[string]string{
		path: "JoWo_SeinThem_ShpQn",
		"/Game/DLC/Songs/a-b_1/Meta_a-b_1.Meta_a-b_1": "a-b_1",
	} {
		if got, ok := ShortName(p); !ok || got != want {
			t.Errorf("%s: %q %v", p, got, ok)
		}
	}
	for _, p := range []string{
		"/Game/DLC/Songs/X/Meta_Y.Meta_X",       // folder and asset disagree
		"/Game/DLC/Songs/X/Meta_X",              // no object name
		"/Game/Audio/Songs/X/Meta_X.Meta_X",     // not a DLC song path
		"/Game/DLC/Songs/../Meta_...Meta_..",    // traversal
		"/Game/DLC/Songs/X Y/Meta_X Y.Meta_X Y", // a space
		"",
	} {
		if got, ok := ShortName(p); ok {
			t.Errorf("%q accepted as %q", p, got)
		}
	}
}

// The happy path: ask, offer, accept, transferring, done - and who sees what.
func TestTransferFlow(t *testing.T) {
	r, c := newTest()
	in := members(1, 2, 3)
	r.Poll(session, 2, true, in) // owner 2 polls and sends; 3 never polled
	tr, err := r.Request(session, req, path, []int64{3, 2, 1, 2, 99}, in)
	if err != nil {
		t.Fatal(err)
	}
	if tr.Song != "JoWo_SeinThem_ShpQn" || tr.State != StateAsked || len(tr.Owners) != 2 || tr.Owner() != 2 {
		t.Fatalf("request: %+v (want owners [2 3]: the polling sender first, requester/repeats/non-members dropped)", tr)
	}
	again, _ := r.Request(session, req, path, []int64{3}, in)
	if again.ID != tr.ID {
		t.Fatal("a second request for the same song must return the open one")
	}
	if got := r.Poll(session, 3, true, in); len(got) != 0 {
		t.Fatalf("owner 3 isn't current: %+v", got)
	}
	got := r.Poll(session, 2, true, in)
	if len(got) != 1 || got[0].ID != tr.ID || got[0].State != StateAsked {
		t.Fatalf("owner poll: %+v", got)
	}
	if _, err := r.Offer(session, tr.ID, 3, offer(40000)); !errors.Is(err, ErrNotYours) {
		t.Fatalf("offer by a non-current owner: %v", err)
	}
	if _, err := r.Offer(session, tr.ID, 2, Offer{Endpoint: endpoint(40000)}); !errors.Is(err, ErrBadOffer) {
		t.Fatalf("offer without hashes: %v", err)
	}
	of, err := r.Offer(session, tr.ID, 2, offer(40000))
	if err != nil || of.State != StateOffered || of.PairID == 0 || len(of.Key) != 64 || of.Offer.Size != 7404350 {
		t.Fatalf("offer: %+v %v", of, err)
	}
	c.add(time.Second)
	ac, err := r.Accept(session, tr.ID, req, endpoint(50000))
	if err != nil || ac.State != StateConnecting || ac.Want == nil || ac.PairID != of.PairID {
		t.Fatalf("accept: %+v %v", ac, err)
	}
	if _, err := r.Report(session, tr.ID, 2, ReportDone, "", in); !errors.Is(err, ErrNotYours) {
		t.Fatalf("only the requester reports done: %v", err)
	}
	if x, err := r.Report(session, tr.ID, 2, ReportTransferring, "", in); err != nil || x.State != StateTransferring {
		t.Fatalf("owner transferring: %+v %v", x, err)
	}
	if x, err := r.Report(session, tr.ID, req, ReportDone, "", in); err != nil || x.State != StateDone {
		t.Fatalf("requester done: %+v %v", x, err)
	}
	// Finished transfers stay visible for a minute, then go.
	if got := r.Poll(session, req, false, in); len(got) != 1 || got[0].State != StateDone {
		t.Fatalf("requester poll after done: %+v", got)
	}
	c.add(KeepFinished + time.Second)
	if got := r.Poll(session, req, false, in); len(got) != 0 {
		t.Fatalf("kept too long: %+v", got)
	}
	if ev := r.TakeEvents(); len(ev) != 0 {
		t.Fatalf("no owner changes expected: %+v", ev)
	}
}

// Every way an attempt moves on to the next owner, then failure when they run out.
func TestNextOwner(t *testing.T) {
	r, c := newTest()
	in := members(1, 2, 3, 4, 5, 6)
	tr, err := r.Request(session, req, path, []int64{2, 3, 4, 5, 6}, in)
	if err != nil {
		t.Fatal(err)
	}
	owner := func() int64 {
		t.Helper()
		for _, x := range r.Poll(session, req, false, in) {
			if x.ID == tr.ID {
				return x.Owner()
			}
		}
		t.Fatal("transfer gone")
		return 0
	}
	// 2 declines.
	if x, err := r.Decline(session, tr.ID, 2, "not_found", in); err != nil || x.Owner() != 3 || x.State != StateAsked {
		t.Fatalf("decline: %+v %v", x, err)
	}
	// 3 never answers.
	c.add(AskTimeout + time.Second)
	if o := owner(); o != 4 {
		t.Fatalf("after no answer: owner %d", o)
	}
	// 4 offers, the requester accepts, but no path comes up.
	if _, err := r.Offer(session, tr.ID, 4, offer(40000)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Accept(session, tr.ID, req, endpoint(50000)); err != nil {
		t.Fatal(err)
	}
	c.add(ConnectTimeout + time.Second)
	if o := owner(); o != 5 {
		t.Fatalf("after no path: owner %d", o)
	}
	// 5 sends a file the requester rejects.
	r.Offer(session, tr.ID, 5, offer(40000))
	r.Accept(session, tr.ID, req, endpoint(50000))
	if x, err := r.Report(session, tr.ID, req, "rejected", "wrong sha256", in); err != nil || x.Owner() != 6 || x.Key != "" {
		t.Fatalf("rejected: %+v %v (the next attempt starts without the old pair)", x, err)
	}
	// 6 polls without sending: declined at once, and the transfer fails.
	r.Poll(session, 6, false, in)
	x := r.Poll(session, req, false, in)
	if len(x) != 1 || x[0].State != StateFailed || len(x[0].Outcomes) != 5 {
		t.Fatalf("after the last owner: %+v", x)
	}
	if ev := r.TakeEvents(); len(ev) != 5 || ev[4].What != "failed" {
		t.Fatalf("events: %+v", ev)
	}
}

// Leaving: an owner who left is skipped; a requester who left cancels; a session ending cancels everything.
func TestMembership(t *testing.T) {
	r, _ := newTest()
	tr, _ := r.Request(session, req, path, []int64{2, 3}, members(1, 2, 3))
	if x := r.Poll(session, 3, true, members(1, 3)); len(x) != 1 || x[0].Owner() != 3 {
		t.Fatalf("owner 2 left: %+v", x)
	}
	if x := r.Poll(session, 3, true, members(3)); len(x) != 1 || x[0].State != StateCancelled {
		t.Fatalf("requester left: %+v", x)
	}
	tr2, err := r.Request(session, req, path, []int64{2}, members(1, 2))
	if err != nil || tr2.ID == tr.ID {
		t.Fatalf("a new request after a cancelled one: %+v %v", tr2, err)
	}
	r.Drop(session)
	if x := r.Poll(session, req, false, members(1, 2)); len(x) != 2 || x[1].State != StateCancelled {
		t.Fatalf("after the session ended: %+v", x)
	}
	if _, err := r.Request(session, req, path, []int64{1, 99}, members(1, 2)); !errors.Is(err, ErrNoOwner) {
		t.Fatalf("no owner: %v", err)
	}
	r.Poll(session, 2, false, members(1, 2)) // 2 says it doesn't send
	if _, err := r.Request(session, req, path, []int64{2}, members(1, 2)); !errors.Is(err, ErrNoOwner) {
		t.Fatalf("only a known non-sender: %v", err)
	}
	if _, err := r.Request(session, req, "/Game/DLC/Songs/X/Meta_Y.Meta_Y", []int64{2}, members(1, 2)); !errors.Is(err, ErrBadPath) {
		t.Fatalf("bad path: %v", err)
	}
}

func TestLimitsAndSteps(t *testing.T) {
	r, c := newTest()
	in := members(1, 2)
	for i := 0; i < MaxOpenPerRequester; i++ {
		p := "/Game/DLC/Songs/S" + string(rune('a'+i)) + "/Meta_S" + string(rune('a'+i)) + ".Meta_S" + string(rune('a'+i))
		if _, err := r.Request(session, req, p, []int64{2}, in); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.Request(session, req, path, []int64{2}, in); !errors.Is(err, ErrTooMany) {
		t.Fatalf("over the open-request limit: %v", err)
	}
	tr, _ := r.Request(session, 2, path, []int64{1}, in)
	if _, err := r.Accept(session, tr.ID, 2, endpoint(1)); !errors.Is(err, ErrWrongStep) {
		t.Fatalf("accept before an offer: %v", err)
	}
	if _, err := r.Report(session, tr.ID, 1, "failed", "", in); !errors.Is(err, ErrWrongStep) {
		t.Fatalf("an owner's failure before offering (it should decline): %v", err)
	}
	if _, err := r.Offer("gsess-other", tr.ID, 1, offer(1)); !errors.Is(err, ErrUnknown) {
		t.Fatalf("another session's transfer: %v", err)
	}
	r.Offer(session, tr.ID, 1, offer(40000))
	c.add(AcceptTimeout + time.Second)
	if x := r.Poll(session, 2, false, in); len(x) != 1 || x[0].State != StateFailed {
		t.Fatalf("the requester didn't accept: %+v", x)
	}
	tr3, _ := r.Request(session, 2, "/Game/DLC/Songs/Q/Meta_Q.Meta_Q", []int64{1}, in)
	if x, err := r.Report(session, tr3.ID, 2, ReportCancel, "installed meanwhile", in); err != nil || x.State != StateCancelled {
		t.Fatalf("withdrawn: %+v %v", x, err)
	}
}

// Sync rounds: a round starts when a player fetches, collects every fetching player's failures, ends when nobody
// polling is fetching, and a later fetch starts another.
func TestSyncRounds(t *testing.T) {
	r, c := newTest()
	in := members(1, 2, 3)
	if v, ch := r.Sync(session, 1, false, 0, in); v.Round != 0 || v.Active || ch != SyncNoChange {
		t.Fatalf("idle: %+v %v", v, ch)
	}
	if v, ch := r.Sync(session, 2, true, 0, in); !v.Active || v.Round != 1 || v.Syncing != 1 || ch != SyncStarted {
		t.Fatalf("B starts: %+v %v", v, ch)
	}
	r.Sync(session, 3, true, 0, in)
	if v, ch := r.Sync(session, 2, false, 1, in); !v.Active || v.Syncing != 1 || ch != SyncNoChange {
		t.Fatalf("B done, C still fetching: %+v %v", v, ch)
	}
	v, ch := r.Sync(session, 3, false, 0, in)
	if v.Active || v.Done != 1 || v.Players != 2 || v.Failed != 1 || ch != SyncFinished {
		t.Fatalf("all done: %+v %v", v, ch)
	}
	if v, _ := r.Sync(session, 1, false, 0, in); v.Done != 1 || v.Active {
		t.Fatalf("A sees the finished round: %+v", v)
	}
	// a second round; a fetching player who stops polling no longer holds it open
	r.Sync(session, 2, true, 0, in)
	c.add(PresenceTimeout + time.Second)
	if v, ch := r.Sync(session, 1, false, 0, in); v.Active || v.Done != 2 || ch != SyncFinished {
		t.Fatalf("B stopped polling: %+v %v", v, ch)
	}
	r.Drop(session)
	if v, _ := r.Sync(session, 1, false, 0, in); v.Round != 0 {
		t.Fatalf("after the session ended: %+v", v)
	}
}
